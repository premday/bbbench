package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParseFioSingleLog_IOPS verifies that IOPS values from 10 job files are
// correctly aggregated: timestamps in the 30050–30208ms range all map to the
// 1-second bucket sec=30, and their values are summed (not averaged).
func TestParseFioSingleLog_IOPS(t *testing.T) {
	prefix := "testdata/fio_logs/sdg_seq_write_log"
	pts := parseFioSingleLog(prefix, "iops", 1.0, false, 1000)
	if len(pts) == 0 {
		t.Fatal("expected time points, got none")
	}

	// Find the point at t=30 (the first 30-second averaging window).
	var found bool
	for _, p := range pts {
		if p.T != 30 {
			continue
		}
		found = true
		// All entries are write (direction=1); read should be zero.
		if p.R != 0 {
			t.Errorf("IOPS t=30: R = %.2f, want 0", p.R)
		}
		// Sum of 10 files × 39 IOPS each = 390 exactly.
		wantW := 390.0
		if math.Abs(p.W-wantW) > 1.0 {
			t.Errorf("IOPS t=30: W = %.2f, want %.2f (±1)", p.W, wantW)
		}
	}
	if !found {
		t.Error("no time point found at t=30")
	}
}

// TestParseFioSingleLog_BW verifies bandwidth aggregation: KiB/s values from
// 10 job files are summed and converted to MB/s via scale=1/1024.
func TestParseFioSingleLog_BW(t *testing.T) {
	prefix := "testdata/fio_logs/sdg_seq_write_log"
	pts := parseFioSingleLog(prefix, "bw", 1.0/1024.0, false, 1000)
	if len(pts) == 0 {
		t.Fatal("expected time points, got none")
	}

	var found bool
	for _, p := range pts {
		if p.T != 30 {
			continue
		}
		found = true
		if p.R != 0 {
			t.Errorf("BW t=30: R = %.4f, want 0", p.R)
		}
		// Sum of 10 files = 1575 KiB/s; scaled → 1575/1024 ≈ 1.5381 MB/s.
		wantW := 1575.0 / 1024.0
		if math.Abs(p.W-wantW) > 0.01 {
			t.Errorf("BW t=30: W = %.4f MB/s, want %.4f MB/s (±0.01)", p.W, wantW)
		}
	}
	if !found {
		t.Error("no time point found at t=30")
	}
}

// TestParseFioSingleLog_Clat verifies completion-latency aggregation: ns values
// from 10 job files are averaged (not summed) and converted to µs via
// scale=1/1000.
func TestParseFioSingleLog_Clat(t *testing.T) {
	prefix := "testdata/fio_logs/sdg_seq_write_log"
	// The test files are named _clat.*.log; use "clat" as the kind.
	pts := parseFioSingleLog(prefix, "clat", 1.0/1000.0, true, 1000)
	if len(pts) == 0 {
		t.Fatal("expected time points, got none")
	}

	var found bool
	for _, p := range pts {
		if p.T != 30 {
			continue
		}
		found = true
		if p.R != 0 {
			t.Errorf("Clat t=30: R = %.2f, want 0", p.R)
		}
		// Average of 10 files' clat values at sec=30, then scaled ns → µs.
		// Sum from actual log files = 32,023,445,176 ns; avg = 3,202,344,517.6 ns.
		// Scaled: 3,202,344,517.6 / 1000 ≈ 3,202,344.5 µs.
		wantW := 32023445176.0 / 10.0 / 1000.0
		if math.Abs(p.W-wantW) > 1000 {
			t.Errorf("Clat t=30: W = %.2f µs, want %.2f µs (±1000)", p.W, wantW)
		}
	}
	if !found {
		t.Error("no time point found at t=30")
	}
}

// TestParseFioSingleLog_TimestampBucketing verifies that timestamp jitter
// between job files (e.g. 30050ms vs 30208ms) does not produce separate
// time points — all entries for the same 30-second window must merge into
// a single point at t=30.
func TestParseFioSingleLog_TimestampBucketing(t *testing.T) {
	prefix := "testdata/fio_logs/sdg_seq_write_log"
	pts := parseFioSingleLog(prefix, "iops", 1.0, false, 1000)
	if len(pts) == 0 {
		t.Fatal("expected time points, got none")
	}

	// Count how many points fall in the [30, 31) second range.
	count := 0
	for _, p := range pts {
		if p.T >= 30 && p.T < 31 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 point in [30,31)s range, got %d — "+
			"timestamp jitter between job files is not being bucketed correctly", count)
	}
}

