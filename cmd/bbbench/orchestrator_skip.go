package main

import (
	"fmt"

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
// device, or "" when there is none. The phase still runs.
func (ec *ExecutionCoordinator) flushWarning(device string, phase []FioJob) string {
	if !ec.writeThrough(device, phase) {
		return ""
	}
	return fmt.Sprintf("%s is write through (volatile write cache off or absent): "+
		"the kernel sends it no Flush, so there is no device Flush latency to trace "+
		"(fio's fsync latency is still measured). Enable the drive's write cache "+
		"for Flush numbers", device)
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
