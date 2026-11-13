package flushtrace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Readers of an Instance.
const (
	ReaderRaw  = "raw"  // per_cpu/cpuN/trace_pipe_raw, binary pages, one goroutine per CPU
	ReaderText = "text" // trace_pipe, kernel-formatted text, one goroutine
)

// rawReader reads an instance's per-CPU binary pipes: one goroutine per CPU
// decodes pages without a shared lock and sends them to one merger
// goroutine, which restores the time order and feeds the instance in runs
// (one lock per run, not per event).
type rawReader struct {
	readers  []*cpuReader
	msgs     chan readerMsg
	pageSize int
	pool     sync.Pool
	done     chan struct{}
	late     atomic.Int64
	bad      atomic.Int64
	unknown  atomic.Int64
	overrun  *atomic.Int64
	deliver  func(func(handle func(*Event)))
}

// deliverRun is the most events fed under one lock.
const deliverRun = 1 << 14

type readerMsg struct {
	cpu    int
	events []Event
	idle   bool
	done   bool
}

type cpuReader struct {
	cpu      int
	path     string
	f        *os.File
	rc       syscall.RawConn
	pollable bool
	draining atomic.Bool
	dec      *decoder
}

// openRawReader opens per_cpu/cpu*/trace_pipe_raw of the instance dir and
// reads the formats of the enabled events from the tracefs root. An error
// means the raw path cannot be used (the caller falls back to trace_pipe)
// and says why.
func openRawReader(root, dir string, enable []string, overrun *atomic.Int64, deliver func(func(handle func(*Event)))) (*rawReader, error) {
	ents, err := os.ReadDir(filepath.Join(dir, "per_cpu"))
	if err != nil {
		return nil, fmt.Errorf("no per_cpu pipes: %w", err)
	}
	var cpus []int
	for _, e := range ents {
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "cpu")); err == nil && strings.HasPrefix(e.Name(), "cpu") {
			cpus = append(cpus, n)
		}
	}
	if len(cpus) == 0 {
		return nil, errors.New("no per_cpu pipes")
	}
	sort.Ints(cpus)
	rf, err := loadRawFormats(root, enable)
	if err != nil {
		return nil, err
	}
	r := &rawReader{
		msgs:     make(chan readerMsg, 4*len(cpus)+16),
		pageSize: max(rf.layout.pageSize(), os.Getpagesize()),
		done:     make(chan struct{}),
		overrun:  overrun,
		deliver:  deliver,
	}
	per := r.pageSize / 16
	r.pool.New = func() any { s := make([]Event, 0, per); return &s }
	for i, c := range cpus {
		p := filepath.Join(dir, "per_cpu", "cpu"+strconv.Itoa(c), "trace_pipe_raw")
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			r.closeFiles()
			return nil, fmt.Errorf("open %s: %w", p, err)
		}
		rc, err := f.SyscallConn()
		if err != nil {
			_ = f.Close()
			r.closeFiles()
			return nil, err
		}
		// A real tracefs pipe has a poll method (tracing_buffers_poll), so
		// the netpoller can park the reader and a read deadline can wake
		// it. A plain file (the tests' fake tracefs) is not pollable: it
		// is read to EOF.
		pollable := f.SetReadDeadline(time.Time{}) == nil
		r.readers = append(r.readers, &cpuReader{cpu: i, path: p, f: f, rc: rc, pollable: pollable, dec: newDecoder(rf)})
	}
	return r, nil
}

func (r *rawReader) closeFiles() {
	for _, c := range r.readers {
		_ = c.f.Close()
	}
}

func (r *rawReader) getBatch() []Event { return (*r.pool.Get().(*[]Event))[:0] }

func (r *rawReader) putBatch(b []Event) {
	if cap(b) == 0 {
		return
	}
	clear(b) // drop the strings for the GC
	b = b[:0]
	r.pool.Put(&b)
}

// start runs the readers and the merger.
func (r *rawReader) start() {
	for _, c := range r.readers {
		go r.read(c)
	}
	go r.merge()
}

// read is the loop of one CPU: read a page, decode it, send it; on an empty
// buffer say idle and park in the netpoller until data comes (no polling
// with sleeps). Once draining, an empty buffer ends it.
func (r *rawReader) read(c *cpuReader) {
	defer func() { r.msgs <- readerMsg{cpu: c.cpu, done: true} }()
	buf := make([]byte, r.pageSize)
	idle := false
	for {
		var n int
		var rerr error
		err := c.rc.Read(func(fd uintptr) bool {
			for {
				n, rerr = syscall.Read(int(fd), buf)
				if rerr != syscall.EINTR {
					break
				}
			}
			empty := rerr == syscall.EAGAIN || (rerr == nil && n == 0 && c.pollable)
			if empty && c.pollable && !c.draining.Load() {
				if !idle {
					idle = true
					r.msgs <- readerMsg{cpu: c.cpu, idle: true}
				}
				return false // wait for the pipe to become readable
			}
			return true
		})
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			// woken by drain: read on without parking
			_ = c.f.SetReadDeadline(time.Time{})
			continue
		case err != nil:
			return // the file was closed
		case rerr != nil || n == 0:
			// empty while draining (tracing is off: everything this CPU
			// stored was read), EOF of a plain file, or a read error
			return
		}
		idle = false
		evs, lost := c.dec.decodePage(buf[:n], r.getBatch())
		if lost > 0 {
			r.overrun.Add(lost)
		}
		r.msgs <- readerMsg{cpu: c.cpu, events: evs}
	}
}

// merge restores the global time order and feeds the instance.
func (r *rawReader) merge() {
	defer close(r.done)
	m := newMerger(len(r.readers))
	m.recycle = r.putBatch
	flush := func(now time.Time) {
		for more := true; more; {
			r.deliver(func(handle func(*Event)) {
				more = m.emit(now, deliverRun, handle) == deliverRun
			})
		}
		r.late.Store(m.late)
	}
	var timer *time.Timer
	var timerC <-chan time.Time
	live := len(r.readers)
	handle := func(msg readerMsg) {
		switch {
		case msg.done:
			m.setDone(msg.cpu)
			live--
		case msg.idle:
			m.setIdle(msg.cpu)
		default:
			m.add(msg.cpu, msg.events, time.Now())
		}
	}
	for live > 0 {
		select {
		case msg := <-r.msgs:
			handle(msg)
			// take whatever else is waiting before emitting
			for more := true; more && live > 0; {
				select {
				case msg := <-r.msgs:
					handle(msg)
				default:
					more = false
				}
			}
		case <-timerC:
			timerC = nil
		}
		flush(time.Now())
		if at, ok := m.deadline(); ok && timerC == nil {
			d := max(time.Until(at), time.Millisecond)
			if timer == nil {
				timer = time.NewTimer(d)
			} else {
				timer.Reset(d)
			}
			timerC = timer.C
		}
	}
	if timer != nil {
		timer.Stop()
	}
	flush(time.Now()) // every CPU is done: everything left
	var bad, unknown int64
	for _, c := range r.readers {
		bad += c.dec.bad
		unknown += c.dec.unknown
	}
	r.bad.Store(bad)
	r.unknown.Store(unknown)
}

// drain tells every reader to read until its buffer is empty, then stop.
// Tracing must be off already, so empty means everything was delivered.
func (r *rawReader) drain() {
	for _, c := range r.readers {
		c.draining.Store(true)
		if c.pollable {
			_ = c.f.SetReadDeadline(time.Now()) // wakes a parked read
		}
	}
}
