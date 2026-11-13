package tracefake

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Event is one trace event of a fake stream, or (Lost > 0) a ring-buffer
// overrun of Lost events on CPU before its next page.
type Event struct {
	CPU    int
	TimeNs int64
	Name   string // "block_rq_issue", ..., "nvme_setup_cmd", "print" (a trace_marker write)

	Dev    uint32 // kernel dev_t: major<<20 | minor
	Sector uint64
	Nr     uint32
	RWBS   string
	Comm   string

	CtrlID int
	Disk   string
	QID    int
	CID    int
	Opcode int

	Text string // the trace_marker text of a print event

	Lost int64
}

// Dev is the kernel encoding of major:minor.
func Dev(maj, min int) uint32 { return uint32(maj)<<20 | uint32(min) }

// PageOptions vary how the stream is encoded, so every record type of the
// ring buffer is exercised.
type PageOptions struct {
	LongEvery     int  // every k-th data event uses type_len 0 (length word) even when small
	AbsoluteEvery int  // every k-th event is preceded by an absolute time stamp (type 31)
	PadEvery      int  // every k-th event is preceded by a discarded event (padding, part of the delta)
	NoRoomForLost bool // a page after an overrun has the missed flag but no stored count
}

const (
	typePadding = 29
	typeExtend  = 30
	typeStamp   = 31
	maxDelta    = 1<<27 - 1
	missed      = 1 << 31
	missedSaved = 1 << 30
	dataOff     = 16
)

type cpuPage struct {
	buf     []byte
	used    int // bytes of data
	started bool
	lastTs  int64
	missed  int64
	pages   [][]byte
	n       int
}

// Pages encodes the events into ring-buffer pages, per CPU, in the order
// given (a CPU's events must be in time order unless AbsoluteEvery places a
// stamp before a backward step: a backward step always gets one).
func (t *Tree) Pages(evs []Event, o PageOptions) (map[int][][]byte, error) {
	cpus := map[int]*cpuPage{}
	get := func(c int) *cpuPage {
		p := cpus[c]
		if p == nil {
			p = &cpuPage{}
			cpus[c] = p
		}
		return p
	}
	for _, ev := range evs {
		p := get(ev.CPU)
		if ev.Lost > 0 {
			t.finish(p, o)
			p.missed += ev.Lost
			continue
		}
		entry, err := t.entry(ev)
		if err != nil {
			return nil, err
		}
		p.n++
		long := o.LongEvery > 0 && p.n%o.LongEvery == 0
		data := dataRecord(entry, long)
		extra := 16 // room for an extend or a stamp, plus a padding
		if o.PadEvery > 0 {
			extra += 8
		}
		if !p.started || dataOff+p.used+4*len(data)+extra > t.PageSize {
			t.finish(p, o)
			p.buf = make([]byte, t.PageSize)
			p.started = true
			p.used = 0
			binary.LittleEndian.PutUint64(p.buf[0:], uint64(ev.TimeNs))
			p.lastTs = ev.TimeNs
		}
		delta := ev.TimeNs - p.lastTs
		if delta < 0 || (o.AbsoluteEvery > 0 && p.n%o.AbsoluteEvery == 0) {
			ts := uint64(ev.TimeNs) & (1<<59 - 1)
			p.put(header(typeStamp, uint32(ts&maxDelta)), uint32(ts>>27))
			delta = 0
		} else {
			if o.PadEvery > 0 && p.n%o.PadEvery == 0 && delta > 1 {
				d := delta / 2
				p.put(header(typePadding, uint32(d)), 4) // a discarded event: header + length word
				delta -= d
			}
			if delta > maxDelta {
				p.put(header(typeExtend, uint32(delta&maxDelta)), uint32(delta>>27))
				delta = 0
			}
		}
		h := data[0] | uint32(delta)<<5
		p.put(h)
		p.putBytes(data[1:])
		p.lastTs = ev.TimeNs
	}
	out := map[int][][]byte{}
	for c, p := range cpus {
		t.finish(p, o)
		if p.missed > 0 { // an overrun at the end: an empty page carries it
			p.buf, p.started, p.used = make([]byte, t.PageSize), true, 0
			t.finish(p, o)
		}
		out[c] = p.pages
	}
	return out, nil
}

