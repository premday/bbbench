// Package tracefake builds what a kernel's tracefs shows for the trace events
// bbbench uses, for tests: the events' format files, header_page and
// header_event, and binary ring-buffer pages as per_cpu/cpuN/trace_pipe_raw
// returns them. It also renders the same events as trace_pipe text lines and
// reads such lines back (FromText), so one event stream can be fed to both
// the text and the raw reader.
//
// The layouts are hand-written from the kernel's TP_STRUCT__entry
// declarations (block events from include/trace/events/block.h of Linux 7.0,
// NVMe events from drivers/nvme/host/trace.h as remembered) with C natural
// alignment, and the format text follows the kernel's syntax. The decoder
// under test reads every offset from these files, so a fixture layout that
// differs from a real kernel cannot make it right by accident; nothing here
// has been compared with a real kernel's files.
package tracefake

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Field is one field of an event's entry.
type Field struct {
	Decl   string // C declaration as the format file prints it, e.g. "unsigned int nr_sector", "char rwbs[10]"
	Name   string
	Offset int
	Size   int
	Signed bool
}

// Format is an event's format: its id and fields (the common ones first).
type Format struct {
	System, Name string
	ID           int
	Fields       []Field
	Size         int // size of the fixed part of the entry
}

// FieldByName returns the named field.
func (f *Format) FieldByName(name string) (Field, bool) {
	for _, x := range f.Fields {
		if x.Name == name {
			return x, true
		}
	}
	return Field{}, false
}

// ctype is a C field: declaration type, name, size, alignment, signedness;
// arr > 0 makes it an array of that many elements; arr < 0 a flexible array.
type ctype struct {
	typ          string
	name         string
	size, align  int
	signed       bool
	arr          int
	dataLoc, flx bool
}

var common = []ctype{
	{typ: "unsigned short", name: "common_type", size: 2, align: 2},
	{typ: "unsigned char", name: "common_flags", size: 1, align: 1},
	{typ: "unsigned char", name: "common_preempt_count", size: 1, align: 1},
	{typ: "int", name: "common_pid", size: 4, align: 4, signed: true},
}

func u(t, n string, size int) ctype { return ctype{typ: t, name: n, size: size, align: size} }
func s(t, n string, size int) ctype {
	return ctype{typ: t, name: n, size: size, align: size, signed: true}
}
func chars(n string, l int) ctype {
	return ctype{typ: "char", name: n, size: 1, align: 1, signed: true, arr: l}
}

// RWBSLen is the kernel's RWBS_LEN (10 since Linux 6.x; 8 before).
const RWBSLen = 10

// layouts of the events, without the common fields.
var layouts = map[string][]ctype{
	// block_rq (insert, issue, merge): include/trace/events/block.h
	"block/block_rq_insert": blockRq(),
	"block/block_rq_issue":  blockRq(),
	"block/block_rq_merge":  blockRq(),
	"block/block_rq_complete": {
		u("dev_t", "dev", 4), u("sector_t", "sector", 8), u("unsigned int", "nr_sector", 4),
		s("int", "error", 4), u("unsigned short", "ioprio", 2), chars("rwbs", RWBSLen),
		{typ: "__data_loc char[]", name: "cmd", size: 4, align: 4, signed: true, dataLoc: true},
	},
	"block/block_bio_queue": {
		u("dev_t", "dev", 4), u("sector_t", "sector", 8), u("unsigned int", "nr_sector", 4),
		chars("rwbs", RWBSLen), chars("comm", 16),
	},
	"block/block_bio_complete": {
		u("dev_t", "dev", 4), u("sector_t", "sector", 8), u("unsigned", "nr_sector", 4),
		s("int", "error", 4), chars("rwbs", RWBSLen),
	},
	// drivers/nvme/host/trace.h
	"nvme/nvme_setup_cmd": {
		chars("disk", 32), s("int", "ctrl_id", 4), s("int", "qid", 4), u("u8", "opcode", 1),
		u("u8", "flags", 1), u("u8", "fctype", 1), u("u16", "cid", 2), u("u32", "nsid", 4),
		u("bool", "metadata", 1), {typ: "u8", name: "cdw10", size: 1, align: 1, arr: 24},
	},
	"nvme/nvme_complete_rq": {
		chars("disk", 32), s("int", "ctrl_id", 4), s("int", "qid", 4), s("int", "cid", 4),
		u("u64", "result", 8), u("u8", "retries", 1), u("u8", "flags", 1), u("u16", "status", 2),
	},
	// kernel/trace/trace_entries.h: the trace_marker writes
	"ftrace/print": {
		u("unsigned long", "ip", 8),
		{typ: "char", name: "buf", size: 1, align: 1, signed: true, flx: true},
	},
}

