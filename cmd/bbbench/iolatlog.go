package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"bbbench/internal/flushtrace"
	"bbbench/internal/iotrace"
)

// ioTraceRun is the I/O latency trace of one phase on one drive. Its methods
// are no-ops on a nil run (tracing off or not started).
type ioTraceRun struct {
	sess      *iotrace.Session
	device    string
	logPrefix string
	once      sync.Once
	sum       *iotrace.Summary
}

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
	return &ioTraceRun{sess: sess, device: drive.Device.Name, logPrefix: logPrefix}, ""
}

// MarkStart moves the trace's time zero to now (the start gate opened).
func (r *ioTraceRun) MarkStart() {
	if r != nil {
		r.sess.MarkStart()
	}
}

// Stop ends the session, stores the log next to fio's and returns the
// summary (nil for a nil run). Idempotent.
func (r *ioTraceRun) Stop() *iotrace.Summary {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		res, err := r.sess.Stop()
		if err != nil && logger != nil {
			logger.Warn("I/O latency tracing cleanup", "device", r.device, "err", err)
		}
		if err := writeIOLatLog(r.logPrefix, res); err != nil && logger != nil {
			logger.Warn("storing I/O latency", "device", r.device, "err", err)
		}
		r.sum = &res.Summary
	})
	return r.sum
}

// Close ends the session quickly without storing anything (force quit).
func (r *ioTraceRun) Close() {
	if r != nil {
		_ = r.sess.Close()
	}
}

// Trace ring buffer sizing. The kernel allocates buffer_size_kb for every
// CPU of every session, and a parallel run has one I/O session per drive (its
// members included) in each phase, plus one Flush session per drive in a
// phase that fsyncs: 16 MiB x 64 CPUs x 12 drives would be 12 GiB. Both kinds
// count as one session each against the budget. The reader drains trace_pipe continuously, so the buffer only has
// to absorb a reader stall; the concurrent sessions share ioTraceBudgetKB,
// each CPU getting at least ioTraceMinBufferKB (about 13k events, a stall of
// a fraction of a second at a few hundred thousand IOPS per CPU: an overrun
// beyond it is counted and shown, never hidden) and at most the iotrace
// default.
const (
	ioTraceBudgetKB    = 256 * 1024
	ioTraceMinBufferKB = 1024
	ioTraceMaxBufferKB = 16384
)

// ioTraceBufferKB is the per-CPU buffer of one of sessions concurrent
// sessions on cpus CPUs.
func ioTraceBufferKB(sessions, cpus int) int {
	sessions, cpus = max(sessions, 1), max(cpus, 1)
	return min(max(ioTraceBudgetKB/(sessions*cpus), ioTraceMinBufferKB), ioTraceMaxBufferKB)
}

// flushTraceBufferKB is the per-CPU buffer of a Flush session: the same
// share of the budget, at most the Flush default (it sees only Flushes).
func flushTraceBufferKB(sessions, cpus int) int {
	return min(ioTraceBufferKB(sessions, cpus), flushtrace.DefaultBufferKB)
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
