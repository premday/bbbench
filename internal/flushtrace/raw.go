package flushtrace

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Fields are the values of an event decoded from a binary ring-buffer page
// (the raw reader). The text reader leaves them zero (Decoded false) and the
// collectors parse Payload instead; both give the same values.
type Fields struct {
	Sector   uint64
	Dev      uint32 // kernel dev_t: major<<20 | minor
	NrSector uint32
	CtrlID   int32 // nvme: the controller number (nvme<CtrlID>)
	QID      int32
	CID      int32
	Opcode   uint8
	Decoded  bool
	RWBS     string // interned
	Disk     string // nvme_setup_cmd's disk ("" when absent), interned
	Ctrl     string // "nvme<CtrlID>", interned
}

// Major and Minor split Dev like the kernel's MAJOR() and MINOR().
func (f *Fields) Major() int { return int(f.Dev >> 20) }
func (f *Fields) Minor() int { return int(f.Dev & (1<<20 - 1)) }

// Ring buffer record types (events/header_event).
const (
	rbTypePadding = 29
	rbTypeExtend  = 30
	rbTypeStamp   = 31
	rbTypeDataMax = 28
	rbDeltaBits   = 27
	// commit word flags (RB_MISSED_EVENTS, RB_MISSED_STORED) and the
	// length mask libtraceevent's kbuffer uses.
	rbMissedEvents = 1 << 31
	rbMissedStored = 1 << 30
	rbCommitMask   = 1<<27 - 1
	// top bits of an absolute time stamp, filled from the current time
	// (the kernel's TS_MSB in rb_fix_abs_ts).
	rbTsMSB = uint64(0xf8) << 56
)

// fieldDef is one field of a format file.
type fieldDef struct {
	off, size int
	signed    bool
	array     bool // char x[N] or flexible
}

// eventFormat is a parsed events/<system>/<event>/format file.
type eventFormat struct {
	name   string
	id     int
	fields map[string]fieldDef
}

// parseFormat parses an event format file:
//
//	name: block_rq_issue
//	ID: 1234
//	format:
//		field:unsigned short common_type;	offset:0;	size:2;	signed:0;
//		...
func parseFormat(text string) (eventFormat, error) {
	f := eventFormat{id: -1, fields: map[string]fieldDef{}}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "name:"):
			f.name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		case strings.HasPrefix(line, "ID:"):
			id, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "ID:")))
			if err != nil {
				return f, fmt.Errorf("bad ID line %q", line)
			}
			f.id = id
		case strings.HasPrefix(line, "field:"):
			name, d, err := parseField(line)
			if err != nil {
				return f, err
			}
			f.fields[name] = d
		}
	}
	if f.name == "" || f.id < 0 {
		return f, errors.New("no name or ID")
	}
	return f, nil
}

// parseField parses "field:<decl>;	offset:N;	size:N;	signed:N;".
func parseField(line string) (string, fieldDef, error) {
	var d fieldDef
	parts := strings.Split(line, ";")
	if len(parts) < 4 {
		return "", d, fmt.Errorf("bad field line %q", line)
	}
	decl := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(parts[0]), "field:"))
	words := strings.Fields(decl)
	if len(words) == 0 {
		return "", d, fmt.Errorf("bad field line %q", line)
	}
	name := words[len(words)-1]
	if k := strings.IndexByte(name, '['); k >= 0 {
		name, d.array = name[:k], true
	}
	if strings.Contains(decl, "[") {
		d.array = true
	}
	seen := 0
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(strings.TrimSpace(p), ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return "", d, fmt.Errorf("bad %s in %q", k, line)
		}
		switch k {
		case "offset":
			d.off = n
			seen++
		case "size":
			d.size = n
			seen++
		case "signed":
			d.signed = n != 0
		}
	}
	if seen != 2 || name == "" || d.off < 0 || d.size < 0 {
		return "", d, fmt.Errorf("bad field line %q", line)
	}
	return name, d, nil
}

// pageLayout is events/header_page: where the page's time stamp, commit
// word and data are.
type pageLayout struct {
	tsOff, commitOff, commitSize, dataOff, dataSize int
}

func (l pageLayout) pageSize() int { return l.dataOff + l.dataSize }

