package iotrace

import (
	"encoding/json"
	"hash/fnv"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"bbbench/internal/flushtrace"
	"bbbench/internal/tracefake"
)

// runRaw runs a session whose instance has binary per-CPU pipes holding the
// events, on a tree with format files for the given events.
func runRaw(t *testing.T, devices, events []string, sys string, opts Options, evs []tracefake.Event, cpus int, po tracefake.PageOptions) Result {
	t.Helper()
	root, inst := fakeTracefs(t, events, nil)
	tr, err := tracefake.WriteTree(root, 4096, events...)
	must(t, err)
	var keep []tracefake.Event
	for _, e := range evs { // the kernel records only the events the tree has
		if _, ok := tr.Formats[e.Name]; ok || e.Lost > 0 {
			keep = append(keep, e)
		}
	}
	pages, err := tr.Pages(keep, po)
	must(t, err)
	must(t, tracefake.WritePipes(inst, cpus, pages))
	opts.TracefsRoot, opts.SysBlock, opts.InstanceName = root, sys, "test"
	s, err := Start(devices, opts)
	if err != nil {
		t.Fatalf("Start (raw): %v", err)
	}
	res, _ := s.Stop()
	if res.Summary.Reader != flushtrace.ReaderRaw {
		t.Fatalf("raw session read %q: %s", res.Summary.Reader, res.Summary.ReaderNote)
	}
	return res
}

// comparable drops what differs by construction between the readers.
func comparable(r Result) Result {
	r.Summary.Reader, r.Summary.ReaderNote, r.Summary.Clock, r.Summary.Late = "", "", "", 0
	return r
}

func dump(r Result) string { b, _ := json.MarshalIndent(r, "", " "); return string(b) }

// checkRawLikeText runs the lines of a text-path test through the raw
// reader as well: read back as events (tracefake's own reading of the
// text), spread over 4 CPUs at random when they are in time order (all on
// one CPU otherwise, which keeps their order), encoded as pages. Both
// readers must give the same result.
func checkRawLikeText(t *testing.T, devices, events []string, sys string, opts Options, lines []string, want Result) {
	t.Helper()
	evs, _ := tracefake.FromText(lines)
	cpus := 4
	if tracefake.Sorted(evs) {
		h := fnv.New64()
		h.Write([]byte(t.Name()))
		evs = tracefake.Spread(evs, cpus, int64(h.Sum64()>>1))
	} else {
		cpus = 1
		for i := range evs {
			evs[i].CPU = 0
		}
	}
	got := runRaw(t, devices, events, sys, opts, evs, cpus, tracefake.PageOptions{})
	if a, b := comparable(want), comparable(got); !reflect.DeepEqual(a, b) {
		t.Errorf("raw reader differs from text reader\ntext: %s\nraw:  %s", dump(a), dump(b))
	}
}

func TestRawSummaryFields(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	d := tracefake.Dev(8, 16)
	res := runRaw(t, []string{"sdb"}, issueOnly, sys, Options{}, []tracefake.Event{
		{CPU: 0, TimeNs: start, Name: "print", Text: "bbbench-start"},
		{CPU: 1, TimeNs: 2 * sec, Name: "block_rq_issue", Dev: d, Nr: 8, RWBS: "R"},
		{CPU: 0, TimeNs: 2*sec + 90*usec, Name: "block_rq_complete", Dev: d, Nr: 8, RWBS: "R"},
	}, 2, tracefake.PageOptions{})
	wantStat(t, dev(t, res.Summary, "sdb"), KindDevice, DirRead, 1, 90_000, 90_000, 90_000)
	if res.Summary.Late != 0 || res.Summary.ReaderNote != "" || strings.Contains(res.Summary.Text, "raw") {
		t.Errorf("summary %+v", res.Summary)
	}
}

