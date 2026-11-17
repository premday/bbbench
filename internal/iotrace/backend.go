package iotrace

import (
	"fmt"
	"log/slog"
	"strings"
)

// Session is one running I/O latency trace of a set of devices, whichever
// collector (Mode) produces it.
type Session interface {
	// MarkStart sets the time zero of the windows (the start gate opened).
	MarkStart()
	// Stop ends the trace and returns the result; Close is still needed.
	Stop() (Result, error)
	// Close releases everything, fast and idempotent.
	Close() error
}

// startEBPF starts the eBPF collector. It is a variable so tests can replace
// it.
var startEBPF = openEBPF

// Start begins tracing the reads and writes of the named block devices (the
// target first, then its members) with the collector Options.Mode asks for:
// ModeFtrace (the zero value) reads every trace event; ModeEBPF aggregates in
// the kernel and fails when it cannot; ModeAuto tries eBPF and falls back to
// ftrace, saying why in Summary.ModeNote.
func Start(devices []string, opts Options) (Session, error) {
	switch opts.Mode {
	case ModeEBPF:
		s, err := startEBPF(devices, opts)
		if err != nil {
			return nil, fmt.Errorf("ebpf I/O trace mode: %w", err)
		}
		return s, nil
	case ModeAuto:
		s, err := startEBPF(devices, opts)
		if err == nil {
			return s, nil
		}
		f, ferr := startFtrace(devices, opts)
		if ferr != nil {
			return nil, ferr
		}
		slog.Warn("ebpf I/O trace not used, falling back to ftrace", "reason", err.Error())
		f.modeNote = shortReason(err.Error())
		return f, nil
	}
	f, err := startFtrace(devices, opts)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// shortReason keeps the first and the last line of a multi-line error (a
// kernel verifier log runs to hundreds of lines, the cause is at its end),
// joined on one line; the whole text is logged by the caller.
func shortReason(s string) string {
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	switch len(lines) {
	case 0:
		return ""
	case 1:
		return lines[0]
	}
	return lines[0] + " ... " + strings.Join(lines[max(1, len(lines)-2):], " ")
}
