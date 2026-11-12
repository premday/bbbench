// Package iotrace measures per-request I/O latency from kernel trace events.
package iotrace

import "math/bits"

// Kind says where a latency was measured.
type Kind string

const (
	KindDevice     Kind = "device"
	KindBlockLayer Kind = "blocklayer"
	// KindBio is the latency of the bios submitted to a bio-based node (md,
	// most dm): block_bio_queue -> block_bio_complete on the node itself.
	KindBio Kind = "bio"
)

// Dir is the direction of a request.
type Dir string

const (
	DirRead  Dir = "r"
	DirWrite Dir = "w"
)

// Stats summarises a set of latencies. Count, mean, min and max are exact;
// the percentiles have a relative error of at most 1/64.
type Stats struct {
	Count  int64   `json:"count"`
	MeanNs float64 `json:"mean_ns"`
	MinNs  int64   `json:"min_ns"`
	MaxNs  int64   `json:"max_ns"`
	P50Ns  int64   `json:"p50_ns"`
	P99Ns  int64   `json:"p99_ns"`
}

const (
	linearBuckets = 128 // one bucket per ns below 128
	subBits       = 6   // 64 sub-buckets per power of two
	firstExp      = 7   // 2^7 = 128
	maxExp        = 39  // powers of two up to 2^40
	numBuckets    = linearBuckets + (maxExp-firstExp+1)<<subBits
)

// Hist is a log-bucketed latency histogram. The zero value is ready to use.
type Hist struct {
	buckets  [numBuckets]int64
	count    int64
	sum      float64
	min, max int64
}

func bucketOf(v int64) int {
	if v < linearBuckets {
		return int(v)
	}
	e := bits.Len64(uint64(v)) - 1
	if e > maxExp {
		return numBuckets - 1
	}
	sub := int(v>>(e-subBits)) & (1<<subBits - 1)
	return linearBuckets + (e-firstExp)<<subBits + sub
}

// upperEdge is the largest value that falls in bucket i.
func upperEdge(i int) int64 {
	if i < linearBuckets {
		return int64(i)
	}
	j := i - linearBuckets
	e := firstExp + j>>subBits
	sub := int64(j & (1<<subBits - 1))
	shift := e - subBits
	return (1<<subBits+sub)<<shift + (1 << shift) - 1
}

// Add records one latency; a negative value counts as 0.
func (h *Hist) Add(ns int64) {
	if ns < 0 {
		ns = 0
	}
	if h.count == 0 || ns < h.min {
		h.min = ns
	}
	if ns > h.max {
		h.max = ns
	}
	h.count++
	h.sum += float64(ns)
	h.buckets[bucketOf(ns)]++
}

// Count is the number of recorded latencies.
func (h *Hist) Count() int64 { return h.count }

// Reset forgets every latency.
func (h *Hist) Reset() { *h = Hist{} }

// rank is the 1-based rank of quantile p among the recorded latencies.
func (h *Hist) rank(p float64) int64 {
	rank := int64(p * float64(h.count))
	if float64(rank) < p*float64(h.count) {
		rank++
	}
	return max(rank, 1)
}

// valueOf is the value reported for a quantile falling in bucket i.
func (h *Hist) valueOf(i int) int64 {
	v := upperEdge(i)
	if i == numBuckets-1 {
		v = h.max // the last bucket is open-ended
	}
	return min(max(v, h.min), h.max)
}

// quantiles returns the quantiles p and q (p <= q) in one pass over the
// buckets.
func (h *Hist) quantiles(p, q float64) (vp, vq int64) {
	rp, rq := h.rank(p), h.rank(q)
	vp, vq = h.max, h.max
	var cum int64
	found := false
	for i, c := range h.buckets {
		cum += c
		if !found && cum >= rp {
			vp, found = h.valueOf(i), true
		}
		if cum >= rq {
			vq = h.valueOf(i)
			break
		}
	}
	return vp, vq
}

// Stats returns the summary; the zero Stats when empty.
func (h *Hist) Stats() Stats {
	if h.count == 0 {
		return Stats{}
	}
	p50, p99 := h.quantiles(0.5, 0.99)
	return Stats{
		Count:  h.count,
		MeanNs: h.sum / float64(h.count),
		MinNs:  h.min,
		MaxNs:  h.max,
		P50Ns:  p50,
		P99Ns:  p99,
	}
}
