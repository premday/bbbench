package iotrace

import (
	"errors"
	"testing"
	"time"

	kbpf "bbbench/internal/iotrace/ebpf"
)

// fakeProg is a kernel program whose cumulative state the test scripts.
type fakeProg struct {
	specs     []kbpf.DevSpec
	snaps     []kbpf.Snapshot // returned by successive Reads, the last one repeats
	reads     int
	attached  []string
	readErr   error
	closeCall int
}

func (f *fakeProg) SetDevices(s []kbpf.DevSpec) error { f.specs = s; return nil }
func (f *fakeProg) Attached() []string                { return f.attached }
func (f *fakeProg) Close() error                      { f.closeCall++; return nil }
func (f *fakeProg) Read() (kbpf.Snapshot, error) {
	if f.readErr != nil {
		return kbpf.Snapshot{}, f.readErr
	}
	i := min(f.reads, len(f.snaps)-1)
	f.reads++
	return f.snaps[i], nil
}

// lats adds latencies (ns) to a slot.
func lats(s *kbpf.Slot, ns ...int64) *kbpf.Slot {
	if s == nil {
		s = &kbpf.Slot{}
	}
	for _, v := range ns {
		s.Buckets[bucketOf(v)]++
		s.Count++
		s.SumNs += uint64(v)
	}
	return s
}

// wantBuckets is wantStat for a result built from histogram buckets: the mean
// and count are exact, min and max are bucket edges (within 1/64).
func wantBuckets(t *testing.T, d DeviceSummary, k Kind, dir Dir, count int64, mean float64, min, max int64) {
	t.Helper()
	st, ok := d.Stats[k][dir]
	if !ok {
		t.Errorf("%s: no %s/%s stats in %+v", d.Name, k, dir, d.Stats)
		return
	}
	near := func(got, want int64) bool {
		diff := float64(got - want)
		return diff <= float64(want)/64+1 && diff >= -float64(want)/64-1
	}
	if st.Count != count || st.MeanNs != mean || !near(st.MinNs, min) || !near(st.MaxNs, max) {
		t.Errorf("%s %s/%s = %+v, want count %d mean %v min~%d max~%d", d.Name, k, dir, st, count, mean, min, max)
	}
}

func snap(slots map[kbpf.SlotKey]*kbpf.Slot) kbpf.Snapshot {
	return kbpf.Snapshot{Slots: slots}
}

