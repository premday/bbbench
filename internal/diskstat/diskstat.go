// Package diskstat samples the kernel's per-disk I/O counters
// (/sys/block/<dev>/stat) at a fixed interval, so a run can show what each
// member of an array did. It only reads.
package diskstat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bbbench/internal/util"
)

// Counters are the cumulative counters of one disk (stat fields 0, 2, 3, 4, 6, 7).
type Counters struct {
	ReadIOs, ReadSectors, ReadTicksMs    uint64
	WriteIOs, WriteSectors, WriteTicksMs uint64
}

// ReadCounters reads <sysBlock>/<name>/stat; sysBlock "" is /sys/block.
func ReadCounters(sysBlock, name string) (Counters, error) {
	if sysBlock == "" {
		sysBlock = "/sys/block"
	}
	b, err := os.ReadFile(filepath.Join(sysBlock, name, "stat"))
	if err != nil {
		return Counters{}, err
	}
	f := strings.Fields(string(b))
	if len(f) < 8 {
		return Counters{}, fmt.Errorf("diskstat: %s: %d fields, want at least 8", name, len(f))
	}
	var v [8]uint64
	for _, i := range []int{0, 2, 3, 4, 6, 7} {
		v[i], err = strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return Counters{}, fmt.Errorf("diskstat: %s field %d: %w", name, i, err)
		}
	}
	return Counters{v[0], v[2], v[3], v[4], v[6], v[7]}, nil
}

// Window is what one device did between two samples. D is the delta; a
// counter that went backwards counts as 0.
type Window struct {
	EndMs, SpanMs int64
	Device        string
	D             Counters
}

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// Rates returns per-second rates over SpanMs; bandwidth is MiB/s, latency is
// the mean per I/O in microseconds (0 without I/O or span).
func (w Window) Rates() (readIOPS, writeIOPS, readMBps, writeMBps, readLatUs, writeLatUs float64) {
	if w.SpanMs > 0 {
		sec := float64(w.SpanMs) / 1000
		readIOPS = float64(w.D.ReadIOs) / sec
		writeIOPS = float64(w.D.WriteIOs) / sec
		readMBps = float64(w.D.ReadSectors) * 512 / 1048576 / sec
		writeMBps = float64(w.D.WriteSectors) * 512 / 1048576 / sec
	}
	if w.D.ReadIOs > 0 {
		readLatUs = float64(w.D.ReadTicksMs) * 1000 / float64(w.D.ReadIOs)
	}
	if w.D.WriteIOs > 0 {
		writeLatUs = float64(w.D.WriteTicksMs) * 1000 / float64(w.D.WriteIOs)
	}
	return
}

type last struct {
	c  Counters
	ms int64
	ok bool
}

// Sampler records one Window per device per Sample call.
type Sampler struct {
	names    []string
	sysBlock string

	mu      sync.Mutex
	started bool
	t0      time.Time
	prev    map[string]last
	windows []Window
	lastAt  time.Time // time of the latest Sample

	array     string
	members   []string
	mdPrevMs  int64
	mdHasPrev bool
	mdWindows []MDWindow
}

// WithArray also records the md state of the array (<sysBlock>/<array>/md)
// at every Sample after the first, when that directory exists. It returns s.
func (s *Sampler) WithArray(name string, members []string) *Sampler {
	s.array = name
	s.members = append([]string(nil), members...)
	return s
}

// MDWindows returns a copy of the md windows recorded so far.
func (s *Sampler) MDWindows() []MDWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MDWindow(nil), s.mdWindows...)
}

// NewSampler samples the named devices under sysBlock ("" = /sys/block).
func NewSampler(names []string, sysBlock string) *Sampler {
	return &Sampler{names: append([]string(nil), names...), sysBlock: sysBlock, prev: map[string]last{}}
}

