package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"bbbench/internal/flushtrace"
	"bbbench/internal/iotrace"
)

// ioTraceRun is the I/O latency trace of one phase on one drive.
type ioTraceRun = traceRun[iotrace.Summary]

// startIOTraceWith times every read and write of a phase from kernel trace
// events: one session for the drive, or for an array target together with
// its members (an md or dm node is bio-based and has no request events of
// its own: iotrace times its bios instead, kind "bio", and its members carry
// the device and block-layer series). opts sets the host paths and the
// ring buffer; AvgMsec and OffsetMs come from avgMsec and rampMs. Tracing
// never fails a phase: when it cannot start, the run is nil and note says
// why ("no ftrace latency: <reason>", for the phase's io_trace_notes).
//
// Not verified on a real kernel yet: the NVMe tracepoint filter on ctrl_id
// and the NVMe native multipath sysfs layout (iotrace.nvmeControllers). If
// either differs, the NVMe source may see more events than needed or none
// (the source line then says "no request events seen"); the phase still runs.
func startIOTraceWith(drive DriveInfo, logPrefix string, avgMsec, rampMs int, opts iotrace.Options) (run *ioTraceRun, note string) {
	devices := []string{drive.Device.Name}
	for _, m := range drive.Members {
		devices = append(devices, m.Name)
	}
	opts.AvgMsec, opts.OffsetMs = avgMsec, rampMs
	sess, err := iotrace.Start(devices, opts)
	if err != nil {
		if logger != nil {
			logger.Warn("I/O latency tracing unavailable", "device", drive.Device.Name, "err", err)
		}
		// "no ftrace latency" already says it is unsupported: the reason only.
		reason := strings.TrimPrefix(err.Error(), iotrace.ErrUnsupported.Error()+": ")
		return nil, "no ftrace latency: " + reason
	}
	device := drive.Device.Name
	stop := func() *iotrace.Summary {
		res, err := sess.Stop()
		if err != nil && logger != nil {
			logger.Warn("I/O latency tracing cleanup", "device", device, "err", err)
		}
		if err := writeIOLatLog(logPrefix, res); err != nil && logger != nil {
			logger.Warn("storing I/O latency", "device", device, "err", err)
		}
		return &res.Summary
	}
	return &ioTraceRun{markStart: sess.MarkStart, stop: stop, close: sess.Close}, ""
}

// Trace ring buffer sizing. The kernel allocates buffer_size_kb for every
// CPU of every session, and a parallel run has one I/O session per drive (its
// members included) in each phase, plus one Flush session per drive in a
// phase that fsyncs: 64 MiB x 64 CPUs x 12 drives would be 48 GiB. Both kinds
// count as one session each against the budget, --trace-buffer-mb (default
// defaultTraceBufferMB). The readers drain the buffers continuously (one per
// CPU on the raw path), so a buffer only has to absorb a reader stall or a
// burst; each CPU gets budget / (sessions x CPUs), at least
// ioTraceMinBufferKB (about 13k events, a stall of a fraction of a second at
// a few hundred thousand IOPS per CPU: an overrun beyond it is counted and
// shown, never hidden) and at most ioTraceMaxBufferKB. The kernel memory is
// sessions x CPUs x the per-CPU size: at most the budget, except at the
// floor, where it is sessions x CPUs MiB.
const (
	defaultTraceBufferMB = 256
	maxTraceBufferMB     = 1 << 20 // 1 TiB: anything larger is a typo
	ioTraceMinBufferKB   = 1024
	ioTraceMaxBufferKB   = 64 * 1024
)

// traceLossHint is what to do about ring-buffer overruns, for the terminal
// and the graphs page.
const traceLossHint = "raise --trace-buffer-mb or lower the IOPS"

// checkTraceBufferMB validates --trace-buffer-mb.
func checkTraceBufferMB(mb int) error {
	if mb < 1 || mb > maxTraceBufferMB {
		return fmt.Errorf("invalid --trace-buffer-mb %d: must be from 1 to %d (MiB shared by the concurrent trace sessions)", mb, maxTraceBufferMB)
	}
	return nil
}

// ioTraceBufferKB is the per-CPU buffer of one of sessions concurrent
// sessions on cpus CPUs sharing budgetMB (0 = the default).
func ioTraceBufferKB(budgetMB, sessions, cpus int) int {
	if budgetMB <= 0 {
		budgetMB = defaultTraceBufferMB
	}
	sessions, cpus = max(sessions, 1), max(cpus, 1)
	return min(max(budgetMB*1024/(sessions*cpus), ioTraceMinBufferKB), ioTraceMaxBufferKB)
}

// flushTraceBufferKB is the per-CPU buffer of a Flush session: the same
// share of the budget, at most the Flush default (it sees only Flushes).
func flushTraceBufferKB(budgetMB, sessions, cpus int) int {
	return min(ioTraceBufferKB(budgetMB, sessions, cpus), flushtrace.DefaultBufferKB)
}

