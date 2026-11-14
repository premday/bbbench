package flushtrace

import (
	"math/rand"
	"slices"
	"testing"
	"time"
)

func evs(ts ...int64) []Event {
	out := make([]Event, len(ts))
	for i, t := range ts {
		out[i] = Event{TimeNs: t, Name: "x"}
	}
	return out
}

func times(out []Event) []int64 {
	var ts []int64
	for _, e := range out {
		ts = append(ts, e.TimeNs)
	}
	return ts
}

var t0 = time.Unix(1000, 0)

// emitAll collects what emit passes on.
func emitAll(m *merger, now time.Time, out []Event) []Event {
	m.emit(now, 0, func(e *Event) { out = append(out, *e) })
	return out
}

// A request issued on one CPU and completed on another comes out issue
// first whichever CPU delivers first.
func TestMergeCrossCPUBothOrders(t *testing.T) {
	for _, first := range []int{0, 1} {
		m := newMerger(2)
		issue, done := Event{TimeNs: 10, Name: "issue"}, Event{TimeNs: 20, Name: "complete"}
		batch := map[int][]Event{first: {issue}, 1 - first: {done}}
		// the completion's CPU delivers first: nothing can go out yet
		m.add(1-first, batch[1-first], t0)
		if out := emitAll(m, t0, nil); len(out) != 0 {
			t.Fatalf("first=%d: emitted %v before the other CPU spoke", first, out)
		}
		m.add(first, batch[first], t0)
		m.setDone(0)
		m.setDone(1)
		out := emitAll(m, t0, nil)
		if len(out) != 2 || out[0].Name != "issue" || out[1].Name != "complete" || m.late != 0 {
			t.Errorf("first=%d: %+v late %d", first, out, m.late)
		}
	}
}

// A busy CPU with nothing queued holds the others back to its last
// timestamp: nothing older can come from it.
func TestMergeBusyWatermark(t *testing.T) {
	m := newMerger(2)
	m.add(0, evs(5), t0)
	m.add(1, evs(1, 2, 3, 7, 9), t0)
	out := emitAll(m, t0, nil) // CPU 0 is busy with nothing queued after 5
	if got := times(out); !slices.Equal(got, []int64{1, 2, 3, 5}) {
		t.Fatalf("emitted %v, want [1 2 3 5]", got)
	}
	m.add(0, evs(8), t0)
	if got := times(emitAll(m, t0, nil)); !slices.Equal(got, []int64{7, 8}) {
		t.Errorf("then %v, want [7 8]", got)
	}
}

// An idle CPU holds events back for the hold time after they arrived, then
// lets them out; something it delivers later and older is late.
func TestMergeIdleHold(t *testing.T) {
	m := newMerger(2)
	m.setIdle(1)
	m.add(0, evs(10, 20), t0)
	m.setIdle(0)
	if out := emitAll(m, t0.Add(mergeHold/2), nil); len(out) != 0 {
		t.Fatalf("emitted %v within the hold", out)
	}
	at, ok := m.deadline()
	if !ok || !at.Equal(t0.Add(mergeHold)) {
		t.Fatalf("deadline %v %v", at, ok)
	}
	if got := times(emitAll(m, t0.Add(mergeHold), nil)); !slices.Equal(got, []int64{10, 20}) {
		t.Fatalf("after the hold %v", got)
	}
	if _, ok := m.deadline(); ok {
		t.Error("deadline with nothing held")
	}
	m.add(1, evs(15), t0.Add(mergeHold))
	m.setDone(0)
	m.setDone(1)
	if got := times(emitAll(m, t0.Add(mergeHold), nil)); !slices.Equal(got, []int64{15}) || m.late != 1 {
		t.Errorf("late event: %v, late %d", got, m.late)
	}
}

// Over the cap the oldest go out regardless of the watermark.
func TestMergeCap(t *testing.T) {
	m := newMerger(2)
	m.cap = 3
	m.add(0, evs(1, 2, 3, 4, 5), t0) // CPU 1 is busy and has said nothing
	if got := times(emitAll(m, t0, nil)); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("emitted %v, want the 2 over the cap", got)
	}
}

// Ties go to the lower CPU first, like the kernel's merge of the text pipe.
func TestMergeTies(t *testing.T) {
	m := newMerger(3)
	a := []Event{{TimeNs: 5, Name: "c2"}}
	b := []Event{{TimeNs: 5, Name: "c0"}}
	c := []Event{{TimeNs: 5, Name: "c1"}}
	m.add(2, a, t0)
	m.add(0, b, t0)
	m.add(1, c, t0)
	for i := 0; i < 3; i++ {
		m.setDone(i)
	}
	out := emitAll(m, t0, nil)
	if len(out) != 3 || out[0].Name != "c0" || out[1].Name != "c1" || out[2].Name != "c2" {
		t.Errorf("order %+v", out)
	}
}

// Random per-CPU streams come out as one sorted stream, every event once.
func TestMergeRandom(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	const cpus = 5
	var all []int64
	streams := make([][]int64, cpus)
	for i := 0; i < 5000; i++ {
		ts := int64(r.Intn(100000))
		all = append(all, ts)
		c := r.Intn(cpus)
		streams[c] = append(streams[c], ts)
	}
	for c := range streams {
		slices.Sort(streams[c])
	}
	slices.Sort(all)
	m := newMerger(cpus)
	var out []Event
	pos := make([]int, cpus)
	for live := cpus; live > 0; {
		c := r.Intn(cpus)
		if pos[c] < 0 {
			continue
		}
		if pos[c] == len(streams[c]) {
			m.setDone(c)
			pos[c] = -1
			live--
		} else {
			n := min(1+r.Intn(30), len(streams[c])-pos[c])
			m.add(c, evs(streams[c][pos[c]:pos[c]+n]...), t0)
			pos[c] += n
		}
		out = emitAll(m, t0, out)
	}
	out = emitAll(m, t0, out)
	if got := times(out); !slices.Equal(got, all) || m.late != 0 || m.held != 0 {
		t.Errorf("merged %d events (late %d, held %d), want %d sorted", len(got), m.late, m.held, len(all))
	}
}
