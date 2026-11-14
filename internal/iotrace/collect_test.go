package iotrace

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unsafe"

	"bbbench/internal/flushtrace"
)

func TestParseBlockRq(t *testing.T) {
	good := []struct {
		payload   string
		withBytes bool
		want      blockRq
	}{
		// block_rq_insert / block_rq_issue: dev rwbs bytes (cmd) sector + nr [comm]
		{"8,16 R 4096 () 1000 + 8 [fio]", true, blockRq{8, 16, "R", 1000, 8}},
		// with the ioprio field of newer kernels
		{"259,0 WS 131072 () 123456789 + 256 none,0,0 [fio]", true, blockRq{259, 0, "WS", 123456789, 256}},
		// block_rq_complete: no bytes field
		{"259,0 F () 0 + 0 [0]", false, blockRq{259, 0, "F", 0, 0}},
		{"8,32 W () 5000 + 16 none,0,0 [0]", false, blockRq{8, 32, "W", 5000, 16}},
		// a passthrough command with spaces in (cmd)
		{"8,0 R 512 (28 00 00 00 00 08 00 00 01 00) 8 + 1 be,0,4 [sg_dd]", true, blockRq{8, 0, "R", 8, 1}},
		{"8,0 R (28 00 00 00 00 08 00 00 01 00) 8 + 1 [0]", false, blockRq{8, 0, "R", 8, 1}},
	}
	for _, g := range good {
		got, ok := parseBlockRq(g.payload, g.withBytes)
		if !ok || got != g.want {
			t.Errorf("%q: got %+v ok=%v, want %+v", g.payload, got, ok, g.want)
		}
	}
	bad := []struct {
		payload   string
		withBytes bool
	}{
		{"", true}, {"garbage", false}, {"8,16 R", true}, {"8 R 4096 () 0 + 8 [fio]", true},
		{"8,16 R () 0 + 8 [0]", true},            // bytes field missing
		{"8,16 R 4096 () x + 8 [fio]", true},     // sector not a number
		{"8,16 R 4096 () 0 - 8 [fio]", true},     // no " + "
		{"8,16 R 4096 () 0 + [fio]", true},       // nr missing
		{"a,16 R 4096 () 0 + 8 [fio]", true},     // major not a number
		{"8,16 R 4096 (no close 0 + 8", true},    // cmd not closed
		{"8,16 R 4096 0 + 8 [fio]", true},        // no (cmd)
		{"8,16 R 4096 () 0 + 99999999999", true}, // nr overflows uint32
	}
	for _, b := range bad {
		if got, ok := parseBlockRq(b.payload, b.withBytes); ok {
			t.Errorf("%q must not parse: %+v", b.payload, got)
		}
	}
}

func TestParseNVMe(t *testing.T) {
	type setup struct {
		ctrl, disk string
		qid, cmdid int
		op         string
	}
	good := []struct {
		payload string
		want    setup
	}{
		{"nvme0: disk=nvme0n1, qid=1, cmdid=40, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_read slba=0, len=7, ctrl=0x0, dsmgmt=0, reftag=0)",
			setup{"nvme0", "nvme0n1", 1, 40, "nvme_cmd_read"}},
		{"nvme1: disk=nvme1n2, qid=12, cmdid=4127, nsid=2, flags=0x0, meta=0x0, cmd=(nvme_cmd_write slba=8, len=255, ctrl=0x0, dsmgmt=0, reftag=0)",
			setup{"nvme1", "nvme1n2", 12, 4127, "nvme_cmd_write"}},
		{"nvme0: disk=nvme0n1, qid=6, cmdid=832, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_flush cdw10=00 00 00 00)",
			setup{"nvme0", "nvme0n1", 6, 832, "nvme_cmd_flush"}},
		// admin command: no disk= field
		{"nvme0: qid=0, cmdid=12, nsid=0, flags=0x0, meta=0x0, cmd=(nvme_admin_identify cns=1, ctrlid=0)",
			setup{"nvme0", "", 0, 12, "nvme_admin_identify"}},
		{"nvme0: qid=0, cmdid=13, nsid=0, flags=0x0, meta=0x0, cmd=(nvme_admin_keep_alive)",
			setup{"nvme0", "", 0, 13, "nvme_admin_keep_alive"}},
	}
	for _, g := range good {
		ctrl, disk, qid, cmdid, op, ok := parseNVMeSetup(g.payload)
		if got := (setup{ctrl, disk, qid, cmdid, op}); !ok || got != g.want {
			t.Errorf("%q: got %+v ok=%v, want %+v", g.payload, got, ok, g.want)
		}
	}
	for _, bad := range []string{"", "nvme0", "nvme0: disk=nvme0n1, cmdid=1, cmd=(nvme_cmd_read)",
		"nvme0: qid=x, cmdid=1, cmd=(nvme_cmd_read)", "nvme0: qid=1, cmdid=1, nsid=1"} {
		if _, _, _, _, _, ok := parseNVMeSetup(bad); ok {
			t.Errorf("setup %q must not parse", bad)
		}
	}

	done := []struct {
		payload    string
		ctrl       string
		qid, cmdid int
	}{
		{"nvme0: disk=nvme0n1, qid=2, cmdid=40, res=0x0, retries=0, flags=0x0, status=0x0", "nvme0", 2, 40},
		{"nvme3: qid=1, cmdid=7, res=0x0, retries=0, flags=0x0, status=0x0", "nvme3", 1, 7},
	}
	for _, d := range done {
		ctrl, qid, cmdid, ok := parseNVMeDone(d.payload)
		if !ok || ctrl != d.ctrl || qid != d.qid || cmdid != d.cmdid {
			t.Errorf("%q: got %s %d %d ok=%v", d.payload, ctrl, qid, cmdid, ok)
		}
	}
	for _, bad := range []string{"", "nvme0: disk=nvme0n1, res=0x0", "x: qid=1, cmdid=", "nvme0: qid=1, cmdid=z"} {
		if _, _, _, ok := parseNVMeDone(bad); ok {
			t.Errorf("complete %q must not parse", bad)
		}
	}
}