// randomStream simulates a phase on an HDD with a scheduler (sdb: insert,
// issue, merge, complete), an NVMe namespace (setup, complete; cmdids
// reused) and an md node (bios), with two start markers, overruns and
// events dropped at random; timestamps are unique, in ns.
func randomStream(seed int64, n int) []tracefake.Event {
	r := newRand(seed)
	var evs []tracefake.Event
	sdb, md := tracefake.Dev(8, 16), tracefake.Dev(9, 127)
	rw := []string{"R", "W", "WS", "RA"}
	for i := 0; i < n; i++ {
		at := sec/2 + r.int63n(20*sec)
		lat := 1 + r.int63n(5_000_000)
		switch r.intn(3) {
		case 0:
			sector, nr, op := uint64(r.intn(64))*8, uint32(8*(1+r.intn(2))), rw[r.intn(len(rw))]
			e := tracefake.Event{Dev: sdb, Sector: sector, Nr: nr, RWBS: op, Comm: "fio"}
			ins, iss, done := e, e, e
			ins.Name, ins.TimeNs = "block_rq_insert", at
			evs = append(evs, ins)
			if r.intn(10) == 0 { // absorbed by a merge
				m := e
				m.Name, m.TimeNs = "block_rq_merge", at+lat/3
				evs = append(evs, m)
				continue
			}
			iss.Name, iss.TimeNs = "block_rq_issue", at+lat/3
			done.Name, done.TimeNs = "block_rq_complete", at+lat
			evs = append(evs, iss, done)
		case 1:
			e := tracefake.Event{CtrlID: 0, Disk: "nvme0n1", QID: 1 + r.intn(4), CID: r.intn(40), Opcode: 1 + r.intn(2)}
			s, d := e, e
			s.Name, s.TimeNs = "nvme_setup_cmd", at
			d.Name, d.TimeNs = "nvme_complete_rq", at+lat
			evs = append(evs, s, d)
		case 2:
			e := tracefake.Event{Dev: md, Sector: uint64(r.intn(256)) * 8, Nr: uint32(8 * (1 + r.intn(4))), RWBS: rw[r.intn(2)], Comm: "fio"}
			q, d := e, e
			q.Name, q.TimeNs = "block_bio_queue", at
			d.Name, d.TimeNs = "block_bio_complete", at+lat
			evs = append(evs, q, d)
		}
	}
	evs = append(evs,
		tracefake.Event{TimeNs: start, Name: "print", Text: "bbbench-start"},
		tracefake.Event{TimeNs: 3 * sec, Name: "print", Text: "bbbench-start"})
	// unique timestamps, then time order
	for i := range evs {
		evs[i].TimeNs = evs[i].TimeNs*16 + int64(i%16)
	}
	sortEvents(evs)
	var out []tracefake.Event
	for _, e := range evs {
		if r.intn(200) == 0 && e.Name != "print" {
			continue // dropped by an overrun
		}
		if r.intn(500) == 0 {
			out = append(out, tracefake.Event{Lost: int64(1 + r.intn(1000))})
		}
		out = append(out, e)
	}
	return out
}

// The same random stream through the text pipe and through raw pages on
// several CPUs (with every record type) gives the same pairing, windows,
// unpaired and overrun counts.
func TestRawMatchesTextRandom(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		sys := fakeSys(t, map[string]string{"sdb": "8:16", "nvme0n1": "259:0", "md0": "9:127"}, "sdb")
		bioNode(t, sys, "md0")
		devices := []string{"md0", "sdb", "nvme0n1"}
		events := append(append(append([]string{}, blockEvents...), nvmeEvents...), bioEvents...)
		evs := randomStream(seed, 3000)
		cpus := 1 + int(seed%6)
		spread := tracefake.Spread(evs, cpus, seed)
		var lines []string
		for _, e := range spread {
			lines = append(lines, e.Line())
		}
		opts := Options{AvgMsec: 500, OffsetMs: 1000}
		root, _ := fakeTracefs(t, events, lines)
		o := opts
		o.TracefsRoot, o.SysBlock, o.InstanceName = root, sys, "test"
		s, err := Start(devices, o)
		must(t, err)
		text, _ := s.Stop()
		po := tracefake.PageOptions{LongEvery: int(seed % 3), AbsoluteEvery: int(seed % 4 * 7), PadEvery: int(seed % 5 * 3)}
		raw := runRaw(t, devices, events, sys, opts, spread, cpus, po)
		if raw.Summary.Late != 0 {
			t.Errorf("seed %d: %d late", seed, raw.Summary.Late)
		}
		if a, b := comparable(text), comparable(raw); !reflect.DeepEqual(a, b) {
			t.Errorf("seed %d (%d CPUs, %+v): raw differs\ntext: %s\nraw:  %s", seed, cpus, po, dump(a), dump(b))
		}
		if text.Summary.Overrun == 0 || dev(t, text.Summary, "sdb").Stats[KindBlockLayer][DirWrite].Count == 0 ||
			dev(t, text.Summary, "nvme0n1").Stats[KindDevice][DirRead].Count == 0 || dev(t, text.Summary, "md0").Stats[KindBio][DirRead].Count == 0 {
			t.Errorf("seed %d: the stream does not exercise every source: %s", seed, dump(text))
		}
	}
}

type rnd struct{ s uint64 }

func newRand(seed int64) *rnd { return &rnd{uint64(seed)*0x9e3779b97f4a7c15 + 1} }

