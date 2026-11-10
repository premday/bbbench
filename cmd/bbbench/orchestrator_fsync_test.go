package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	distassets "bbbench/dist"
	"bbbench/internal/blockdev"
	"bbbench/internal/config"
	"bbbench/internal/flushtrace"
	"bbbench/internal/templating"
)

func TestFsyncLatency(t *testing.T) {
	// Shape of fio --output-format=json for a job with fsync=1.
	out, err := parseFioJsonOutput([]byte(`{"fio version":"fio-3.36","jobs":[
	  {"jobname":"a","write":{"iops":3917.0,"total_ios":100},
	   "sync":{"total_ios":100,"lat_ns":{"min":1,"max":9,"mean":236100.0,"stddev":1.0,"N":100}}},
	  {"jobname":"b","write":{"iops":1.0,"total_ios":300},
	   "sync":{"total_ios":300,"lat_ns":{"mean":238100.0}}}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	us, n := fsyncLatency(out)
	if n != 400 {
		t.Errorf("count = %d, want 400", n)
	}
	// weighted mean: (236100*100 + 238100*300) / 400 = 237600 ns
	if us < 237.59 || us > 237.61 {
		t.Errorf("mean = %.3f us, want 237.6", us)
	}
}

func TestFsyncLatency_NoFsyncs(t *testing.T) {
	out, err := parseFioJsonOutput([]byte(`{"jobs":[{"jobname":"a","write":{"iops":1}}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if us, n := fsyncLatency(out); us != 0 || n != 0 {
		t.Errorf("want (0,0) for a job without fsync, got (%v,%v)", us, n)
	}
}

func TestFormatFlushSummary(t *testing.T) {
	nvme := &flushtrace.Summary{Source: "nvme", Count: 4012, MeanNs: 194_800, P99Ns: 210_000, MaxNs: 480_000, Exact: true}
	if got, want := formatFlushSummary(nvme), "Flush (nvme): avg 194.8 us, p99 210.0 us, max 480.0 us (4012 flushes)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	blk := &flushtrace.Summary{Source: "block", Count: 10, MeanNs: 250_000}
	if got, want := formatFlushSummary(blk), "Flush (block): avg 250.0 us (10 flushes)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := formatFlushSummary(&flushtrace.Summary{Source: "nvme"}); !strings.Contains(got, "none seen") {
		t.Errorf("got %q", got)
	}
}

// The default fsync workloads must be traced; the other SSD ones must not.
func TestPhaseHasFsync_DefaultWorkloads(t *testing.T) {
	cfg, err := config.LoadFS(distassets.FS, "default.yml")
	if err != nil {
		t.Fatal(err)
	}
	eng, err := templating.DefaultEngine()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"ssd_fsync_psync_qd1": true, "ssd_fsync_uring_qd1": true, "ssd_fsync_uring_qd32": true,
		"ssd_randwrite": false, "ssd_seqread": false,
	} {
		ctx := map[string]any{
			"Manufacturer": "V", "WorkloadsDB": cfg.BBBench.WorkloadsDB, "Workloads": []string{name},
			"Disk":   map[string]any{"path": "/dev/sdz", "model": "M", "serial": "S", "capacity": 1, "vendor": "V"},
			"Device": "sdz", "Serial": "S", "Host": "h", "WCE": nil,
		}
		out, err := eng.Render("disk.fio", ctx)
		if err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(t.TempDir(), "x.fio")
		if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		w, err := parseFioFile(f)
		if err != nil || len(w.Phases) == 0 {
			t.Fatalf("%s: parse: %v (phases=%d)", name, err, len(w.Phases))
		}
		if got := phaseHasFsync(w.Phases[0]); got != want {
			t.Errorf("%s: phaseHasFsync = %v, want %v", name, got, want)
		}
	}
}

func TestFormatFlushSummary_WriteThroughExplainsNoFlushes(t *testing.T) {
	got := formatFlushSummary(&flushtrace.Summary{Source: "nvme", WriteCache: "write through"})
	if !strings.Contains(got, "none seen") || !strings.Contains(got, "write-through") {
		t.Errorf("got %q", got)
	}
}

// A phase that fsyncs runs on a write-through device with a warning (the
// kernel sends it no Flush); with --skip-write-through it is refused.
func TestFlushGate(t *testing.T) {
	sys := t.TempDir()
	set := func(dev, mode string) {
		t.Helper()
		d := filepath.Join(sys, dev, "queue")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "write_cache"), []byte(mode+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	set("nvme0n1", "write through")
	set("nvme1n1", "write back")
	fsync := []FioJob{{Name: "j", Section: "[j]\nrw=randwrite\nfsync=1\n"}}
	plain := []FioJob{{Name: "j", Section: "[j]\nrw=randwrite\n"}}
	ec := &ExecutionCoordinator{sysBlock: sys}

	// By default a write-through device runs the phase, with a warning.
	warn, skip := ec.flushGate("nvme0n1", fsync)
	if skip != "" || !strings.Contains(warn, "write through") {
		t.Errorf("default gate: warn=%q skip=%q, want a warning only", warn, skip)
	}
	for _, want := range []string{"write_cache", "nvme set-feature /dev/nvme0 -f 6 -V 1", "get-feature /dev/nvme0", "vwc", "_flush.1.log", "A plain run only reads this setting"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warning lacks %q:\n%s", want, warn)
		}
	}
	if h := writeCacheHint("sdb"); !strings.Contains(h, "hdparm -W 1 /dev/sdb") || strings.Contains(h, "nvme") {
		t.Errorf("sata hint = %q", h)
	}
	if w, k := ec.flushGate("nvme1n1", fsync); w != "" || k != "" {
		t.Errorf("write-back device: warn=%q skip=%q", w, k)
	}
	if w, k := ec.flushGate("nvme0n1", plain); w != "" || k != "" {
		t.Errorf("non-fsync phase on write-through: warn=%q skip=%q", w, k)
	}
	if w, k := ec.flushGate("sdz", fsync); w != "" || k != "" {
		t.Errorf("unknown cache mode: warn=%q skip=%q", w, k)
	}
	// --skip-write-through turns the warning into a skip (and only that).
	ec.skipWriteThrough = true
	warn, skip = ec.flushGate("nvme0n1", fsync)
	if warn != "" || !strings.Contains(skip, "--skip-write-through") {
		t.Errorf("with the flag: warn=%q skip=%q", warn, skip)
	}
	if w, k := ec.flushGate("nvme1n1", fsync); w != "" || k != "" {
		t.Errorf("write-back device with the flag: warn=%q skip=%q", w, k)
	}
}

// With --skip-write-through a skipped phase is reported (not an error, not completed, fio not started)
// and reaches the graph data with its reason.
func TestSkippedPhase_ResultAndGraphData(t *testing.T) {
	sys := t.TempDir()
	os.MkdirAll(filepath.Join(sys, "nvme0n1", "queue"), 0o755)
	os.WriteFile(filepath.Join(sys, "nvme0n1", "queue", "write_cache"), []byte("write through\n"), 0o644)
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	drive := DriveInfo{Device: blockdev.Device{Name: "nvme0n1", Path: "/dev/nvme0n1"}}
	phase := []FioJob{{Name: "ssd_fsync", Section: "[ssd_fsync]\nfsync=1\n"}}
	ec := &ExecutionCoordinator{
		drives:           []DriveInfo{drive},
		sysBlock:         sys,
		skipWriteThrough: true,
		workloads:        map[string]*FioWorkload{"nvme0n1": {Phases: [][]FioJob{phase}}},
		results:          map[string][]*FioResult{},
		liveResult:       &BenchmarkResult{ID: "x", Drives: []DriveInfo{drive}},
	}
	res := ec.executePhase(drive, phase, 0, nil, nil)
	if res.Error != nil || res.Skipped == "" || res.OutputPath != "" {
		t.Fatalf("result = %+v", res)
	}
	ec.results["nvme0n1"] = []*FioResult{res}
	ec.updateLivePhase(0)

	data := generateDiskPhaseData(ec.liveResult)
	if len(data) != 1 || data[0].Skipped == "" || data[0].Disk != "nvme0n1" || data[0].PhaseName != "ssd_fsync" {
		t.Fatalf("graph data = %+v", data)
	}
	if got := ec.skippedInPhase(0); len(got) != 1 {
		t.Errorf("skippedInPhase = %v", got)
	}
}

// The orchestrator's --log-avg-msec is the only polling interval: the
// template's own log_avg_msec must not reach the phase file, where it would
// override the command line.
func TestBuildPhaseFioFile_DropsTemplateLogAvgMsec(t *testing.T) {
	w := &FioWorkload{GlobalSection: "[global]\nioengine=libaio\nlog_avg_msec=30000\nramp_time=10\n"}
	out := buildPhaseFioFile(w, []FioJob{{Name: "j", Section: "[j]\nrw=randwrite\n"}})
	if strings.Contains(out, "log_avg_msec") {
		t.Errorf("log_avg_msec kept:\n%s", out)
	}
	for _, keep := range []string{"ioengine=libaio", "ramp_time=10", "[j]", "rw=randwrite"} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q:\n%s", keep, out)
		}
	}
}