func clone(m map[kbpf.SlotKey]*kbpf.Slot) map[kbpf.SlotKey]*kbpf.Slot {
	out := map[kbpf.SlotKey]*kbpf.Slot{}
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

var allAttached = []string{"block_rq_insert", "block_rq_issue", "block_rq_complete", "block_bio_queue", "block_bio_complete"}

func TestEBPFWindowsAndSummary(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16", "md0": "9:0"})
	bioNode(t, sys, "md0")
	devKey := kbpf.SlotKey{Idx: 0, Kind: kbpf.KindDevice}
	blkKey := kbpf.SlotKey{Idx: 0, Kind: kbpf.KindBlockLayer}
	bioKey := kbpf.SlotKey{Idx: 1, Kind: kbpf.KindBio, Write: true}
	cum := map[kbpf.SlotKey]*kbpf.Slot{}
	var snaps []kbpf.Snapshot
	snaps = append(snaps, snap(clone(cum))) // baseline at start
	// Window 1 (0-1 s) is inside the ramp (offset 1 s); windows 2 and 3 count.
	cum[devKey] = lats(nil, 100_000, 100_000)
	cum[blkKey] = lats(nil, 150_000, 150_000)
	cum[bioKey] = lats(nil, 400_000)
	snaps = append(snaps, snap(clone(cum)))
	lats(cum[devKey], 200_000, 300_000)
	lats(cum[blkKey], 250_000, 350_000)
	snaps = append(snaps, snap(clone(cum)))
	lats(cum[devKey], 400_000) // the last, partial window at Stop
	snaps = append(snaps, kbpf.Snapshot{Slots: clone(cum), Started: 9, Completed: 8, NoStart: 2, Full: 1})

	fp := &fakeProg{snaps: snaps, attached: allAttached}
	s, err := newEBPFSession(fp, []string{"sdb", "md0"}, Options{SysBlock: sys, AvgMsec: 1000, OffsetMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.specs) != 2 || fp.specs[0].Dev != 8<<20|16 || fp.specs[0].Source != kbpf.SourceBlock ||
		fp.specs[1].Dev != 9<<20 || fp.specs[1].Source != kbpf.SourceBio {
		t.Fatalf("devices set: %+v", fp.specs)
	}
	if err := s.sample(1000); err != nil {
		t.Fatal(err)
	}
	if err := s.sample(2000); err != nil {
		t.Fatal(err)
	}
	res, err := s.Stop()
	if err != nil {
		t.Fatal(err)
	}
	sum := res.Summary
	if sum.Mode != ModeEBPF || sum.AvgMsec != 1000 || sum.OffsetMs != 1000 || sum.Overrun != 0 {
		t.Errorf("summary %+v", sum)
	}
	var w1, w3 *Window
	for i := range res.Windows {
		w := &res.Windows[i]
		if w.Dev == "sdb" && w.Kind == KindDevice && w.Dir == DirRead {
			switch w.EndMs {
			case 1000:
				w1 = w
			case 3000:
				w3 = w
			}
		}
	}
	if w1 == nil || w1.Count != 2 || w1.MeanNs != 100_000 {
		t.Errorf("window 1 = %+v", w1)
	}
	if w3 == nil || w3.Count != 1 {
		t.Errorf("partial last window = %+v", w3)
	}
	// Phase = windows after the ramp: 200k, 300k, 400k.
	d := dev(t, sum, "sdb")
	wantBuckets(t, d, KindDevice, DirRead, 3, 300_000, 200_000, 400_000)
	if d.Source != SourceBlock || !d.Exact || !d.BlockLayer || d.BioBased {
		t.Errorf("sdb = %+v", d)
	}
	wantBuckets(t, d, KindBlockLayer, DirRead, 2, 300_000, 250_000, 350_000)
	m := dev(t, sum, "md0")
	if !m.BioBased || m.Source != SourceBio || m.BlockLayer {
		t.Errorf("md0 = %+v", m)
	}
	// The bio sample of window 1 is in the ramp: no phase stats, one window.
	if len(m.Stats[KindBio]) != 0 {
		t.Errorf("md0 bio stats inside the ramp: %+v", m.Stats)
	}
	if sum.EBPF == nil || sum.EBPF.NoStart != 2 || sum.EBPF.Full != 1 || sum.EBPF.Started != 9 {
		t.Errorf("ebpf counters %+v", sum.EBPF)
	}
	if sum.Lost != 3 { // no start + full, shown as unpaired on the first device
		t.Errorf("lost %d, want 3", sum.Lost)
	}
}

func TestEBPFNoDoubleCountAcrossWindows(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"})
	k := kbpf.SlotKey{Idx: 0, Kind: kbpf.KindDevice}
	a := map[kbpf.SlotKey]*kbpf.Slot{k: lats(nil, 1000, 1000, 1000)}
	b := clone(a)
	lats(b[k], 2000)
	fp := &fakeProg{snaps: []kbpf.Snapshot{snap(map[kbpf.SlotKey]*kbpf.Slot{}), snap(a), snap(b), snap(b)}, attached: allAttached}
	s, err := newEBPFSession(fp, []string{"sdb"}, Options{SysBlock: sys, AvgMsec: 1000})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.sample(1000)
	_ = s.sample(2000)
	res, _ := s.Stop()
	var total int64
	for _, w := range res.Windows {
		total += w.Count
	}
	if total != 4 {
		t.Errorf("windows hold %d samples, want 4", total)
	}
	wantBuckets(t, dev(t, res.Summary, "sdb"), KindDevice, DirRead, 4, 1250, 1000, 2000)
}

func TestEBPFBaselineIsNotCounted(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"})
	k := kbpf.SlotKey{Idx: 0, Kind: kbpf.KindDevice}
	before := map[kbpf.SlotKey]*kbpf.Slot{k: lats(nil, 5000, 5000)}
	after := clone(before)
	lats(after[k], 7000)
	fp := &fakeProg{snaps: []kbpf.Snapshot{snap(before), snap(after)}, attached: allAttached}
	s, err := newEBPFSession(fp, []string{"sdb"}, Options{SysBlock: sys, AvgMsec: 1000})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := s.Stop()
	wantBuckets(t, dev(t, res.Summary, "sdb"), KindDevice, DirRead, 1, 7000, 7000, 7000)
}

func TestEBPFMarkStartRestartsTheWindows(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"})
	k := kbpf.SlotKey{Idx: 0, Kind: kbpf.KindDevice}
	early := map[kbpf.SlotKey]*kbpf.Slot{k: lats(nil, 9000)}
	late := clone(early)
	lats(late[k], 1000)
	fp := &fakeProg{snaps: []kbpf.Snapshot{snap(map[kbpf.SlotKey]*kbpf.Slot{}), snap(early), snap(late)}, attached: allAttached}
	s, err := newEBPFSession(fp, []string{"sdb"}, Options{SysBlock: sys, AvgMsec: 1000})
	if err != nil {
		t.Fatal(err)
	}
	s.MarkStart() // re-reads the baseline: the 9000 ns completed before the gate
	res, _ := s.Stop()
	wantBuckets(t, dev(t, res.Summary, "sdb"), KindDevice, DirRead, 1, 1000, 1000, 1000)
}

func TestEBPFReadErrorIsReportedAtStop(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"})
	fp := &fakeProg{snaps: []kbpf.Snapshot{snap(nil)}, attached: allAttached}
	s, err := newEBPFSession(fp, []string{"sdb"}, Options{SysBlock: sys})
	if err != nil {
		t.Fatal(err)
	}
	fp.readErr = errors.New("map gone")
	if _, err := s.Stop(); err == nil {
		t.Error("want the read error")
	}
}

func TestEBPFNoTracedDeviceIsUnsupported(t *testing.T) {
	fp := &fakeProg{snaps: []kbpf.Snapshot{snap(nil)}, attached: allAttached}
	_, err := newEBPFSession(fp, []string{"nope"}, Options{SysBlock: t.TempDir()})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestEBPFWithoutInsertHasNoBlockLayer(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"})
	fp := &fakeProg{snaps: []kbpf.Snapshot{snap(nil)}, attached: []string{"block_rq_issue", "block_rq_complete"}}
	s, err := newEBPFSession(fp, []string{"sdb"}, Options{SysBlock: sys})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := s.Stop()
	if dev(t, res.Summary, "sdb").BlockLayer {
		t.Error("block layer reported without block_rq_insert")
	}
}

func TestEBPFCloseClosesTheProgram(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"})
	fp := &fakeProg{snaps: []kbpf.Snapshot{snap(nil)}, attached: allAttached}
	s, _ := newEBPFSession(fp, []string{"sdb"}, Options{SysBlock: sys})
	_, _ = s.Stop()
	_ = s.Close()
	_ = s.Close()
	if fp.closeCall != 1 {
		t.Errorf("program closed %d times", fp.closeCall)
	}
}

func TestEBPFMultipathHeadTimesItsPaths(t *testing.T) {
	sys := fakeSys(t, map[string]string{"nvme0n1": "259:0", "nvme0c1n1": "259:1", "nvme0c2n1": "259:2"})
	nvmeLink(t, sys, "nvme0n1", "nvme0", "nvme0c1n1", "nvme0c2n1")
	bioNode(t, sys, "nvme0n1") // a native multipath head has no blk-mq queue
	fp := &fakeProg{snaps: []kbpf.Snapshot{snap(nil)}, attached: allAttached}
	s, err := newEBPFSession(fp, []string{"nvme0n1"}, Options{SysBlock: sys})
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.specs) != 2 {
		t.Fatalf("specs %+v, want the two path devices", fp.specs)
	}
	for _, sp := range fp.specs {
		if sp.Idx != 0 || sp.Source != kbpf.SourceBlock || (sp.Dev != 259<<20|1 && sp.Dev != 259<<20|2) {
			t.Errorf("spec %+v", sp)
		}
	}
	res, _ := s.Stop()
	if d := dev(t, res.Summary, "nvme0n1"); d.BioBased || d.Source != SourceBlock || !d.BlockLayer {
		t.Errorf("head = %+v", d)
	}
}

func TestEBPFLoopSamplesOncePerWindow(t *testing.T) {
	sys := fakeSys(t, map[string]string{"sdb": "8:16"})
	k := kbpf.SlotKey{Idx: 0, Kind: kbpf.KindDevice}
	var snaps []kbpf.Snapshot
	cum := map[kbpf.SlotKey]*kbpf.Slot{}
	snaps = append(snaps, snap(clone(cum)))
	for i := 0; i < 30; i++ {
		cum[k] = lats(cum[k], 1000)
		snaps = append(snaps, snap(clone(cum)))
	}
	fp := &fakeProg{snaps: snaps, attached: allAttached}
	s, err := newEBPFSession(fp, []string{"sdb"}, Options{SysBlock: sys, AvgMsec: 20})
	if err != nil {
		t.Fatal(err)
	}
	s.startLoop()
	time.Sleep(150 * time.Millisecond)
	res, _ := s.Stop()
	ends := map[int64]bool{}
	for _, w := range res.Windows {
		ends[w.EndMs] = true
	}
	if len(ends) < 4 {
		t.Errorf("only %d windows in 150 ms of 20 ms windows: %v", len(ends), ends)
	}
	for e := range ends {
		if e%20 != 0 {
			t.Errorf("window end %d is not a multiple of the window", e)
		}
	}
}
