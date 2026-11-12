package main

import (
	"fmt"
	"os"
	"sync"

	"bbbench/internal/flushtrace"
)

// flushTraceRun is the Flush trace of one phase on one drive. Its methods
// are no-ops on a nil run (not an fsync phase, tracing off or not started).
type flushTraceRun struct {
	sess      *flushtrace.Session
	device    string
	logPrefix string
	avgMsec   int
	rampMs    int
	once      sync.Once
	sum       *flushtrace.Summary
}

// startFlushTrace times the device's Flush commands from kernel trace events
// while fio runs. Failure to trace never fails the phase: it is logged and
// the run is nil.
func startFlushTrace(device, logPrefix string, avgMsec, rampMs int, opts flushtrace.Options) *flushTraceRun {
	sess, err := flushtrace.Start(device, opts)
	if err != nil {
		if logger != nil {
			logger.Warn("flush tracing unavailable", "device", device, "err", err)
		}
		return nil
	}
	return &flushTraceRun{sess: sess, device: device, logPrefix: logPrefix, avgMsec: avgMsec, rampMs: rampMs}
}

// MarkStart moves the trace's time zero to now (the start gate opened).
func (r *flushTraceRun) MarkStart() {
	if r != nil {
		r.sess.MarkStart()
	}
}

// Stop ends the session, stores the samples next to fio's logs and returns
// the summary without the ramp (nil for a nil run). Idempotent.
func (r *flushTraceRun) Stop() *flushtrace.Summary {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		sum, err := r.sess.Stop()
		if err != nil && logger != nil {
			logger.Warn("flush tracing cleanup", "device", r.device, "err", err)
		}
		sum.AvgMsec = r.avgMsec // series follow fio's polling interval
		sum.OffsetMs = r.rampMs // and fio's clock, which starts after its ramp
		samples := r.sess.Samples()
		// Same statistics as a later load of the log: without the ramp.
		final := flushtrace.Finish(sum, samples)
		r.sum = &final
		// Keep the samples with the fio logs, so the run can be
		// re-summarized and graphed later.
		if err := writeFlushLog(r.logPrefix, final, samples); err != nil && logger != nil {
			logger.Warn("storing flush samples", "device", r.device, "err", err)
		}
	})
	return r.sum
}

// Close ends the session quickly without storing anything (force quit).
func (r *flushTraceRun) Close() {
	if r != nil {
		_ = r.sess.Close()
	}
}

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