// TestParseFioSingleLog_AllPointsPresent verifies that time points are present
// and sorted. Each file has 19 entries (~30s to ~570s in 30s steps). Small
// timestamp jitter (e.g. 540894ms vs 541006ms) may split one window across two
// adjacent 1-second buckets, so the actual count can be slightly above 19.
func TestParseFioSingleLog_AllPointsPresent(t *testing.T) {
	prefix := "testdata/fio_logs/sdg_seq_write_log"
	pts := parseFioSingleLog(prefix, "iops", 1.0, false, 1000)

	// At least 19 distinct time points must be present.
	if len(pts) < 19 {
		t.Errorf("expected at least 19 time points, got %d", len(pts))
	}

	// Verify points are sorted in ascending order.
	for i := 1; i < len(pts); i++ {
		if pts[i].T <= pts[i-1].T {
			t.Errorf("points not sorted: pts[%d].T=%.0f <= pts[%d].T=%.0f",
				i, pts[i].T, i-1, pts[i-1].T)
		}
	}
}

// TestParseFioLogFiles verifies the combined parsing of all three metric types
// through the higher-level parseFioLogFiles function. It uses "clat" files
// which are present in the testdata, but parseFioLogFiles looks for "lat" — so
// only iops and bw should be populated from these testdata files.
func TestParseFioLogFiles_IOPSAndBW(t *testing.T) {
	prefix := "testdata/fio_logs/sdg_seq_write_log"
	ts := parseFioLogFiles(prefix, 1000)
	if ts == nil {
		t.Fatal("parseFioLogFiles returned nil")
	}

	if len(ts.IOPS) == 0 {
		t.Error("expected IOPS time series, got none")
	}
	if len(ts.BW) == 0 {
		t.Error("expected BW time series, got none")
	}
	// Lat uses "lat" suffix; testdata only has "clat" files, so Lat will be empty.
	if len(ts.Lat) != 0 {
		t.Errorf("expected no Lat series (testdata uses 'clat' not 'lat'), got %d points", len(ts.Lat))
	}

	// Spot-check IOPS at t=30.
	var gotIOPS bool
	for _, p := range ts.IOPS {
		if p.T == 30 {
			gotIOPS = true
			if math.Abs(p.W-390) > 1 {
				t.Errorf("IOPS via parseFioLogFiles at t=30: W=%.2f, want 390", p.W)
			}
		}
	}
	if !gotIOPS {
		t.Error("no IOPS point at t=30")
	}
}

// TestParseFioSingleLog_MissingThreadPoint verifies that Incomplete=true is set
// when one thread's log file is missing an entry that the other files have.
// The testdata has 3 files; file 2 has no entry in the t≈2s window:
//
//	file 1: t=1050, t=2060, t=3070  (write=39,41,38)
//	file 2: t=1020,          t=3090  (write=40,37)  ← gap at t≈2s
//	file 3: t=1080, t=2040, t=3050  (write=38,42,40)
//
// Expected:
//
//	t=1s → 3 contributors → Incomplete=false
//	t=2s → 2 contributors → Incomplete=true
//	t=3s → 3 contributors → Incomplete=false
func TestParseFioSingleLog_MissingThreadPoint(t *testing.T) {
	prefix := "testdata/fio_logs/missing_pt"
	pts := parseFioSingleLog(prefix, "iops", 1.0, false, 1000)
	if len(pts) == 0 {
		t.Fatal("expected time points, got none")
	}

	byT := make(map[float64]TimePoint, len(pts))
	for _, p := range pts {
		byT[p.T] = p
	}

	cases := []struct {
		t              float64
		wantW          float64
		wantIncomplete bool
	}{
		{1, 39 + 40 + 38, false}, // all 3 files; sum = 117
		{2, 41 + 42, true},       // only files 1 and 3; sum = 83
		{3, 38 + 37 + 40, false}, // all 3 files; sum = 115
	}
	for _, tc := range cases {
		p, ok := byT[tc.t]
		if !ok {
			t.Errorf("t=%.0f: point not found (got %v)", tc.t, pts)
			continue
		}
		if math.Abs(p.W-tc.wantW) > 0.5 {
			t.Errorf("t=%.0f: W=%.1f, want %.1f", tc.t, p.W, tc.wantW)
		}
		if p.Incomplete != tc.wantIncomplete {
			t.Errorf("t=%.0f: Incomplete=%v, want %v", tc.t, p.Incomplete, tc.wantIncomplete)
		}
	}
}