func header(typ int, delta uint32) uint32 { return uint32(typ) | delta<<5 }

func (p *cpuPage) put(words ...uint32) {
	for _, w := range words {
		binary.LittleEndian.PutUint32(p.buf[dataOff+p.used:], w)
		p.used += 4
	}
}

func (p *cpuPage) putBytes(ws []uint32) { p.put(ws...) }

func (t *Tree) finish(p *cpuPage, o PageOptions) {
	if !p.started {
		return
	}
	commit := uint64(p.used)
	if p.missed > 0 {
		if !o.NoRoomForLost && dataOff+p.used+8 <= t.PageSize {
			binary.LittleEndian.PutUint64(p.buf[dataOff+p.used:], uint64(p.missed))
			commit |= missedSaved
		}
		commit |= missed
		p.missed = 0
	}
	binary.LittleEndian.PutUint64(p.buf[8:], commit)
	p.pages = append(p.pages, p.buf)
	p.started = false
}

// dataRecord returns the record's words: [0] the type_len bits of the
// header (delta added later), then the payload words.
func dataRecord(entry []byte, long bool) []uint32 {
	n := (len(entry) + 3) / 4
	padded := make([]byte, n*4)
	copy(padded, entry)
	var ws []uint32
	if long || n > 28 {
		ws = append(ws, 0, uint32(4+n*4))
	} else {
		ws = append(ws, uint32(n))
	}
	for i := 0; i < n; i++ {
		ws = append(ws, binary.LittleEndian.Uint32(padded[i*4:]))
	}
	return ws
}

// entry builds the trace entry of an event from its format.
func (t *Tree) entry(ev Event) ([]byte, error) {
	f := t.Formats[ev.Name]
	if f == nil {
		return nil, fmt.Errorf("tracefake: no format for %s", ev.Name)
	}
	b := make([]byte, f.Size)
	set := func(name string, v uint64) {
		x, ok := f.FieldByName(name)
		if !ok {
			return
		}
		switch x.Size {
		case 1:
			b[x.Offset] = byte(v)
		case 2:
			binary.LittleEndian.PutUint16(b[x.Offset:], uint16(v))
		case 4:
			binary.LittleEndian.PutUint32(b[x.Offset:], uint32(v))
		case 8:
			binary.LittleEndian.PutUint64(b[x.Offset:], v)
		}
	}
	str := func(name, v string) {
		if x, ok := f.FieldByName(name); ok {
			copy(b[x.Offset:x.Offset+x.Size-1], v)
		}
	}
	set("common_type", uint64(f.ID))
	set("common_pid", 1)
	set("dev", uint64(ev.Dev))
	set("sector", ev.Sector)
	set("nr_sector", uint64(ev.Nr))
	set("bytes", uint64(ev.Nr)*512)
	str("rwbs", ev.RWBS)
	str("comm", ev.Comm)
	str("disk", ev.Disk)
	set("ctrl_id", uint64(ev.CtrlID))
	set("qid", uint64(ev.QID))
	set("cid", uint64(ev.CID))
	set("opcode", uint64(ev.Opcode))
	if x, ok := f.FieldByName("cmd"); ok { // __data_loc: one NUL byte after the fixed part
		binary.LittleEndian.PutUint32(b[x.Offset:], uint32(1)<<16|uint32(len(b)))
		b = append(b, 0)
	}
	if _, ok := f.FieldByName("buf"); ok {
		b = append(b, ev.Text...)
		if !strings.HasSuffix(ev.Text, "\n") {
			b = append(b, '\n')
		}
		b = append(b, 0)
	}
	return b, nil
}