func parseHeaderPage(text string) (pageLayout, error) {
	var l pageLayout
	got := map[string]fieldDef{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "field:") {
			continue
		}
		name, d, err := parseField(line)
		if err != nil {
			return l, err
		}
		got[name] = d
	}
	ts, ok1 := got["timestamp"]
	c, ok2 := got["commit"]
	data, ok3 := got["data"]
	if !ok1 || !ok2 || !ok3 || ts.size != 8 || (c.size != 4 && c.size != 8) || data.size <= 0 || data.off < c.off+c.size {
		return l, fmt.Errorf("unexpected header_page: %v", got)
	}
	return pageLayout{tsOff: ts.off, commitOff: c.off, commitSize: c.size, dataOff: data.off, dataSize: data.size}, nil
}

// checkHeaderEvent verifies events/header_event describes the record header
// the decoder knows (5-bit type_len, 27-bit time_delta, types 29/30/31,
// data types up to 28).
func checkHeaderEvent(text string) error {
	norm := strings.Join(strings.Fields(text), " ")
	for _, want := range []string{
		"type_len : 5 bits", "time_delta : 27 bits", "padding : type == 29",
		"time_extend : type == 30", "time_stamp : type == 31", "data max type_len == 28",
	} {
		if !strings.Contains(norm, want) {
			return fmt.Errorf("header_event lacks %q", want)
		}
	}
	return nil
}

// plan kinds.
const (
	planBlock = iota + 1 // dev, sector, nr_sector, rwbs
	planNVMeSetup
	planNVMeDone
	planPrint
)

// plan is how to decode one event id.
type plan struct {
	name                                            string
	kind                                            int
	minSize                                         int
	dev, sector, nr, rwbs, ctrl, qid, cid, op, disk fieldDef
	hasDisk                                         bool
	buf                                             fieldDef
}

// rawEventKinds maps the events the collectors use to their plan kind.
var rawEventKinds = map[string]int{
	"block_rq_insert": planBlock, "block_rq_issue": planBlock, "block_rq_merge": planBlock,
	"block_rq_complete": planBlock, "block_bio_queue": planBlock, "block_bio_complete": planBlock,
	"nvme_setup_cmd": planNVMeSetup, "nvme_complete_rq": planNVMeDone,
}

func intField(f eventFormat, name string, sizes ...int) (fieldDef, error) {
	d, ok := f.fields[name]
	if !ok {
		return d, fmt.Errorf("%s: no field %s", f.name, name)
	}
	if d.array {
		return d, fmt.Errorf("%s: field %s is an array", f.name, name)
	}
	for _, s := range sizes {
		if d.size == s {
			return d, nil
		}
	}
	return d, fmt.Errorf("%s: field %s has size %d", f.name, name, d.size)
}

func charField(f eventFormat, name string) (fieldDef, error) {
	d, ok := f.fields[name]
	if !ok {
		return d, fmt.Errorf("%s: no field %s", f.name, name)
	}
	if !d.array {
		return d, fmt.Errorf("%s: field %s is not a char array", f.name, name)
	}
	return d, nil
}

// newPlan builds the decode plan of an event from its format; an error
// means the event cannot be decoded (a field missing or of another shape).
func newPlan(f eventFormat) (*plan, error) {
	p := &plan{name: f.name}
	if f.name == "print" {
		p.kind, p.name = planPrint, "tracing_mark_write"
		var err error
		if p.buf, err = charField(f, "buf"); err != nil {
			return nil, err
		}
		p.minSize = p.buf.off
		return p, nil
	}
	p.kind = rawEventKinds[f.name]
	var err error
	need := func(d *fieldDef, name string, sizes ...int) {
		if err == nil {
			*d, err = intField(f, name, sizes...)
		}
	}
	switch p.kind {
	case planBlock:
		need(&p.dev, "dev", 4, 8)
		need(&p.sector, "sector", 8, 4)
		need(&p.nr, "nr_sector", 4, 8, 2)
		if err == nil {
			p.rwbs, err = charField(f, "rwbs")
		}
	case planNVMeSetup, planNVMeDone:
		need(&p.ctrl, "ctrl_id", 4, 2, 1, 8)
		need(&p.qid, "qid", 4, 2, 1, 8)
		need(&p.cid, "cid", 2, 4, 8)
		if p.kind == planNVMeSetup {
			need(&p.op, "opcode", 1, 2, 4)
			if d, ok := f.fields["disk"]; ok && d.array {
				p.disk, p.hasDisk = d, true
			}
		}
	default:
		return nil, fmt.Errorf("%s: not an event bbbench decodes", f.name)
	}
	if err != nil {
		return nil, err
	}
	for _, d := range []fieldDef{p.dev, p.sector, p.nr, p.rwbs, p.ctrl, p.qid, p.cid, p.op, p.disk} {
		p.minSize = max(p.minSize, d.off+d.size)
	}
	return p, nil
}

