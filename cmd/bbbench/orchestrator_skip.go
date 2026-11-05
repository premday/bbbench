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

// flushWarning is the caveat shown for a Flush-latency phase on a write-through
// device, or "" when there is none. The phase still runs. It is several lines:
// what is wrong, how to check, how to fix, where the result shows up.
func (ec *ExecutionCoordinator) flushWarning(device string, phase []FioJob) string {
	if !ec.writeThrough(device, phase) {
		return ""
	}
	return fmt.Sprintf("%s is write through (volatile write cache off or absent): "+
		"the kernel sends it no Flush, so there is no device Flush latency to trace "+
		"(fio's fsync latency is still measured).\n%s", device, writeCacheHint(device))
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
	sb.WriteString("\nbbbench only reads this setting and never changes the drive's configuration: apply the fix yourself. " +
		"Then run again: this phase counts as done, so --resume will not redo it. " +
		"Where to look: the run output and web UI show this warning; the Flush graph and the " +
		"<fio log>_flush.log header (write_cache=) record the cache mode the trace saw")
	return sb.String()
}

// flushSkipReason returns why the phase is not run on the device with
// --skip-write-through, or "" when it runs (the default).
func (ec *ExecutionCoordinator) flushSkipReason(device string, phase []FioJob) string {
	if !ec.skipWriteThrough || !ec.writeThrough(device, phase) {
		return ""
	}
	return fmt.Sprintf("%s is write through (volatile write cache off or absent): "+
		"the kernel sends it no Flush, so this Flush-latency test would measure "+
		"nothing; skipped because of --skip-write-through. Enable the drive's "+
		"write cache and run again", device)
}

// skippedInPhase lists the skip reasons of the drives that did not run the phase.
func (ec *ExecutionCoordinator) skippedInPhase(phaseIdx int) []string {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	var out []string
	for _, drive := range ec.drives {
		for _, r := range ec.results[drive.Device.Name] {
			if r.Phase == phaseIdx && r.Skipped != "" {
				out = append(out, r.Skipped)
			}
		}
	}
	return out
}

// printPhaseWarnings prints the warnings of the drives that ran the phase,
// each with its fix hints, once the phase has ended.
func (ec *ExecutionCoordinator) printPhaseWarnings(phaseIdx int, indent string) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, drive := range ec.drives {
		for _, r := range ec.results[drive.Device.Name] {
			if r.Phase != phaseIdx || r.Warning == "" {
				continue
			}
			fmt.Printf("%sWARNING: %s\n", indent, strings.ReplaceAll(r.Warning, "\n", "\n"+indent+"  "))
		}
	}
}
