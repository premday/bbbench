package main

import (
	"fmt"
	"regexp"
	"strings"

	"bbbench/internal/flushtrace"
)

// writeThrough reports whether a phase that fsyncs runs on a device whose
// cache mode is "write through": the kernel never sends it a Flush, so fsync
// does not reach the drive and there is no Flush latency to measure (fio's own
// fsync time is still measured). sysBlock is /sys/block unless overridden
// (tests).
func (ec *ExecutionCoordinator) writeThrough(device string, phase []FioJob) bool {
	return phaseHasFsync(phase) && flushtrace.WriteCache(ec.sysBlock, device) == "write through"
}

// flushGate decides what a phase that fsyncs does on a device: with a skip
// text it is not run (--skip-write-through), with a warn text it runs and the
// text is shown as a caveat; both empty means nothing to say. A skip wins.
func (ec *ExecutionCoordinator) flushGate(device string, phase []FioJob) (warn, skip string) {
	if !ec.writeThrough(device, phase) {
		return "", ""
	}
	if ec.skipWriteThrough {
		return "", fmt.Sprintf("%s is write through (volatile write cache off or absent): "+
			"the kernel sends it no Flush, so this Flush-latency test would measure "+
			"nothing; skipped because of --skip-write-through. Enable the drive's "+
			"write cache and run again", device)
	}
	return fmt.Sprintf("%s is write through (volatile write cache off or absent): "+
		"the kernel sends it no Flush, so there is no device Flush latency to trace "+
		"(fio's fsync latency is still measured).\n%s", device, writeCacheHint(device)), ""
}

// noteFlushGate marks the drive on the status page and prints a short marker
// on the open "Phase N/M (...): " line when the gate has something to say.
// The full warning is printed once the phase ends (printPhaseWarnings).
func (ec *ExecutionCoordinator) noteFlushGate(device string, phase []FioJob) {
	warn, skip := ec.flushGate(device, phase)
	switch {
	case skip != "":
		fmt.Printf("[%s skipped: write through] ", device)
		markDrive(device, func(d *DriveStatus) { d.Status, d.Skipped = "skipped", skip })
	case warn != "":
		fmt.Printf("[%s: write through, see warning below] ", device)
		markDrive(device, func(d *DriveStatus) { d.Warning = warn })
	}
}

var nvmeNamespaceRe = regexp.MustCompile(`^(nvme\d+)n\d+`)

// writeCacheHint tells how to check and enable the drive's write cache, and
// where the outcome shows up.
func writeCacheHint(device string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Check: cat /sys/block/%s/queue/write_cache (want \"write back\")", device)
	if m := nvmeNamespaceRe.FindStringSubmatch(device); m != nil {
		ctrl := "/dev/" + m[1]
		fmt.Fprintf(&sb, "; nvme id-ctrl %s | grep -i vwc (0 = no volatile cache: nothing to enable, Flush is a no-op there)", ctrl)
		fmt.Fprintf(&sb, "\nFix: nvme set-feature %s -f 6 -V 1 (capital V; -v is verbose), then nvme get-feature %s -f 6 -H should say Enabled. "+
			"On a kernel that honors the cache state write_cache flips to write back; on a stock kernel write through means the drive reports no volatile cache", ctrl, ctrl)
	} else {
		fmt.Fprintf(&sb, "\nFix: hdparm -W 1 /dev/%s (SATA) or sdparm --set WCE=1 /dev/%s (SAS/SCSI)", device, device)
	}
	sb.WriteString("\nA plain run only reads this setting; bbbench changes a drive's cache setting only in a setting test, after you confirm it, so apply the fix yourself or run a setting test. " +
		"Then run again: this phase counts as done, so --resume will not redo it. " +
		"Where to look: the run output and web UI show this warning; the Flush graph and the " +
		"<fio log>_flush.1.log header (write_cache=) record the cache mode the trace saw")
	return sb.String()
}

// eachPhaseResult calls fn for the result of every drive in the phase.
func (ec *ExecutionCoordinator) eachPhaseResult(phaseIdx int, fn func(*FioResult)) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, drive := range ec.drives {
		for _, r := range ec.results[drive.Device.Name] {
			if r.Phase == phaseIdx {
				fn(r)
			}
		}
	}
}

// skippedInPhase lists the skip reasons of the drives that did not run the phase.
func (ec *ExecutionCoordinator) skippedInPhase(phaseIdx int) []string {
	var out []string
	ec.eachPhaseResult(phaseIdx, func(r *FioResult) {
		if r.Skipped != "" {
			out = append(out, r.Skipped)
		}
	})
	return out
}

// printPhaseWarnings prints the warnings of the drives that ran the phase,
// each with its fix hints, once the phase has ended.
func (ec *ExecutionCoordinator) printPhaseWarnings(phaseIdx int, indent string) {
	ec.eachPhaseResult(phaseIdx, func(r *FioResult) {
		if r.Warning != "" {
			fmt.Printf("%sWARNING: %s\n", indent, strings.ReplaceAll(r.Warning, "\n", "\n"+indent+"  "))
		}
	})
}