// traceBufferLine says how big the trace buffers of a run are: the budget,
// the per-CPU size at the run's most concurrent phase and the kernel memory
// it takes.
func traceBufferLine(budgetMB, sessions, cpus int) string {
	if budgetMB <= 0 {
		budgetMB = defaultTraceBufferMB
	}
	sessions, cpus = max(sessions, 1), max(cpus, 1)
	per := ioTraceBufferKB(budgetMB, sessions, cpus)
	plural := "s"
	if sessions == 1 {
		plural = ""
	}
	return fmt.Sprintf("ftrace buffers: %d MiB budget (--trace-buffer-mb), %s per CPU for up to %d concurrent session%s on %d CPUs: %s of kernel memory",
		budgetMB, fmtKB(per), sessions, plural, cpus, fmtKB(per*sessions*cpus))
}

// fmtKB renders a size in KiB as MiB or GiB.
func fmtKB(kb int) string {
	if kb >= 1024*1024 && kb%(1024*1024) == 0 {
		return strconv.Itoa(kb/(1024*1024)) + " GiB"
	}
	if kb%1024 == 0 {
		return strconv.Itoa(kb/1024) + " MiB"
	}
	return strconv.Itoa(kb) + " KiB"
}

// IOLatPoint is the I/O latency of one traced device over one window, in µs;
// T is the window start in seconds on the trace clock. Kind is "device",
// "blocklayer" or "bio" (the bios of a bio-based md/dm node).
type IOLatPoint struct {
	T    float64 `json:"t"`
	Kind string  `json:"kind"`
	Dir  string  `json:"dir"`
	N    int64   `json:"n"`
	Mean float64 `json:"mean"`
	P99  float64 `json:"p99"`
}

// writeIOLatLog stores the I/O latency of a traced phase next to fio's logs.
func writeIOLatLog(logPrefix string, r iotrace.Result) error {
	return writeLogFile(iotrace.LogName(logPrefix), func(w io.Writer) error {
		if err := iotrace.WriteLog(w, r); err != nil {
			return fmt.Errorf("write iolat log: %w", err)
		}
		return nil
	})
}

// loadIOLatLog reads the stored I/O latency log of a phase run: the summary
// and, per device, the series. Windows that ended inside the ramp-up are left
// out, like fio's own series; points are stamped at the window start.
func loadIOLatLog(logPrefix string) (*iotrace.Summary, map[string][]IOLatPoint, bool) {
	f, err := os.Open(iotrace.LogName(logPrefix))
	if err != nil {
		return nil, nil, false
	}
	defer f.Close()
	res, err := iotrace.ReadLog(f)
	if err != nil {
		if logger != nil {
			logger.Warn("unreadable iolat log", "prefix", logPrefix, "err", err)
		}
		return nil, nil, false
	}
	sum := res.Summary
	series := map[string][]IOLatPoint{}
	for _, w := range res.Windows {
		t, keep := windowStartSec(w.EndMs, sum.AvgMsec, sum.OffsetMs)
		if !keep {
			continue
		}
		series[w.Dev] = append(series[w.Dev], IOLatPoint{
			T: t, Kind: string(w.Kind), Dir: string(w.Dir), N: w.Count,
			Mean: w.MeanNs / 1000, P99: float64(w.P99Ns) / 1000,
		})
	}
	return &sum, series, true
}

// applyIOLatLog fills the phase's I/O latency summary and series for one
// drive from the stored log and reports whether there was one.
// IOTraceNotes is never touched.
func applyIOLatLog(phase *PhaseResult, device, logPrefix string) bool {
	sum, series, ok := loadIOLatLog(logPrefix)
	if !ok {
		return false
	}
	if phase.IOTrace == nil {
		phase.IOTrace = make(map[string]*iotrace.Summary)
	}
	phase.IOTrace[device] = sum
	if len(series) > 0 {
		if phase.IOTraceSeries == nil {
			phase.IOTraceSeries = make(map[string]map[string][]IOLatPoint)
		}
		phase.IOTraceSeries[device] = series
	}
	return true
}

// possibleCPUsPath lists the CPUs the kernel allocates a ring buffer for
// (every possible CPU, not only the online ones or this process's affinity).
const possibleCPUsPath = "/sys/devices/system/cpu/possible"

// traceCPUs is the number of per-CPU ring buffers of one trace instance:
// the possible CPUs, else runtime.NumCPU.
func traceCPUs() int {
	if b, err := os.ReadFile(possibleCPUsPath); err == nil {
		if n, ok := parseCPUList(string(b)); ok {
			return n
		}
	}
	return runtime.NumCPU()
}

// parseCPUList counts the CPUs of a kernel CPU list ("0-63", "0,2-3,8").
func parseCPUList(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n := 0
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return 0, false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return 0, false
			}
		}
		n += b - a + 1
	}
	return n, true
}
