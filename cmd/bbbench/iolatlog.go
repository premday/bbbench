package main

import (
	"fmt"
	"os"
	"sync"

	"bbbench/internal/iotrace"
)

// startIOTraceWith times every read and write of a phase from kernel trace
// events: one session for the drive, or for an array target together with
// its members (an md or dm node is bio-based and has no request events of
// its own: its members carry the series). opts sets the host paths and the
// ring buffer; AvgMsec and OffsetMs come from avgMsec and rampMs. Tracing never fails a phase: when it cannot
// start, note says why ("no ftrace latency: <reason>", for the phase's
// io_trace_notes) and stop returns nil. Otherwise stop ends the session,
// stores the log next to fio's and returns the summary; it is idempotent.
//
// Not verified on a real kernel yet: the NVMe tracepoint filter on ctrl_id
// and the NVMe native multipath sysfs layout (iotrace.nvmeControllers). If
// either differs, the NVMe source may see no events (the source line then
// says "no request events seen") but the phase still runs.
func startIOTraceWith(drive DriveInfo, logPrefix string, avgMsec, rampMs int, opts iotrace.Options) (stop func() *iotrace.Summary, note string) {
	devices := []string{drive.Device.Name}
	for _, m := range drive.Members {
		devices = append(devices, m.Name)
	}
	opts.AvgMsec, opts.OffsetMs = avgMsec, rampMs
	sess, err := iotrace.Start(devices, opts)
	if err != nil {
		note = "no ftrace latency: " + err.Error()
		if logger != nil {
			logger.Warn("I/O latency tracing unavailable", "device", drive.Device.Name, "err", err)
		}
		return func() *iotrace.Summary { return nil }, note
	}
	var once sync.Once
	var sum *iotrace.Summary
	return func() *iotrace.Summary {
		once.Do(func() {
			res, err := sess.Stop()
			if err != nil && logger != nil {
				logger.Warn("I/O latency tracing cleanup", "device", drive.Device.Name, "err", err)
			}
			if err := writeIOLatLog(logPrefix, res); err != nil && logger != nil {
				logger.Warn("storing I/O latency", "device", drive.Device.Name, "err", err)
			}
			sum = &res.Summary
		})
		return sum
	}, ""
}

// I/O trace ring buffer sizing. The kernel allocates buffer_size_kb for every
// CPU of every session, and a parallel run has one session per drive (its
// members included) in each phase: 16 MiB x 64 CPUs x 12 drives would be
// 12 GiB. The reader drains trace_pipe continuously, so the buffer only has
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

// IOLatPoint is the I/O latency of one traced device over one window, in µs;
// T is the window start in seconds on the trace clock.
type IOLatPoint struct {
	T    float64 `json:"t"`
	Kind string  `json:"kind"`
	Dir  string  `json:"dir"`
	N    int64   `json:"n"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P99  float64 `json:"p99"`
}

// writeIOLatLog stores the I/O latency of a traced phase next to fio's logs.
func writeIOLatLog(logPrefix string, r iotrace.Result) error {
	f, err := os.Create(iotrace.LogName(logPrefix))
	if err != nil {
		return err
	}
	if err := iotrace.WriteLog(f, r); err != nil {
		f.Close()
		return fmt.Errorf("write iolat log: %w", err)
	}
	return f.Close()
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
	avg := sum.AvgMsec
	if avg <= 0 {
		avg = defaultLogAvgMsec
	}
	series := map[string][]IOLatPoint{}
	for _, w := range res.Windows {
		if w.EndMs <= int64(sum.OffsetMs) {
			continue
		}
		series[w.Dev] = append(series[w.Dev], IOLatPoint{
			T: float64(w.EndMs-int64(avg)) / 1000, Kind: string(w.Kind), Dir: string(w.Dir), N: w.Count,
			Mean: w.MeanNs / 1000, P50: float64(w.P50Ns) / 1000, P99: float64(w.P99Ns) / 1000,
		})
	}
	return &sum, series, true
}

// applyIOLatLog fills the phase's I/O latency summary and series for one
// drive from the stored log. IOTraceNotes is never touched.
func applyIOLatLog(phase *PhaseResult, device, logPrefix string) {
	sum, series, ok := loadIOLatLog(logPrefix)
	if !ok {
		return
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
}
