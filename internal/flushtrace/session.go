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
)

// ErrUnsupported means this host cannot trace Flushes for the device
// (no tracefs, no matching tracepoints, or unknown device).
var ErrUnsupported = errors.New("flush tracing unsupported")

// Options override host paths; the zero value uses the real system.
type Options struct {
	TracefsRoot  string // default: autodetect /sys/kernel/tracing, /sys/kernel/debug/tracing
	SysBlock     string // default /sys/block
	InstanceName string // default bbbench-flush-<pid>-<n>
}

var instanceSeq atomic.Int64

// Session is one running trace of one device's Flushes. It uses a private
// ftrace instance, so the system-wide trace buffer and settings are left
// alone, and parallel sessions do not interfere.
type Session struct {
	inst      string
	pipe      *os.File
	events    []string // enable files switched on, to switch off again
	collector Collector
	mu        sync.Mutex
	done      chan struct{}
}

func findTracefs(opts Options) (string, error) {
	cands := []string{opts.TracefsRoot}
	if opts.TracefsRoot == "" {
		cands = []string{"/sys/kernel/tracing", "/sys/kernel/debug/tracing"}
	}
	for _, c := range cands {
		if st, err := os.Stat(filepath.Join(c, "instances")); err == nil && st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: tracefs not found (is it mounted, and are you root?)", ErrUnsupported)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Start begins tracing Flushes of the block device `name` (e.g. "nvme0n1",
// "sdb"). NVMe namespaces use the NVMe tracepoints when the kernel has them;
// anything else uses the generic block-layer tracepoints.
func Start(name string, opts Options) (*Session, error) {
	root, err := findTracefs(opts)
	if err != nil {
		return nil, err
	}
	var (
		col     Collector
		enable  []string
		filters = map[string]string{}
	)
	nvmeEv := filepath.Join(root, "events/nvme")
	blockEv := filepath.Join(root, "events/block")
	switch {
	case strings.HasPrefix(name, "nvme") && exists(filepath.Join(nvmeEv, "nvme_setup_cmd")) &&
		exists(filepath.Join(nvmeEv, "nvme_complete_rq")):
		col = NewNVMe(name)
		enable = []string{"events/nvme/nvme_setup_cmd", "events/nvme/nvme_complete_rq"}
		// Only I/O-queue Flush commands; completions cannot be filtered by opcode.
		filters["events/nvme/nvme_setup_cmd"] = "opcode == 0 && qid > 0"
	case exists(filepath.Join(blockEv, "block_rq_issue")) && exists(filepath.Join(blockEv, "block_rq_complete")):
		sysBlock := opts.SysBlock
		if sysBlock == "" {
			sysBlock = "/sys/block"
		}
		maj, min, err := readDevNumbers(filepath.Join(sysBlock, name, "dev"))
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrUnsupported, name, err)
		}
		col = NewBlock(maj, min)
		enable = []string{"events/block/block_rq_issue", "events/block/block_rq_complete"}
		for _, e := range enable {
			filters[e] = `rwbs ~ "F*" && nr_sector == 0`
		}
	default:
		return nil, fmt.Errorf("%w: no nvme or block tracepoints for %s", ErrUnsupported, name)
	}

	instName := opts.InstanceName
	if instName == "" {
		instName = fmt.Sprintf("bbbench-flush-%d-%d", os.Getpid(), instanceSeq.Add(1))
	}
	inst := filepath.Join(root, "instances", instName)
	if err := os.MkdirAll(inst, 0o755); err != nil {
		return nil, fmt.Errorf("create trace instance: %w", err)
	}
	s := &Session{inst: inst, collector: col, done: make(chan struct{})}
	fail := func(err error) (*Session, error) { s.cleanup(); return nil, err }

	// A generous buffer so a busy device does not overwrite events.
	_ = s.write("buffer_size_kb", "8192")
	pipe, err := os.Open(filepath.Join(inst, "trace_pipe"))
	if err != nil {
		return fail(fmt.Errorf("open trace_pipe: %w", err))
	}
	s.pipe = pipe
	go s.read()

	for _, e := range enable {
		if f, ok := filters[e]; ok {
			_ = s.write(e+"/filter", f) // best effort: events are also filtered in userspace
		}
		if err := s.write(e+"/enable", "1"); err != nil {
			return fail(fmt.Errorf("enable %s: %w", e, err))
		}
		s.events = append(s.events, e)
	}
	if err := s.write("tracing_on", "1"); err != nil {
		return fail(fmt.Errorf("start tracing: %w", err))
	}
	// Guarantees trace_pipe has data, so that it reports EOF (instead of
	// blocking forever) once tracing is switched off in Stop.
	_ = s.write("trace_marker", "bbbench-start")
	return s, nil
}

func (s *Session) write(rel, val string) error {
	return os.WriteFile(filepath.Join(s.inst, rel), []byte(val), 0o644)
}

func (s *Session) read() {
	defer close(s.done)
	r := bufio.NewReaderSize(s.pipe, 256*1024)
	for {
		line, err := r.ReadString('\n')
		if ev, ok := ParseLine(line); ok {
			s.mu.Lock()
			s.collector.Feed(ev)
			s.mu.Unlock()
		}
		if err != nil {
			return // io.EOF once tracing is off and the buffer is drained
		}
	}
}

// Stop ends tracing and returns the measured Flush latency. The Summary is
// valid even when the returned error is non-nil; the error then describes a
// cleanup problem (reader not finishing, instance not removable).
func (s *Session) Stop() (Summary, error) {
	for _, e := range s.events {
		_ = s.write(e+"/enable", "0")
	}
	_ = s.write("tracing_on", "0")
	var errs []string
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		errs = append(errs, "trace reader did not finish")
	}
	s.mu.Lock()
	sum := s.collector.Summary()
	s.mu.Unlock()
	if err := s.cleanup(); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return sum, errors.New(strings.Join(errs, "; "))
	}
	return sum, nil
}

func (s *Session) cleanup() error {
	if s.pipe != nil {
		_ = s.pipe.Close()
	}
	if err := os.Remove(s.inst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove trace instance %s: %w", s.inst, err)
	}
	return nil
}

func readDevNumbers(path string) (maj, min int, err error) {
	b, err := os.ReadFile(path)
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
