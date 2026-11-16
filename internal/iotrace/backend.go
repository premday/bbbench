package iotrace

import (
	"errors"
	"fmt"
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
// it; the real one is set where the collector is built.
var startEBPF = func([]string, Options) (Session, error) {
	return nil, errors.New("ebpf: the collector is not built into this binary")
}

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
		f.modeNote = err.Error()
		return f, nil
	}
	f, err := startFtrace(devices, opts)
	if err != nil {
		return nil, err
	}
	return f, nil
}
