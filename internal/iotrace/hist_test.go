package iotrace

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func exactRank(sorted []int64, p float64) int64 {
	r := int(math.Ceil(p * float64(len(sorted))))
	if r < 1 {
		r = 1
	}
	return sorted[r-1]
}

func within(t *testing.T, name string, got, want int64, tol float64) {
	t.Helper()
	if math.Abs(float64(got-want)) > tol*float64(want) {
		t.Errorf("%s = %d, want %d within %.0f%%", name, got, want, tol*100)
	}
}

func logUniform(r *rand.Rand, n int, lo, hi float64) []int64 {
	vals := make([]int64, n)
	for i := range vals {
		vals[i] = int64(math.Exp(math.Log(lo) + r.Float64()*(math.Log(hi)-math.Log(lo))))
	}
	return vals
}

func checkAgainstExact(t *testing.T, vals []int64) {
	t.Helper()
	var h Hist
	for _, v := range vals {
		h.Add(v)
	}
	sorted := slices.Clone(vals)
	slices.Sort(sorted)
	s := h.Stats()
	if s.Count != int64(len(vals)) || s.MinNs != sorted[0] || s.MaxNs != sorted[len(sorted)-1] {
		t.Fatalf("count/min/max wrong: %+v", s)
	}
	within(t, "p50", s.P50Ns, exactRank(sorted, 0.5), 0.03)
	within(t, "p99", s.P99Ns, exactRank(sorted, 0.99), 0.03)
}

func TestHistExactSmall(t *testing.T) {
	var h Hist
	for v := int64(1); v <= 100; v++ {
		h.Add(v)
	}
	s := h.Stats()
	if s.Count != 100 || s.MinNs != 1 || s.MaxNs != 100 || s.P50Ns != 50 || s.P99Ns != 99 || s.MeanNs != 50.5 {
		t.Errorf("stats %+v", s)
	}
}

func TestHistRelativeError(t *testing.T) {
	checkAgainstExact(t, logUniform(rand.New(rand.NewPCG(1, 2)), 100000, 1e3, 1e8))
}

func TestHistConstant(t *testing.T) {
	var h Hist
	for i := 0; i < 100000; i++ {
		h.Add(100000)
	}
	s := h.Stats()
	if s.MinNs != 100000 || s.MaxNs != 100000 || s.P50Ns != 100000 || s.P99Ns != 100000 {
		t.Errorf("stats %+v", s)
	}
}

func TestHistEmpty(t *testing.T) {
	var h Hist
	if s := h.Stats(); s != (Stats{}) || math.IsNaN(s.MeanNs) {
		t.Errorf("stats %+v", s)
	}
}

func TestHistNegativeAndReset(t *testing.T) {
	var h Hist
	h.Add(-5)
	if s := h.Stats(); s.Count != 1 || s.MaxNs != 0 {
		t.Errorf("stats %+v", s)
	}
	h.Reset()
	if h.Count() != 0 || h.Stats() != (Stats{}) {
		t.Errorf("not reset")
	}
}

func TestHistHuge(t *testing.T) {
	var h Hist
	big := int64(1) << 42
	h.Add(big)
	h.Add(1000)
	s := h.Stats()
	if s.MaxNs != big || s.P99Ns > s.MaxNs || s.P99Ns < s.MinNs {
		t.Errorf("stats %+v", s)
	}
}

func TestHistHDDRange(t *testing.T) {
	checkAgainstExact(t, logUniform(rand.New(rand.NewPCG(3, 4)), 100000, 2e6, 1e10))

	r := rand.New(rand.NewPCG(5, 6))
	var vals []int64
	for i := 0; i < 50000; i++ {
		vals = append(vals, 100000)
	}
	for i := 0; i < 50000; i++ {
		vals = append(vals, 8_000_000+r.Int64N(7_000_001))
	}
	checkAgainstExact(t, vals)
	var h Hist
	for _, v := range vals {
		h.Add(v)
	}
	s := h.Stats()
	if s.P50Ns > 200000 || s.P99Ns < 8_000_000 {
		t.Errorf("modes lost: %+v", s)
	}
}
