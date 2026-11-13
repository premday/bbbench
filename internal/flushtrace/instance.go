package flushtrace

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bbbench/internal/util"
)

// startMarker is written to trace_marker when tracing starts; its timestamp
// is time zero of the recorded samples.
const startMarker = "bbbench-start"

// stopMarker is written (while tracing is still on) once the events are
// disabled. A blocked trace_pipe read is only woken by new data, never by
// tracing being switched off, so seeing this marker is how the reader knows
// every earlier event has been delivered.
const stopMarker = "bbbench-stop"

var instanceSeq atomic.Int64

// ErrNoTracefs means no tracefs mount was found. It names no feature: Flush
// and I/O latency tracing each wrap it once with their own ErrUnsupported.
var ErrNoTracefs = errors.New("tracefs not found (is it mounted, and are you root?)")

// FindTracefs returns the tracefs mount, or ErrNoTracefs.
func FindTracefs(opts Options) (string, error) {
	cands := []string{opts.TracefsRoot}
	if opts.TracefsRoot == "" {
		cands = []string{"/sys/kernel/tracing", "/sys/kernel/debug/tracing"}
	}
	for _, c := range cands {
		if st, err := os.Stat(filepath.Join(c, "instances")); err == nil && st.IsDir() {
			return c, nil
		}
	}
	return "", ErrNoTracefs
}

// TracefsAvailable reports whether tracefs can be found (nil, else
// ErrNoTracefs), so a run can say before it starts that Flushes or I/O
// latency will not be traced.
func TracefsAvailable() error {
	_, err := FindTracefs(Options{})
	return err
}

// HasEvents reports whether every event ("<group>/<name>", e.g.
// "block/block_rq_insert") exists under the tracefs root.
func HasEvents(root string, events ...string) bool {
	for _, e := range events {
		if _, err := os.Stat(filepath.Join(root, "events", e)); err != nil {
			return false
		}
	}
	return true
}

// DevNumbers reads the major and minor of a block device from
// <sysBlock>/<name>/dev; sysBlock "" means /sys/block.
func DevNumbers(sysBlock, name string) (maj, min int, err error) {
	sysBlock = util.SysBlockOr(sysBlock)
	b, err := os.ReadFile(filepath.Join(sysBlock, name, "dev"))
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Split(strings.TrimSpace(string(b)), ":")
	if len(parts) != 2 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	if maj, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, err
	}
	min, err = strconv.Atoi(parts[1])
	return maj, min, err
}

// BioBased reports whether a block device is bio-based: it has a request
// queue directory (<sysBlock>/<name>/queue) but no blk-mq one (no mq/), like
// md and most dm nodes. Such a device sees no block_rq_* events at all; its
// I/O is traced at the bio level (block_bio_queue -> block_bio_complete).
// A device sysfs says nothing about (no queue/) is not bio-based. This is the
// notion of the queue dump (internal/diskinfo). sysBlock "" means /sys/block.
func BioBased(sysBlock, name string) bool {
	sysBlock = util.SysBlockOr(sysBlock)
	if st, err := os.Stat(filepath.Join(sysBlock, name, "queue")); err != nil || !st.IsDir() {
		return false
	}
	_, err := os.Stat(filepath.Join(sysBlock, name, "mq"))
	return os.IsNotExist(err)
}

// InstanceName returns a unique private instance name, bbbench-<kind>-<pid>-<seq>.
func InstanceName(kind string) string {
	return fmt.Sprintf("bbbench-%s-%d-%d", kind, os.Getpid(), instanceSeq.Add(1))
}

