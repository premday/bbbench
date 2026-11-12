package flushtrace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Session is one running trace of one device's Flushes. It uses a private
// ftrace instance, so the system-wide trace buffer and settings are left
// alone, and parallel sessions do not interfere.
type Session struct {
	inst       *Instance
	collector  Collector
	writeCache string // queue/write_cache of the device at Start
}

// Start begins tracing Flushes of the block device `name` (e.g. "nvme0n1",
// "sdb"). NVMe namespaces use the NVMe tracepoints when the kernel has them;
// anything else uses the generic block-layer tracepoints.
func Start(name string, opts Options) (*Session, error) {
	root, err := FindTracefs(opts)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	var (
		col     Collector
		enable  []string
		filters = map[string]string{}
	)
	switch {
	case strings.HasPrefix(name, "nvme") && HasEvents(root, "nvme/nvme_setup_cmd", "nvme/nvme_complete_rq"):
		col = NewNVMe(name)
		enable = []string{"events/nvme/nvme_setup_cmd", "events/nvme/nvme_complete_rq"}
		// Only I/O-queue Flush commands; completions cannot be filtered by opcode.
		filters["events/nvme/nvme_setup_cmd"] = "opcode == 0 && qid > 0"
	case HasEvents(root, "block/block_rq_issue", "block/block_rq_complete"):
		maj, min, err := DevNumbers(opts.SysBlock, name)
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
		instName = InstanceName("flush")
	}
	// A generous buffer so a busy device does not overwrite events.
	inst, err := OpenInstance(root, instName, 8192, enable, filters, col.Feed)
	if err != nil {
		return nil, err
	}
	return &Session{writeCache: WriteCache(opts.SysBlock, name), inst: inst, collector: col}, nil
}

// WriteCache returns the kernel's cache mode of a block device, "write back"
// or "write through" (from <sysBlock>/<name>/queue/write_cache), or "" when
// unknown. Only a write-back device is sent Flush commands. This function only reads;
// changing a drive's cache setting is the job of a setting test (internal/settings),
// never of Flush tracing. sysBlock defaults to /sys/block.
func WriteCache(sysBlock, name string) string {
	if sysBlock == "" {
		sysBlock = "/sys/block"
	}
	b, err := os.ReadFile(filepath.Join(sysBlock, name, "queue", "write_cache"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Stop ends tracing and returns the measured Flush latency. The Summary is
// valid even when the returned error is non-nil; the error then describes a
// cleanup problem (reader not finishing, instance not removable).
func (s *Session) Stop() (Summary, error) {
	err := s.inst.Stop()
	var sum Summary
	s.inst.Locked(func() { sum = s.collector.Summary() })
	sum.WriteCache = s.writeCache
	return sum, err
}

// MarkStart moves time zero to now (see Instance.MarkStart).
func (s *Session) MarkStart() { s.inst.MarkStart() }

// Close ends the trace quickly without a result, for a force quit; a later
// Stop still returns what was collected.
func (s *Session) Close() error { return s.inst.Close() }

// Samples returns every completed Flush with its completion time relative to
// the start of tracing (so comparable with fio's own time-series logs). Call
// it after Stop.
func (s *Session) Samples() []Sample {
	var raw []Sample
	s.inst.Locked(func() { raw = s.collector.Samples() })
	out := make([]Sample, len(raw))
	t0 := s.inst.StartNs()
	if t0 == 0 && len(raw) > 0 { // marker not seen: start at the first Flush
		t0 = raw[0].TimeNs - raw[0].LatencyNs
	}
	for i, x := range raw {
		out[i] = Sample{TimeNs: x.TimeNs - t0, LatencyNs: x.LatencyNs}
	}
	return out
}