// WritePipes writes the pages as <instDir>/per_cpu/cpuN/trace_pipe_raw for
// CPUs 0..cpus-1 (a CPU without pages gets an empty file).
func WritePipes(instDir string, cpus int, pages map[int][][]byte) error {
	for c := 0; c < cpus; c++ {
		dir := filepath.Join(instDir, "per_cpu", "cpu"+strconv.Itoa(c))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		var all []byte
		for _, p := range pages[c] {
			all = append(all, p...)
		}
		if err := os.WriteFile(filepath.Join(dir, "trace_pipe_raw"), all, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Spread moves the events onto cpus CPUs at random (seeded), so a request
// issued on one CPU completes on another. Events with the timestamp of the
// previous one stay on its CPU, so ties keep the order given; an overrun
// stays on the CPU of the event that follows it.
func Spread(evs []Event, cpus int, seed int64) []Event {
	r := rand.New(rand.NewSource(seed))
	out := make([]Event, len(evs))
	copy(out, evs)
	cpu := 0
	for i := range out {
		if i == 0 || out[i].TimeNs != out[i-1].TimeNs || out[i-1].Lost > 0 {
			cpu = r.Intn(cpus)
		}
		out[i].CPU = cpu
	}
	for i := len(out) - 2; i >= 0; i-- {
		if out[i].Lost > 0 {
			out[i].CPU = out[i+1].CPU
		}
	}
	return out
}

// Sorted reports whether the events are in time order.
func Sorted(evs []Event) bool {
	for i := 1; i < len(evs); i++ {
		if evs[i].Lost == 0 && evs[i-1].Lost == 0 && evs[i].TimeNs < evs[i-1].TimeNs {
			return false
		}
	}
	return true
}

// ---- text ------------------------------------------------------------------

func secs(ns int64) string { return fmt.Sprintf("%d.%09d", ns/1_000_000_000, ns%1_000_000_000) }

func opName(qid, op int) string {
	if qid == 0 {
		return "nvme_admin_" + strconv.Itoa(op)
	}
	switch op {
	case 0:
		return "nvme_cmd_flush"
	case 1:
		return "nvme_cmd_write"
	case 2:
		return "nvme_cmd_read"
	case 9:
		return "nvme_cmd_dsm"
	}
	return "nvme_cmd_" + strconv.Itoa(op)
}

func opOf(name string) int {
	switch name {
	case "nvme_cmd_flush":
		return 0
	case "nvme_cmd_write":
		return 1
	case "nvme_cmd_read":
		return 2
	case "nvme_cmd_dsm":
		return 9
	}
	if n, err := strconv.Atoi(name[strings.LastIndexByte(name, '_')+1:]); err == nil {
		return n
	}
	return 0x7e
}

// Line renders the event as a trace_pipe text line, in the kernel's
// TP_printk shapes (with ns timestamps: the kernel prints µs by default,
// which ParseLine reads the same way).
func (e Event) Line() string {
	if e.Lost > 0 {
		return fmt.Sprintf("CPU:%d [LOST %d EVENTS]", e.CPU, e.Lost)
	}
	maj, min := e.Dev>>20, e.Dev&(1<<20-1)
	disk := ""
	if e.Disk != "" {
		disk = "disk=" + e.Disk + ", "
	}
	var p string
	switch e.Name {
	case "block_rq_insert", "block_rq_issue", "block_rq_merge":
		p = fmt.Sprintf("%d,%d %s %d () %d + %d none,0,0 [%s]", maj, min, e.RWBS, e.Nr*512, e.Sector, e.Nr, e.Comm)
	case "block_rq_complete":
		p = fmt.Sprintf("%d,%d %s () %d + %d none,0,0 [0]", maj, min, e.RWBS, e.Sector, e.Nr)
	case "block_bio_queue":
		p = fmt.Sprintf("%d,%d %s %d + %d [%s]", maj, min, e.RWBS, e.Sector, e.Nr, e.Comm)
	case "block_bio_complete":
		p = fmt.Sprintf("%d,%d %s %d + %d [0]", maj, min, e.RWBS, e.Sector, e.Nr)
	case "nvme_setup_cmd":
		p = fmt.Sprintf("nvme%d: %sqid=%d, cmdid=%d, nsid=1, flags=0x0, meta=0x0, cmd=(%s slba=0, len=7)", e.CtrlID, disk, e.QID, e.CID, opName(e.QID, e.Opcode))
	case "nvme_complete_rq":
		p = fmt.Sprintf("nvme%d: %sqid=%d, cmdid=%d, res=0x0, retries=0, flags=0x0, status=0x0", e.CtrlID, disk, e.QID, e.CID)
	case "print":
		return fmt.Sprintf("bbbench-1 [%03d] ..... %s: tracing_mark_write: %s", e.CPU, secs(e.TimeNs), e.Text)
	}
	return fmt.Sprintf("fio-1 [%03d] ..... %s: %s: %s", e.CPU, secs(e.TimeNs), e.Name, p)
}

var (
	lineRe   = regexp.MustCompile(`^\s*(.*?)-(\d+)\s+\[(\d+)\]\s+(\S+)\s+(\d+)\.(\d+):\s+(\w+):\s?(.*)$`)
	lostRe   = regexp.MustCompile(`^CPU:(\d+) \[LOST (\d+) EVENTS\]`)
	rqRe     = regexp.MustCompile(`^(\d+),(\d+) (\S+) (\d+) \(.*?\) (\d+) \+ (\d+)(?: \S+ \[(.*)\])?`)
	rqDoneRe = regexp.MustCompile(`^(\d+),(\d+) (\S+) \(.*?\) (\d+) \+ (\d+)`)
	bioRe    = regexp.MustCompile(`^(\d+),(\d+) (\S+) (\d+) \+ (\d+)(?: \[(.*)\])?`)
	nvmeRe   = regexp.MustCompile(`^nvme(\d+): (?:disk=([^,]+), )?qid=(\d+), cmdid=(\d+),(?:.*cmd=\((\w+))?`)
)

// FromText reads trace_pipe lines back into events. It is its own reading
// of the kernel's text (regular expressions), independent of the production
// parser. Lines it cannot read (headers, malformed payloads) are left out and
// counted in skipped.
func FromText(lines []string) (evs []Event, skipped int) {
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	atou := func(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }
	for _, l := range lines {
		l = strings.TrimRight(l, "\r\n")
		if m := lostRe.FindStringSubmatch(l); m != nil {
			evs = append(evs, Event{CPU: atoi(m[1]), Lost: int64(atou(m[2]))})
			continue
		}
		m := lineRe.FindStringSubmatch(l)
		if m == nil {
			skipped++
			continue
		}
		frac := (m[6] + "000000000")[:9]
		e := Event{CPU: atoi(m[3]), TimeNs: int64(atou(m[5]))*1_000_000_000 + int64(atou(frac)), Name: m[7]}
		p := m[8]
		ok := true
		switch e.Name {
		case "block_rq_insert", "block_rq_issue", "block_rq_merge":
			if x := rqRe.FindStringSubmatch(p); x != nil {
				e.Dev, e.RWBS, e.Sector, e.Nr, e.Comm = Dev(atoi(x[1]), atoi(x[2])), x[3], atou(x[5]), uint32(atou(x[6])), x[7]
			} else {
				ok = false
			}
		case "block_rq_complete":
			if x := rqDoneRe.FindStringSubmatch(p); x != nil {
				e.Dev, e.RWBS, e.Sector, e.Nr = Dev(atoi(x[1]), atoi(x[2])), x[3], atou(x[4]), uint32(atou(x[5]))
			} else {
				ok = false
			}
		case "block_bio_queue", "block_bio_complete":
			if x := bioRe.FindStringSubmatch(p); x != nil {
				e.Dev, e.RWBS, e.Sector, e.Nr = Dev(atoi(x[1]), atoi(x[2])), x[3], atou(x[4]), uint32(atou(x[5]))
				if e.Name == "block_bio_queue" {
					e.Comm = x[6]
				}
			} else {
				ok = false
			}
		case "nvme_setup_cmd", "nvme_complete_rq":
			x := nvmeRe.FindStringSubmatch(p)
			if x == nil || (e.Name == "nvme_setup_cmd" && x[5] == "") {
				ok = false
				break
			}
			e.CtrlID, e.Disk, e.QID, e.CID = atoi(x[1]), x[2], atoi(x[3]), atoi(x[4])
			if e.Name == "nvme_setup_cmd" {
				e.Opcode = opOf(x[5])
			}
		case "tracing_mark_write":
			e.Name, e.Text = "print", p
		default:
			ok = false
		}
		if !ok {
			skipped++
			continue
		}
		evs = append(evs, e)
	}
	return evs, skipped
}
