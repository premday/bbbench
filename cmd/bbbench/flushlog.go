package main

import (
	"fmt"
	"os"

	"bbbench/internal/flushtrace"
)

// writeFlushLog stores the Flush latency of a traced phase next to fio's own
// logs, in fio's shape: one line per polling window plus the phase summary.
func writeFlushLog(logPrefix string, sum flushtrace.Summary, samples []flushtrace.Sample) error {
	f, err := os.Create(flushtrace.LogName(logPrefix))
	if err != nil {
		return err
	}
	if err := flushtrace.WriteLog(f, sum, samples); err != nil {
		f.Close()
		return fmt.Errorf("write flush log: %w", err)
	}
	return f.Close()
}

// loadFlushLog reads the stored Flush log of the phase run identified by its
// fio log prefix: the phase summary from its header and the series, the mean
// latency per polling window in µs (in the W column, like fio write latency).
// ok is false when there is no usable log, e.g. runs from before the log
// existed or in the older per-Flush format.
func loadFlushLog(logPrefix string) (sum *flushtrace.Summary, series []TimePoint, ok bool) {
	f, err := os.Open(flushtrace.LogName(logPrefix))
	if err != nil {
		return nil, nil, false
	}
	defer f.Close()
	s, windows, err := flushtrace.ReadLog(f)
	if err != nil {
		if logger != nil {
			logger.Warn("unreadable flush log", "prefix", logPrefix, "err", err)
		}
		return nil, nil, false
	}
	// The series counts from the start of tracing (about when fio is
	// launched), but, like fio's own series, has no points for the ramp-up:
	// windows that ended before it are left out (the log keeps them). The log
	// stamps window ends like fio; the series is stamped at the window START,
	// like the fio points once the graph page moves them back one window.
	windowMs := s.AvgMsec
	if windowMs <= 0 {
		windowMs = defaultLogAvgMsec
	}
	for _, w := range windows {
		if w.EndMs <= int64(s.OffsetMs) {
			continue // entirely in the ramp-up
		}
		series = append(series, TimePoint{T: float64(w.EndMs-int64(windowMs)) / 1000, W: float64(w.MeanNs) / 1000})
	}
	return &s, series, true
}

// applyFlushLog fills the phase's flush summary and series for one device
// from the stored log, replacing whatever summary was saved with the run.
func applyFlushLog(phase *PhaseResult, device, logPrefix string) {
	sum, series, ok := loadFlushLog(logPrefix)
	if !ok {
		return
	}
	if phase.FlushTrace == nil {
		phase.FlushTrace = make(map[string]*flushtrace.Summary)
	}
	phase.FlushTrace[device] = sum
	if len(series) > 0 {
		if phase.FlushSeries == nil {
			phase.FlushSeries = make(map[string][]TimePoint)
		}
		phase.FlushSeries[device] = series
	}
}
