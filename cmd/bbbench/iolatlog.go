package main

import (
	"fmt"
	"os"

	"bbbench/internal/iotrace"
)

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
