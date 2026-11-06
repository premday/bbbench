package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/flushtrace"
)

func testFlushSamples() []flushtrace.Sample {
	return []flushtrace.Sample{
		{TimeNs: 100e6, LatencyNs: 100_000},
		{TimeNs: 900e6, LatencyNs: 300_000},
		{TimeNs: 1_500e6, LatencyNs: 1_000_000},
	}
}

func TestFlushLog_RoundTrip(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "nvme0n1_phase1_1_log")
	all := testFlushSamples()
	sum := flushtrace.Finish(flushtrace.Summary{Source: "nvme", Exact: true}, all)
	if err := writeFlushLog(prefix, sum, all); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prefix + "_flush.1.log"); err != nil {
		t.Fatalf("log must sit next to the fio logs as <prefix>_flush.1.log: %v", err)
	}
	got, series, ok := loadFlushLog(prefix)
	if !ok || *got != sum {
		t.Fatalf("loaded %+v ok=%v, want %+v", got, ok, sum)
	}
	// 1s windows, stamped at their start: [0,1s) mean 200us at t=0, [1s,2s)
	// mean 1000us at t=1 (values in µs).
	if len(series) != 2 || series[0].T != 0 || series[0].W != 200 || series[1].T != 1 || series[1].W != 1000 {
		t.Errorf("series = %+v", series)
	}
}

func TestFlushLog_MissingIsNotAnError(t *testing.T) {
	if _, _, ok := loadFlushLog(filepath.Join(t.TempDir(), "nothing_log")); ok {
		t.Error("no log must report ok=false")
	}
	p := &PhaseResult{}
	applyFlushLog(p, "sda", filepath.Join(t.TempDir(), "nothing_log"))
	if p.FlushTrace != nil || p.FlushSeries != nil {
		t.Errorf("phase must stay untouched: %+v", p)
	}
}

