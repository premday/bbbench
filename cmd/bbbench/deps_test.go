package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func lookIn(present ...string) func(string) (string, error) {
	return func(bin string) (string, error) {
		for _, p := range present {
			if p == bin {
				return "/usr/bin/" + bin, nil
			}
		}
		return "", errors.New("not found")
	}
}

func fakeVersion(bin string, _ ...string) string { return bin + "-1.0" }

func TestCheckDependencies_AllPresent(t *testing.T) {
	r := checkDependencies(lookIn("fio", "smartctl", "nvme", "hdparm", "sdparm", "lspci", "mdadm", "lvm", "udevadm"), fakeVersion)
	if len(r.missing) != 0 || len(r.notes) != 0 || len(r.found) != 9 || r.found[0] != "fio-1.0" {
		t.Errorf("%+v", r)
	}
	if err := printDependencies(r); err != nil {
		t.Error(err)
	}
}

// A missing optional program is a note naming what is lost and the package to
// install; a missing fio stops the run.
func TestCheckDependencies_MissingOptionalAndRequired(t *testing.T) {
	r := checkDependencies(lookIn("fio", "nvme"), fakeVersion)
	if len(r.missing) != 0 || len(r.notes) != 7 {
		t.Fatalf("%+v", r)
	}
	all := strings.Join(r.notes, "\n")
	for _, want := range []string{"smartctl not found (install: smartmontools)", "SMART health", "hdparm not found (install: hdparm)", "sdparm not found (install: sdparm)", "lspci not found (install: pciutils)", "mdadm not found (install: mdadm)", "lvm not found (install: lvm2)", "udevadm not found (install: udev (systemd))"} {
		if !strings.Contains(all, want) {
			t.Errorf("notes lack %q:\n%s", want, all)
		}
	}
	if err := printDependencies(r); err != nil {
		t.Errorf("optional programs must not stop the run: %v", err)
	}

	r = checkDependencies(lookIn("smartctl"), fakeVersion)
	err := printDependencies(r)
	if err == nil || !strings.Contains(err.Error(), "fio not found (install: fio)") {
		t.Errorf("missing fio must be an error with the package: %v", err)
	}
}

const enghelpSample = `Available IO engines:
	cpuio
	sync
	psync
	libaio
	io_uring
`

func TestFioEngines(t *testing.T) {
	e := fioEngines(enghelpSample)
	if len(e) != 5 || !e["libaio"] || !e["io_uring"] || e["Available IO engines:"] {
		t.Errorf("%v", e)
	}
}

func TestCheckFioEngines(t *testing.T) {
	w := map[string]*FioWorkload{
		"nvme0n1": {
			GlobalSection: "[global]\nioengine=libaio\n",
			Jobs: []FioJob{
				{Name: "a", Section: "[a]\nrw=write\n"},
				{Name: "b", Section: "[b]\nioengine=io_uring\n"},
			},
		},
	}
	if got := enginesUsed(w); len(got) != 2 || got["io_uring"][0] != "nvme0n1/b" {
		t.Errorf("used = %v", got)
	}
	if err := checkFioEngines(fioEngines(enghelpSample), w); err != nil {
		t.Errorf("all engines present: %v", err)
	}
	err := checkFioEngines(fioEngines("Available IO engines:\n\tsync\n\tlibaio\n"), w)
	if err == nil || !strings.Contains(err.Error(), `no "io_uring" I/O engine`) || !strings.Contains(err.Error(), "liburing") || !strings.Contains(err.Error(), "nvme0n1/b") {
		t.Errorf("missing io_uring: %v", err)
	}
	// If the engine list could not be read, fio itself reports it later.
	if err := checkFioEngines(nil, w); err != nil {
		t.Error(err)
	}
}

// The tracefs warnings print once per process, not once per run of a group.
func TestWarnTracingOncePerProcess(t *testing.T) {
	tracingWarned = struct{ flush, io sync.Once }{}
	t.Cleanup(func() { tracingWarned = struct{ flush, io sync.Once }{} })
	fsync := []FioJob{{Name: "j", Section: "[j]\nfsync=1\n"}}
	ec := &ExecutionCoordinator{flushTrace: true, ioTrace: true,
		workloads: map[string]*FioWorkload{"sda": {Phases: [][]FioJob{fsync}}}}
	missing := func() error { return errors.New("tracefs not found") }
	var out strings.Builder
	ec.warnTracing(missing, &out)
	ec.warnTracing(missing, &out)
	got := out.String()
	if strings.Count(got, "warning: I/O latency cannot be traced: tracefs not found") != 1 ||
		strings.Count(got, "warning: Flush latency cannot be traced") != 1 ||
		!strings.Contains(got, "           the phases still run and report fio's latency only\n") {
		t.Errorf("output:\n%s", got)
	}
	// Flush tracing off: the I/O warning still comes.
	tracingWarned = struct{ flush, io sync.Once }{}
	out.Reset()
	(&ExecutionCoordinator{ioTrace: true}).warnTracing(missing, &out)
	if !strings.Contains(out.String(), "I/O latency cannot be traced") || strings.Contains(out.String(), "Flush") {
		t.Errorf("output:\n%s", out.String())
	}
}
