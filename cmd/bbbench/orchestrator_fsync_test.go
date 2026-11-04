package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	distassets "bbbench/dist"
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