// Instance is one private tracefs instance and its reader: the binary
// per-CPU pipes when the kernel offers them and their formats decode (one
// goroutine per CPU and a merger), else the text trace_pipe (one goroutine).
// The system-wide trace buffer and settings are left alone, so instances do
// not interfere with each other.
type Instance struct {
	dir        string
	pipe       *os.File   // text reader
	raw        *rawReader // raw reader (nil = text)
	readerNote string     // why the raw reader is not used
	clock      string     // the instance's trace clock ("" = unknown)
	events     []string   // enable files switched on, to switch off again
	feed       func(ev Event, startNs int64)
	mu         sync.Mutex
	done       chan struct{}
	stopSeen   chan struct{} // closed when the stop marker was read
	startNs    int64         // trace-clock time of the last start marker (0 = not seen)
	overrun    atomic.Int64  // events the kernel dropped ([LOST N EVENTS], missed-events flags)
	stopOnce   sync.Once
	stopErr    error
}

// Clocks the private instance uses, in order of preference: both are
// global (comparable across CPUs, which pairing events of different CPUs
// needs) and in ns. The default "local" is per CPU.
var preferredClocks = []string{"mono", "global"}

// nsClocks are the trace clocks whose timestamps are ns.
var nsClocks = map[string]bool{"local": true, "global": true, "mono": true, "mono_raw": true, "boot": true, "perf": true, "tai": true}

// setClock switches the instance to a global ns clock when its trace_clock
// file offers one and returns the clock in effect ("" when unknown).
// Changing the clock resets the buffer: it runs before anything is recorded.
func (i *Instance) setClock() string {
	b, err := os.ReadFile(filepath.Join(i.dir, "trace_clock"))
	if err != nil {
		return ""
	}
	cur, offered := "", map[string]bool{}
	for _, f := range strings.Fields(string(b)) {
		if strings.HasPrefix(f, "[") && strings.HasSuffix(f, "]") {
			f = strings.Trim(f, "[]")
			cur = f
		}
		offered[f] = true
	}
	for _, c := range preferredClocks {
		if c == cur {
			return cur
		}
		if offered[c] && i.write("trace_clock", c) == nil {
			return c
		}
	}
	return cur
}

// OpenInstance creates instances/<name>, sets its buffer size (bufferKB > 0),
// starts the reader, writes the filters (best effort: events are also
// filtered in userspace), enables the events, turns tracing on and writes the
// start marker. Start and stop markers are consumed here; every other parsed
// event goes to feed, called with the instance mutex held. On failure the
// instance is cleaned up.
func OpenInstance(root, name string, bufferKB int, enable []string, filters map[string]string, feed func(Event)) (*Instance, error) {
	return OpenInstanceStart(root, name, bufferKB, enable, filters, func(ev Event, _ int64) { feed(ev) })
}

// OpenInstanceStart is OpenInstance with feed also given the trace-clock time
// of the start marker (0 = not seen yet), so a collector can tell events
// before the start from events after it while streaming: StartNs cannot be
// called from inside feed (the mutex is held).
func OpenInstanceStart(root, name string, bufferKB int, enable []string, filters map[string]string, feed func(ev Event, startNs int64)) (*Instance, error) {
	RemoveStale(root)
	dir := filepath.Join(root, "instances", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create trace instance: %w", err)
	}
	i := &Instance{dir: dir, feed: feed, done: make(chan struct{}), stopSeen: make(chan struct{})}
	fail := func(err error) (*Instance, error) {
		if cerr := i.cleanup(); cerr != nil {
			err = fmt.Errorf("%w (cleanup: %v)", err, cerr)
		}
		return nil, err
	}

	i.clock = i.setClock()
	if bufferKB > 0 {
		_ = i.write("buffer_size_kb", strconv.Itoa(bufferKB))
	}
	if err := i.openRaw(root, enable); err != nil {
		i.readerNote = err.Error()
		pipe, err := os.Open(filepath.Join(dir, "trace_pipe"))
		if err != nil {
			return fail(fmt.Errorf("open trace_pipe: %w", err))
		}
		i.pipe = pipe
		go i.read()
	}

	for _, e := range enable {
		if f, ok := filters[e]; ok {
			_ = i.write(e+"/filter", f)
		}
		if err := i.write(e+"/enable", "1"); err != nil {
			i.disable()
			return fail(fmt.Errorf("enable %s: %w", e, err))
		}
		i.events = append(i.events, e)
	}
	if err := i.write("tracing_on", "1"); err != nil {
		i.disable()
		return fail(fmt.Errorf("start tracing: %w", err))
	}
	// Time zero of the recorded samples.
	_ = i.write("trace_marker", startMarker)
	return i, nil
}

