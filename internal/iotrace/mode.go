package iotrace

import "fmt"

// Mode is how the I/O latency is collected.
type Mode string

const (
	// ModeAuto picks eBPF when it can be loaded, else ftrace. It is only a
	// request: a Summary never carries it.
	ModeAuto Mode = "auto"
	// ModeEBPF aggregates in the kernel (histograms read per window).
	ModeEBPF Mode = "ebpf"
	// ModeFtrace reads every trace event (the tracefs ring buffers).
	ModeFtrace Mode = "ftrace"
)

// ParseMode reads --io-trace; "" is auto.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeEBPF:
		return ModeEBPF, nil
	case ModeFtrace:
		return ModeFtrace, nil
	}
	return "", fmt.Errorf("invalid I/O trace mode %q: must be auto, ebpf or ftrace", s)
}
