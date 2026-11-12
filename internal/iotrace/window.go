package iotrace

import (
	"sort"

	"bbbench/internal/flushtrace"
)

// Window is the latency summary of one device, kind and direction over one
// averaging window; EndMs is the window end relative to the phase start.
type Window struct {
	EndMs int64
	Dev   string
	Kind  Kind
	Dir   Dir
	Stats
}

type seriesKey struct {
	dev  string
	kind Kind
	dir  Dir
}

type openWindow struct {
	idx  int64
	hist Hist
}

type aggregator struct {
	windowNs int64
	windowMs int64
	offsetNs int64
	open     map[seriesKey]*openWindow
	phase    map[seriesKey]*Hist
	done     []Window
}

func newAggregator(windowMs, offsetMs int) *aggregator {
	if windowMs <= 0 {
		windowMs = flushtrace.DefaultWindowMs
	}
	return &aggregator{
		windowNs: int64(windowMs) * 1_000_000,
		windowMs: int64(windowMs),
		offsetNs: int64(offsetMs) * 1_000_000,
		open:     map[seriesKey]*openWindow{},
		phase:    map[seriesKey]*Hist{},
	}
}

// reset discards every window and the phase summary (time zero moved).
func (a *aggregator) reset() {
	a.open, a.phase, a.done = map[seriesKey]*openWindow{}, map[seriesKey]*Hist{}, nil
}

func (a *aggregator) close(k seriesKey, w *openWindow) {
	a.done = append(a.done, Window{EndMs: (w.idx + 1) * a.windowMs, Dev: k.dev, Kind: k.kind, Dir: k.dir, Stats: w.hist.Stats()})
}

// add records a completion at relNs (since the phase start) with latency latNs.
func (a *aggregator) add(dev string, k Kind, d Dir, relNs, latNs int64) {
	key := seriesKey{dev, k, d}
	idx := relNs / a.windowNs
	if idx < 0 {
		idx = 0
	}
	w := a.open[key]
	if w != nil && idx > w.idx {
		a.close(key, w)
		w = nil
	}
	if w == nil {
		w = &openWindow{idx: idx}
		a.open[key] = w
	}
	w.hist.Add(latNs)
	if relNs >= a.offsetNs {
		h := a.phase[key]
		if h == nil {
			h = &Hist{}
			a.phase[key] = h
		}
		h.Add(latNs)
	}
}

// finish closes the open windows and returns every window (sorted by end,
// device, kind, direction) and the phase summary per device, kind and direction.
func (a *aggregator) finish() ([]Window, map[string]map[Kind]map[Dir]Stats) {
	for k, w := range a.open {
		a.close(k, w)
	}
	a.open = map[seriesKey]*openWindow{}
	ws := a.done
	sort.Slice(ws, func(i, j int) bool {
		x, y := ws[i], ws[j]
		if x.EndMs != y.EndMs {
			return x.EndMs < y.EndMs
		}
		if x.Dev != y.Dev {
			return x.Dev < y.Dev
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Dir < y.Dir
	})
	sum := map[string]map[Kind]map[Dir]Stats{}
	for k, h := range a.phase {
		if sum[k.dev] == nil {
			sum[k.dev] = map[Kind]map[Dir]Stats{}
		}
		if sum[k.dev][k.kind] == nil {
			sum[k.dev][k.kind] = map[Dir]Stats{}
		}
		sum[k.dev][k.kind][k.dir] = h.Stats()
	}
	return ws, sum
}
