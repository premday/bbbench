// Package flushtrace measures how long the kernel takes to complete device
// Flush commands, by timing pairs of kernel trace events (issue -> complete)
// read from tracefs. fio only sees the whole fsync() call; this isolates the
// Flush itself.
//
// Two collectors are provided:
//   - NVMe, from the nvme:nvme_setup_cmd and nvme:nvme_complete_rq
//     tracepoints. Each command is identified by (controller, queue, cmdid),
//     so every Flush is paired with its own completion exactly.
//   - Generic block device, from block:block_rq_issue and
//     block:block_rq_complete. These carry no request identifier, so issues
//     and completions are paired first-in first-out per device. The mean is
//     exact regardless of pairing; individual samples (hence percentiles)
//     are only trustworthy when one Flush is in flight at a time.
package flushtrace

import "strings"

// Event is one parsed line of the ftrace text format.
type Event struct {
	TimeNs  int64  // timestamp in nanoseconds
	Name    string // event name, e.g. "block_rq_issue"
	Payload string // the event's own fields, as printed by the kernel (text reader; a marker's text on the raw reader)
	// F holds the fields decoded from a binary page (raw reader, F.Decoded);
	// the text reader leaves it zero and the collectors parse Payload.
	F Fields
}

// ParseLine parses one ftrace text line, e.g.
//
//	fio-1234  [003] .....  1234.567890: block_rq_issue: 259,0 F 0 () 0 + 0 ...
//
// The task name may itself contain '-' and spaces, so the "[cpu]" column is
// looked for rather than assumed. ok is false for anything that is not an
// event line (headers, comments, "[LOST N EVENTS]" lines). trace_marker
// writes parse fine but are ignored later because their name matches no
// collector. It runs for every traced I/O, so it is written by hand.
func ParseLine(line string) (ev Event, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	for from := 0; ; {
		k := strings.IndexByte(line[from:], '[')
		if k < 0 {
			return Event{}, false
		}
		k += from
		from = k + 1
		if ev, ok := parseAfterTask(line, k); ok {
			return ev, true
		}
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\v' || c == '\f' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// parseAfterTask tries the line with "[cpu]" starting at index k.
func parseAfterTask(line string, k int) (Event, bool) {
	// "<task>-<pid>" then whitespace, before the '['.
	j := k
	for j > 0 && isSpace(line[j-1]) {
		j--
	}
	if j == k { // no whitespace before '['
		return Event{}, false
	}
	d := j
	for d > 0 && isDigit(line[d-1]) {
		d--
	}
	if d == j || d < 2 || line[d-1] != '-' {
		return Event{}, false
	}
	if strings.TrimLeft(line[:d-1], " \t\v\f") == "" { // task name must not be empty
		return Event{}, false
	}
	// "[cpu]"
	p := k + 1
	for p < len(line) && isDigit(line[p]) {
		p++
	}
	if p == k+1 || p >= len(line) || line[p] != ']' {
		return Event{}, false
	}
	p++
	// whitespace, flags column, whitespace
	q := p
	for q < len(line) && isSpace(line[q]) {
		q++
	}
	if q == p {
		return Event{}, false
	}
	p = q
	for q < len(line) && !isSpace(line[q]) {
		q++
	}
	if q == p {
		return Event{}, false
	}
	p = q
	for q < len(line) && isSpace(line[q]) {
		q++
	}
	if q == p {
		return Event{}, false
	}
	p = q
	// seconds.fraction:
	var sec int64
	for q < len(line) && isDigit(line[q]) {
		sec = sec*10 + int64(line[q]-'0')
		q++
	}
	if q == p || q >= len(line) || line[q] != '.' {
		return Event{}, false
	}
	q++
	fs := q
	var frac int64
	digits := 0
	for q < len(line) && isDigit(line[q]) {
		if digits < 9 {
			frac = frac*10 + int64(line[q]-'0')
			digits++
		}
		q++
	}
	if q == fs || q >= len(line) || line[q] != ':' {
		return Event{}, false
	}
	for ; digits < 9; digits++ {
		frac *= 10
	}
	q++
	// whitespace, event name, ':'
	p = q
	for q < len(line) && isSpace(line[q]) {
		q++
	}
	if q == p {
		return Event{}, false
	}
	p = q
	for q < len(line) && (line[q] == '_' || isDigit(line[q]) || line[q]|0x20 >= 'a' && line[q]|0x20 <= 'z') {
		q++
	}
	if q == p || q >= len(line) || line[q] != ':' {
		return Event{}, false
	}
	name := line[p:q]
	q++
	if q < len(line) && isSpace(line[q]) {
		q++
	}
	return Event{TimeNs: sec*1_000_000_000 + frac, Name: name, Payload: line[q:]}, true
}