// rawFormats is what a raw reader needs: the page layout, where the event
// id is and the plan of each id. It is read once per session and shared by
// the per-CPU decoders (read-only).
type rawFormats struct {
	layout    pageLayout
	typeField fieldDef
	plans     map[int]*plan
}

// loadRawFormats reads header_page, header_event, ftrace/print and the format
// of every enabled event ("events/<system>/<event>") under the tracefs root.
func loadRawFormats(root string, enable []string) (*rawFormats, error) {
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		return string(b), err
	}
	hp, err := read("events/header_page")
	if err != nil {
		return nil, fmt.Errorf("header_page: %w", err)
	}
	rf := &rawFormats{plans: map[int]*plan{}}
	if rf.layout, err = parseHeaderPage(hp); err != nil {
		return nil, err
	}
	he, err := read("events/header_event")
	if err != nil {
		return nil, fmt.Errorf("header_event: %w", err)
	}
	if err := checkHeaderEvent(he); err != nil {
		return nil, err
	}
	for i, e := range append([]string{"events/ftrace/print"}, enable...) {
		text, err := read(filepath.Join(e, "format"))
		if err != nil {
			return nil, fmt.Errorf("format of %s: %w", strings.TrimPrefix(e, "events/"), err)
		}
		f, err := parseFormat(text)
		if err != nil {
			return nil, fmt.Errorf("format of %s: %w", strings.TrimPrefix(e, "events/"), err)
		}
		tf, err := intField(f, "common_type", 2, 4)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			rf.typeField = tf
		} else if tf != rf.typeField {
			return nil, fmt.Errorf("%s: common_type at another place", f.name)
		}
		p, err := newPlan(f)
		if err != nil {
			return nil, err
		}
		rf.plans[f.id] = p
	}
	return rf, nil
}

// decoder decodes the pages of one CPU. Its intern tables are its own, so
// each reader goroutine has one.
type decoder struct {
	*rawFormats
	rwbs, disks map[string]string
	ctrls       map[int32]string
	unknown     int64 // data events of an id without a plan
	bad         int64 // records or pages that do not fit
}

func newDecoder(rf *rawFormats) *decoder {
	return &decoder{rawFormats: rf, rwbs: map[string]string{}, disks: map[string]string{}, ctrls: map[int32]string{}}
}

func readUint(b []byte, d fieldDef) uint64 {
	switch d.size {
	case 1:
		if d.signed {
			return uint64(int64(int8(b[d.off])))
		}
		return uint64(b[d.off])
	case 2:
		v := binary.NativeEndian.Uint16(b[d.off:])
		if d.signed {
			return uint64(int64(int16(v)))
		}
		return uint64(v)
	case 4:
		v := binary.NativeEndian.Uint32(b[d.off:])
		if d.signed {
			return uint64(int64(int32(v)))
		}
		return uint64(v)
	case 8:
		return binary.NativeEndian.Uint64(b[d.off:])
	}
	return 0
}

// cstr returns the NUL-terminated string of a char array field.
func cstr(b []byte, d fieldDef) []byte {
	end := len(b)
	if d.size > 0 {
		end = min(end, d.off+d.size)
	}
	s := b[d.off:end]
	for i, c := range s {
		if c == 0 {
			return s[:i]
		}
	}
	return s
}

func intern(m map[string]string, b []byte) string {
	if s, ok := m[string(b)]; ok {
		return s
	}
	s := string(b)
	m[s] = s
	return s
}

