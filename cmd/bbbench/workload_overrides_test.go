package main

import (
	distassets "bbbench/dist"
	"strings"
	"testing"

	"bbbench/internal/config"
	"bbbench/internal/templating"
)

// renderWorkloads renders disk.fio and output.yml for the given workloads
// defined in an inline config.
func renderWorkloads(t *testing.T, yml string, names ...string) (fio, plot string) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	eng, err := templating.DefaultEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	ctx := map[string]any{
		"Manufacturer": "V", "WorkloadsDB": cfg.BBBench.WorkloadsDB, "Workloads": names,
		"Disk":   map[string]any{"path": "/dev/nvme0n1", "model": "M", "serial": "S", "capacity": 1, "vendor": "V"},
		"Device": "nvme0n1", "Serial": "S", "Host": "h", "WCE": nil,
	}
	fio, err = eng.Render("disk.fio", ctx)
	if err != nil {
		t.Fatalf("render disk.fio: %v", err)
	}
	plot, err = eng.Render("output.yml", ctx)
	if err != nil {
		t.Fatalf("render output.yml: %v", err)
	}
	return fio, plot
}

const overridesCfg = `
bbbench:
  workloadsdb:
    fsync_buffered:
      blocksize: 4k
      duration: 30
      write_log: true
      ioengine: psync
      iodepth: 1
      direct: false
      fsync: 1
      process: {randwrite: 1}
    plain:
      blocksize: 4k
      duration: 30
      write_log: true
      process: {randwrite: 1}
  workloads:
    ssd: [fsync_buffered, plain]
`

func TestRender_WorkloadOverrides(t *testing.T) {
	fio, plot := renderWorkloads(t, overridesCfg, "fsync_buffered")
	for _, want := range []string{
		"ioengine=psync", "iodepth=1", "direct=0", "fsync=1", "rw=randwrite",
		"seconds_iodepth1_buffered_4k_fsync_buffered_",
	} {
		if !strings.Contains(fio, want) {
			t.Errorf("disk.fio missing %q:\n%s", want, fio)
		}
	}
	if !strings.Contains(plot, "iodepth: 1\n") || !strings.Contains(plot, "seconds_iodepth1_buffered_") {
		t.Errorf("output.yml does not follow the overrides:\n%s", plot)
	}
}

func TestRender_WorkloadWithoutOverridesIsUnchanged(t *testing.T) {
	fio, plot := renderWorkloads(t, overridesCfg, "plain")
	job := fio[strings.Index(fio, "[001-"):] // skip the [global] section
	for _, banned := range []string{"ioengine=", "iodepth=", "direct=", "fsync="} {
		if strings.Contains(job, banned) {
			t.Errorf("plain workload must not emit %q:\n%s", banned, job)
		}
	}
	if !strings.Contains(fio, "seconds_iodepth128_direct_4k_plain_") || !strings.Contains(plot, "iodepth: 128\n") {
		t.Errorf("plain workload must keep iodepth128/direct naming:\n%s\n%s", fio, plot)
	}
}

func TestDefaultConfig_FlushLatencyWorkloads(t *testing.T) {
	cfg, err := config.LoadFS(distassets.FS, "default.yml")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	want := map[string][]string{
		"ssd_fsync_psync_qd1":  {"ioengine=psync", "iodepth=1", "direct=0", "fsync=1", "rw=randwrite", "bs=4k", "runtime=30"},
		"ssd_fsync_uring_qd1":  {"ioengine=io_uring", "iodepth=1", "direct=1", "fsync=1", "rw=randwrite", "bs=4k", "runtime=30"},
		"ssd_fsync_uring_qd32": {"ioengine=io_uring", "iodepth=32", "direct=1", "fsync=1", "rw=randwrite", "bs=4k", "runtime=30"},
	}
	eng, err := templating.DefaultEngine()
	if err != nil {
		t.Fatal(err)
	}
	inSSD := map[string]bool{}
	for _, n := range cfg.BBBench.Workloads["ssd"] {
		inSSD[n] = true
	}
	for name, lines := range want {
		if !inSSD[name] {
			t.Errorf("%s is not in the default ssd workload list", name)
		}
		ctx := map[string]any{
			"Manufacturer": "V", "WorkloadsDB": cfg.BBBench.WorkloadsDB, "Workloads": []string{name},
			"Disk":   map[string]any{"path": "/dev/nvme0n1", "model": "M", "serial": "S", "capacity": 1, "vendor": "V"},
			"Device": "nvme0n1", "Serial": "S", "Host": "h", "WCE": nil,
		}
		out, err := eng.Render("disk.fio", ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		job := out[strings.Index(out, "[001-"):]
		for _, l := range lines {
			if !strings.Contains(job, l) {
				t.Errorf("%s: job missing %q:\n%s", name, l, job)
			}
		}
		if strings.Contains(job, "write_bw_log") {
			t.Errorf("%s: short flush workloads must not write interval logs", name)
		}
	}
}
