package iotrace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bbbench/internal/flushtrace"
	kbpf "bbbench/internal/iotrace/ebpf"
)

// PairRequest is the pairing of the eBPF collector: a request completes the
// start stored under its own pointer (exact).
const PairRequest = "request"

// ebpfSession times requests and bios in the kernel (internal/iotrace/ebpf)
// and turns its cumulative histograms into windows: every window is the
// difference of two reads.
type ebpfSession struct {
	prog     kbpf.Program
	devices  []DeviceSummary
	names    []string // device name by program index
	windowMs int64
	offsetMs int64
	attached []string

	mu       sync.Mutex
	t0       time.Time
	prev     kbpf.Snapshot
	base     kbpf.Snapshot // counters at the baseline
	cur      kbpf.Snapshot // latest counters
	lastEnd  int64         // end of the last window written, ms
	windows  []Window
	phase    map[seriesKey]*Hist
	err      error
	gen      int
	stopped  bool
	closed   bool
	stop     chan struct{}
	kick     chan struct{}
	loopDone chan struct{}
	hasLoop  bool
	stopOnce sync.Once
}

// openEBPF loads the program and starts the session; it is the real startEBPF.
func openEBPF(devices []string, opts Options) (Session, error) {
	prog, err := kbpf.Open(len(devices))
	if err != nil {
		return nil, err
	}
	s, err := newEBPFSession(prog, devices, opts)
	if err != nil {
		_ = prog.Close()
		return nil, err
	}
	s.startLoop()
	return s, nil
}

// newEBPFSession sets the devices on an open program and takes the baseline.
// It reads nothing in the background: startLoop does that, tests call sample.
func newEBPFSession(prog kbpf.Program, devices []string, opts Options) (*ebpfSession, error) {
	attached := prog.Attached()
	hasInsert := false
	for _, a := range attached {
		hasInsert = hasInsert || a == "block_rq_insert"
	}
	s := &ebpfSession{
		prog: prog, attached: attached, windowMs: int64(opts.AvgMsec), offsetMs: int64(opts.OffsetMs),
		phase: map[seriesKey]*Hist{}, stop: make(chan struct{}), kick: make(chan struct{}, 1), loopDone: make(chan struct{}),
	}
	if s.windowMs <= 0 {
		s.windowMs = flushtrace.DefaultWindowMs
	}
	var specs []kbpf.DevSpec
	var reasons []string
	for _, name := range devices {
		ds := DeviceSummary{Name: name}
		maj, min, err := flushtrace.DevNumbers(opts.SysBlock, name)
		ds.BioBased = err == nil && flushtrace.BioBased(opts.SysBlock, name)
		paths := multipathPaths(opts.SysBlock, name)
		idx := len(s.names)
		switch {
		case err != nil:
			ds.Note = "unknown device: " + err.Error()
		case idx >= kbpf.MaxDevs:
			ds.Note = fmt.Sprintf("more than %d devices", kbpf.MaxDevs)
		case len(paths) > 0:
			// A native multipath head queues no requests: they run on its paths.
			for _, p := range paths {
				if pm, pn, perr := flushtrace.DevNumbers(opts.SysBlock, p); perr == nil {
					specs = append(specs, kbpf.DevSpec{Dev: uint32(pm<<20 | pn), Idx: idx, Source: kbpf.SourceBlock})
				}
			}
			ds.BioBased = false
			ds.Source, ds.Pair, ds.Exact, ds.BlockLayer = SourceBlock, PairRequest, true, hasInsert
		case ds.BioBased:
			specs = append(specs, kbpf.DevSpec{Dev: uint32(maj<<20 | min), Idx: idx, Source: kbpf.SourceBio})
			ds.Source, ds.Pair, ds.Events = SourceBio, PairBio, []string{bioEventsText}
		default:
			specs = append(specs, kbpf.DevSpec{Dev: uint32(maj<<20 | min), Idx: idx, Source: kbpf.SourceBlock})
			ds.Source, ds.Pair, ds.Exact, ds.BlockLayer = SourceBlock, PairRequest, true, hasInsert
		}
		if ds.Source == "" {
			reasons = append(reasons, name+": "+ds.Note)
			s.devices = append(s.devices, ds)
			continue
		}
		if ds.Source == SourceBlock {
			ds.Events = []string{"block_rq_issue -> block_rq_complete"}
			if ds.BlockLayer {
				ds.Events = append(ds.Events, "block_rq_insert -> block_rq_complete")
			}
		}
		s.names = append(s.names, name)
		s.devices = append(s.devices, ds)
	}
	if len(s.names) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, strings.Join(reasons, "; "))
	}
	if err := prog.SetDevices(specs); err != nil {
		return nil, err
	}
	snap, err := prog.Read()
	if err != nil {
		return nil, fmt.Errorf("read the eBPF baseline: %w", err)
	}
	s.prev, s.base, s.cur = snap, snap, snap
	s.t0 = time.Now()
	return s, nil
}

// multipathPaths names the path devices of a native NVMe multipath head
// (<sysBlock>/<name>/multipath/<path>); none for any other device.
func multipathPaths(sysBlock, name string) []string {
	entries, _ := os.ReadDir(filepath.Join(sysBlock, name, "multipath"))
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Name())
	}
	return paths
}

// startLoop samples once per window until Stop.
func (s *ebpfSession) startLoop() {
	s.hasLoop = true
	go s.loop()
}