// decodePage appends the events of one page to out and returns the events
// the kernel reported lost before the page (its missed-events flags: the
// stored count, or 1 when the page had no room for it).
func (d *decoder) decodePage(page []byte, out []Event) ([]Event, int64) {
	l := d.layout
	if len(page) < l.dataOff {
		d.bad++
		return out, 0
	}
	ts := binary.NativeEndian.Uint64(page[l.tsOff:])
	var commit uint64
	if l.commitSize == 8 {
		commit = binary.NativeEndian.Uint64(page[l.commitOff:])
	} else {
		commit = uint64(binary.NativeEndian.Uint32(page[l.commitOff:]))
	}
	length := int(commit & rbCommitMask)
	data := page[l.dataOff:]
	if length > len(data) || length > l.dataSize {
		d.bad++
		return out, 0
	}
	var lost int64
	if commit&rbMissedEvents != 0 {
		lost = 1
		if commit&rbMissedStored != 0 && length+l.commitSize <= len(data) {
			if l.commitSize == 8 {
				lost = int64(binary.NativeEndian.Uint64(data[length:]))
			} else {
				lost = int64(binary.NativeEndian.Uint32(data[length:]))
			}
		}
	}
	data = data[:length]
	for i := 0; i+4 <= len(data); {
		h := binary.NativeEndian.Uint32(data[i:])
		typ, delta := int(h&0x1f), uint64(h>>5)
		switch {
		case typ == rbTypePadding:
			if delta == 0 { // the rest of the page is empty
				return out, lost
			}
			if i+8 > len(data) {
				d.bad++
				return out, lost
			}
			ts += delta
			i += 4 + int(binary.NativeEndian.Uint32(data[i+4:]))
			continue
		case typ == rbTypeExtend || typ == rbTypeStamp:
			if i+8 > len(data) {
				d.bad++
				return out, lost
			}
			v := uint64(binary.NativeEndian.Uint32(data[i+4:]))<<rbDeltaBits | delta
			if typ == rbTypeExtend {
				ts += v
			} else {
				if ts&rbTsMSB != 0 {
					v |= ts & rbTsMSB
					if v < ts {
						v += 1 << 59
					}
				}
				ts = v
			}
			i += 8
			continue
		}
		ts += delta
		var entry []byte
		if typ == 0 {
			if i+8 > len(data) {
				d.bad++
				return out, lost
			}
			n := int(binary.NativeEndian.Uint32(data[i+4:]))
			size := (n - 4 + 3) &^ 3
			if n < 4 || i+8+size > len(data) {
				d.bad++
				return out, lost
			}
			entry = data[i+8 : i+8+size]
			i += 8 + size
		} else {
			size := typ * 4
			if i+4+size > len(data) {
				d.bad++
				return out, lost
			}
			entry = data[i+4 : i+4+size]
			i += 4 + size
		}
		if ev, ok := d.decodeEntry(entry, int64(ts)); ok {
			out = append(out, ev)
		}
	}
	return out, lost
}

func (d *decoder) decodeEntry(entry []byte, ts int64) (Event, bool) {
	tf := d.typeField
	if len(entry) < tf.off+tf.size {
		d.bad++
		return Event{}, false
	}
	p := d.plans[int(readUint(entry, tf))]
	if p == nil {
		d.unknown++
		return Event{}, false
	}
	if len(entry) < p.minSize {
		d.bad++
		return Event{}, false
	}
	ev := Event{TimeNs: ts, Name: p.name}
	f := &ev.F
	f.Decoded = true
	switch p.kind {
	case planPrint:
		ev.Payload = string(cstr(entry, p.buf))
	case planBlock:
		f.Dev = uint32(readUint(entry, p.dev))
		f.Sector = readUint(entry, p.sector)
		f.NrSector = uint32(readUint(entry, p.nr))
		f.RWBS = intern(d.rwbs, cstr(entry, p.rwbs))
	case planNVMeSetup, planNVMeDone:
		f.CtrlID = int32(readUint(entry, p.ctrl))
		f.QID = int32(readUint(entry, p.qid))
		f.CID = int32(readUint(entry, p.cid))
		if s, ok := d.ctrls[f.CtrlID]; ok {
			f.Ctrl = s
		} else {
			f.Ctrl = "nvme" + strconv.Itoa(int(f.CtrlID))
			d.ctrls[f.CtrlID] = f.Ctrl
		}
		if p.kind == planNVMeSetup {
			f.Opcode = uint8(readUint(entry, p.op))
			if p.hasDisk {
				f.Disk = intern(d.disks, cstr(entry, p.disk))
			}
		}
	}
	return ev, true
}