func TestDirOf(t *testing.T) {
	for _, c := range []struct {
		rwbs string
		nr   uint32
		want Dir
	}{
		{"R", 8, DirRead}, {"RA", 8, DirRead}, {"RS", 8, DirRead}, {"RM", 8, DirRead},
		{"W", 8, DirWrite}, {"WS", 8, DirWrite}, {"WFS", 8, DirWrite}, {"FWS", 8, DirWrite}, {"WF", 8, DirWrite},
	} {
		if d, ok := dirOf(c.rwbs, c.nr); !ok || d != c.want {
			t.Errorf("dirOf(%q, %d) = %q, %v; want %q", c.rwbs, c.nr, d, ok, c.want)
		}
	}
	for _, c := range []struct {
		rwbs string
		nr   uint32
	}{{"F", 0}, {"FF", 0}, {"W", 0}, {"R", 0}, {"D", 8}, {"DS", 8}, {"DE", 8}, {"E", 8}, {"N", 8}, {"", 8}, {"S", 8}} {
		if d, ok := dirOf(c.rwbs, c.nr); ok {
			t.Errorf("dirOf(%q, %d) = %q, must be ignored", c.rwbs, c.nr, d)
		}
	}
}

func BenchmarkFeed(b *testing.B) {
	const n = 100_000
	lines := make([]string, 0, 2*n)
	for i := 0; i < n; i++ {
		t := int64(10_000_000_000) + int64(i)*10_000
		sector := uint64(i%64) * 8
		lines = append(lines,
			fmt.Sprintf("fio-1234    [003] ..... %s: block_rq_issue: 8,16 W 4096 () %d + 8 none,0,0 [fio]", secs(t), sector),
			fmt.Sprintf("<idle>-0    [003] d.h1. %s: block_rq_complete: 8,16 W () %d + 8 none,0,0 [0]", secs(t+5_000), sector))
	}
	b.ResetTimer()
	for it := 0; it < b.N; it++ {
		c := newCollector(1000, 0)
		c.addDevice(&device{name: "sdb", source: SourceBlock, maj: 8, min: 16, blockLayer: true})
		for _, l := range lines {
			ev, ok := flushtrace.ParseLine(l)
			if !ok {
				b.Fatal("no parse")
			}
			c.feed(ev, 1_000_000_000)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(lines)), "ns/line")
}

// secs formats a trace-clock time in ns as the kernel prints it.
func secs(ns int64) string { return fmt.Sprintf("%d.%06d", ns/1e9, ns%1e9/1e3) }