func (r *rnd) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}
func (r *rnd) intn(n int) int       { return int(r.next() % uint64(n)) }
func (r *rnd) int63n(n int64) int64 { return int64(r.next() % uint64(n)) }

func sortEvents(evs []tracefake.Event) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].TimeNs < evs[j].TimeNs })
}

// benchEvents is a phase of n random reads and writes on an NVMe namespace
// and an HDD with a scheduler, without overruns.
func benchEvents(n int) []tracefake.Event {
	r := newRand(42)
	sdb := tracefake.Dev(8, 16)
	var evs []tracefake.Event
	for i := 0; len(evs) < n; i++ {
		at := start + int64(i)*10*usec
		if i%2 == 0 {
			e := tracefake.Event{Dev: sdb, Sector: uint64(i) * 8, Nr: 8, RWBS: "R", Comm: "fio"}
			a, b, c := e, e, e
			a.Name, a.TimeNs = "block_rq_insert", at
			b.Name, b.TimeNs = "block_rq_issue", at+usec
			c.Name, c.TimeNs = "block_rq_complete", at+usec+r.int63n(4000*usec)
			evs = append(evs, a, b, c)
		} else {
			e := tracefake.Event{Disk: "nvme0n1", QID: 1 + i%8, CID: i % 4096, Opcode: 2}
			a, b := e, e
			a.Name, a.TimeNs = "nvme_setup_cmd", at
			b.Name, b.TimeNs = "nvme_complete_rq", at+r.int63n(200*usec)
			evs = append(evs, a, b)
		}
	}
	evs = append(evs, tracefake.Event{TimeNs: start - 1, Name: "print", Text: "bbbench-start"})
	sortEvents(evs)
	return evs
}

const benchN = 200_000

// BenchmarkSessionText and BenchmarkSessionRaw time a whole session on the
// same events: read (from a file standing in for the pipe), parse or
// decode, order (the raw merge over 8 CPUs), pair and aggregate. The text
// side leaves out the kernel's formatting of every event, which a real
// trace_pipe costs on top: the comparison favours the text reader.
func BenchmarkSessionText(b *testing.B) {
	t := &testing.T{}
	evs := benchEvents(benchN)
	lines := make([]string, len(evs))
	for i, e := range evs {
		lines[i] = e.Line()
	}
	sys := fakeSys(t, map[string]string{"sdb": "8:16", "nvme0n1": "259:0"}, "sdb")
	events := append(append([]string{}, blockEvents...), nvmeEvents...)
	b.ResetTimer()
	for k := 0; k < b.N; k++ {
		b.StopTimer()
		root, _ := fakeTracefs(t, events, lines)
		runtime.GC()
		b.StartTimer()
		s, err := Start([]string{"sdb", "nvme0n1"}, Options{TracefsRoot: root, SysBlock: sys, InstanceName: "test"})
		if err != nil {
			b.Fatal(err)
		}
		res, _ := s.Stop()
		if res.Summary.Reader != flushtrace.ReaderText || res.Summary.Lost != 0 {
			b.Fatalf("%+v", res.Summary)
		}
	}
	b.ReportMetric(float64(len(evs))*float64(b.N)/b.Elapsed().Seconds(), "events/s")
}

func BenchmarkSessionRaw(b *testing.B) {
	t := &testing.T{}
	evs := tracefake.Spread(benchEvents(benchN), 8, 1)
	sys := fakeSys(t, map[string]string{"sdb": "8:16", "nvme0n1": "259:0"}, "sdb")
	events := append(append([]string{}, blockEvents...), nvmeEvents...)
	tr, err := tracefake.WriteTree(b.TempDir(), 4096, events...)
	if err != nil {
		b.Fatal(err)
	}
	pages, err := tr.Pages(evs, tracefake.PageOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for k := 0; k < b.N; k++ {
		b.StopTimer()
		root, inst := fakeTracefs(t, events, nil)
		if _, err := tracefake.WriteTree(root, 4096, events...); err != nil {
			b.Fatal(err)
		}
		if err := tracefake.WritePipes(inst, 8, pages); err != nil {
			b.Fatal(err)
		}
		runtime.GC()
		b.StartTimer()
		s, err := Start([]string{"sdb", "nvme0n1"}, Options{TracefsRoot: root, SysBlock: sys, InstanceName: "test"})
		if err != nil {
			b.Fatal(err)
		}
		res, _ := s.Stop()
		if res.Summary.Reader != flushtrace.ReaderRaw || res.Summary.Lost != 0 {
			b.Fatalf("%+v", res.Summary)
		}
	}
	b.ReportMetric(float64(len(evs))*float64(b.N)/b.Elapsed().Seconds(), "events/s")
}
