package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/disktemp"
)

func fakeSensor() *disktemp.Sensor {
	var n atomic.Int64
	return disktemp.NewSensor("hwmon", "Composite", 82.85, 84.85, func() (float64, error) {
		return 40 + float64(n.Add(1)), nil
	})
}

// A phase's temperature is sampled while it runs, shown live on the status
// page, stored next to the fio logs and read back as a series and a summary.
func TestStartTempSampling(t *testing.T) {
	ResetStatus()
	defer ResetStatus()
	UpdateStatus(func(s *BenchmarkStatus) { s.Drives = []DriveStatus{{Name: "nvme0n1"}} })
	prefix := filepath.Join(t.TempDir(), "nvme0n1_phase0_1_log")
	ec := &ExecutionCoordinator{
		tempInterval: 5 * time.Millisecond,
		findTemp:     func(string) (*disktemp.Sensor, error) { return fakeSensor(), nil },
	}
	result := &FioResult{}
	stop := ec.startTempSampling("nvme0n1", prefix, result)
	time.Sleep(40 * time.Millisecond)
	if live := GetStatus().Drives[0].TempC; live == nil || *live <= 40 {
		t.Errorf("live temperature = %v", live)
	}
	stop()

	if result.Temp == nil || result.Temp.Count < 3 || result.Temp.Source != "hwmon" || result.Temp.WarnC != 82.85 {
		t.Fatalf("summary = %+v", result.Temp)
	}
	if _, err := os.Stat(prefix + "_temp.1.log"); err != nil {
		t.Fatalf("log must sit next to the fio logs: %v", err)
	}
	p := &PhaseResult{}
	applyTempLog(p, "nvme0n1", prefix)
	if got := p.Temp["nvme0n1"]; got == nil || got.Count != result.Temp.Count || got.MaxC != result.Temp.MaxC {
		t.Errorf("reloaded summary = %+v, want %+v", got, result.Temp)
	}
	s := p.TempSeries["nvme0n1"]
	if len(s) != result.Temp.Count || s[0].T > 0.01 || s[0].W != 41 || s[len(s)-1].W <= s[0].W {
		t.Errorf("series = %+v", s)
	}
}

// No sensor, or sampling switched off, costs nothing and never fails a phase.
func TestStartTempSampling_Optional(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "x_log")
	result := &FioResult{}
	off := &ExecutionCoordinator{tempInterval: 0, findTemp: func(string) (*disktemp.Sensor, error) {
		t.Error("sensor looked up although sampling is off")
		return nil, nil
	}}
	off.startTempSampling("sda", prefix, result)()
	missing := &ExecutionCoordinator{tempInterval: time.Millisecond, findTemp: func(string) (*disktemp.Sensor, error) {
		return nil, errors.New("no sensor")
	}}
	missing.startTempSampling("sda", prefix, result)()
	if result.Temp != nil {
		t.Errorf("summary without sampling: %+v", result.Temp)
	}
	if _, err := os.Stat(prefix + "_temp.1.log"); err == nil {
		t.Error("log written without samples")
	}
}

func TestFormatTempSummary(t *testing.T) {
	got := formatTempSummary(&disktemp.Summary{Count: 3, AvgC: 41.234, MaxC: 45, WarnC: 82.85})
	if got != "Temp: avg 41.2 C, max 45.0 C" {
		t.Errorf("%q", got)
	}
	if got := formatTempSummary(&disktemp.Summary{Count: 3, AvgC: 80, MaxC: 83, WarnC: 82.85}); !strings.Contains(got, "warning limit") {
		t.Errorf("reaching the limit must be said: %q", got)
	}
	if got := formatTempSummary(&disktemp.Summary{}); got != "Temp: not read" {
		t.Errorf("%q", got)
	}
}

// A finished run is reloaded from its stored temperature log, and the series
// reaches the graph data the UI draws.
func TestLoadBenchmarkSummary_RebuildsTemperatureFromStoredLog(t *testing.T) {
	old := logger
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() { logger = old })
	dir := t.TempDir()
	fioPath := filepath.Join(dir, "nvme0n1_phase1_1.json")
	sensor := disktemp.NewSensor("hwmon", "Composite", 82.85, 84.85, nil)
	all := []disktemp.Sample{{TimeMs: 0, MilliC: 41000}, {TimeMs: 5000, MilliC: 43000}, {TimeMs: 10000, MilliC: 46000}}
	if err := writeTempLog(filepath.Join(dir, "nvme0n1_phase1_1_log"), disktemp.Summarize(sensor, 5000, all), all); err != nil {
		t.Fatal(err)
	}
	run := BenchmarkResult{
		ID:        "run-temp-1",
		Timestamp: time.Now(),
		Drives:    []DriveInfo{{Device: blockdev.Device{Name: "nvme0n1"}}},
		Phases: []PhaseResult{{
			PhaseNumber: 1, PhaseName: "ssd_randwrite",
			Jobs:     []JobResult{{Device: "nvme0n1", WriteStats: &IOStats{IOPS: 1000}}},
			FioFiles: map[string]string{"nvme0n1": fioPath},
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
	got, ok := globalResultsStore.GetResult("run-temp-1")
	if !ok {
		t.Fatal("result not loaded")
	}
	if s := got.Phases[0].Temp["nvme0n1"]; s == nil || s.MaxC != 46 || s.MinC != 41 || s.WarnC != 82.85 || s.Count != 3 {
		t.Errorf("summary = %+v", s)
	}
	var dp *DiskPhaseData
	for _, d := range generateDiskPhaseData(got) {
		if d.Disk == "nvme0n1" {
			d := d
			dp = &d
		}
	}
	if dp == nil || len(dp.TStemp) != 3 || dp.TStemp[1].T != 5 || dp.TStemp[1].W != 43 || dp.Temp == nil || dp.Temp.Sensor != "Composite" {
		t.Fatalf("graph data lacks temperature: %+v", dp)
	}
	b, _ := json.Marshal(dp)
	if !strings.Contains(string(b), `"ts_temp":[{"t":0,"r":0,"w":41}`) || !strings.Contains(string(b), `"temp":{"source":"hwmon"`) {
		t.Errorf("json = %s", b)
	}
}
