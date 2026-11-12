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

func (h *Hist) quantile(p float64) int64 {
	rank := int64(p * float64(h.count))
	if float64(rank) < p*float64(h.count) {
		rank++
	}
	if rank < 1 {
		rank = 1
	}
	var cum int64
	for i, c := range h.buckets {
		cum += c
		if cum >= rank {
			v := upperEdge(i)
			if i == numBuckets-1 {
				v = h.max // the last bucket is open-ended
			}
			if v < h.min {
				v = h.min
			}
			if v > h.max {
				v = h.max
			}
			return v
		}
	}
	return h.max
}

// Stats returns the summary; the zero Stats when empty.
func (h *Hist) Stats() Stats {
	if h.count == 0 {
		return Stats{}
	}
	return Stats{
		Count:  h.count,
		MeanNs: h.sum / float64(h.count),
		MinNs:  h.min,
		MaxNs:  h.max,
		P50Ns:  h.quantile(0.5),
		P99Ns:  h.quantile(0.99),
	}
}
