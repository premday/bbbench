package flushtrace

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"bbbench/internal/tracefake"
)

var rawTestEvents = []string{
	"block/block_rq_insert", "block/block_rq_issue", "block/block_rq_merge", "block/block_rq_complete",
	"block/block_bio_queue", "block/block_bio_complete", "nvme/nvme_setup_cmd", "nvme/nvme_complete_rq",
}

func rawTree(t *testing.T, pageSize int, events ...string) *tracefake.Tree {
	t.Helper()
	tr, err := tracefake.WriteTree(t.TempDir(), pageSize, events...)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func enableOf(events []string) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = "events/" + e
	}
	return out
}

func TestParseFormat(t *testing.T) {
	tr := rawTree(t, 4096, "block/block_rq_issue")
	text, err := os.ReadFile(filepath.Join(tr.Root, "events/block/block_rq_issue/format"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseFormat(string(text))
	if err != nil {
		t.Fatal(err)
	}
	want := tr.Formats["block_rq_issue"]
	if f.name != "block_rq_issue" || f.id != want.ID {
		t.Fatalf("name %q id %d", f.name, f.id)
	}
	for _, x := range want.Fields {
		got, ok := f.fields[x.Name]
		if !ok || got.off != x.Offset || got.size != x.Size || got.signed != x.Signed {
			t.Errorf("%s = %+v, want %+v", x.Name, got, x)
		}
	}
	if !f.fields["rwbs"].array || f.fields["dev"].array || !f.fields["cmd"].array {
		t.Errorf("array flags: %+v", f.fields)
	}
	// rwbs is char[10] on 7.0 (it was 8): the offsets of what follows come
	// from the file.
	if f.fields["rwbs"].size != tracefake.RWBSLen {
		t.Errorf("rwbs size %d", f.fields["rwbs"].size)
	}
	for _, bad := range []string{"", "name: x\n", "ID: 3\n", "name: x\nID: y\n", "name: x\nID: 1\n\tfield:int a;\toffset:z;\tsize:4;\tsigned:1;\n"} {
		if _, err := parseFormat(bad); err == nil {
			t.Errorf("parseFormat(%q) succeeded", bad)
		}
	}
}

func TestParseHeaderPage(t *testing.T) {
	l, err := parseHeaderPage(tracefake.HeaderPage(4096))
	if err != nil {
		t.Fatal(err)
	}
	if l != (pageLayout{tsOff: 0, commitOff: 8, commitSize: 8, dataOff: 16, dataSize: 4080}) || l.pageSize() != 4096 {
		t.Errorf("layout %+v", l)
	}
	if _, err := parseHeaderPage("\tfield: u64 timestamp;\toffset:0;\tsize:8;\tsigned:0;\n"); err == nil {
		t.Error("header_page without commit and data accepted")
	}
	if err := checkHeaderEvent(tracefake.HeaderEvent); err != nil {
		t.Error(err)
	}
	if err := checkHeaderEvent(strings.Replace(tracefake.HeaderEvent, "27 bits", "26 bits", 1)); err == nil {
		t.Error("a different time_delta width accepted")
	}
}

// A missing format, or one without a field the collectors need, makes the
// raw path unavailable (never wrong data).
func TestLoadRawFormatsRefuses(t *testing.T) {
	tr := rawTree(t, 4096, "block/block_rq_issue")
	if _, err := loadRawFormats(tr.Root, []string{"events/block/block_rq_issue"}); err != nil {
		t.Fatalf("good tree refused: %v", err)
	}
	if _, err := loadRawFormats(tr.Root, []string{"events/block/block_rq_complete"}); err == nil || !strings.Contains(err.Error(), "block_rq_complete") {
		t.Errorf("missing format: %v", err)
	}
	p := filepath.Join(tr.Root, "events/block/block_rq_issue/format")
	b, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), " sector;", " sector_x;", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRawFormats(tr.Root, []string{"events/block/block_rq_issue"}); err == nil || !strings.Contains(err.Error(), "no field sector") {
		t.Errorf("missing field: %v", err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "char rwbs[10];", "int rwbs;", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRawFormats(tr.Root, []string{"events/block/block_rq_issue"}); err == nil || !strings.Contains(err.Error(), "rwbs") {
		t.Errorf("rwbs not an array: %v", err)
	}
	if err := os.Remove(filepath.Join(tr.Root, "events/header_event")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRawFormats(tr.Root, nil); err == nil {
		t.Error("missing header_event accepted")
	}
}

func sampleStream() []tracefake.Event {
	d := tracefake.Dev(8, 16)
	return []tracefake.Event{
		{CPU: 0, TimeNs: 1_000_000_000, Name: "print", Text: "bbbench-start"},
		{CPU: 0, TimeNs: 1_000_000_100, Name: "block_rq_insert", Dev: d, Sector: 2048, Nr: 8, RWBS: "WS", Comm: "fio"},
		{CPU: 0, TimeNs: 1_000_000_200, Name: "block_rq_issue", Dev: d, Sector: 2048, Nr: 8, RWBS: "WS", Comm: "fio"},
		{CPU: 0, TimeNs: 1_000_000_250, Name: "block_rq_merge", Dev: d, Sector: 4096, Nr: 16, RWBS: "R", Comm: "fio"},
		{CPU: 0, TimeNs: 5_000_000_000, Name: "block_rq_complete", Dev: d, Sector: 2048, Nr: 8, RWBS: "WS"}, // a delta beyond 27 bits: time extend
		{CPU: 0, TimeNs: 5_000_000_001, Name: "block_bio_queue", Dev: tracefake.Dev(9, 127), Sector: 1 << 40, Nr: 1 << 20, RWBS: "FWS", Comm: "kworker/1:1"},
		{CPU: 0, TimeNs: 5_000_000_002, Name: "block_bio_complete", Dev: tracefake.Dev(9, 127), Sector: 1 << 40, Nr: 1 << 20, RWBS: "FWS"},
		{CPU: 0, TimeNs: 5_000_000_003, Name: "nvme_setup_cmd", CtrlID: 12, Disk: "nvme12n1", QID: 7, CID: 65535, Opcode: 2},
		{CPU: 0, TimeNs: 5_000_000_004, Name: "nvme_complete_rq", CtrlID: 12, Disk: "nvme12n1", QID: 7, CID: 65535},
		{CPU: 0, TimeNs: 5_000_000_005, Name: "nvme_setup_cmd", CtrlID: 0, QID: 0, CID: 3, Opcode: 6},
	}
}

func wantDecoded(e tracefake.Event) Event {
	ev := Event{TimeNs: e.TimeNs, Name: e.Name}
	ev.F.Decoded = true
	switch e.Name {
	case "print":
		ev.Name, ev.Payload = "tracing_mark_write", e.Text+"\n"
	case "nvme_setup_cmd", "nvme_complete_rq":
		ev.F.CtrlID, ev.F.QID, ev.F.CID = int32(e.CtrlID), int32(e.QID), int32(e.CID)
		ev.F.Ctrl = "nvme" + strconv.Itoa(e.CtrlID)
		if e.Name == "nvme_setup_cmd" {
			ev.F.Opcode, ev.F.Disk = uint8(e.Opcode), e.Disk
		}
	default:
		ev.F.Dev, ev.F.Sector, ev.F.NrSector, ev.F.RWBS = e.Dev, e.Sector, e.Nr, e.RWBS
	}
	return ev
}

func decodeAll(t *testing.T, tr *tracefake.Tree, pages [][]byte) ([]Event, int64, *decoder) {
	t.Helper()
	rf, err := loadRawFormats(tr.Root, enableOf(rawTestEvents))
	if err != nil {
		t.Fatal(err)
	}
	d := newDecoder(rf)
	var out []Event
	var lost int64
	for _, p := range pages {
		var n int64
		out, n = d.decodePage(p, out)
		lost += n
	}
	return out, lost, d
}

// Every record type decodes back to the events encoded: small and long data
// records, time extends, absolute stamps, discarded events (padding).
func TestDecodePage(t *testing.T) {
	for name, o := range map[string]tracefake.PageOptions{
		"plain":    {},
		"long":     {LongEvery: 1},
		"absolute": {AbsoluteEvery: 2},
		"padding":  {PadEvery: 2},
		"mixed":    {LongEvery: 3, AbsoluteEvery: 4, PadEvery: 2},
	} {
		t.Run(name, func(t *testing.T) {
			tr := rawTree(t, 4096, rawTestEvents...)
			in := sampleStream()
			pages, err := tr.Pages(in, o)
			if err != nil {
				t.Fatal(err)
			}
			got, lost, d := decodeAll(t, tr, pages[0])
			if lost != 0 || d.bad != 0 || d.unknown != 0 {
				t.Errorf("lost %d bad %d unknown %d", lost, d.bad, d.unknown)
			}
			if len(got) != len(in) {
				t.Fatalf("%d events, want %d: %+v", len(got), len(in), got)
			}
			for i, e := range in {
				if w := wantDecoded(e); !reflect.DeepEqual(got[i], w) {
					t.Errorf("event %d = %+v\nwant %+v", i, got[i], w)
				}
			}
		})
	}
}

// Events spill over several small pages; each page's time stamp restarts
// the deltas.
func TestDecodeManyPages(t *testing.T) {
	tr := rawTree(t, 256, rawTestEvents...)
	var in []tracefake.Event
	for i := 0; i < 200; i++ {
		in = append(in, tracefake.Event{TimeNs: int64(1e9 + i*1000), Name: "block_rq_issue", Dev: tracefake.Dev(8, 0), Sector: uint64(i), Nr: 8, RWBS: "R"})
	}
	pages, err := tr.Pages(in, tracefake.PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages[0]) < 10 {
		t.Fatalf("only %d pages", len(pages[0]))
	}
	got, _, _ := decodeAll(t, tr, pages[0])
	if len(got) != len(in) {
		t.Fatalf("%d events, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i].TimeNs != in[i].TimeNs || got[i].F.Sector != in[i].Sector {
			t.Fatalf("event %d = %+v", i, got[i])
		}
	}
}

// The missed-events flags of a page count like the text's [LOST N EVENTS]:
// the stored count, or 1 when the page had no room for it.
func TestDecodeMissedEvents(t *testing.T) {
	tr := rawTree(t, 4096, rawTestEvents...)
	in := []tracefake.Event{
		{TimeNs: 1, Name: "block_rq_issue", Dev: 1, Nr: 8, RWBS: "R"},
		{Lost: 120},
		{TimeNs: 2, Name: "block_rq_issue", Dev: 1, Nr: 8, RWBS: "R"},
		{Lost: 3},
	}
	pages, err := tr.Pages(in, tracefake.PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, lost, _ := decodeAll(t, tr, pages[0])
	if len(got) != 2 || lost != 123 {
		t.Errorf("%d events, lost %d, want 2 and 123", len(got), lost)
	}
	pages, _ = tr.Pages(in, tracefake.PageOptions{NoRoomForLost: true})
	if _, lost, _ := decodeAll(t, tr, pages[0]); lost != 2 {
		t.Errorf("without stored counts lost = %d, want 2 (one per flagged page)", lost)
	}
}

// A page whose commit length runs past the page, or a record cut short, is
// counted bad, never decoded into wrong values.
func TestDecodeCorruptPage(t *testing.T) {
	tr := rawTree(t, 4096, rawTestEvents...)
	pages, _ := tr.Pages(sampleStream(), tracefake.PageOptions{})
	p := append([]byte(nil), pages[0][0]...)
	p[8], p[9], p[10] = 0xff, 0xff, 0x07 // length beyond the page
	if got, _, d := decodeAll(t, tr, [][]byte{p}); len(got) != 0 || d.bad != 1 {
		t.Errorf("events %d bad %d", len(got), d.bad)
	}
	if got, _, d := decodeAll(t, tr, [][]byte{p[:10]}); len(got) != 0 || d.bad != 1 {
		t.Errorf("short page: events %d bad %d", len(got), d.bad)
	}
}

// The Flush collectors give the same result for decoded events as for the
// text lines of the same stream.
func TestFlushCollectorsDecodedLikeText(t *testing.T) {
	tr := rawTree(t, 4096, rawTestEvents...)
	d := tracefake.Dev(8, 16)
	md := tracefake.Dev(9, 127)
	in := []tracefake.Event{
		{TimeNs: 1000, Name: "nvme_setup_cmd", CtrlID: 0, Disk: "nvme0n1", QID: 1, CID: 5, Opcode: 0},
		{TimeNs: 1100, Name: "nvme_setup_cmd", CtrlID: 0, Disk: "nvme0n1", QID: 0, CID: 6, Opcode: 0}, // admin, not a Flush
		{TimeNs: 1150, Name: "nvme_setup_cmd", CtrlID: 0, Disk: "nvme0n2", QID: 1, CID: 7, Opcode: 0}, // other namespace
		{TimeNs: 1195, Name: "nvme_complete_rq", CtrlID: 0, Disk: "nvme0n1", QID: 1, CID: 5},
		{TimeNs: 2000, Name: "block_rq_issue", Dev: d, RWBS: "F", Comm: "fio"},
		{TimeNs: 2100, Name: "block_rq_issue", Dev: d, RWBS: "FWS", Nr: 8, Comm: "fio"},
		{TimeNs: 2300, Name: "block_rq_complete", Dev: d, RWBS: "F"},
		{TimeNs: 3000, Name: "block_bio_queue", Dev: md, RWBS: "FWS", Comm: "fio"},
		{TimeNs: 3900, Name: "block_bio_complete", Dev: md, RWBS: "FWS"},
	}
	pages, err := tr.Pages(in, tracefake.PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dec, _, _ := decodeAll(t, tr, pages[0])
	for _, mk := range []func() Collector{
		func() Collector { return NewNVMe("nvme0n1") },
		func() Collector { return NewBlock(8, 16) },
		func() Collector { return NewBio(9, 127) },
	} {
		text, raw := mk(), mk()
		for _, e := range in {
			ev, ok := ParseLine(e.Line())
			if !ok {
				t.Fatalf("unparsable %q", e.Line())
			}
			text.Feed(ev)
		}
		for _, ev := range dec {
			raw.Feed(ev)
		}
		if a, b := text.Summary(), raw.Summary(); !reflect.DeepEqual(a, b) || a.Count != 1 {
			t.Errorf("text %+v\nraw  %+v", a, b)
		}
		if a, b := text.Samples(), raw.Samples(); !reflect.DeepEqual(a, b) {
			t.Errorf("samples text %v raw %v", a, b)
		}
	}
}