func blockRq() []ctype {
	return []ctype{
		u("dev_t", "dev", 4), u("sector_t", "sector", 8), u("unsigned int", "nr_sector", 4),
		u("unsigned int", "bytes", 4), u("unsigned short", "ioprio", 2), chars("rwbs", RWBSLen),
		chars("comm", 16),
		{typ: "__data_loc char[]", name: "cmd", size: 4, align: 4, signed: true, dataLoc: true},
	}
}

// Events lists every event this package knows ("system/name").
func Events() []string {
	out := make([]string, 0, len(layouts))
	for k := range layouts {
		out = append(out, k)
	}
	return out
}

// Kernel returns the format of an event ("block/block_rq_issue"), with the
// given id, laid out like the C compiler lays out the kernel's struct.
func Kernel(event string, id int) (*Format, error) {
	fields, ok := layouts[event]
	if !ok {
		return nil, fmt.Errorf("tracefake: unknown event %s", event)
	}
	sys, name, _ := strings.Cut(event, "/")
	if event == "ftrace/print" {
		name = "print"
	}
	f := &Format{System: sys, Name: name, ID: id}
	off := 0
	for _, c := range append(append([]ctype{}, common...), fields...) {
		off = (off + c.align - 1) / c.align * c.align
		size := c.size
		decl := c.typ + " " + c.name
		switch {
		case c.arr > 0:
			size = c.size * c.arr
			decl = fmt.Sprintf("%s %s[%d]", c.typ, c.name, c.arr)
		case c.flx:
			size = 0
			decl = c.typ + " " + c.name + "[]"
		}
		f.Fields = append(f.Fields, Field{Decl: decl, Name: c.name, Offset: off, Size: size, Signed: c.signed})
		off += size
	}
	f.Size = off
	return f, nil
}

// Text is the format file as the kernel prints it.
func (f *Format) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\nID: %d\nformat:\n", f.Name, f.ID)
	for i, x := range f.Fields {
		if i == len(common) {
			b.WriteString("\n")
		}
		sg := 0
		if x.Signed {
			sg = 1
		}
		fmt.Fprintf(&b, "\tfield:%s;\toffset:%d;\tsize:%d;\tsigned:%d;\n", x.Decl, x.Offset, x.Size, sg)
	}
	b.WriteString("\nprint fmt: \"...\"\n")
	return b.String()
}

// HeaderPage is events/header_page of a 64-bit kernel with pages of
// pageSize bytes.
func HeaderPage(pageSize int) string {
	return "\tfield: u64 timestamp;\toffset:0;\tsize:8;\tsigned:0;\n" +
		"\tfield: local_t commit;\toffset:8;\tsize:8;\tsigned:1;\n" +
		"\tfield: int overwrite;\toffset:8;\tsize:1;\tsigned:1;\n" +
		fmt.Sprintf("\tfield: char data;\toffset:16;\tsize:%d;\tsigned:1;\n", pageSize-16)
}

// HeaderEvent is events/header_event.
const HeaderEvent = "# compressed entry header\n" +
	"\ttype_len    :    5 bits\n" +
	"\ttime_delta  :   27 bits\n" +
	"\tarray       :   32 bits\n\n" +
	"\tpadding     : type == 29\n" +
	"\ttime_extend : type == 30\n" +
	"\ttime_stamp : type == 31\n" +
	"\tdata max type_len  == 28\n"

// Tree is a fake tracefs root with format files.
type Tree struct {
	Root     string
	PageSize int
	Formats  map[string]*Format // by event name ("block_rq_issue", "print")
}

// WriteTree writes, under root, events/header_page, events/header_event and
// the format file of each event ("block/block_rq_issue"), plus ftrace/print.
// Ids are assigned from 1000 in the order given.
func WriteTree(root string, pageSize int, events ...string) (*Tree, error) {
	t := &Tree{Root: root, PageSize: pageSize, Formats: map[string]*Format{}}
	all := append([]string{"ftrace/print"}, events...)
	for i, e := range all {
		if _, dup := t.Formats[name(e)]; dup {
			continue
		}
		f, err := Kernel(e, 1000+i)
		if err != nil {
			return nil, err
		}
		dir := filepath.Join(root, "events", e)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "format"), []byte(f.Text()), 0o644); err != nil {
			return nil, err
		}
		t.Formats[f.Name] = f
	}
	ev := filepath.Join(root, "events")
	if err := os.WriteFile(filepath.Join(ev, "header_page"), []byte(HeaderPage(pageSize)), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(ev, "header_event"), []byte(HeaderEvent), 0o644); err != nil {
		return nil, err
	}
	return t, nil
}

func name(e string) string {
	if e == "ftrace/print" {
		return "print"
	}
	_, n, _ := strings.Cut(e, "/")
	return n
}
