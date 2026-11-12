package iotrace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	sec   = int64(1_000_000_000)
	usec  = int64(1_000)
	start = 1 * sec // trace-clock time of the start marker in the fake pipes
)

var (
	nvmeEvents  = []string{"nvme/nvme_setup_cmd", "nvme/nvme_complete_rq"}
	blockEvents = []string{"block/block_rq_issue", "block/block_rq_complete", "block/block_rq_insert", "block/block_rq_merge"}
	issueOnly   = []string{"block/block_rq_issue", "block/block_rq_complete"}
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeTracefs builds a tracefs tree with the given events and a pre-created
// instance "test" whose trace_pipe holds the given lines.
func fakeTracefs(t *testing.T, events []string, lines []string) (root, inst string) {
	t.Helper()
	root = t.TempDir()
	inst = filepath.Join(root, "instances", "test")
	must(t, os.MkdirAll(inst, 0o755))
	for _, e := range events {
		must(t, os.MkdirAll(filepath.Join(root, "events", e), 0o755))
		must(t, os.MkdirAll(filepath.Join(inst, "events", e), 0o755))
	}
	pipe := strings.Join(lines, "\n")
	if pipe != "" {
		pipe += "\n"
	}
	must(t, os.WriteFile(filepath.Join(inst, "trace_pipe"), []byte(pipe), 0o644))
	return root, inst
}

// fakeSys builds /sys/block with the given devices: name -> "maj:min" and
// whether it is rotational.
func fakeSys(t *testing.T, devs map[string]string, rotational ...string) string {
	t.Helper()
	sys := t.TempDir()
	for name, dev := range devs {
		must(t, os.MkdirAll(filepath.Join(sys, name, "queue"), 0o755))
		must(t, os.WriteFile(filepath.Join(sys, name, "dev"), []byte(dev+"\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(sys, name, "queue", "rotational"), []byte("0\n"), 0o644))
	}
	for _, name := range rotational {
		must(t, os.WriteFile(filepath.Join(sys, name, "queue", "rotational"), []byte("1\n"), 0o644))
	}
	return sys
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func marker(ns int64, text string) string {
	return fmt.Sprintf("bbbench-1 [000] ..... %s: tracing_mark_write: %s", secs(ns), text)
}

// blk returns a block_rq_insert/issue/merge line (with the bytes field) or a
// block_rq_complete line (without).
func blk(ns int64, ev, dev, rwbs string, sector uint64, nr uint32) string {
	if ev == "block_rq_complete" {
		return fmt.Sprintf("<idle>-0 [001] d.h1. %s: block_rq_complete: %s %s () %d + %d none,0,0 [0]", secs(ns), dev, rwbs, sector, nr)
	}
	return fmt.Sprintf("fio-1 [001] ..... %s: %s: %s %s %d () %d + %d none,0,0 [fio]", secs(ns), ev, dev, rwbs, nr*512, sector, nr)
}

func nvmeSetup(ns int64, disk string, qid, cmdid int, cmd string) string {
	d := ""
	if disk != "" {
		d = "disk=" + disk + ", "
	}
	return fmt.Sprintf("fio-1 [001] ..... %s: nvme_setup_cmd: nvme0: %sqid=%d, cmdid=%d, nsid=1, flags=0x0, meta=0x0, cmd=(%s)", secs(ns), d, qid, cmdid, cmd)
}

func nvmeDone(ns int64, disk string, qid, cmdid int) string {
	d := ""
	if disk != "" {
		d = "disk=" + disk + ", "
	}
	return fmt.Sprintf("<idle>-0 [001] d.h1. %s: nvme_complete_rq: nvme0: %sqid=%d, cmdid=%d, res=0x0, retries=0, flags=0x0, status=0x0", secs(ns), d, qid, cmdid)
}

func run(t *testing.T, devices []string, events []string, sys string, opts Options, lines ...string) (Result, string) {
	t.Helper()
	root, inst := fakeTracefs(t, events, lines)
	opts.TracefsRoot, opts.SysBlock, opts.InstanceName = root, sys, "test"
	s, err := Start(devices, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res, _ := s.Stop() // the fake instance directory is not empty: a cleanup error is expected
	return res, inst
}

func dev(t *testing.T, s Summary, name string) DeviceSummary {
	t.Helper()
	for _, d := range s.Devices {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no device %s in %+v", name, s.Devices)
	return DeviceSummary{}
}

func wantStat(t *testing.T, d DeviceSummary, k Kind, dir Dir, count int64, mean float64, min, max int64) {
	t.Helper()
	st, ok := d.Stats[k][dir]
	if !ok {
		t.Errorf("%s: no %s/%s stats in %+v", d.Name, k, dir, d.Stats)
		return
	}
	if st.Count != count || st.MeanNs != mean || st.MinNs != min || st.MaxNs != max {
		t.Errorf("%s %s/%s = %+v, want count %d mean %v min %d max %d", d.Name, k, dir, st, count, mean, min, max)
	}
}

func TestSession_NVMeReadWrite(t *testing.T) {
	sys := fakeSys(t, map[string]string{"nvme0n1": "259:0"})
	t0 := 10 * sec
	res, inst := run(t, []string{"nvme0n1"}, append(append([]string{}, nvmeEvents...), blockEvents...), sys, Options{AvgMsec: 1000},
		marker(start, "bbbench-start"),
		// read: block insert -> issue -> nvme setup -> nvme complete -> block complete
		blk(t0, "block_rq_insert", "259,0", "R", 100, 8),
		blk(t0+10*usec, "block_rq_issue", "259,0", "R", 100, 8),
		nvmeSetup(t0+20*usec, "nvme0n1", 1, 5, "nvme_cmd_read slba=100, len=7, ctrl=0x0, dsmgmt=0, reftag=0"),
		nvmeDone(t0+120*usec, "nvme0n1", 1, 5),
		blk(t0+130*usec, "block_rq_complete", "259,0", "R", 100, 8),
		// write
		blk(t0+500_000*usec, "block_rq_insert", "259,0", "WS", 200, 8),
		blk(t0+500_005*usec, "block_rq_issue", "259,0", "WS", 200, 8),
		nvmeSetup(t0+500_010*usec, "nvme0n1", 1, 6, "nvme_cmd_write slba=200, len=7, ctrl=0x0, dsmgmt=0, reftag=0"),
		nvmeDone(t0+500_310*usec, "nvme0n1", 1, 6),
		blk(t0+500_320*usec, "block_rq_complete", "259,0", "WS", 200, 8),
		// an admin command and a Flush: ignored
		nvmeSetup(t0+600_000*usec, "", 0, 1, "nvme_admin_identify cns=1, ctrlid=0"),
		nvmeDone(t0+700_000*usec, "", 0, 1),
		blk(t0+799_000*usec, "block_rq_issue", "259,0", "FF", 0, 0),
		nvmeSetup(t0+800_000*usec, "nvme0n1", 1, 7, "nvme_cmd_flush cdw10=00 00 00 00"),
		nvmeDone(t0+900_000*usec, "nvme0n1", 1, 7),
		blk(t0+900_010*usec, "block_rq_complete", "259,0", "FF", 0, 0),
		marker(t0+sec, "bbbench-stop"),
	)
	d := dev(t, res.Summary, "nvme0n1")
	if d.Source != SourceNVMe || d.Pair != "ctrl,qid,cmdid" || !d.Exact || !d.BlockLayer || d.Unpaired != 0 || d.Note != "" {
		t.Errorf("device = %+v", d)
	}
	if want := []string{"nvme_setup_cmd -> nvme_complete_rq", "block_rq_insert -> block_rq_complete"}; strings.Join(d.Events, "|") != strings.Join(want, "|") {
		t.Errorf("events = %q", d.Events)
	}
	wantStat(t, d, KindDevice, DirRead, 1, 100_000, 100_000, 100_000)
	wantStat(t, d, KindDevice, DirWrite, 1, 300_000, 300_000, 300_000)
	wantStat(t, d, KindBlockLayer, DirRead, 1, 130_000, 130_000, 130_000)
	wantStat(t, d, KindBlockLayer, DirWrite, 1, 320_000, 320_000, 320_000)
	if res.Summary.Lost != 0 || res.Summary.AvgMsec != 1000 {
		t.Errorf("summary = %+v", res.Summary)
	}
	// Completions 9.0001 s .. 9.5003 s after the start: all in the window ending at 10 s.
	if len(res.Windows) != 4 {
		t.Fatalf("windows = %+v", res.Windows)
	}
	for _, w := range res.Windows {
		if w.EndMs != 10_000 || w.Dev != "nvme0n1" || w.Count != 1 {
			t.Errorf("window %+v", w)
		}
	}
	if f := readFile(t, filepath.Join(inst, "events/nvme/nvme_setup_cmd/filter")); f != "(opcode == 1 || opcode == 2) && qid > 0" {
		t.Errorf("nvme filter = %q", f)
	}
	for _, e := range []string{"block_rq_issue", "block_rq_complete", "block_rq_insert", "block_rq_merge"} {
		if f := readFile(t, filepath.Join(inst, "events/block", e, "filter")); f != fmt.Sprint("dev == ", 259<<20) {
			t.Errorf("%s filter = %q", e, f)
		}
		if v := readFile(t, filepath.Join(inst, "events/block", e, "enable")); v != "0" {
			t.Errorf("%s left enabled: %q", e, v)
		}
	}
}

func TestSession_HDD(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	t0 := 10 * sec
	ms := int64(1_000_000)
	res, inst := run(t, []string{"sdb"}, blockEvents, sys, Options{},
		marker(start, "bbbench-start"),
		blk(t0, "block_rq_insert", "8,16", "R", 1000, 8),
		blk(t0+2*ms, "block_rq_issue", "8,16", "R", 1000, 8),
		blk(t0+7*ms, "block_rq_complete", "8,16", "R", 1000, 8), // device 5 ms, block layer 7 ms
		blk(t0+sec, "block_rq_insert", "8,16", "W", 5000, 8),
		blk(t0+sec+100*ms, "block_rq_issue", "8,16", "W", 5000, 8),
		blk(t0+2*sec, "block_rq_complete", "8,16", "W", 5000, 8), // device 900 ms, block layer 1 s
	)
	if _, err := os.Stat(filepath.Join(inst, "events", "nvme")); err == nil {
		t.Error("an nvme event directory was touched")
	}
	d := dev(t, res.Summary, "sdb")
	if d.Source != SourceBlock || d.Pair != "dev,sector" || !d.Exact || !d.BlockLayer || d.Unpaired != 0 {
		t.Errorf("device = %+v", d)
	}
	if want := "block_rq_issue -> block_rq_complete|block_rq_insert -> block_rq_complete"; strings.Join(d.Events, "|") != want {
		t.Errorf("events = %q", d.Events)
	}
	wantStat(t, d, KindDevice, DirRead, 1, 5e6, 5*ms, 5*ms)
	wantStat(t, d, KindBlockLayer, DirRead, 1, 7e6, 7*ms, 7*ms)
	wantStat(t, d, KindDevice, DirWrite, 1, 900e6, 900*ms, 900*ms)
	wantStat(t, d, KindBlockLayer, DirWrite, 1, 1e9, sec, sec)
	want := "ftrace: block_rq_issue -> block_rq_complete, block_rq_insert -> block_rq_complete · pairing: exact (dev,sector) · lost events: 0"
	if res.Summary.Text != want || strings.Contains(res.Summary.Text, "nvme") {
		t.Errorf("text = %q\nwant   %q", res.Summary.Text, want)
	}
}

func TestSession_HDDLongQueue(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	const n = 32
	t0 := 10 * sec
	ms := int64(1_000_000)
	type req struct {
		sector        uint64
		nr            uint32
		rwbs          string
		ins, iss, cmp int64
	}
	reqs := make([]req, n)
	merged := map[int]bool{3: true, 7: true, 11: true}
	var lines []struct {
		ns   int64
		line string
	}
	add := func(ns int64, l string) {
		lines = append(lines, struct {
			ns   int64
			line string
		}{ns, l})
	}
	for i := range reqs {
		r := &reqs[i]
		r.sector, r.nr, r.rwbs = uint64(i)*1000, 8, "R"
		if i%2 == 1 || i == 30 {
			r.rwbs = "WS"
		}
		if i == 31 { // a second write to the sector of 30, pending at the same time
			r.sector = reqs[30].sector
		}
		r.ins = t0 + int64(i)*ms
		add(r.ins, blk(r.ins, "block_rq_insert", "8,16", r.rwbs, r.sector, 8))
		if merged[i] { // the following request is back-merged into this one
			add(r.ins+500*usec, blk(r.ins+500*usec, "block_rq_insert", "8,16", r.rwbs, r.sector+8, 8))
			add(r.ins+600*usec, blk(r.ins+600*usec, "block_rq_merge", "8,16", r.rwbs, r.sector+8, 8))
			r.nr = 16
		}
	}
	// Issued in reverse order (the two same-sector writes last, in order),
	// completed in a scrambled order over about 3 s.
	order := []int{}
	for i := 29; i >= 0; i-- {
		order = append(order, i)
	}
	order = append(order, 30, 31)
	for k, i := range order {
		reqs[i].iss = t0 + 100*ms + int64(k)*ms
		add(reqs[i].iss, blk(reqs[i].iss, "block_rq_issue", "8,16", reqs[i].rwbs, reqs[i].sector, reqs[i].nr))
	}
	slotOf := map[int]int{30: 30, 31: 31}
	for k := 0; k < 30; k++ {
		slotOf[(k*7)%30] = k
	}
	for i := range reqs {
		r := &reqs[i]
		r.cmp = t0 + 200*ms + int64(slotOf[i])*95*ms + 300*usec
		add(r.cmp, blk(r.cmp, "block_rq_complete", "8,16", r.rwbs, r.sector, r.nr))
	}
	sort.SliceStable(lines, func(a, b int) bool { return lines[a].ns < lines[b].ns })
	pipe := []string{marker(start, "bbbench-start")}
	for _, l := range lines {
		pipe = append(pipe, l.line)
	}

	// 1 ms windows: each completion lands in a window of its own, so every
	// pair's latency can be checked.
	res, _ := run(t, []string{"sdb"}, blockEvents, sys, Options{AvgMsec: 1}, pipe...)
	d := dev(t, res.Summary, "sdb")
	if d.Unpaired != 0 || res.Summary.Lost != 0 {
		t.Errorf("unpaired %d, lost %d", d.Unpaired, res.Summary.Lost)
	}
	var count int64
	for _, byDir := range d.Stats[KindDevice] {
		count += byDir.Count
	}
	if count != n {
		t.Errorf("%d device completions, want %d", count, n)
	}
	for i, r := range reqs {
		dir := DirRead
		if r.rwbs == "WS" {
			dir = DirWrite
		}
		end := (r.cmp-start)/ms + 1
		for _, c := range []struct {
			k   Kind
			lat int64
		}{{KindDevice, r.cmp - r.iss}, {KindBlockLayer, r.cmp - r.ins}} {
			w, ok := find(res.Windows, end, "sdb", c.k, dir)
			if !ok || w.Count != 1 || w.MeanNs != float64(c.lat) {
				t.Errorf("request %d %s: window %+v ok=%v, want latency %d", i, c.k, w, ok, c.lat)
			}
		}
	}
	first, last := res.Windows[0].EndMs, res.Windows[len(res.Windows)-1].EndMs
	if last-first < 2900 {
		t.Errorf("windows span %d..%d ms, want about 3 s", first, last)
	}
}

func TestSession_BlockDirectIssue(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	res, _ := run(t, []string{"sdb"}, blockEvents, sys, Options{},
		marker(start, "bbbench-start"),
		blk(10*sec, "block_rq_issue", "8,16", "R", 0, 8),
		blk(10*sec+4000*usec, "block_rq_complete", "8,16", "R", 0, 8),
	)
	d := dev(t, res.Summary, "sdb")
	wantStat(t, d, KindDevice, DirRead, 1, 4e6, 4e6, 4e6)
	wantStat(t, d, KindBlockLayer, DirRead, 1, 4e6, 4e6, 4e6)
	if d.Unpaired != 0 {
		t.Errorf("unpaired = %d", d.Unpaired)
	}
}

func TestSession_MergeNotLost(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	res, _ := run(t, []string{"sdb"}, blockEvents, sys, Options{},
		marker(start, "bbbench-start"),
		blk(10*sec, "block_rq_insert", "8,16", "W", 0, 8),
		blk(10*sec+10*usec, "block_rq_insert", "8,16", "W", 8, 8),
		blk(10*sec+20*usec, "block_rq_merge", "8,16", "W", 8, 8),
		blk(10*sec+30*usec, "block_rq_issue", "8,16", "W", 0, 16),
		blk(10*sec+530*usec, "block_rq_complete", "8,16", "W", 0, 16),
	)
	d := dev(t, res.Summary, "sdb")
	if d.Unpaired != 0 || res.Summary.Lost != 0 {
		t.Errorf("unpaired = %d, lost = %d", d.Unpaired, res.Summary.Lost)
	}
	wantStat(t, d, KindDevice, DirWrite, 1, 500_000, 500_000, 500_000)
	wantStat(t, d, KindBlockLayer, DirWrite, 1, 530_000, 530_000, 530_000)
}

func TestSession_UnpairedAndOverrun(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	res, _ := run(t, []string{"sdb"}, blockEvents, sys, Options{},
		marker(start, "bbbench-start"),
		"CPU:1 [LOST 10 EVENTS]",
		blk(10*sec, "block_rq_complete", "8,16", "R", 64, 8), // its issue was lost
		blk(11*sec, "block_rq_issue", "8,16", "R", 0, 8),
		blk(11*sec+100*usec, "block_rq_complete", "8,16", "R", 0, 8),
	)
	d := dev(t, res.Summary, "sdb")
	if d.Unpaired != 1 || res.Summary.Overrun != 10 || res.Summary.Lost != 11 {
		t.Errorf("unpaired %d, overrun %d, lost %d", d.Unpaired, res.Summary.Overrun, res.Summary.Lost)
	}
	wantStat(t, d, KindDevice, DirRead, 1, 100_000, 100_000, 100_000)
	if !strings.HasSuffix(res.Summary.Text, " · lost events: 11 (ring buffer 10, unpaired 1)") {
		t.Errorf("text = %q", res.Summary.Text)
	}
}

func TestSession_PendingAtStopIsUnpaired(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16", "nvme0n1": "259:0"}, "sdb")
	res, _ := run(t, []string{"sdb", "nvme0n1"}, append(append([]string{}, nvmeEvents...), blockEvents...), sys, Options{},
		marker(start, "bbbench-start"),
		blk(10*sec, "block_rq_insert", "8,16", "W", 0, 8),
		nvmeSetup(10*sec, "nvme0n1", 3, 9, "nvme_cmd_read slba=0, len=7"),
	)
	if a, b := dev(t, res.Summary, "sdb").Unpaired, dev(t, res.Summary, "nvme0n1").Unpaired; a != 1 || b != 1 {
		t.Errorf("unpaired sdb %d nvme0n1 %d, want 1 and 1", a, b)
	}
}

func TestSession_NoInsertTracepoint(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	res, inst := run(t, []string{"sdb"}, issueOnly, sys, Options{},
		marker(start, "bbbench-start"),
		blk(10*sec, "block_rq_issue", "8,16", "R", 0, 8),
		blk(10*sec+250*usec, "block_rq_complete", "8,16", "R", 0, 8),
	)
	d := dev(t, res.Summary, "sdb")
	if d.BlockLayer || len(d.Events) != 1 || d.Events[0] != "block_rq_issue -> block_rq_complete" {
		t.Errorf("device = %+v", d)
	}
	if _, ok := d.Stats[KindBlockLayer]; ok {
		t.Errorf("block layer stats without block_rq_insert: %+v", d.Stats)
	}
	wantStat(t, d, KindDevice, DirRead, 1, 250_000, 250_000, 250_000)
	for _, e := range []string{"block_rq_insert", "block_rq_merge"} {
		if _, err := os.Stat(filepath.Join(inst, "events/block", e)); err == nil {
			t.Errorf("%s was touched", e)
		}
	}
	if want := "ftrace: block_rq_issue -> block_rq_complete · pairing: exact (dev,sector) · lost events: 0"; res.Summary.Text != want {
		t.Errorf("text = %q", res.Summary.Text)
	}
}

func TestSession_NVMeFallsBackToBlock(t *testing.T) {
	sys := fakeSys(t, map[string]string{"nvme0n1": "259:0"})
	res, _ := run(t, []string{"nvme0n1"}, blockEvents, sys, Options{},
		marker(start, "bbbench-start"),
		blk(10*sec, "block_rq_insert", "259,0", "W", 0, 8),
		blk(10*sec+5*usec, "block_rq_issue", "259,0", "W", 0, 8),
		blk(10*sec+25*usec, "block_rq_complete", "259,0", "W", 0, 8),
	)
	d := dev(t, res.Summary, "nvme0n1")
	if d.Source != SourceBlock || d.Pair != "dev,sector" || d.Events[0] != "block_rq_issue -> block_rq_complete" {
		t.Errorf("device = %+v", d)
	}
	wantStat(t, d, KindDevice, DirWrite, 1, 20_000, 20_000, 20_000)
	wantStat(t, d, KindBlockLayer, DirWrite, 1, 25_000, 25_000, 25_000)
}

func TestSession_ArrayMembers(t *testing.T) {
	blockText := "block_rq_issue -> block_rq_complete, block_rq_insert -> block_rq_complete · pairing: exact (dev,sector)"
	t.Run("hdd members", func(t *testing.T) {
		sys := fakeSys(t, map[string]string{"md0": "9:0", "sdb": "8:16", "sdc": "8:32"}, "sdb", "sdc")
		res, inst := run(t, []string{"md0", "sdb", "sdc"}, blockEvents, sys, Options{},
			marker(start, "bbbench-start"),
			// md0 is bio-based: only its members see requests.
			blk(10*sec, "block_rq_insert", "8,16", "W", 2048, 8),
			blk(10*sec+usec, "block_rq_insert", "8,32", "W", 2048, 8),
			blk(10*sec+2*usec, "block_rq_issue", "8,16", "W", 2048, 8),
			blk(10*sec+3*usec, "block_rq_issue", "8,32", "W", 2048, 8),
			blk(10*sec+8003*usec, "block_rq_complete", "8,32", "W", 2048, 8),
			blk(10*sec+9002*usec, "block_rq_complete", "8,16", "W", 2048, 8),
		)
		if names := []string{res.Summary.Devices[0].Name, res.Summary.Devices[1].Name, res.Summary.Devices[2].Name}; strings.Join(names, ",") != "md0,sdb,sdc" {
			t.Errorf("device order = %v", names)
		}
		md := dev(t, res.Summary, "md0")
		if md.Source != SourceBlock || len(md.Stats) != 0 {
			t.Errorf("md0 = %+v", md)
		}
		wantStat(t, dev(t, res.Summary, "sdb"), KindDevice, DirWrite, 1, 9e6, 9e6, 9e6)
		wantStat(t, dev(t, res.Summary, "sdc"), KindDevice, DirWrite, 1, 8e6, 8e6, 8e6)
		want := "ftrace: md0: no request events seen; sdb: " + blockText + "; sdc: " + blockText + " · lost events: 0"
		if res.Summary.Text != want {
			t.Errorf("text = %q\nwant   %q", res.Summary.Text, want)
		}
		f := readFile(t, filepath.Join(inst, "events/block/block_rq_issue/filter"))
		if f != fmt.Sprintf("dev == %d || dev == %d || dev == %d", 9<<20, 8<<20|16, 8<<20|32) {
			t.Errorf("filter = %q", f)
		}
	})
	t.Run("mixed members", func(t *testing.T) {
		sys := fakeSys(t, map[string]string{"md0": "9:0", "sdb": "8:16", "nvme0n1": "259:0"}, "sdb")
		res, _ := run(t, []string{"md0", "sdb", "nvme0n1", "sdz"}, append(append([]string{}, nvmeEvents...), blockEvents...), sys, Options{},
			marker(start, "bbbench-start"),
			blk(10*sec, "block_rq_issue", "8,16", "R", 0, 8),
			blk(10*sec, "block_rq_issue", "259,0", "R", 0, 8),
			nvmeSetup(10*sec+10*usec, "nvme0n1", 2, 77, "nvme_cmd_read slba=0, len=7"),
			nvmeDone(10*sec+90*usec, "nvme0n1", 2, 77),
			blk(10*sec+100*usec, "block_rq_complete", "259,0", "R", 0, 8),
			blk(10*sec+6000*usec, "block_rq_complete", "8,16", "R", 0, 8),
		)
		if d := dev(t, res.Summary, "sdb"); d.Source != SourceBlock {
			t.Errorf("sdb = %+v", d)
		}
		nv := dev(t, res.Summary, "nvme0n1")
		if nv.Source != SourceNVMe || nv.Pair != "ctrl,qid,cmdid" {
			t.Errorf("nvme0n1 = %+v", nv)
		}
		wantStat(t, nv, KindDevice, DirRead, 1, 80_000, 80_000, 80_000)
		wantStat(t, nv, KindBlockLayer, DirRead, 1, 100_000, 100_000, 100_000)
		wantStat(t, dev(t, res.Summary, "sdb"), KindDevice, DirRead, 1, 6e6, 6e6, 6e6)
		if len(dev(t, res.Summary, "md0").Stats) != 0 {
			t.Error("md0 has stats")
		}
		sdz := dev(t, res.Summary, "sdz")
		if sdz.Source != "" || sdz.Note == "" || sdz.Stats != nil {
			t.Errorf("sdz = %+v", sdz)
		}
		for _, part := range []string{"ftrace: md0: no request events seen; sdb: ",
			"; nvme0n1: nvme_setup_cmd -> nvme_complete_rq, block_rq_insert -> block_rq_complete · pairing: exact (ctrl,qid,cmdid); sdz: not traced (",
			" · lost events: 0"} {
			if !strings.Contains(res.Summary.Text, part) {
				t.Errorf("text %q lacks %q", res.Summary.Text, part)
			}
		}
	})
}

func TestSession_EventsBeforeStartMarkerIgnored(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	res, _ := run(t, []string{"sdb"}, blockEvents, sys, Options{},
		blk(500_000*usec, "block_rq_issue", "8,16", "R", 0, 8),
		blk(600_000*usec, "block_rq_complete", "8,16", "R", 0, 8),
		blk(700_000*usec, "block_rq_complete", "8,16", "R", 99, 8), // its issue was before tracing
		marker(start, "bbbench-start"),
		blk(2*sec, "block_rq_issue", "8,16", "R", 0, 8),
		blk(2*sec+300*usec, "block_rq_complete", "8,16", "R", 0, 8),
	)
	d := dev(t, res.Summary, "sdb")
	if d.Unpaired != 0 {
		t.Errorf("unpaired = %d", d.Unpaired)
	}
	wantStat(t, d, KindDevice, DirRead, 1, 300_000, 300_000, 300_000)
	if len(res.Windows) != 2 || res.Windows[0].EndMs != 2000 {
		t.Errorf("windows = %+v", res.Windows)
	}
}

func TestSession_NVMeWithoutDiskField(t *testing.T) {
	sys := fakeSys(t, map[string]string{"nvme0n1": "259:0"})
	res, _ := run(t, []string{"nvme0n1"}, nvmeEvents, sys, Options{},
		marker(start, "bbbench-start"),
		nvmeSetup(10*sec, "", 1, 3, "nvme_cmd_write slba=0, len=7"),
		nvmeDone(10*sec+40*usec, "", 1, 3),
	)
	d := dev(t, res.Summary, "nvme0n1")
	wantStat(t, d, KindDevice, DirWrite, 1, 40_000, 40_000, 40_000)
	if d.BlockLayer || len(d.Events) != 1 {
		t.Errorf("device = %+v", d)
	}
}

func TestSession_Unsupported(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	// no tracefs
	if _, err := Start([]string{"sdb"}, Options{TracefsRoot: t.TempDir(), SysBlock: sys}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("no tracefs: err = %v", err)
	}
	// no tracepoints
	root, _ := fakeTracefs(t, nil, nil)
	_, err := Start([]string{"sdb"}, Options{TracefsRoot: root, SysBlock: sys, InstanceName: "test"})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "sdb") {
		t.Errorf("no tracepoints: err = %v", err)
	}
	// unknown device
	root, _ = fakeTracefs(t, blockEvents, nil)
	_, err = Start([]string{"sdq"}, Options{TracefsRoot: root, SysBlock: sys, InstanceName: "test"})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "sdq") {
		t.Errorf("unknown device: err = %v", err)
	}
}

func TestSourceLine(t *testing.T) {
	stats := map[Kind]map[Dir]Stats{KindDevice: {DirRead: {Count: 1}}}
	nvme := DeviceSummary{Name: "nvme0n1", Source: SourceNVMe, Exact: true, BlockLayer: true, Pair: "ctrl,qid,cmdid",
		Events: []string{"nvme_setup_cmd -> nvme_complete_rq", "block_rq_insert -> block_rq_complete"}, Stats: stats}
	sdb := DeviceSummary{Name: "sdb", Source: SourceBlock, Exact: true, Pair: "dev,sector",
		Events: []string{"block_rq_issue -> block_rq_complete"}, Stats: stats}
	md := DeviceSummary{Name: "md0", Source: SourceBlock, Exact: true, Pair: "dev,sector",
		Events: []string{"block_rq_issue -> block_rq_complete"}}
	sdz := DeviceSummary{Name: "sdz", Note: "no such device"}
	lossy := sdb
	lossy.Unpaired = 2
	for _, c := range []struct {
		s    Summary
		want string
	}{
		{Summary{Devices: []DeviceSummary{nvme}},
			"ftrace: nvme_setup_cmd -> nvme_complete_rq, block_rq_insert -> block_rq_complete · pairing: exact (ctrl,qid,cmdid) · lost events: 0"},
		{Summary{Overrun: 10, Devices: []DeviceSummary{lossy}},
			"ftrace: block_rq_issue -> block_rq_complete · pairing: exact (dev,sector) · lost events: 12 (ring buffer 10, unpaired 2)"},
		{Summary{Devices: []DeviceSummary{md}},
			"ftrace: block_rq_issue -> block_rq_complete · no request events seen · lost events: 0"},
		{Summary{Overrun: 3, Devices: []DeviceSummary{sdb, md, sdz}},
			"ftrace: sdb: block_rq_issue -> block_rq_complete · pairing: exact (dev,sector); md0: no request events seen; sdz: not traced (no such device) · lost events: 3 (ring buffer 3, unpaired 0)"},
	} {
		if got := SourceLine(&c.s); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
	s := Summary{Overrun: 10, Devices: []DeviceSummary{lossy}}
	s.Fill()
	if s.Lost != 12 || s.Text != SourceLine(&s) {
		t.Errorf("Fill: %+v", s)
	}
}
