package main

import (
	"fmt"
	"os"

	"bbbench/internal/flushtrace"
)

// defaultFlushWindowMs is the averaging window of the Flush latency series for
// logs that do not record fio's polling interval (the --log-avg-msec default).
const defaultFlushWindowMs = 1000

// writeFlushLog stores the Flush samples of a traced phase next to fio's own
// logs, so statistics and graphs can be rebuilt for a finished run.
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
// fio log prefix. The Summary is recomputed from the samples (not taken from
// any saved summary), and the series is the mean latency per second in µs
// (in the W column, like fio write latency). ok is false when there is no
// usable log, e.g. runs from before Flush samples were stored.
func loadFlushLog(logPrefix string) (sum *flushtrace.Summary, series []TimePoint, ok bool) {
	f, err := os.Open(flushtrace.LogName(logPrefix))
	if err != nil {
		return nil, nil, false
	}
	defer f.Close()
	s, samples, err := flushtrace.ReadLog(f)
	if err != nil {
		if logger != nil {
			logger.Warn("unreadable flush log", "prefix", logPrefix, "err", err)
		}
		return nil, nil, false
	}
	// Same windows as fio's own series: its --log-avg-msec, stored in the log.
	windowMs := s.AvgMsec
	if windowMs <= 0 {
		windowMs = defaultFlushWindowMs
	}
	// Same clock as fio's logs: t = 0 when fio's ramp ends, and a point is
	// stamped at the END of its window (fio logs a window when it closes), so
	// the first point of a 30 s interval is at 30 s, not 0.
	kept := flushtrace.AfterMs(samples, s.OffsetMs)
	shifted := make([]flushtrace.Sample, len(kept))
	for i, x := range kept {
		shifted[i] = flushtrace.Sample{TimeNs: x.TimeNs - int64(s.OffsetMs)*1_000_000, LatencyNs: x.LatencyNs}
	}
	for _, b := range flushtrace.Bins(shifted, int64(windowMs)*1_000_000) {
		series = append(series, TimePoint{T: b.TSec + float64(windowMs)/1000, W: b.MeanNs / 1000})
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
