package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"bbbench/internal/flushtrace"
)

// dependency is an external program bbbench runs.
type dependency struct {
	bin         string
	versionArgs []string
	required    bool
	purpose     string // what is lost without it
	pkg         string // package name to install
}

var dependencies = []dependency{
	{"fio", []string{"--version"}, true, "runs every benchmark", "fio"},
	{"smartctl", []string{"--version"}, false, "SMART health on the status page and in the disk dump", "smartmontools"},
	{"nvme", []string{"--version"}, false, "NVMe power state, APST, drive write cache and the SMART fallback on the status page, and changing the write cache of NVMe drives in a setting test", "nvme-cli"},
	{"hdparm", []string{"-V"}, false, "the SATA identify report in the disk dump, and changing the write cache of SATA drives in a setting test", "hdparm"},
	{"sdparm", []string{"--version"}, false, "changing the write cache of SAS/SCSI drives in a setting test", "sdparm"},
	{"lspci", []string{"--version"}, false, "the storage controller's name on the status page", "pciutils"},
	{"mdadm", []string{"--version"}, false, "building software RAID arrays in a layout setting test (mdadm backend)", "mdadm"},
	{"lvm", []string{"version"}, false, "building logical volumes in a layout setting test (LVM backend), and listing LVM physical volumes so that they are never offered", "lvm2"},
	{"udevadm", []string{"--version"}, false, "waiting for udev after a layout teardown", "udev (systemd)"},
}

// depReport is the outcome of checking the external programs.
type depReport struct {
	found   []string // "fio fio-3.36", for display
	notes   []string // one line per missing optional program
	missing []string // one line per missing required program
}

// checkDependencies looks every external program up. lookPath and version are
// parameters so tests need no real programs.
func checkDependencies(lookPath func(string) (string, error), version func(bin string, args ...string) string) depReport {
	var r depReport
	for _, d := range dependencies {
		if _, err := lookPath(d.bin); err != nil {
			line := fmt.Sprintf("%s not found (install: %s)", d.bin, d.pkg)
			if d.required {
				r.missing = append(r.missing, fmt.Sprintf("%s: %s", line, d.purpose))
			} else {
				r.notes = append(r.notes, fmt.Sprintf("%s: without it, %s is missing", line, d.purpose))
			}
			continue
		}
		v := version(d.bin, d.versionArgs...)
		if v == "" {
			r.found = append(r.found, d.bin)
		} else {
			r.found = append(r.found, v)
		}
	}
	return r
}

// programVersion is the first line of `bin args`, shortened; "" on failure.
func programVersion(bin string, args ...string) string {
	out, err := exec.Command(bin, args...).Output()
	if err != nil && len(out) == 0 {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	line, _, _ = strings.Cut(line, " [") // smartctl: "smartctl 7.5 2025-04-30 r5714 [x86_64-linux-...]"
	if len(line) > 40 {
		line = line[:40]
	}
	return strings.TrimSpace(line)
}

// printDependencies prints the check at the start of a run and returns an
// error when something required is missing.
func printDependencies(r depReport) error {
	if len(r.found) > 0 {
		fmt.Printf("Dependencies: %s\n", strings.Join(r.found, ", "))
	}
	for _, n := range r.notes {
		fmt.Printf("  warning: %s\n", n)
	}
	if len(r.missing) > 0 {
		return fmt.Errorf("missing required program(s):\n  %s", strings.Join(r.missing, "\n  "))
	}
	return nil
}

// fioEngines lists the I/O engines of an `fio --enghelp` output.
func fioEngines(enghelp string) map[string]bool {
	engines := map[string]bool{}
	for _, line := range strings.Split(enghelp, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, ":") {
			continue // blank or the "Available IO engines:" header
		}
		engines[line] = true
	}
	return engines
}

// enginesUsed is the set of ioengine values the workloads' fio files set, in
// [global] or in a job, with the workload phases that name them.
func enginesUsed(workloads map[string]*FioWorkload) map[string][]string {
	used := map[string][]string{}
	add := func(engine, where string) {
		if engine != "" {
			used[engine] = append(used[engine], where)
		}
	}
	for dev, w := range workloads {
		add(parseFioOptions(w.GlobalSection)["ioengine"], dev)
		for _, job := range w.Jobs {
			add(parseFioOptions(job.Section)["ioengine"], dev+"/"+job.Name)
		}
	}
	return used
}

// checkFioEngines returns an error when a workload uses an I/O engine this fio
// was built without (io_uring needs liburing at build time).
func checkFioEngines(available map[string]bool, workloads map[string]*FioWorkload) error {
	if len(available) == 0 {
		return nil // could not list them; fio itself will complain
	}
	var problems []string
	used := enginesUsed(workloads)
	engines := make([]string, 0, len(used))
	for e := range used {
		engines = append(engines, e)
	}
	sort.Strings(engines)
	for _, e := range engines {
		if !available[e] {
			where := used[e]
			sort.Strings(where)
			if len(where) > 3 {
				where = append(where[:3], "...")
			}
			problems = append(problems, fmt.Sprintf("this fio has no %q I/O engine, used by %s (for io_uring, install an fio built with liburing)", e, strings.Join(where, ", ")))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	return nil
}

// fioEngineList runs `fio --enghelp`; nil when it cannot.
func fioEngineList() map[string]bool {
	out, err := exec.Command("fio", "--enghelp").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	return fioEngines(string(out))
}

// checkEnvironment verifies, once the workloads are known, what they need from
// fio and from the kernel: the I/O engines used (an error if missing) and
// tracefs for the phases that fsync (a note: those phases still run, with fio's
// fsync latency only).
func (ec *ExecutionCoordinator) checkEnvironment() error {
	if err := checkFioEngines(fioEngineList(), ec.workloads); err != nil {
		return err
	}
	ec.warnTracing(flushtrace.TracefsAvailable, os.Stdout)
	return nil
}

// tracingWarned makes each tracefs warning print once per process: a run
// group calls checkEnvironment for every run.
var tracingWarned struct{ flush, io sync.Once }

// warnTracing says once per process that Flush or I/O latency cannot be
// traced (tracefs missing): the phases still run.
func (ec *ExecutionCoordinator) warnTracing(available func() error, w io.Writer) {
	if ec.flushTrace && ec.hasFsyncPhase() {
		if err := available(); err != nil {
			tracingWarned.flush.Do(func() {
				fmt.Fprintf(w, "  warning: Flush latency cannot be traced: %v\n"+
					"           mount it with `mount -t tracefs nodev /sys/kernel/tracing`; the fsync phases still run and report fio's fsync latency only\n", err)
			})
		}
	}
	if ec.ioTrace {
		if err := available(); err != nil {
			tracingWarned.io.Do(func() {
				fmt.Fprintf(w, "  warning: I/O latency cannot be traced: %v\n"+
					"           the phases still run and report fio's latency only\n", err)
			})
		}
	}
}

// hasFsyncPhase reports whether any workload has a phase that fsyncs.
func (ec *ExecutionCoordinator) hasFsyncPhase() bool {
	for _, w := range ec.workloads {
		for _, phase := range w.Phases {
			if phaseHasFsync(phase) {
				return true
			}
		}
	}
	return false
}
