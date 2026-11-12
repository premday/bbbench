package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/iotrace"
	"bbbench/internal/rungroup"
)

func testIOLatResult(offsetMs int) iotrace.Result {
	st := iotrace.Stats{Count: 10, MeanNs: 100_000, MinNs: 1000, MaxNs: 900_000, P50Ns: 90_000, P99Ns: 800_000}
	slow := iotrace.Stats{Count: 2, MeanNs: 3.5e9, MinNs: 1e9, MaxNs: 6e9, P50Ns: 3e9, P99Ns: 6e9}
	r := iotrace.Result{
		Summary: iotrace.Summary{AvgMsec: 1000, OffsetMs: offsetMs, Overrun: 4, Devices: []iotrace.DeviceSummary{
			{Name: "nvme0n1", Source: "nvme", Exact: true, Unpaired: 1, Stats: map[iotrace.Kind]map[iotrace.Dir]iotrace.Stats{
				iotrace.KindDevice: {iotrace.DirRead: st}}},
			{Name: "sdb", Source: "block", Stats: map[iotrace.Kind]map[iotrace.Dir]iotrace.Stats{
				iotrace.KindDevice: {iotrace.DirWrite: slow}}},
		}},
		Windows: []iotrace.Window{
			{EndMs: 3000, Dev: "sdb", Kind: iotrace.KindDevice, Dir: iotrace.DirWrite, Stats: slow},
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
	if sum.Lost != 5 {
		t.Errorf("Lost = %d, want 5", sum.Lost)
	}
	hdd := series["sdb"]
	if len(hdd) != 1 || hdd[0].T != 2 || hdd[0].Mean != 3.5e6 || hdd[0].P50 != 3e6 || hdd[0].P99 != 6e6 || hdd[0].Dir != "w" {
		t.Errorf("sdb points = %+v", hdd)
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
	if p1.IOTrace["nvme0n1"] == nil || len(p1.IOTraceSeries["nvme0n1"]["nvme0n1"]) != 3 || len(p1.IOTraceSeries["nvme0n1"]["sdb"]) != 1 {
		t.Errorf("phase 1: %+v %+v", p1.IOTrace, p1.IOTraceSeries)
	}
	if p2.IOTrace != nil || p2.IOTraceNotes["nvme0n1"] != "no tracefs" {
		t.Errorf("phase 2: %+v %+v", p2.IOTrace, p2.IOTraceNotes)
	}
}

// A tracefs that cannot be used is a note for the phase, never an error:
// stop gives nothing and can be called again.
func TestStartIOTraceNoteOnFailure(t *testing.T) {
	quietLogger(t)
	drive := DriveInfo{Device: blockdev.Device{Name: "sda"}}
	prefix := filepath.Join(t.TempDir(), "sda_phase0_1_log")
	run, note := startIOTraceWith(drive, prefix, 1000, 0, iotrace.Options{TracefsRoot: t.TempDir()})
	if note != "no ftrace latency: tracefs not found (is it mounted, and are you root?)" || run != nil {
		t.Errorf("note = %q, run = %v", note, run)
	}
	run.MarkStart()
	run.Close()
	if s := run.Stop(); s != nil {
		t.Errorf("Stop() = %+v, want nil", s)
	}
	if s := run.Stop(); s != nil {
		t.Errorf("second Stop() = %+v, want nil", s)
	}
	if _, err := os.Stat(iotrace.LogName(prefix)); err == nil {
		t.Error("a log was written for an untraced phase")
	}
}

// An array target is traced with its members in one session, and the log
// next to fio's names all of them.
func TestStartIOTraceWritesLog(t *testing.T) {
	quietLogger(t)
	root := t.TempDir()
	inst := filepath.Join(root, "instances", "test")
	for _, e := range []string{"block/block_rq_issue", "block/block_rq_complete", "block/block_rq_insert"} {
		for _, base := range []string{root, inst} {
			if err := os.MkdirAll(filepath.Join(base, "events", e), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	pipe := strings.Join([]string{
		"bbbench-1 [000] ..... 1.000000000: tracing_mark_write: bbbench-start",
		"fio-1 [001] ..... 2.000000000: block_rq_insert: 8,16 W 4096 () 2048 + 8 none,0,0 [fio]",
		"fio-1 [001] ..... 2.000010000: block_rq_issue: 8,16 W 4096 () 2048 + 8 none,0,0 [fio]",
		"<idle>-0 [001] d.h1. 2.005000000: block_rq_complete: 8,16 W () 2048 + 8 none,0,0 [0]",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(inst, "trace_pipe"), []byte(pipe), 0o644); err != nil {
		t.Fatal(err)
	}
	sys := t.TempDir()
	for name, dev := range map[string]string{"md0": "9:0", "sdb": "8:16", "sdc": "8:32"} {
		if err := os.MkdirAll(filepath.Join(sys, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sys, name, "dev"), []byte(dev+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	drive := DriveInfo{Device: blockdev.Device{Name: "md0"}, Members: []rungroup.Member{
		{Name: "sdb", Role: "data"}, {Name: "sdc", Role: "data"}}}
	prefix := filepath.Join(t.TempDir(), "md0_phase0_1_log")
	run, note := startIOTraceWith(drive, prefix, 1000, 0,
		iotrace.Options{TracefsRoot: root, SysBlock: sys, InstanceName: "test"})
	if note != "" {
		t.Fatalf("note = %q", note)
	}
	sum := run.Stop()
	if sum == nil {
		t.Fatal("Stop() = nil")
	}
	if again := run.Stop(); again != sum {
		t.Errorf("second Stop() = %p, want the same summary %p", again, sum)
	}
	var names []string
	for _, d := range sum.Devices {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "md0,sdb,sdc" {
		t.Errorf("devices = %v", names)
	}
	data, err := os.ReadFile(iotrace.LogName(prefix))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"md0", "sdb", "sdc"} {
		if !strings.Contains(string(data), name) {
			t.Errorf("log lacks %s:\n%s", name, data)
		}
	}
	loaded, series, ok := loadIOLatLog(prefix)
	if !ok || len(loaded.Devices) != 3 || len(series["sdb"]) == 0 {
		t.Errorf("reloaded: ok=%v sum=%+v series=%+v", ok, loaded, series)
	}
}

// The live phase carries the note of an untraced drive and the summary and
// series of a traced one (from its stored log, like a reload).
func TestUpdateLivePhaseCarriesIOTrace(t *testing.T) {
	quietLogger(t)
	dir := t.TempDir()
	phase := []FioJob{{Name: "randread", Section: "[randread]\nrw=randread\n"}}
	nv := DriveInfo{Device: blockdev.Device{Name: "nvme0n1"}}
	sd := DriveInfo{Device: blockdev.Device{Name: "sda"}}
	sdPrefix := filepath.Join(dir, "sda_phase0_1_log")
	if err := writeIOLatLog(sdPrefix, testIOLatResult(0)); err != nil {
		t.Fatal(err)
	}
	stored := testIOLatResult(0).Summary
	ec := &ExecutionCoordinator{
		drives: []DriveInfo{nv, sd},
		workloads: map[string]*FioWorkload{
			"nvme0n1": {Phases: [][]FioJob{phase}},
			"sda":     {Phases: [][]FioJob{phase}},
		},
		results: map[string][]*FioResult{
			"nvme0n1": {{DeviceName: "nvme0n1", Phase: 0, LogPrefix: filepath.Join(dir, "nvme0n1_phase0_1_log"),
				IOTraceNote: "no ftrace latency: no tracefs"}},
			"sda": {{DeviceName: "sda", Phase: 0, LogPrefix: sdPrefix, IOTrace: &stored}},
		},
		liveResult: &BenchmarkResult{ID: "live-iotrace", Drives: []DriveInfo{nv, sd}},
	}
	ec.updateLivePhase(0)
	t.Cleanup(globalResultsStore.Clear)
	p := ec.liveResult.Phases[0]
	if p.IOTraceNotes["nvme0n1"] != "no ftrace latency: no tracefs" || len(p.IOTraceNotes) != 1 {
		t.Errorf("IOTraceNotes = %+v", p.IOTraceNotes)
	}
	if p.IOTrace["sda"] == nil || p.IOTrace["nvme0n1"] != nil {
		t.Errorf("IOTrace = %+v", p.IOTrace)
	}
	if len(p.IOTraceSeries["sda"]["nvme0n1"]) == 0 || len(p.IOTraceSeries["sda"]["sdb"]) == 0 {
		t.Errorf("IOTraceSeries = %+v", p.IOTraceSeries)
	}
}

// The ring buffer of the concurrent sessions shares one budget, within a
// floor (a reader stall of a fraction of a second at high IOPS) and the
// iotrace default.
func TestIOTraceBufferKB(t *testing.T) {
	for _, c := range []struct{ sessions, cpus, want int }{
		{1, 8, 16384},  // 256 MiB / 8 CPUs = 32 MiB, capped at the default
		{1, 16, 16384}, // exactly the budget
		{4, 16, 4096},
		{12, 64, 1024}, // floor
		{0, 0, 16384},  // unknown counts = one session on one CPU
	} {
		if got := ioTraceBufferKB(c.sessions, c.cpus); got != c.want {
			t.Errorf("ioTraceBufferKB(%d, %d) = %d, want %d", c.sessions, c.cpus, got, c.want)
		}
	}
}

// A phase whose drive cannot be traced still runs: the reason is a note of
// the result, not an error.
func TestExecutePhaseIOTraceNoteNotError(t *testing.T) {
	quietLogger(t)
	bin := t.TempDir()
	fio := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --output=*) printf '{\"jobs\":[]}' > \"${a#--output=}\";; esac; done\n"
	if err := os.WriteFile(filepath.Join(bin, "fio"), []byte(fio), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	drive := DriveInfo{Device: blockdev.Device{Name: "sdq", Path: "/dev/sdq"}}
	phase := []FioJob{{Name: "randread", Section: "[randread]\nrw=randread\n"}}
	ec := &ExecutionCoordinator{
		ctx:       context.Background(),
		drives:    []DriveInfo{drive},
		outputDir: t.TempDir(),
		sysBlock:  t.TempDir(), // sdq is unknown: tracing cannot start
		ioTrace:   true,
		workloads: map[string]*FioWorkload{"sdq": {Phases: [][]FioJob{phase}}},
	}
	res := ec.executePhase(drive, phase, 0, nil, nil)
	if res.Error != nil {
		t.Fatalf("phase error: %v", res.Error)
	}
	if !strings.HasPrefix(res.IOTraceNote, "no ftrace latency: ") || res.IOTrace != nil {
		t.Errorf("note = %q, trace = %+v", res.IOTraceNote, res.IOTrace)
	}
}

func TestParseCPUList(t *testing.T) {
	for in, want := range map[string]int{"0": 1, "0-63\n": 64, "0,2-3,8-11": 7, " 0-1 ": 2} {
		if n, ok := parseCPUList(in); !ok || n != want {
			t.Errorf("parseCPUList(%q) = %d, %v, want %d", in, n, ok, want)
		}
	}
	for _, bad := range []string{"", "x", "3-1", "0-", "-1", "0,,1"} {
		if _, ok := parseCPUList(bad); ok {
			t.Errorf("parseCPUList(%q) ok", bad)
		}
	}
	if traceCPUs() < 1 {
		t.Error("traceCPUs() < 1")
	}
}

// A force quit (cleanup) ends the trace sessions of the running phases;
// a finished phase is no longer tracked.
func TestCleanupClosesTraces(t *testing.T) {
	quietLogger(t)
	ec := &ExecutionCoordinator{}
	var closed []string
	var mu sync.Mutex
	add := func(name string) func() {
		return ec.registerTrace(func() { mu.Lock(); closed = append(closed, name); mu.Unlock() })
	}
	add("a")
	done := add("b")
	done()
	ec.cleanup()
	if len(closed) != 1 || closed[0] != "a" {
		t.Errorf("closed = %v, want [a]", closed)
	}
}

// Close on a running I/O trace switches its events off at once; the phase's
// own Stop afterwards still returns.
func TestIOTraceCloseThenStop(t *testing.T) {
	quietLogger(t)
	root := t.TempDir()
	inst := filepath.Join(root, "instances", "test")
	for _, base := range []string{root, inst} {
		for _, e := range []string{"block/block_rq_issue", "block/block_rq_complete"} {
			if err := os.MkdirAll(filepath.Join(base, "events", e), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(inst, "trace_pipe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sys := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sys, "sdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "sdb", "dev"), []byte("8:16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run, note := startIOTraceWith(DriveInfo{Device: blockdev.Device{Name: "sdb"}}, filepath.Join(t.TempDir(), "p_log"), 1000, 0,
		iotrace.Options{TracefsRoot: root, SysBlock: sys, InstanceName: "test"})
	if run == nil {
		t.Fatalf("note = %q", note)
	}
	run.Close()
	for _, f := range []string{"tracing_on", "events/block/block_rq_issue/enable"} {
		if b, _ := os.ReadFile(filepath.Join(inst, f)); strings.TrimSpace(string(b)) != "0" {
			t.Errorf("%s = %q after Close", f, b)
		}
	}
	if run.Stop() == nil {
		t.Error("Stop after Close = nil")
	}
}