// openRaw starts the raw reader, or says why it cannot be used.
func (i *Instance) openRaw(root string, enable []string) error {
	if i.clock != "" && !nsClocks[i.clock] {
		return fmt.Errorf("trace clock %s is not in ns", i.clock)
	}
	r, err := openRawReader(root, i.dir, enable, &i.overrun, i.deliver)
	if err != nil {
		return err
	}
	for _, c := range r.readers {
		if !c.pollable {
			if st, err := c.f.Stat(); err != nil || !st.Mode().IsRegular() {
				r.closeFiles()
				return fmt.Errorf("%s cannot be polled", c.path)
			}
		}
	}
	// Poll reports readable as soon as the buffer holds anything (the
	// default 50 % would leave an idle CPU's events in the kernel while
	// the merger takes the CPU for empty).
	if _, err := os.Stat(filepath.Join(i.dir, "buffer_percent")); err == nil {
		_ = i.write("buffer_percent", "0")
	}
	i.raw = r
	r.start()
	go func() { <-r.done; close(i.done) }()
	return nil
}

// deliver runs emit, which passes a run of the raw reader's events in time
// order to handle, with the mutex held: one lock per run.
func (i *Instance) deliver(emit func(handle func(*Event))) {
	i.mu.Lock()
	defer i.mu.Unlock()
	emit(func(ev *Event) { i.handle(*ev) })
}

// handle consumes the start and stop markers and feeds every other event;
// the mutex is held.
func (i *Instance) handle(ev Event) {
	switch {
	case ev.Name == "tracing_mark_write" && strings.Contains(ev.Payload, startMarker):
		i.startNs = ev.TimeNs // MarkStart moves time zero
	case ev.Name == "tracing_mark_write" && strings.Contains(ev.Payload, stopMarker):
		select {
		case <-i.stopSeen:
		default:
			close(i.stopSeen)
		}
	default:
		i.feed(ev, i.startNs)
	}
}

func (i *Instance) write(rel, val string) error {
	return os.WriteFile(filepath.Join(i.dir, rel), []byte(val), 0o644)
}

func (i *Instance) disable() {
	for _, e := range i.events {
		_ = i.write(e+"/enable", "0")
	}
}

func (i *Instance) read() {
	defer close(i.done)
	r := bufio.NewReaderSize(i.pipe, 256*1024)
	for {
		line, err := r.ReadString('\n')
		if ev, ok := ParseLine(line); ok {
			i.mu.Lock()
			i.handle(ev)
			i.mu.Unlock()
		} else if n := lostEvents(line); n > 0 {
			i.overrun.Add(n)
		}
		if err != nil {
			return // io.EOF once tracing is off and the buffer is drained
		}
	}
}

// lostEvents returns N of a "CPU:2 [LOST N EVENTS]" line, else 0.
func lostEvents(line string) int64 {
	k := strings.Index(line, "[LOST ")
	if k < 0 {
		return 0
	}
	rest := line[k+len("[LOST "):]
	e := 0
	for e < len(rest) && isDigit(rest[e]) {
		e++
	}
	if e == 0 || !strings.HasPrefix(rest[e:], " EVENTS]") {
		return 0
	}
	n, _ := strconv.ParseInt(rest[:e], 10, 64)
	return n
}

// MarkStart writes a new start marker: time zero becomes now (the last
// marker read wins). An instance opened before a start gate calls it once
// the gate opens, so the samples line up with fio's clock.
func (i *Instance) MarkStart() {
	_ = i.write("trace_marker", startMarker)
}