// TestParseFioSingleLog_IncompleteFlag verifies that TimePoint.Incomplete is set
// correctly.  The testdata has 10 job files.  All 10 start within the [30000,
// 30208] ms window, so the t=30 bucket always has 10 contributors → complete.
// The last-window timestamps straddle the 540/541 second boundary: 9 files land
// in bucket 540000 ms and 1 file lands in bucket 541000 ms, so both of those
// buckets have fewer than 10 contributors → incomplete.
func TestParseFioSingleLog_IncompleteFlag(t *testing.T) {
	prefix := "testdata/fio_logs/sdg_seq_write_log"
	pts := parseFioSingleLog(prefix, "iops", 1.0, false, 1000)
	if len(pts) == 0 {
		t.Fatal("expected time points, got none")
	}

	byT := make(map[float64]TimePoint, len(pts))
	for _, p := range pts {
		byT[p.T] = p
	}

	// t=30 must be complete: all 10 files contribute.
	p30, ok := byT[30]
	if !ok {
		t.Fatal("no point at t=30")
	}
	if p30.Incomplete {
		t.Errorf("t=30: expected Incomplete=false, got true (W=%.0f)", p30.W)
	}

	// The 540/541 split: at least one of these buckets must exist and be
	// incomplete.  (Depending on threshold rounding, the exact buckets vary,
	// but the jitter always produces at least one partial-contributor bucket.)
	incomplete540 := byT[540].Incomplete
	incomplete541 := byT[541].Incomplete
	if !incomplete540 && !incomplete541 {
		t.Errorf("expected at least one of t=540 or t=541 to be Incomplete=true "+
			"(540: %v, 541: %v)", byT[540], byT[541])
	}
}

// TestParseFioSingleLog_CompletePoints verifies that the well-known mid-phase
// points (t=60, 90, …, 510) have Incomplete=false: all 10 files contribute to
// each of these buckets because their timestamps are tightly clustered within a
// single 1-second window.
func TestParseFioSingleLog_CompletePoints(t *testing.T) {
	prefix := "testdata/fio_logs/sdg_seq_write_log"
	pts := parseFioSingleLog(prefix, "iops", 1.0, false, 1000)
	if len(pts) == 0 {
		t.Fatal("expected time points, got none")
	}

	byT := make(map[float64]TimePoint, len(pts))
	for _, p := range pts {
		byT[p.T] = p
	}

	// t=60 through t=510 in 30-second steps should all be complete.
	for sec := float64(60); sec <= 510; sec += 30 {
		p, ok := byT[sec]
		if !ok {
			t.Errorf("t=%.0f: point not found", sec)
			continue
		}
		if p.Incomplete {
			t.Errorf("t=%.0f: expected Incomplete=false, got true (W=%.0f)", sec, p.W)
		}
	}
}

// TestCoalesceMsec verifies the threshold computation: logAvgMsec/10 with a
// minimum of 1000ms.
func TestCoalesceMsec(t *testing.T) {
	cases := []struct {
		logAvgMsec int
		want       int64
	}{
		{0, 1000},     // 0 → default 1000ms; 1000/10=100 < 1000 → 1000
		{1000, 1000},  // 1000ms sampling; 1000/10=100 < 1000 → 1000
		{5000, 1000},  // 5s sampling; 5000/10=500 < 1000 → 1000
		{10000, 1000}, // 10s sampling; 10000/10=1000 = 1000 → 1000
		{30000, 3000}, // 30s sampling; 30000/10=3000 > 1000 → 3000
		{60000, 6000}, // 60s sampling; 60000/10=6000 > 1000 → 6000
	}
	for _, c := range cases {
		ec := &ExecutionCoordinator{logAvgMsec: c.logAvgMsec}
		got := ec.coalesceMsec()
		if got != c.want {
			t.Errorf("coalesceMsec(logAvgMsec=%d) = %d, want %d", c.logAvgMsec, got, c.want)
		}
	}
}

