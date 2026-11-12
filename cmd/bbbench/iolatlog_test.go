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
	"bbbench/internal/iotrace"
)

func testIOLatResult(offsetMs int) iotrace.Result {
	st := iotrace.Stats{Count: 10, MeanNs: 100_000, MinNs: 1000, MaxNs: 900_000, P50Ns: 90_000, P99Ns: 800_000}
	r := iotrace.Result{
		Summary: iotrace.Summary{AvgMsec: 1000, OffsetMs: offsetMs, Devices: []iotrace.DeviceSummary{
			{Name: "nvme0n1", Source: "nvme", Exact: true, Stats: map[iotrace.Kind]map[iotrace.Dir]iotrace.Stats{
				iotrace.KindDevice: {iotrace.DirRead: st}}},
		}},
		Windows: []iotrace.Window{
			{EndMs: 1000, Dev: "nvme0n1", Kind: iotrace.KindDevice, Dir: iotrace.DirRead, Stats: st},
			{EndMs: 2000, Dev: "nvme0n1", Kind: iotrace.KindDevice, Dir: iotrace.DirRead, Stats: st},
			{EndMs: 3000, Dev: "nvme0n1", Kind: iotrace.KindDevice, Dir: iotrace.DirRead, Stats: st},
		},
	}
	r.Summary.Fill()
	return r
}

func TestIOLatLog_DropsRamp(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "nvme0n1_phase1_1_log")
	if err := writeIOLatLog(prefix, testIOLatResult(2000)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prefix + "_iolat.1.log"); err != nil {
		t.Fatal(err)
	}
	sum, series, ok := loadIOLatLog(prefix)
	if !ok || sum == nil || sum.Text == "" {
		t.Fatalf("ok=%v sum=%+v", ok, sum)
	}
	pts := series["nvme0n1"]
	if len(pts) != 1 || pts[0].T != 2 || pts[0].Mean != 100 || pts[0].P99 != 800 || pts[0].Kind != "device" || pts[0].Dir != "r" || pts[0].N != 10 {
		t.Errorf("points = %+v", pts)
	}
}

func TestIOLatLog_Missing(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "nothing_log")
	if _, _, ok := loadIOLatLog(prefix); ok {
		t.Error("want ok=false")
	}
	p := &PhaseResult{}
	applyIOLatLog(p, "sda", prefix)
	if p.IOTrace != nil || p.IOTraceSeries != nil {
		t.Errorf("phase must stay untouched: %+v", p)
	}
}

func TestLoadBenchmarkSummary_LoadsIOLat(t *testing.T) {
	old := logger
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() { logger = old })
	dir := t.TempDir()
	fio1 := filepath.Join(dir, "nvme0n1_phase1_1.json")
	fio2 := filepath.Join(dir, "nvme0n1_phase2_1.json")
	if err := writeIOLatLog(filepath.Join(dir, "nvme0n1_phase1_1_log"), testIOLatResult(0)); err != nil {
		t.Fatal(err)
	}
	run := BenchmarkResult{
		ID:        "run-iolat-1",
		Timestamp: time.Now(),
		Drives:    []DriveInfo{{Device: blockdev.Device{Name: "nvme0n1"}}},
		Phases: []PhaseResult{
			{PhaseNumber: 1, PhaseName: "a", FioFiles: map[string]string{"nvme0n1": fio1}},
			{PhaseNumber: 2, PhaseName: "b", FioFiles: map[string]string{"nvme0n1": fio2},
				IOTraceNotes: map[string]string{"nvme0n1": "no tracefs"}},
		},
	}
	data, _ := json.Marshal(run)
	path := filepath.Join(dir, "run_1_summary.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadBenchmarkSummary(path); err != nil {
		t.Fatal(err)
	}
	got, ok := globalResultsStore.GetResult("run-iolat-1")
	if !ok {
		t.Fatal("not loaded")
	}
	p1, p2 := got.Phases[0], got.Phases[1]
	if p1.IOTrace["nvme0n1"] == nil || len(p1.IOTraceSeries["nvme0n1"]["nvme0n1"]) != 3 {
		t.Errorf("phase 1: %+v %+v", p1.IOTrace, p1.IOTraceSeries)
	}
	if p2.IOTrace != nil || p2.IOTraceNotes["nvme0n1"] != "no tracefs" {
		t.Errorf("phase 2: %+v %+v", p2.IOTrace, p2.IOTraceNotes)
	}
}
