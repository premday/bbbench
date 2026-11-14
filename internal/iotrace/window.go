package iotrace

import (
	"cmp"
	"slices"

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

// series is the aggregation of one device, kind and direction: the open
// window (reused from one window to the next) and the phase after the ramp.
// A series is created on its first latency and lives as long as its
// aggregator, so a collector can keep a pointer to it.
type series struct {
	key   seriesKey
	open  bool  // a window is open
	idx   int64 // the open window
	win   Hist
	phase Hist
}

type aggregator struct {
	windowNs int64
	windowMs int64
	offsetNs int64
	byKey    map[seriesKey]*series
	all      []*series
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
		byKey:    map[seriesKey]*series{},
	}
}

// reset discards every window and the phase summary (time zero moved). The
// series stay, emptied, so pointers to them remain valid.
func (a *aggregator) reset() {
	for _, s := range a.all {
		s.open = false
		s.win.Reset()
		s.phase.Reset()
	}
	a.done = nil
}

// series returns the series of a device, kind and direction, created empty
// on first use.
func (a *aggregator) series(dev string, k Kind, d Dir) *series {
	key := seriesKey{dev, k, d}
	s := a.byKey[key]
	if s == nil {
		s = &series{key: key}
		a.byKey[key] = s
		a.all = append(a.all, s)
	}
	return s
}

// closeWindow appends the open window of s to the finished ones.
func (a *aggregator) closeWindow(s *series) {
	a.done = append(a.done, Window{EndMs: (s.idx + 1) * a.windowMs, Dev: s.key.dev, Kind: s.key.kind, Dir: s.key.dir, Stats: s.win.Stats()})
	s.open = false
	s.win.Reset()
}

// add records a completion at relNs (since the phase start) with latency latNs.
func (a *aggregator) add(dev string, k Kind, d Dir, relNs, latNs int64) {
	a.addTo(a.series(dev, k, d), relNs, latNs)
}

// addTo is add on a resolved series; it allocates nothing but the finished
// window it appends when a later window starts.
func (a *aggregator) addTo(s *series, relNs, latNs int64) {
	idx := relNs / a.windowNs
	if idx < 0 {
		idx = 0
	}
	if s.open && idx > s.idx {
		a.closeWindow(s)
	}
	if !s.open {
		s.open, s.idx = true, idx
	}
	s.win.Add(latNs)
	if relNs >= a.offsetNs {
		s.phase.Add(latNs)
	}
}

// finish closes the open windows and returns every window (sorted by end,
// device, kind, direction) and the phase summary per device, kind and direction.
func (a *aggregator) finish() ([]Window, map[string]map[Kind]map[Dir]Stats) {
	for _, s := range a.all {
		if s.open {
			a.closeWindow(s)
		}
	}
	ws := a.done
	slices.SortFunc(ws, func(x, y Window) int {
		return cmp.Or(cmp.Compare(x.EndMs, y.EndMs), cmp.Compare(x.Dev, y.Dev),
			cmp.Compare(x.Kind, y.Kind), cmp.Compare(x.Dir, y.Dir))
	})
	sum := map[string]map[Kind]map[Dir]Stats{}
	for _, s := range a.all {
		if s.phase.Count() == 0 {
			continue
		}
		k := s.key
		if sum[k.dev] == nil {
			sum[k.dev] = map[Kind]map[Dir]Stats{}
		}
		if sum[k.dev][k.kind] == nil {
			sum[k.dev][k.kind] = map[Dir]Stats{}
		}
		sum[k.dev][k.kind][k.dir] = s.phase.Stats()
	}
	return ws, sum
}
