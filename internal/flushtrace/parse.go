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

import (
	"regexp"
	"strings"
)

// Event is one parsed line of the ftrace text format.
type Event struct {
	TimeNs  int64  // timestamp in nanoseconds
	Name    string // event name, e.g. "block_rq_issue"
	Payload string // the event's own fields, as printed by the kernel
}

// lineRe matches e.g.
//
//	fio-1234  [003] .....  1234.567890: block_rq_issue: 259,0 F 0 () 0 + 0 ...
//
// The task name may itself contain '-' and spaces, hence the greedy prefix.
var lineRe = regexp.MustCompile(`^\s*.+-\d+\s+\[\d+\]\s+\S+\s+(\d+)\.(\d+):\s+(\w+):\s?(.*)$`)

// ParseLine parses one ftrace text line. ok is false for anything that is not
// an event line (headers, comments, trace_marker writes parse fine but are
// ignored later because their name matches no collector).
func ParseLine(line string) (ev Event, ok bool) {
	m := lineRe.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
	if m == nil {
		return Event{}, false
	}
	var sec, frac int64
	for _, c := range m[1] {
		sec = sec*10 + int64(c-'0')
	}
	f := m[2]
	if len(f) > 9 {
		f = f[:9]
	}
	for len(f) < 9 {
		f += "0"
	}
	for _, c := range f {
		frac = frac*10 + int64(c-'0')
	}
	return Event{TimeNs: sec*1_000_000_000 + frac, Name: m[3], Payload: m[4]}, true
}
