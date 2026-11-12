// Package iotrace times every read and write of a phase from kernel trace
// events, at the device (NVMe nvme_setup_cmd -> nvme_complete_rq, else
// block_rq_issue -> block_rq_complete) and in the block layer
// (block_rq_insert -> block_rq_complete, which adds the time in the I/O
// scheduler and the software queues). HDDs (SATA/SAS) take the block_rq_*
// path, a first-class source. The latencies go into in-process histograms,
// one window per --log-avg-msec plus a phase summary after the ramp; nothing
// is stored per I/O.
//
// Pairing: NVMe commands by (controller, queue, cmdid), exact; block requests
// by (dev, sector), first-in first-out per sector, with block_rq_merge
// dropping the absorbed request. Requeues are not handled.
//
// It runs in a private tracefs instance (flushtrace.Instance): nothing on a
// drive or in the global trace settings changes.
package iotrace

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"bbbench/internal/flushtrace"
)

// ErrUnsupported means no device of the session can be traced (no tracefs,
// no request tracepoints, or unknown devices).
var ErrUnsupported = errors.New("I/O latency tracing unsupported")

// Options override host paths and set the windows; the zero value uses the
// real system and 1 s windows.
type Options struct {
	TracefsRoot  string // default: autodetect
	SysBlock     string // default /sys/block
	InstanceName string // default bbbench-iolat-<pid>-<n>
	AvgMsec      int    // window length (fio's --log-avg-msec); 0 = flushtrace.DefaultWindowMs
	OffsetMs     int    // fio's ramp_time: completions before it are left out of the summary
	BufferKB     int    // per-CPU ring buffer; 0 = 16384
}

// DeviceSummary is what was traced on one device and its phase statistics.
type DeviceSummary struct {
	Name       string                 `json:"name"`
	Source     string                 `json:"source,omitempty"` // "nvme" | "block"; "" = not traced
	Events     []string               `json:"events,omitempty"` // e.g. "nvme_setup_cmd -> nvme_complete_rq"
	Pair       string                 `json:"pair,omitempty"`   // "ctrl,qid,cmdid" | "dev,sector"
	Exact      bool                   `json:"exact"`
	BlockLayer bool                   `json:"block_layer"`
	Unpaired   int64                  `json:"unpaired,omitempty"`
	Stats      map[Kind]map[Dir]Stats `json:"stats,omitempty"`
	Note       string                 `json:"note,omitempty"` // why not traced
}

// Summary is the I/O latency of a phase: the target first, then its members.
type Summary struct {
	AvgMsec  int             `json:"avg_msec,omitempty"`
	OffsetMs int             `json:"offset_ms,omitempty"`
	Overrun  int64           `json:"overrun,omitempty"` // events the kernel dropped
	Devices  []DeviceSummary `json:"devices"`
	Lost     int64           `json:"lost"` // Overrun + every Unpaired
	Text     string          `json:"text"` // SourceLine
}

// Fill sets Lost and Text from the rest.
func (s *Summary) Fill() {
	s.Lost = s.Overrun
	for _, d := range s.Devices {
		s.Lost += d.Unpaired
	}
	s.Text = SourceLine(s)
}

// Result is a finished session: the summary and the windows.
type Result struct {
	Summary Summary
	Windows []Window
}

// Session is one running I/O latency trace of a set of devices.
type Session struct {
	inst    *flushtrace.Instance
	col     *collector
	devices []DeviceSummary // in the given order; Stats and Unpaired filled at Stop
	traced  map[string]*device
	summary Summary
}

const (
	evNVMeSetup  = "nvme/nvme_setup_cmd"
	evNVMeDone   = "nvme/nvme_complete_rq"
	evBlockIssue = "block/block_rq_issue"
	evBlockDone  = "block/block_rq_complete"
	evBlockIns   = "block/block_rq_insert"
	evBlockMerge = "block/block_rq_merge"
)