// A finished run is reloaded from its stored samples: the summary saved with
// the run is replaced by one recomputed from the log, and the series appears
// even though the saved summary never had one.
func TestLoadBenchmarkSummary_RebuildsFlushFromStoredSamples(t *testing.T) {
	old := logger
	logger = slog.New(slog.NewTextHandler(io.Discard, nil)) // the loader logs through the package logger
	t.Cleanup(func() { logger = old })
	dir := t.TempDir()
	fioPath := filepath.Join(dir, "nvme0n1_phase1_1.json")
	all := testFlushSamples()
	if err := writeFlushLog(filepath.Join(dir, "nvme0n1_phase1_1_log"), flushtrace.Finish(flushtrace.Summary{Source: "nvme", Exact: true}, all), all); err != nil {
		t.Fatal(err)
	}
	stale := &flushtrace.Summary{Source: "nvme", Count: 1, MeanNs: 42, Exact: true}
	run := BenchmarkResult{
		ID:        "run-flush-1",
		Timestamp: time.Now(),
		Drives:    []DriveInfo{{Device: blockdev.Device{Name: "nvme0n1"}}},
		Phases: []PhaseResult{{
			PhaseNumber: 1,
			PhaseName:   "ssd_fsync_uring_qd1",
			Jobs:        []JobResult{{Device: "nvme0n1", WriteStats: &IOStats{IOPS: 1000}}},
			FioFiles:    map[string]string{"nvme0n1": fioPath},
			FlushTrace:  map[string]*flushtrace.Summary{"nvme0n1": stale},
		}},
	}
	data, _ := json.Marshal(run)
	summaryPath := filepath.Join(dir, "run_1_summary.json")
	if err := os.WriteFile(summaryPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadBenchmarkSummary(summaryPath); err != nil {
		t.Fatal(err)
	}
	got, ok := globalResultsStore.GetResult("run-flush-1")
	if !ok {
		t.Fatal("result not loaded")
	}
	ph := got.Phases[0]
	if s := ph.FlushTrace["nvme0n1"]; s == nil || s.Count != 3 || s.MeanNs == 42 {
		t.Errorf("summary must be recomputed from the stored samples, got %+v", s)
	}
	if len(ph.FlushSeries["nvme0n1"]) != 2 {
		t.Errorf("series = %+v", ph.FlushSeries)
	}
	// And it reaches the graph data the UI consumes.
	var dp *DiskPhaseData
	for _, d := range generateDiskPhaseData(got) {
		if d.Disk == "nvme0n1" {
			d := d
			dp = &d
		}
	}
	if dp == nil || len(dp.TSflush) != 2 || dp.Flush == nil || dp.Flush.Count != 3 {
		t.Fatalf("graph data lacks flush: %+v", dp)
	}
}

// The Flush series uses the fio polling interval (--log-avg-msec) the run was
// made with, stored in the log, so it lines up with fio's own series.
func TestFlushLog_SeriesFollowsFioPollingInterval(t *testing.T) {
	all := []flushtrace.Sample{
		{TimeNs: 1_000e6, LatencyNs: 100_000},
		{TimeNs: 29_000e6, LatencyNs: 300_000},
		{TimeNs: 31_000e6, LatencyNs: 500_000},
	}
	write := func(avg int) string {
		prefix := filepath.Join(t.TempDir(), "d_phase1_1_log")
		sum := flushtrace.Finish(flushtrace.Summary{Source: "nvme", Exact: true, AvgMsec: avg}, all)
		if err := writeFlushLog(prefix, sum, all); err != nil {
			t.Fatal(err)
		}
		return prefix
	}

	// log_avg_msec=30000: two points, at the start of their windows (0 s, 30 s).
	got, series, _ := loadFlushLog(write(30000))
	if got.AvgMsec != 30000 || len(series) != 2 || series[0].T != 0 || series[0].W != 200 || series[1].T != 30 || series[1].W != 500 {
		t.Errorf("30 s: summary %+v series %+v", got, series)
	}
	// log_avg_msec=1000: one point per second that has Flushes.
	if _, series, _ := loadFlushLog(write(1000)); len(series) != 3 {
		t.Errorf("1 s: series %+v", series)
	}
	// Logs from before the interval was stored fall back to 1 s windows.
	if _, series, _ := loadFlushLog(write(0)); len(series) != 3 {
		t.Errorf("unknown interval: series %+v", series)
	}
}

// fio's statistics and logs start when its ramp_time ends: Flushes completed
// during the ramp are left out of the summary, and the series has no points
// for it (the graph shades the ramp-up). The series still counts from the
// start of tracing, like the ramp itself.
func TestFlushLog_StartsAfterFioRamp(t *testing.T) {
	all := []flushtrace.Sample{
		{TimeNs: 2_000e6, LatencyNs: 900_000},  // during the 10 s ramp
		{TimeNs: 10_500e6, LatencyNs: 100_000}, // 0.5 s after the ramp
		{TimeNs: 11_500e6, LatencyNs: 300_000}, // 1.5 s after the ramp
	}
	prefix := filepath.Join(t.TempDir(), "d_phase1_1_log")
	sum := flushtrace.Finish(flushtrace.Summary{Source: "nvme", Exact: true, OffsetMs: 10_000}, all)
	if err := writeFlushLog(prefix, sum, all); err != nil {
		t.Fatal(err)
	}
	got, series, ok := loadFlushLog(prefix)
	if !ok || got.OffsetMs != 10_000 || got.Count != 2 || got.MaxNs != 300_000 {
		t.Fatalf("summary %+v ok=%v, want 2 flushes after the ramp", got, ok)
	}
	if len(series) != 2 || series[0].T != 10 || series[0].W != 100 || series[1].T != 11 || series[1].W != 300 {
		t.Errorf("series = %+v", series)
	}
}

func TestPhaseRampMs(t *testing.T) {
	w := &FioWorkload{GlobalSection: "[global]\nramp_time=10\n"}
	if got := phaseRampMs(w, []FioJob{{Section: "[a]\nrw=write\n"}}); got != 10_000 {
		t.Errorf("global ramp = %d", got)
	}
	if got := phaseRampMs(w, []FioJob{{Section: "[a]\nramp_time=0\n"}}); got != 0 {
		t.Errorf("job override = %d", got)
	}
	if got := phaseRampMs(&FioWorkload{}, []FioJob{{Section: "[a]\n"}}); got != 0 {
		t.Errorf("no ramp = %d", got)
	}
}