func (s *ebpfSession) loop() {
	defer close(s.loopDone)
	for {
		s.mu.Lock()
		next := s.t0.Add(time.Duration(s.lastEnd+s.windowMs) * time.Millisecond)
		gen := s.gen
		s.mu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-s.stop:
			timer.Stop()
			return
		case <-s.kick:
			timer.Stop()
		case <-timer.C:
			s.mu.Lock()
			if gen == s.gen {
				if err := s.sampleLocked(s.lastEnd + s.windowMs); err != nil && s.err == nil {
					s.err = err
				}
			}
			s.mu.Unlock()
		}
	}
}

// sample closes the window ending at endMs (relative to the start marker).
func (s *ebpfSession) sample(endMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sampleLocked(endMs)
}

func (s *ebpfSession) sampleLocked(endMs int64) error {
	if s.closed {
		return nil
	}
	snap, err := s.prog.Read()
	if err != nil {
		return fmt.Errorf("read the eBPF histograms: %w", err)
	}
	counted := endMs-s.windowMs >= s.offsetMs // the window starts after the ramp
	for key, cur := range snap.Slots {
		prev := s.prev.Slots[key]
		var d [kbpf.NBuckets]uint64
		var n, sum uint64
		for i, c := range cur.Buckets {
			if prev != nil {
				c -= min(c, prev.Buckets[i]) // cumulative: never goes down
			}
			d[i] = c
			n += c
		}
		if n == 0 {
			continue
		}
		sum = cur.SumNs
		if prev != nil {
			sum -= min(sum, prev.SumNs)
		}
		sk := seriesKey{dev: s.names[key.Idx], kind: kindOf(key.Kind), dir: dirOfWrite(key.Write)}
		var h Hist
		h.addBuckets(d[:], sum)
		s.windows = append(s.windows, Window{EndMs: endMs, Dev: sk.dev, Kind: sk.kind, Dir: sk.dir, Stats: h.Stats()})
		if counted {
			ph := s.phase[sk]
			if ph == nil {
				ph = &Hist{}
				s.phase[sk] = ph
			}
			ph.addBuckets(d[:], sum)
		}
	}
	s.prev, s.cur = snap, snap
	s.lastEnd = endMs
	return nil
}

func kindOf(k int) Kind {
	switch k {
	case kbpf.KindBlockLayer:
		return KindBlockLayer
	case kbpf.KindBio:
		return KindBio
	}
	return KindDevice
}

func dirOfWrite(write bool) Dir {
	if write {
		return DirWrite
	}
	return DirRead
}

// MarkStart moves time zero to now: what completed before is dropped.
func (s *ebpfSession) MarkStart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.prog.Read()
	if err != nil {
		if s.err == nil {
			s.err = fmt.Errorf("read the eBPF baseline: %w", err)
		}
		return
	}
	s.prev, s.base, s.cur = snap, snap, snap
	s.t0 = time.Now()
	s.lastEnd = 0
	s.windows = nil
	s.phase = map[seriesKey]*Hist{}
	s.gen++
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Stop ends the loop, closes the last (partial) window and returns the result.
func (s *ebpfSession) Stop() (Result, error) {
	s.stopLoop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		if !s.closed { // after Close the program is gone: keep what was collected
			if err := s.sampleLocked(s.lastEnd + s.windowMs); err != nil && s.err == nil {
				s.err = err
			}
		}
	}
	var res Result
	res.Windows = append([]Window(nil), s.windows...)
	sortWindows(res.Windows)
	sum := Summary{AvgMsec: int(s.windowMs), OffsetMs: int(s.offsetMs), Mode: ModeEBPF}
	sum.EBPF = &EBPFInfo{
		Started:   s.cur.Started - s.base.Started,
		Completed: s.cur.Completed - s.base.Completed,
		NoStart:   s.cur.NoStart - s.base.NoStart,
		Full:      s.cur.Full - s.base.Full,
		Programs:  s.attached,
	}
	sum.Devices = make([]DeviceSummary, len(s.devices))
	first := true
	for i, ds := range s.devices {
		if ds.Source != "" {
			if first {
				ds.Unpaired = int64(sum.EBPF.NoStart + sum.EBPF.Full)
				first = false
			}
			for sk, h := range s.phase {
				if sk.dev != ds.Name {
					continue
				}
				if ds.Stats == nil {
					ds.Stats = map[Kind]map[Dir]Stats{}
				}
				if ds.Stats[sk.kind] == nil {
					ds.Stats[sk.kind] = map[Dir]Stats{}
				}
				ds.Stats[sk.kind][sk.dir] = h.Stats()
			}
		}
		sum.Devices[i] = ds
	}
	sum.Fill()
	res.Summary = sum
	// Detach now: an attached program times every I/O of the machine until it
	// is closed, and the next phase would stack another one on top.
	if !s.closed {
		s.closed = true
		if err := s.prog.Close(); err != nil && s.err == nil {
			s.err = err
		}
	}
	return res, s.err
}

// stopLoop ends the sampling goroutine, once.
func (s *ebpfSession) stopLoop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		if s.hasLoop {
			<-s.loopDone
		}
	})
}

// Close detaches the program; it is safe to call twice and after Stop.
func (s *ebpfSession) Close() error {
	s.stopLoop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.prog.Close()
}
