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
	sum := flushtrace.Summarize("nvme", true, 0, all)
	if err := writeFlushLog(prefix, sum, all); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prefix + "_flush.log"); err != nil {
		t.Fatalf("log must sit next to the fio logs as <prefix>_flush.log: %v", err)
	}
	got, series, ok := loadFlushLog(prefix)
	if !ok || *got != sum {
		t.Fatalf("loaded %+v ok=%v, want %+v", got, ok, sum)
	}
	// 1s windows: [0,1s) mean 200us, [1s,2s) mean 1000us (values in µs).
	if len(series) != 2 || series[0].W != 200 || series[1].T != 1 || series[1].W != 1000 {
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
	if err := writeFlushLog(filepath.Join(dir, "nvme0n1_phase1_1_log"), flushtrace.Summarize("nvme", true, 0, all), all); err != nil {
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