func TestHeadName(t *testing.T) {
	for in, want := range map[string]string{
		"nvme0c1n1": "nvme0n1", "nvme12c3n45": "nvme12n45", "nvme0n1": "nvme0n1", "sdb": "sdb", "": "",
		"nvme0c1": "nvme0c1", "nvme0cn1": "nvme0cn1", "nvme0c1n": "nvme0c1n", "nvme0c1n1p2": "nvme0c1n1p2",
	} {
		if got := headName(in); got != want {
			t.Errorf("headName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseBlockBio(t *testing.T) {
	for _, c := range []struct {
		in   string
		want blockRq
		ok   bool
	}{
		{"9,127 WS 2048 + 16 [fio]", blockRq{Maj: 9, Min: 127, RWBS: "WS", Sector: 2048, Nr: 16}, true},
		{"253,3 R 0 + 8 [0]", blockRq{Maj: 253, Min: 3, RWBS: "R", Sector: 0, Nr: 8}, true},
		{"9,0 FWS 0 + 0 [kworker/u8:2]", blockRq{Maj: 9, Min: 0, RWBS: "FWS", Sector: 0, Nr: 0}, true},
		{"9,0 R 18446744073709551615 + 8 [-5]", blockRq{Maj: 9, Min: 0, RWBS: "R", Sector: 1<<64 - 1, Nr: 8}, true},
		{"9,0 R 10 8 [fio]", blockRq{}, false},
		{"9 R 10 + 8 [fio]", blockRq{}, false},
		{"", blockRq{}, false},
	} {
		got, ok := parseBlockBio(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseBlockBio(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// bioModel is the bio pairing by its definition, over a plain list of
// pending bios in queue order: the oldest pending bio of the node with the
// same end that starts at or before the completion pairs; else a pending bio
// covering the completion makes it a split part; else it is unpaired.
type bioModel struct {
	pending []struct {
		min        int
		start, end uint64
		t          int64
		dir        Dir
	}
	lat      map[Dir][]int64
	unpaired int64
}

func (m *bioModel) queue(min int, start, end uint64, t int64, dir Dir) {
	m.pending = append(m.pending, struct {
		min        int
		start, end uint64
		t          int64
		dir        Dir
	}{min, start, end, t, dir})
}

func (m *bioModel) complete(min int, start, end uint64, t int64) {
	for i, e := range m.pending {
		if e.min == min && e.end == end && e.start <= start {
			m.lat[e.dir] = append(m.lat[e.dir], t-e.t)
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			return
		}
	}
	for _, e := range m.pending {
		if e.min == min && e.end >= end && e.start <= start {
			return
		}
	}
	m.unpaired++
}

// The per-node index of pending bios pairs exactly like the definition, with
// many bios pending on two nodes, overlapping, split in parts and completed
// out of order, plus completions that match nothing.
func TestBioPairingIndexMatchesDefinition(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 3))
	for round := 0; round < 50; round++ {
		c := newCollector(1000, 0)
		nodes := []*device{
			{name: "md0", source: SourceBio, maj: 9, min: 0},
			{name: "md1", source: SourceBio, maj: 9, min: 1},
		}
		for _, d := range nodes {
			c.addDevice(d)
		}
		m := &bioModel{lat: map[Dir][]int64{}}
		now := int64(2_000_000_000)
		ev := func(name string, min int, rwbs string, start, end uint64) {
			now += 1000 + r.Int64N(100_000)
			c.feed(flushtrace.Event{TimeNs: now, Name: name,
				Payload: fmt.Sprintf("9,%d %s %d + %d [x]", min, rwbs, start, end-start)}, 1_000_000_000)
		}
		for step := 0; step < 3000; step++ {
			min := r.IntN(2)
			rwbs, dir := "W", DirWrite
			if r.IntN(2) == 0 {
				rwbs, dir = "R", DirRead
			}
			switch k := r.IntN(10); {
			case k < 4 || len(m.pending) == 0:
				start := uint64(r.IntN(512)) * 8
				end := start + []uint64{8, 16, 64, 256, 1024}[r.IntN(5)]
				ev("block_bio_queue", min, rwbs, start, end)
				m.queue(min, start, end, now, dir)
			case k < 9:
				p := m.pending[r.IntN(len(m.pending))]
				start, end := p.start, p.end
				if n := end - start; n > 8 {
					cut := start + uint64(1+r.IntN(int(n/8)-1))*8
					switch r.IntN(3) {
					case 0:
						end = cut // the front part split off
					case 1:
						start = cut // the remainder
					}
				}
				ev("block_bio_complete", p.min, rwbs, start, end)
				m.complete(p.min, start, end, now)
			default:
				start := uint64(r.IntN(600)) * 8
				end := start + uint64(1+r.IntN(64))*8
				ev("block_bio_complete", min, rwbs, start, end)
				m.complete(min, start, end, now)
			}
		}
		var unpaired int64
		for _, d := range nodes {
			unpaired += d.unpaired
		}
		if unpaired != m.unpaired {
			t.Fatalf("round %d: unpaired = %d, want %d", round, unpaired, m.unpaired)
		}
		pending := 0
		for _, q := range c.bio {
			pending += len(q)
		}
		spans := len(nodes[0].bioSpans) + len(nodes[1].bioSpans)
		if pending != len(m.pending) || spans != pending {
			t.Fatalf("round %d: pending %d, spans %d, want %d", round, pending, spans, len(m.pending))
		}
		_, sum := c.finish()
		for _, dir := range []Dir{DirRead, DirWrite} {
			var got int64
			for _, d := range nodes {
				got += sum[d.name][KindBio][dir].Count
			}
			if want := int64(len(m.lat[dir])); got != want {
				t.Fatalf("round %d: %s pairs = %d, want %d", round, dir, got, want)
			}
		}
	}
}

// BenchmarkBioSplitParts completes split parts with a deep queue pending on
// the node: each part looks for the bio covering it.
func BenchmarkBioSplitParts(b *testing.B) {
	c := newCollector(1000, 0)
	d := &device{name: "md0", source: SourceBio, maj: 9, min: 0}
	c.addDevice(d)
	for i := 0; i < 1024; i++ {
		c.feed(flushtrace.Event{TimeNs: 2_000_000_000, Name: "block_bio_queue",
			Payload: fmt.Sprintf("9,0 W %d + 256 [fio]", i*256)}, 1_000_000_000)
	}
	parts := make([]flushtrace.Event, 1024)
	for i := range parts {
		parts[i] = flushtrace.Event{TimeNs: 2_000_100_000, Name: "block_bio_complete",
			Payload: fmt.Sprintf("9,0 W %d + 128 [0]", i*256)}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.feed(parts[i%len(parts)], 1_000_000_000)
	}
}

// Once a series is resolved, a completion in the open window allocates
// nothing, through the aggregator or the collector's per-device cache.
func TestAggregatorAddAllocatesNothing(t *testing.T) {
	a := newAggregator(1000, 0)
	s := a.series("sda", KindDevice, DirRead)
	a.addTo(s, 0, 1)
	if n := testing.AllocsPerRun(1000, func() { a.addTo(s, 5*ms, 1000) }); n != 0 {
		t.Errorf("addTo: %v allocs per add", n)
	}
	c := newCollector(1000, 0)
	d := &device{name: "sdb", source: SourceBlock, maj: 8, min: 16}
	c.addDevice(d)
	c.add(d, KindDevice, DirWrite, 0, 1)
	if n := testing.AllocsPerRun(1000, func() { c.add(d, KindDevice, DirWrite, 5*ms, 1000) }); n != 0 {
		t.Errorf("collector add: %v allocs per add", n)
	}
	// A new window reuses the series' histogram: only the finished window
	// is appended (amortized), no histogram is allocated.
	c2 := newCollector(1000, 0)
	d2 := &device{name: "sdc", source: SourceBlock, maj: 8, min: 32}
	c2.addDevice(d2)
	c2.agg.done = make([]Window, 0, 2000)
	rel := int64(0)
	if n := testing.AllocsPerRun(1000, func() { rel += 1000 * ms; c2.add(d2, KindDevice, DirWrite, rel, 1000) }); n != 0 {
		t.Errorf("window change: %v allocs per add", n)
	}
}

// Pending NVMe commands keep their own copy of the controller name, one per
// controller, not a slice of the trace line.
func TestNVMeControllerNameInterned(t *testing.T) {
	c := newCollector(1000, 0)
	c.addDevice(&device{name: "nvme0n1", source: SourceNVMe})
	line := []byte("nvme0: disk=nvme0n1, qid=1, cmdid=40, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_read slba=0, len=7)")
	c.feed(flushtrace.Event{TimeNs: 2, Name: "nvme_setup_cmd", Payload: string(line)}, 1)
	c.feed(flushtrace.Event{TimeNs: 3, Name: "nvme_setup_cmd", Payload: strings.Replace(string(line), "cmdid=40", "cmdid=41", 1)}, 1)
	var names []string
	for k := range c.nvme {
		names = append(names, k.ctrl)
	}
	if len(names) != 2 || names[0] != "nvme0" || unsafe.StringData(names[0]) != unsafe.StringData(names[1]) {
		t.Fatalf("pending controllers = %q", names)
	}
	if len(c.ctrlNames) != 1 {
		t.Errorf("interned names = %v", c.ctrlNames)
	}
}
