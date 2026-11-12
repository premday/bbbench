package main

import (
	"fmt"
	"io"
	"os"
	"sync"

	"bbbench/internal/flushtrace"
)

// traceRun is one trace session of a phase on one drive (the Flush trace or
// the I/O latency trace). Its methods are no-ops on a nil run (tracing off,
// not needed or not started).
type traceRun[S any] struct {
	markStart func()
	stop      func() *S // ends the session and stores its log
	close     func() error
	once      sync.Once
	sum       *S
}

// MarkStart moves the trace's time zero to now (the start gate opened).
func (r *traceRun[S]) MarkStart() {
	if r != nil {
		r.markStart()
	}
}

// Stop ends the session, stores its log next to fio's and returns the
// summary (nil for a nil run). Idempotent.
func (r *traceRun[S]) Stop() *S {
	if r == nil {
		return nil
	}
	r.once.Do(func() { r.sum = r.stop() })
	return r.sum
}

// Close ends the session quickly without storing anything (force quit).
func (r *traceRun[S]) Close() {
	if r != nil {
		_ = r.close()
	}
}

// flushTraceRun is the Flush trace of one phase on one drive.
type flushTraceRun = traceRun[flushtrace.Summary]

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
	// Stop returns the summary without the ramp.
	stop := func() *flushtrace.Summary {
		sum, err := sess.Stop()
		if err != nil && logger != nil {
			logger.Warn("flush tracing cleanup", "device", device, "err", err)
		}
		sum.AvgMsec = avgMsec // series follow fio's polling interval
		sum.OffsetMs = rampMs // and fio's clock, which starts after its ramp
		samples := sess.Samples()
		// Same statistics as a later load of the log: without the ramp.
		final := flushtrace.Finish(sum, samples)
		// Keep the samples with the fio logs, so the run can be
		// re-summarized and graphed later.
		if err := writeFlushLog(logPrefix, final, samples); err != nil && logger != nil {
			logger.Warn("storing flush samples", "device", device, "err", err)
		}
		return &final
	}
	return &flushTraceRun{markStart: sess.MarkStart, stop: stop, close: sess.Close}
}

// writeFlushLog stores the Flush latency of a traced phase next to fio's own
// logs, in fio's shape: one line per polling window plus the phase summary.
func writeFlushLog(logPrefix string, sum flushtrace.Summary, samples []flushtrace.Sample) error {
	return writeLogFile(flushtrace.LogName(logPrefix), func(w io.Writer) error {
		if err := flushtrace.WriteLog(w, sum, samples); err != nil {
			return fmt.Errorf("write flush log: %w", err)
		}
		return nil
	})
}

// writeLogFile creates the log file name and writes it with write.
func writeLogFile(name string, write func(io.Writer) error) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// windowStartSec places a log window that ended at endMs on the series
// clock: its start in seconds (avgMsec <= 0 is the default window). keep is
// false for a window that ended inside the ramp-up (offsetMs), which, like
// fio's own series, has no point.
func windowStartSec(endMs int64, avgMsec, offsetMs int) (t float64, keep bool) {
	if avgMsec <= 0 {
		avgMsec = defaultLogAvgMsec
	}
	if endMs <= int64(offsetMs) {
		return 0, false
	}
	return float64(endMs-int64(avgMsec)) / 1000, true
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
	for _, w := range windows {
		if t, keep := windowStartSec(w.EndMs, s.AvgMsec, s.OffsetMs); keep {
			series = append(series, TimePoint{T: t, W: float64(w.MeanNs) / 1000})
		}
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