// Stop disables the events, wakes the reader with a stop marker and waits for
// it (at most 3 s), turns tracing off and removes the instance. The error
// describes a cleanup problem (reader not finishing, instance not removable).
// Stop and Close are idempotent: a later call waits for the first one and
// returns its error.
func (i *Instance) Stop() error { return i.stopWithin(3 * time.Second) }

// Close is Stop with a short wait for the reader, for a force quit: the
// instance must not outlive the process with its events enabled.
func (i *Instance) Close() error { return i.stopWithin(500 * time.Millisecond) }

func (i *Instance) stopWithin(wait time.Duration) error {
	i.stopOnce.Do(func() { i.stopErr = i.stop(wait) })
	return i.stopErr
}

func (i *Instance) stop(wait time.Duration) error {
	if i.raw != nil {
		return i.stopRaw(wait)
	}
	i.disable()
	// Wake the reader with a marker while tracing is still on, and wait until
	// it has read everything up to it. (On a real kernel trace_pipe does not
	// report EOF by itself: a read blocks until new data arrives.)
	_ = i.write("trace_marker", stopMarker)
	var errs []string
	select {
	case <-i.stopSeen:
	case <-i.done: // pipe ended by itself (e.g. a plain file)
	case <-time.After(wait):
		errs = append(errs, "trace reader did not finish")
	}
	_ = i.write("tracing_on", "0")
	if err := i.cleanup(); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// stopRaw ends a raw reader: no more events (disabled, tracing off), then
// every CPU's pipe is drained until empty, which proves that every stored
// event was delivered, and the merger emits what is left. A reader that does
// not finish within wait is woken by closing its file.
func (i *Instance) stopRaw(wait time.Duration) error {
	i.disable()
	_ = i.write("trace_marker", stopMarker)
	_ = i.write("tracing_on", "0")
	i.raw.drain()
	var errs []string
	select {
	case <-i.done:
	case <-time.After(wait):
		errs = append(errs, "trace reader did not finish")
		i.raw.closeFiles()
		select {
		case <-i.done:
		case <-time.After(time.Second):
		}
	}
	if err := i.cleanup(); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Reader is ReaderRaw or ReaderText.
func (i *Instance) Reader() string {
	if i.raw != nil {
		return ReaderRaw
	}
	return ReaderText
}

// ReaderNote says why the raw reader is not used ("" when it is), and,
// after Stop, how many raw records could not be decoded.
func (i *Instance) ReaderNote() string {
	if i.raw == nil {
		return i.readerNote
	}
	var parts []string
	if n := i.raw.bad.Load(); n > 0 {
		parts = append(parts, strconv.FormatInt(n, 10)+" raw records could not be decoded")
	}
	if n := i.raw.unknown.Load(); n > 0 {
		parts = append(parts, strconv.FormatInt(n, 10)+" raw events of an unknown id")
	}
	return strings.Join(parts, "; ")
}

// Late is the number of events the raw reader's merger received after a
// newer one had been fed (always 0 for the text reader): they were fed out
// of order, and a pair they broke counts as unpaired.
func (i *Instance) Late() int64 {
	if i.raw == nil {
		return 0
	}
	return i.raw.late.Load()
}

// Clock is the instance's trace clock ("" when unknown).
func (i *Instance) Clock() string { return i.clock }

// StartNs is the trace-clock time of the start marker (0 = not seen).
func (i *Instance) StartNs() int64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.startNs
}

// Overrun is the number of events the kernel reported as lost.
func (i *Instance) Overrun() int64 { return i.overrun.Load() }

// Locked runs f with the reader's mutex held, so f may read what feed fills.
// The mutex is not reentrant: StartNs, Overrun and Locked must not be called
// from inside feed or from inside f.
func (i *Instance) Locked(f func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	f()
}

func (i *Instance) cleanup() error {
	if i.pipe != nil {
		_ = i.pipe.Close()
	}
	if i.raw != nil {
		i.raw.closeFiles()
	}
	if err := os.Remove(i.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove trace instance %s: %w", i.dir, err)
	}
	return nil
}