// Start begins tracing the reads and writes of the named block devices (the
// target first, then its members). Each device gets its own source: NVMe
// tracepoints for an NVMe namespace when the kernel has them, else the
// block_rq_* tracepoints. A device that cannot be traced is listed with a
// Note; when none can, the error wraps ErrUnsupported with the reasons.
func Start(devices []string, opts Options) (*Session, error) {
	root, err := flushtrace.FindTracefs(flushtrace.Options{TracefsRoot: opts.TracefsRoot})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	hasNVMe := flushtrace.HasEvents(root, evNVMeSetup, evNVMeDone)
	hasBlock := flushtrace.HasEvents(root, evBlockIssue, evBlockDone)
	hasInsert := hasBlock && flushtrace.HasEvents(root, evBlockIns)
	hasMerge := hasInsert && flushtrace.HasEvents(root, evBlockMerge)

	s := &Session{traced: map[string]*device{}}
	col := newCollector(opts.AvgMsec, opts.OffsetMs)
	var reasons []string
	var anyNVMe, anyBlock bool
	var devFilter []string
	for _, name := range devices {
		ds := DeviceSummary{Name: name}
		maj, min, err := flushtrace.DevNumbers(opts.SysBlock, name)
		switch {
		case err != nil:
			ds.Note = "unknown device: " + err.Error()
		case strings.HasPrefix(name, "nvme") && hasNVMe:
			ds.Source, ds.Pair = SourceNVMe, "ctrl,qid,cmdid"
			ds.Events = []string{"nvme_setup_cmd -> nvme_complete_rq"}
		case hasBlock:
			ds.Source, ds.Pair = SourceBlock, "dev,sector"
			ds.Events = []string{"block_rq_issue -> block_rq_complete"}
		default:
			ds.Note = "no nvme or block request tracepoints"
		}
		if ds.Source == "" {
			reasons = append(reasons, name+": "+ds.Note)
			s.devices = append(s.devices, ds)
			continue
		}
		ds.Exact, ds.BlockLayer = true, hasInsert
		if hasInsert {
			ds.Events = append(ds.Events, "block_rq_insert -> block_rq_complete")
		}
		if _, dup := s.traced[name]; dup {
			continue
		}
		d := &device{name: name, source: ds.Source, maj: maj, min: min, blockLayer: ds.BlockLayer}
		col.addDevice(d)
		s.traced[name] = d
		anyNVMe = anyNVMe || d.source == SourceNVMe
		if d.usesBlock() {
			anyBlock = true
			devFilter = append(devFilter, "dev == "+strconv.Itoa(maj<<20|min))
		}
		s.devices = append(s.devices, ds)
	}
	if len(s.traced) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, strings.Join(reasons, "; "))
	}

	var enable []string
	filters := map[string]string{}
	if anyNVMe {
		enable = append(enable, "events/"+evNVMeSetup, "events/"+evNVMeDone)
		// Reads and writes on I/O queues; completions cannot be filtered by opcode.
		filters["events/"+evNVMeSetup] = "(opcode == 1 || opcode == 2) && qid > 0"
	}
	if anyBlock {
		blk := []string{evBlockIssue, evBlockDone}
		if hasInsert {
			blk = append(blk, evBlockIns)
		}
		if hasMerge {
			blk = append(blk, evBlockMerge)
		}
		f := strings.Join(devFilter, " || ")
		for _, e := range blk {
			enable = append(enable, "events/"+e)
			filters["events/"+e] = f
		}
	}

	name := opts.InstanceName
	if name == "" {
		name = flushtrace.InstanceName("iolat")
	}
	buf := opts.BufferKB
	if buf == 0 {
		buf = 16384
	}
	inst, err := flushtrace.OpenInstanceStart(root, name, buf, enable, filters, col.feed)
	if err != nil {
		return nil, err
	}
	s.inst, s.col = inst, col
	s.summary = Summary{AvgMsec: int(col.agg.windowMs), OffsetMs: opts.OffsetMs}
	return s, nil
}

// Stop ends tracing and returns the result. The Result is valid even when
// the error is non-nil; the error then describes a cleanup problem.
func (s *Session) Stop() (Result, error) {
	err := s.inst.Stop()
	var res Result
	var stats map[string]map[Kind]map[Dir]Stats
	s.inst.Locked(func() {
		res.Windows, stats = s.col.finish()
		sum := s.summary
		sum.Devices = make([]DeviceSummary, len(s.devices))
		for i, ds := range s.devices {
			if d := s.traced[ds.Name]; d != nil {
				ds.Unpaired = d.unpaired
				ds.Stats = stats[ds.Name]
			}
			sum.Devices[i] = ds
		}
		res.Summary = sum
	})
	res.Summary.Overrun = s.inst.Overrun()
	res.Summary.Fill()
	return res, err
}