// Sample takes one reading. The first call sets time zero and the baseline;
// each later call appends one Window per readable device. A device whose
// stat cannot be read is skipped (earlier windows stay); when it comes back
// its window spans from its last good reading.
func (s *Sampler) Sample(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		s.started = true
		s.t0 = now
	}
	s.lastAt = now
	ms := now.Sub(s.t0).Milliseconds()
	for _, n := range s.names {
		c, err := ReadCounters(s.sysBlock, n)
		if err != nil {
			continue
		}
		p := s.prev[n]
		if p.ok {
			s.windows = append(s.windows, Window{
				EndMs: ms, SpanMs: ms - p.ms, Device: n,
				D: Counters{
					ReadIOs:      sub(c.ReadIOs, p.c.ReadIOs),
					ReadSectors:  sub(c.ReadSectors, p.c.ReadSectors),
					ReadTicksMs:  sub(c.ReadTicksMs, p.c.ReadTicksMs),
					WriteIOs:     sub(c.WriteIOs, p.c.WriteIOs),
					WriteSectors: sub(c.WriteSectors, p.c.WriteSectors),
					WriteTicksMs: sub(c.WriteTicksMs, p.c.WriteTicksMs),
				},
			})
		}
		s.prev[n] = last{c: c, ms: ms, ok: true}
	}
	if s.array != "" {
		if st, ok := ReadMD(s.sysBlock, s.array, s.members); ok {
			if s.mdHasPrev {
				s.mdWindows = append(s.mdWindows, MDWindow{EndMs: ms, SpanMs: ms - s.mdPrevMs, S: st})
			}
			s.mdHasPrev, s.mdPrevMs = true, ms
		}
	}
}

// sampleFinal takes the closing reading unless it would span less than half
// of every since the previous one (a sliver that is not a real window).
func (s *Sampler) sampleFinal(now time.Time, every time.Duration) {
	s.mu.Lock()
	short := s.started && now.Sub(s.lastAt) < every/2
	s.mu.Unlock()
	if !short {
		s.Sample(now)
	}
}

// Run samples at start and every `every` until ctx is done, then once more
// unless that last window would be shorter than half of `every`.
func (s *Sampler) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Second // NewTicker panics otherwise; sampling must never fail a phase
	}
	s.Sample(time.Now())
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.sampleFinal(time.Now(), every)
			return
		case now := <-t.C:
			s.Sample(now)
		}
	}
}

// Windows returns a copy of the windows recorded so far.
func (s *Sampler) Windows() []Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Window(nil), s.windows...)
}

// MDState is the state of an md array read from <sysBlock>/<array>/md.
// Members maps a member name to the trimmed text of md/dev-<name>/state ("removed" when
// the dev-<name> directory is gone, "" when the state cannot be read).
type MDState struct {
	Action           string
	Completed, Total uint64
	SpeedKiBps       uint64
	Degraded         int // -1 when it could not be read
	ArrayState       string
	RaidDisks        int
	Members          map[string]string
}

// MDWindow is the md state at the end of one sampling window.
type MDWindow struct {
	EndMs, SpanMs int64
	S             MDState
}

// ReadMD reads the md state of array; sysBlock "" is /sys/block. ok is false
// when <sysBlock>/<array>/md does not exist. A field that cannot be read or
// parsed stays empty or zero, except Degraded, which is -1.
func ReadMD(sysBlock, array string, members []string) (MDState, bool) {
	if sysBlock == "" {
		sysBlock = "/sys/block"
	}
	dir := filepath.Join(sysBlock, array, "md")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return MDState{}, false
	}
	st := MDState{
		Action:     util.ReadTrim(filepath.Join(dir, "sync_action")),
		ArrayState: util.ReadTrim(filepath.Join(dir, "array_state")),
	}
	if f := strings.Split(util.ReadTrim(filepath.Join(dir, "sync_completed")), "/"); len(f) == 2 {
		d, e1 := strconv.ParseUint(strings.TrimSpace(f[0]), 10, 64)
		t, e2 := strconv.ParseUint(strings.TrimSpace(f[1]), 10, 64)
		if e1 == nil && e2 == nil {
			st.Completed, st.Total = d, t
		}
	}
	st.SpeedKiBps, _ = strconv.ParseUint(util.ReadTrim(filepath.Join(dir, "sync_speed")), 10, 64)
	if d, err := strconv.Atoi(util.ReadTrim(filepath.Join(dir, "degraded"))); err == nil && d >= 0 {
		st.Degraded = d
	} else {
		st.Degraded = -1 // unreadable: unknown, not "not degraded"
	}
	st.RaidDisks, _ = strconv.Atoi(util.ReadTrim(filepath.Join(dir, "raid_disks")))
	if len(members) > 0 {
		st.Members = make(map[string]string, len(members))
		for _, m := range members {
			if fi, err := os.Stat(filepath.Join(dir, "dev-"+m)); err != nil || !fi.IsDir() {
				st.Members[m] = "removed" // no longer part of the array
				continue
			}
			st.Members[m] = util.ReadTrim(filepath.Join(dir, "dev-"+m, "state"))
		}
	}
	return st, true
}
