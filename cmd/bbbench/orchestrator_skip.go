package main

import (
	"fmt"

	"bbbench/internal/flushtrace"
)

// flushSkipReason returns why a phase must not run on the device, or "" when
// it can. Phases that fsync measure what a device Flush costs; a device whose
// cache mode is "write through" is never sent a Flush (the kernel drops it),
// so fsync never reaches the drive and the test would measure nothing. The
// result is shown in the run and in the web UI. sysBlock is /sys/block unless
// overridden (tests).
func (ec *ExecutionCoordinator) flushSkipReason(device string, phase []FioJob) string {
	if ec.allowWriteThrough || !phaseHasFsync(phase) {
		return ""
	}
	if flushtrace.WriteCache(ec.sysBlock, device) != "write through" {
		return ""
	}
	return fmt.Sprintf("%s is write through (volatile write cache off or absent): "+
		"the kernel sends it no Flush, so fsync never reaches the drive and "+
		"this Flush-latency test would measure nothing. Enable the drive's "+
		"write cache and run again (--allow-write-through runs it anyway)", device)
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
