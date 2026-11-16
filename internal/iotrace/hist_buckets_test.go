package iotrace

import (
	"math/rand"
	"testing"
)

func TestHistFromBucketsMatchesAdd(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var want Hist
	var counts [numBuckets]uint64
	var sum uint64
	for i := 0; i < 50000; i++ {
		v := int64(rng.ExpFloat64()*20000) + 1000
		want.Add(v)
		counts[bucketOf(v)]++
		sum += uint64(v)
	}
	var got Hist
	got.addBuckets(counts[:], sum)
	w, g := want.Stats(), got.Stats()
	if g.Count != w.Count || g.MeanNs != w.MeanNs || g.P50Ns != w.P50Ns || g.P99Ns != w.P99Ns {
		t.Errorf("stats from buckets %+v, from samples %+v", g, w)
	}
	// min and max are bucket edges: within one bucket (1/64) of the samples'.
	if d := float64(g.MinNs-w.MinNs) / float64(w.MinNs); d > 1.0/64 || d < -1.0/64 {
		t.Errorf("min %d vs %d", g.MinNs, w.MinNs)
	}
	if d := float64(g.MaxNs-w.MaxNs) / float64(w.MaxNs); d > 1.0/64 || d < -1.0/64 {
		t.Errorf("max %d vs %d", g.MaxNs, w.MaxNs)
	}
}

// The eBPF program computes the bucket of a latency with the formula below
// (bucket() in ebpf/bpf/iolat.bpf.c); it must give hist.go's buckets.
func cBucket(v uint64) int {
	if v < 128 {
		return int(v)
	}
	e := 63
	for v>>uint(e) == 0 {
		e--
	}
	if e > 39 {
		return numBuckets - 1
	}
	return 128 + ((e - 7) << 6) + int((v>>uint(e-6))&63)
}

func TestCBucketFormulaMatchesHist(t *testing.T) {
	for _, v := range []int64{0, 1, 127, 128, 129, 1000, 4095, 4096, 12345, 1 << 20, 1<<30 + 7, 1 << 39, 1<<40 - 1, 1 << 40, 1 << 50} {
		if got, want := cBucket(uint64(v)), bucketOf(v); got != want {
			t.Errorf("bucket(%d): C formula %d, hist.go %d", v, got, want)
		}
	}
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 20000; i++ {
		v := rng.Int63n(1 << 41)
		if cBucket(uint64(v)) != bucketOf(v) {
			t.Fatalf("bucket(%d) differs", v)
		}
	}
}
