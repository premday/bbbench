package flushtrace

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Summary is the Flush latency measured for one device during a phase.
type Summary struct {
	Source string  `json:"source"`           // "nvme" or "block"
	Count  int     `json:"count"`            // completed Flushes
	MeanNs float64 `json:"mean_ns"`          // exact for both sources
	MinNs  int64   `json:"min_ns,omitempty"` // only when Exact
	MaxNs  int64   `json:"max_ns,omitempty"` // only when Exact
	P50Ns  int64   `json:"p50_ns,omitempty"` // only when Exact
	P99Ns  int64   `json:"p99_ns,omitempty"` // only when Exact
	Exact  bool    `json:"exact"`            // per-Flush pairing is exact
	Lost   int     `json:"lost,omitempty"`   // issued but never seen completing
}

// Collector turns trace events into a Summary for one device.
type Collector interface {
	// Feed offers one event; events the collector does not care about are ignored.
	Feed(Event)
	Summary() Summary
}

type samples struct {
	deltas []int64
}

func (s *samples) add(d int64) { s.deltas = append(s.deltas, d) }

func (s *samples) summary(source string, exact bool, lost int) Summary {
	out := Summary{Source: source, Count: len(s.deltas), Exact: exact, Lost: lost}
	if len(s.deltas) == 0 {
		return out
	}
	var sum int64
	for _, d := range s.deltas {
		sum += d
	}
	out.MeanNs = float64(sum) / float64(len(s.deltas))
	if !exact {
		return out
	}
	sorted := append([]int64(nil), s.deltas...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out.MinNs, out.MaxNs = sorted[0], sorted[len(sorted)-1]
	out.P50Ns = percentile(sorted, 50)
	out.P99Ns = percentile(sorted, 99)
	return out
}

// percentile uses the nearest-rank method on a sorted slice.
func percentile(sorted []int64, p int) int64 {
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// ---- NVMe ------------------------------------------------------------------

var (
	nvmeSetupRe = regexp.MustCompile(`^nvme(\d+): (?:disk=([^,]+), )?qid=(\d+), cmdid=(\d+),.*cmd=\(nvme_cmd_flush\b`)
	nvmeDoneRe  = regexp.MustCompile(`^nvme(\d+): (?:disk=[^,]+, )?qid=(\d+), cmdid=(\d+),`)
)

// NVMe times Flush commands of one namespace (e.g. "nvme0n1"), pairing each
// nvme_setup_cmd with the nvme_complete_rq carrying the same
// (controller, queue, cmdid).
type NVMe struct {
	disk    string
	pending map[string]int64
	s       samples
}

// NewNVMe returns a collector for the named namespace block device.
func NewNVMe(disk string) *NVMe {
	return &NVMe{disk: disk, pending: map[string]int64{}}
}

func (c *NVMe) Feed(ev Event) {
	switch ev.Name {
	case "nvme_setup_cmd":
		m := nvmeSetupRe.FindStringSubmatch(ev.Payload)
		if m == nil || m[2] != c.disk {
			return
		}
		c.pending[fmt.Sprintf("%s/%s/%s", m[1], m[3], m[4])] = ev.TimeNs
	case "nvme_complete_rq":
		m := nvmeDoneRe.FindStringSubmatch(ev.Payload)
		if m == nil {
			return
		}
		key := fmt.Sprintf("%s/%s/%s", m[1], m[2], m[3])
		if t0, ok := c.pending[key]; ok {
			delete(c.pending, key)
			c.s.add(ev.TimeNs - t0)
		}
	}
}

func (c *NVMe) Summary() Summary { return c.s.summary("nvme", true, len(c.pending)) }

// ---- generic block device --------------------------------------------------

var (
	// "259,0 F 0 () 0 + 0 none,0,0 [fio]"  (rwbs, bytes, cmd, sector + nr_sector)
	blockIssueRe = regexp.MustCompile(`^(\d+),(\d+) (\S+) \d+ \(.*?\) \d+ \+ (\d+)`)
	// "259,0 F () 0 + 0 [0]"
	blockDoneRe = regexp.MustCompile(`^(\d+),(\d+) (\S+) \(.*?\) \d+ \+ (\d+)`)
)

// Block times Flush requests of one block device identified by its
// major:minor, using block_rq_issue / block_rq_complete. A request is a pure
// Flush when its rwbs string starts with 'F' and it covers no sectors.
type Block struct {
	major, minor int
	pending      []int64 // issue times, oldest first
	s            samples
}

// NewBlock returns a collector for the device with the given major:minor.
func NewBlock(major, minor int) *Block { return &Block{major: major, minor: minor} }

func (c *Block) isFlush(re *regexp.Regexp, payload string) bool {
	m := re.FindStringSubmatch(payload)
	if m == nil || !strings.HasPrefix(m[3], "F") || m[4] != "0" {
		return false
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	return maj == c.major && min == c.minor
}

func (c *Block) Feed(ev Event) {
	switch ev.Name {
	case "block_rq_issue":
		if c.isFlush(blockIssueRe, ev.Payload) {
			c.pending = append(c.pending, ev.TimeNs)
		}
	case "block_rq_complete":
		if c.isFlush(blockDoneRe, ev.Payload) && len(c.pending) > 0 {
			t0 := c.pending[0]
			c.pending = c.pending[1:]
			c.s.add(ev.TimeNs - t0)
		}
	}
}

func (c *Block) Summary() Summary { return c.s.summary("block", false, len(c.pending)) }