// fio stamps most windows a millisecond early (5000, 9999, 14999, 19999): each
// must stay in its own bucket, not merge into the previous one.
func TestParseFioSingleLog_EarlyStampsKeepTheirWindow(t *testing.T) {
	dir := t.TempDir()
	prefix := dir + "/d_log"
	body := "5000, 157395, 0, 0, 0\n9999, 157377, 0, 0, 0\n14999, 157401, 0, 0, 0\n19999, 157417, 0, 0, 0\n"
	if err := os.WriteFile(prefix+"_iops.1.log", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// 5 s windows: the threshold is max(5000/10, 1000) = 1000 ms.
	pts := parseFioSingleLog(prefix, "iops", 1.0, false, 1000)
	var got []float64
	for _, p := range pts {
		got = append(got, p.T)
	}
	want := []float64{5, 10, 15, 20}
	if len(got) != len(want) {
		t.Fatalf("points at %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("points at %v, want %v", got, want)
		}
	}
}

// A second signal never exits during a run group (the drives' settings are
// being restored); a plain run still force-quits.
func TestSecondSignal_GroupDoesNotExit(t *testing.T) {
	quietLogger(t)
	exits := 0
	exit := func(code int) {
		if code != exitCodeSIGINT {
			t.Errorf("exit code %d", code)
		}
		exits++
	}
	grp := &ExecutionCoordinator{groupID: "1700000000"}
	if forceQuitAllowed(grp) {
		t.Error("force quit allowed in a group")
	}
	grp.onSecondSignal(exit)
	grp.onSecondSignal(exit)
	if exits != 0 {
		t.Errorf("group coordinator exited %d times", exits)
	}
	if !strings.Contains(groupSignalMessage, "recovery file") || !strings.Contains(groupSignalMessage, "restor") {
		t.Errorf("group message %q", groupSignalMessage)
	}
	plain := &ExecutionCoordinator{}
	if !forceQuitAllowed(plain) {
		t.Error("force quit not allowed in a plain run")
	}
	plain.onSecondSignal(exit)
	if exits != 1 {
		t.Errorf("plain coordinator exits = %d, want 1", exits)
	}
}

// interrupted is written under mu by the signal handler and by the context
// watcher of executeRun while the run goroutine reads it (go test -race).
func TestInterruptedFlag_NoRace(t *testing.T) {
	quietLogger(t)
	ec := &ExecutionCoordinator{results: map[string][]*FioResult{}, stateFile: filepath.Join(t.TempDir(), "state.json")}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ec.setInterrupted()
	}()
	stdout := os.Stdout
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = devnull
	displayResults(ec)
	ec.cleanupStateFile()
	os.Stdout = stdout
	devnull.Close()
	wg.Wait()
	if !ec.isInterrupted() {
		t.Error("isInterrupted false after setInterrupted")
	}
}

// After the first signal a plain run force-quits on the next one, but only
// while the run lasts: once it is over (the final "Press Ctrl+C" wait) the
// coordinator no longer answers signals. A group's coordinator leaves further
// signals to runGroup.
func TestWatchSignals_StopsWhenRunEnds(t *testing.T) {
	quietLogger(t)
	for _, group := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		ec := &ExecutionCoordinator{ctx: ctx, cancel: cancel, finished: make(chan struct{})}
		if group {
			ec.groupID = "1700000000"
		}
		var mu sync.Mutex
		exits := 0
		exit := func(int) { mu.Lock(); exits++; mu.Unlock() }
		sigs := make(chan os.Signal, 1)
		returned := make(chan struct{})
		go func() { ec.watchSignals(sigs, exit); close(returned) }()
		sigs <- os.Interrupt
		<-ctx.Done()
		if !ec.isInterrupted() {
			t.Errorf("group=%v: not marked interrupted", group)
		}
		if group {
			<-returned // the group coordinator stops listening after the first signal
		} else {
			sigs <- os.Interrupt
			deadline := time.Now().Add(5 * time.Second)
			for {
				mu.Lock()
				n := exits
				mu.Unlock()
				if n == 1 || time.Now().After(deadline) {
					break
				}
				time.Sleep(time.Millisecond)
			}
			ec.finish()
			<-returned
		}
		select {
		case sigs <- os.Interrupt:
		default:
		}
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		want := 1
		if group {
			want = 0
		}
		if exits != want {
			t.Errorf("group=%v: %d exits, want %d", group, exits, want)
		}
		mu.Unlock()
	}
}

// TestParseFioSingleLog_MixedDirectionsComplete: in a mixed workload each job
// logs one direction only (here 2 readers, 1 writer). Every point has all the
// files that log its direction, so none is incomplete.
func TestParseFioSingleLog_MixedDirectionsComplete(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "mixed")
	logs := map[string]string{
		"1": "1000, 10, 0, 4096, 0\n2000, 11, 0, 4096, 0\n",
		"2": "1000, 12, 0, 4096, 0\n2000, 13, 0, 4096, 0\n",
		"3": "1000, 20, 1, 4096, 0\n2000, 21, 1, 4096, 0\n",
	}
	for n, body := range logs {
		if err := os.WriteFile(prefix+"_iops."+n+".log", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pts := parseFioSingleLog(prefix, "iops", 1.0, false, 1000)
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2", len(pts))
	}
	for _, p := range pts {
		if p.Incomplete {
			t.Errorf("point t=%v flagged incomplete in a mixed workload", p.T)
		}
	}
}
