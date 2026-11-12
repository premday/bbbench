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
	"bbbench/internal/iotrace"
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

// The flush gate reads the composite's own queue/write_cache: no new branch.
func TestFlushGate_ArrayOwnWriteCache(t *testing.T) {
	sys := t.TempDir()
	q := filepath.Join(sys, "md127", "queue")
	if err := os.MkdirAll(q, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q, "write_cache"), []byte("write through\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ec := &ExecutionCoordinator{sysBlock: sys}
	fsync := []FioJob{{Name: "j", Section: "[j]\nrw=randwrite\nfsync=1\n"}}
	warn, skip := ec.flushGate("md127", fsync)
	if skip != "" || !strings.HasPrefix(warn, "md127 is write through") {
		t.Errorf("warn=%q skip=%q", warn, skip)
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

func TestFormatIOTraceSummary(t *testing.T) {
	rw := map[iotrace.Kind]map[iotrace.Dir]iotrace.Stats{
		iotrace.KindDevice: {
			iotrace.DirRead:  {Count: 10, MeanNs: 85_000, P99Ns: 120_000},
			iotrace.DirWrite: {Count: 5, MeanNs: 30_100, P99Ns: 41_000},
		},
		iotrace.KindBlockLayer: {iotrace.DirRead: {Count: 10, MeanNs: 99_000, P99Ns: 150_000}},
	}
	s := &iotrace.Summary{Devices: []iotrace.DeviceSummary{
		{Name: "md0", Source: "block", Stats: rw},
		{Name: "sdb", Source: "block", Stats: map[iotrace.Kind]map[iotrace.Dir]iotrace.Stats{
			iotrace.KindDevice: {iotrace.DirRead: {Count: 1, MeanNs: 9e6, P99Ns: 9e6}}}},
	}}
	want := "ftrace device (block): read avg 85.0 us p99 120.0 us, write avg 30.1 us p99 41.0 us"
	if got := formatIOTraceSummary(s); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	s.Lost = 12
	if got := formatIOTraceSummary(s); got != want+" (12 events lost)" {
		t.Errorf("with loss: %q", got)
	}
	readOnly := &iotrace.Summary{Devices: []iotrace.DeviceSummary{{Name: "nvme0n1", Source: "nvme", Stats: map[iotrace.Kind]map[iotrace.Dir]iotrace.Stats{
		iotrace.KindDevice: {iotrace.DirRead: {Count: 3, MeanNs: 4.5e6, P99Ns: 9e6}}}}}}
	if got := formatIOTraceSummary(readOnly); got != "ftrace device (nvme): read avg 4500.0 us p99 9000.0 us" {
		t.Errorf("read only: %q", got)
	}
	// An md node is bio-based: no request events of its own; its members
	// were traced, and their device latency is summed up (mean over all
	// their requests).
	sdc := map[iotrace.Kind]map[iotrace.Dir]iotrace.Stats{
		iotrace.KindDevice: {iotrace.DirRead: {Count: 30, MeanNs: 125_000, P99Ns: 1e6}}}
	md := &iotrace.Summary{Lost: 2, Devices: []iotrace.DeviceSummary{{Name: "md0", Source: "block"},
		{Name: "sdb", Source: "block", Stats: rw}, {Name: "nvme1n1", Source: "nvme", Stats: sdc}, {Name: "sdz", Note: "unknown device"}}}
	if got, want := formatIOTraceSummary(md), "ftrace (block, nvme): target bio-based, 2 members traced, members' device read avg 115.0 us, write avg 30.1 us (2 events lost)"; got != want {
		t.Errorf("md:\ngot  %q\nwant %q", got, want)
	}
	// With the bio tracepoints the node has its own bio-level latency,
	// given first, then the members'.
	bio := &iotrace.Summary{Devices: []iotrace.DeviceSummary{{Name: "md0", Source: "bio", BioBased: true, Events: []string{"block_bio_queue -> block_bio_complete"},
		Stats: map[iotrace.Kind]map[iotrace.Dir]iotrace.Stats{iotrace.KindBio: {
			iotrace.DirRead: {Count: 4, MeanNs: 200_000, P99Ns: 400_000}, iotrace.DirWrite: {Count: 4, MeanNs: 9.5e6, P99Ns: 1.2e7}}}},
		{Name: "sdb", Source: "block", Stats: rw}}}
	if got, want := formatIOTraceSummary(bio), "ftrace bio (md0, block_bio_queue -> block_bio_complete): read avg 200.0 us p99 400.0 us, write avg 9500.0 us p99 12000.0 us; 1 member traced (block), members' device read avg 85.0 us, write avg 30.1 us"; got != want {
		t.Errorf("md bio:\ngot  %q\nwant %q", got, want)
	}
	quiet := &iotrace.Summary{Devices: []iotrace.DeviceSummary{{Name: "md0", Source: "block"}, {Name: "sdb", Source: "block"}}}
	if got, want := formatIOTraceSummary(quiet), "ftrace (block): target bio-based, 1 member traced, no request events seen"; got != want {
		t.Errorf("md, no member events: %q", got)
	}
	if got := formatIOTraceSummary(&iotrace.Summary{Devices: []iotrace.DeviceSummary{{Name: "sda", Source: "block"}}}); got != "ftrace (block): no request events seen" {
		t.Errorf("no events: %q", got)
	}
	if got := formatIOTraceSummary(&iotrace.Summary{}); got != "ftrace: no request events seen" {
		t.Errorf("no devices: %q", got)
	}
}
