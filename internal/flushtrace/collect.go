package flushtrace

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Summary is the Flush latency measured for one device during a phase.
type Summary struct {
	Source string `json:"source"` // "nvme" or "block"
	// Events are the tracepoint pairs the Flushes were timed from (EventsOf(Source)).
	Events []string `json:"events,omitempty"`
	Count  int      `json:"count"`            // completed Flushes
	MeanNs float64  `json:"mean_ns"`          // exact for both sources
	MinNs  int64    `json:"min_ns,omitempty"` // only when Exact
	MaxNs  int64    `json:"max_ns,omitempty"` // only when Exact
	P50Ns  int64    `json:"p50_ns,omitempty"` // only when Exact
	P99Ns  int64    `json:"p99_ns,omitempty"` // only when Exact
	Exact  bool     `json:"exact"`            // per-Flush pairing is exact
	Lost   int      `json:"lost,omitempty"`   // issued but never seen completing
	// WriteCache is the kernel's view of the device cache while tracing
	// ("write back" or "write through"; empty = unknown). A write-through
	// device is sent no Flush commands at all, so zero Flushes is expected.
	WriteCache string `json:"write_cache,omitempty"`
	// AvgMsec is the fio log polling interval (--log-avg-msec) the run used;
	// the Flush time series is averaged over the same windows, so it lines up
	// with fio's IOPS/bandwidth/latency points. 0 = unknown.
	AvgMsec int `json:"avg_msec,omitempty"`
	// OffsetMs is when fio's own clock starts, in ms after tracing started: fio's
	// logs and statistics begin when its ramp_time ends. Flushes completed before
	// that are not part of the summary or the series. 0 = unknown / no ramp.
	OffsetMs int `json:"offset_ms,omitempty"`
}

// Collector turns trace events into a Summary for one device.
type Collector interface {
	// Feed offers one event; events the collector does not care about are ignored.
	Feed(Event)
	Summary() Summary
	// Samples returns every completed Flush, with raw trace-clock times.
	Samples() []Sample
}

// Sample is one completed Flush: when it completed and how long it took.
// TimeNs is the raw trace-clock timestamp while collecting; Session.Samples
// and the stored log make it relative to the start of tracing.
type Sample struct {
	TimeNs    int64
	LatencyNs int64
}

type samples struct {
	all []Sample
}

func (s *samples) add(completedNs, latencyNs int64) {
	s.all = append(s.all, Sample{TimeNs: completedNs, LatencyNs: latencyNs})
}

func (s *samples) summary(source string, exact bool, lost int) Summary {
	return Summarize(source, exact, lost, s.all)
}

// Summarize computes the Summary of a set of samples. It is the single place
// where statistics are derived, so results stored as sample logs can be
// re-summarized later (and pick up improvements) without re-running a test.
// lost is the number of Flushes issued but never seen completing.
func Summarize(source string, exact bool, lost int, all []Sample) Summary {
	out := Summary{Source: source, Events: EventsOf(source), Count: len(all), Exact: exact, Lost: lost}
	if len(all) == 0 {
		return out
	}
	var sum int64
	for _, x := range all {
		sum += x.LatencyNs
	}
	out.MeanNs = float64(sum) / float64(len(all))
	if !exact {
		return out
	}
	sorted := make([]int64, len(all))
	for i, x := range all {
		sorted[i] = x.LatencyNs
	}
	slices.Sort(sorted)
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
			c.s.add(ev.TimeNs, ev.TimeNs-t0)
		}
	}
}

func (c *NVMe) Summary() Summary  { return c.s.summary("nvme", true, len(c.pending)) }
func (c *NVMe) Samples() []Sample { return c.s.all }

// ---- generic block device --------------------------------------------------

var (
	// "259,0 F 0 () 0 + 0 none,0,0 [fio]"  (rwbs, bytes, cmd, sector + nr_sector)
	blockIssueRe = regexp.MustCompile(`^(\d+),(\d+) (\S+) \d+ \(.*?\) \d+ \+ (\d+)`)
	// "259,0 F () 0 + 0 [0]"
	blockDoneRe = regexp.MustCompile(`^(\d+),(\d+) (\S+) \(.*?\) \d+ \+ (\d+)`)
)

// "9,127 FWS 0 + 0 [fio]" (block_bio_queue) and "9,127 FWS 0 + 0 [0]"
// (block_bio_complete): rwbs, sector + nr_sector.
var bioRe = regexp.MustCompile(`^(\d+),(\d+) (\S+) \d+ \+ (\d+)`)

// Block times Flush requests of one block device identified by its
// major:minor, using block_rq_issue / block_rq_complete. A request is a pure
// Flush when its rwbs string starts with 'F' and it covers no sectors.
// NewBio makes the same collector for a bio-based node over the bio events.
type Block struct {
	major, minor    int
	source          string // "block" or "bio"
	issueEv, doneEv string
	issueRe, doneRe *regexp.Regexp
	pending         []int64 // issue times, oldest first
	s               samples
}

// NewBlock returns a collector for the device with the given major:minor.
func NewBlock(major, minor int) *Block {
	return &Block{major: major, minor: minor, source: "block",
		issueEv: "block_rq_issue", doneEv: "block_rq_complete", issueRe: blockIssueRe, doneRe: blockDoneRe}
}

// NewBio returns a collector for a bio-based node (md, most dm), which has
// no request events: its Flushes are the empty flush bios submitted to it,
// block_bio_queue -> block_bio_complete, paired first-in first-out. The time
// covers the node's handling, its members' flushes included.
func NewBio(major, minor int) *Block {
	return &Block{major: major, minor: minor, source: "bio",
		issueEv: "block_bio_queue", doneEv: "block_bio_complete", issueRe: bioRe, doneRe: bioRe}
}

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
	case c.issueEv:
		if c.isFlush(c.issueRe, ev.Payload) {
			c.pending = append(c.pending, ev.TimeNs)
		}
	case c.doneEv:
		if c.isFlush(c.doneRe, ev.Payload) && len(c.pending) > 0 {
			t0 := c.pending[0]
			c.pending = c.pending[1:]
			c.s.add(ev.TimeNs, ev.TimeNs-t0)
		}
	}
}

func (c *Block) Summary() Summary  { return c.s.summary(c.source, false, len(c.pending)) }
func (c *Block) Samples() []Sample { return c.s.all }

// EventsOf names the tracepoint pair a Flush source times: "nvme" nvme_setup_cmd
// -> nvme_complete_rq (flush opcode), "block" block_rq_issue -> block_rq_complete
// (rwbs F, 0 sectors), "bio" block_bio_queue -> block_bio_complete (the same,
// on a bio-based node); nil for an unknown source.
func EventsOf(source string) []string {
	switch source {
	case "nvme":
		return []string{"nvme_setup_cmd -> nvme_complete_rq"}
	case "block":
		return []string{"block_rq_issue -> block_rq_complete"}
	case "bio":
		return []string{"block_bio_queue -> block_bio_complete"}
	}
	return nil
}
