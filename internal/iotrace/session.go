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
// dropping the absorbed request (the newest queued, not yet issued entry of
// the same size and direction; a request merged at insert time has none and
// its merge is ignored). NVMe native multipath path devices (nvme0c1n1) count
// for their head (nvme0n1); the controllers come from sysfs.
//
// Unpaired events: for block devices, completions with no pending request
// plus requests still pending at Stop; for NVMe, commands still pending at
// Stop plus a pending command whose cmdid is reused by a new setup (its
// completion was lost). An NVMe completion without a pending command is not
// counted: completions cannot be filtered by opcode, so those of Flushes,
// discards and admin commands, whose setups the kernel filter drops, look the
// same.
//
// Known limitations, all of the block path:
//   - requeues (block_rq_requeue) are not handled: a requeued request is
//     issued twice and its first issue time is kept;
//   - partial completions (a request completed in several block_rq_complete
//     events with an advancing sector) leave the later parts unpaired;
//   - a bio front-merged into an already inserted request moves its start
//     sector: the insert entry ends up unpaired and the issue starts a new
//     entry with insertNs = 0, so that request's block-layer latency is
//     understated (counted from its issue).
//
// It runs in a private tracefs instance (flushtrace.Instance): nothing on a
// drive or in the global trace settings changes.
package iotrace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	Name       string   `json:"name"`
	Source     string   `json:"source,omitempty"` // "nvme" | "block"; "" = not traced
	Events     []string `json:"events,omitempty"` // e.g. "nvme_setup_cmd -> nvme_complete_rq"
	Pair       string   `json:"pair,omitempty"`   // "ctrl,qid,cmdid" | "dev,sector"
	Exact      bool     `json:"exact"`
	BlockLayer bool     `json:"block_layer"`
	// Unpaired is the number of events that found no partner: see the
	// package doc (block: completions without a request and requests pending
	// at Stop; NVMe: commands pending at Stop or whose cmdid was reused).
	Unpaired int64                  `json:"unpaired,omitempty"`
	Stats    map[Kind]map[Dir]Stats `json:"stats,omitempty"`
	Note     string                 `json:"note,omitempty"` // why not traced
}

// Summary is the I/O latency of a phase: the target first, then its members.
type Summary struct {
	AvgMsec  int   `json:"avg_msec,omitempty"`
	OffsetMs int   `json:"offset_ms,omitempty"`
	Overrun  int64 `json:"overrun,omitempty"` // events the kernel dropped
	// StartMissing: the start marker was never read, so no completion was
	// counted (the events cannot be placed on the phase's clock).
	StartMissing bool            `json:"start_missing,omitempty"`
	Devices      []DeviceSummary `json:"devices"`
	Lost         int64           `json:"lost"` // Overrun + every Unpaired
	Text         string          `json:"text"` // SourceLine
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
	var devFilter, ctrls []string
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
		if d.source == SourceNVMe {
			d.ctrls = nvmeControllers(opts.SysBlock, name)
			for _, c := range d.ctrls {
				if !slices.Contains(ctrls, c) {
					ctrls = append(ctrls, c)
				}
			}
		}
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
		// The ctrl_id field is taken from the kernel's nvme trace header and
		// not verified on a real kernel. Should the kernel reject it, the
		// whole filter of that event is not applied (filters are best effort)
		// and the collector filters the same way in userspace (opcode, qid,
		// disk or controller), at a higher event rate.
		setup, done := "(opcode == 1 || opcode == 2) && qid > 0", "qid > 0"
		if len(ctrls) > 0 {
			ids := make([]string, len(ctrls))
			for i, c := range ctrls {
				ids[i] = "ctrl_id == " + strings.TrimPrefix(c, "nvme")
			}
			f := " && (" + strings.Join(ids, " || ") + ")"
			setup, done = setup+f, done+f
		}
		filters["events/"+evNVMeSetup] = setup
		filters["events/"+evNVMeDone] = done
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
	res.Summary.StartMissing = s.inst.StartNs() == 0
	res.Summary.Fill()
	return res, err
}

// nvmeControllers returns the controllers ("nvme1") of an NVMe namespace,
// from sysfs: <sysBlock>/<name>/device links to the controller, or, for a
// native multipath head, to the subsystem, and then the path devices under
// <sysBlock>/<name>/multipath/ (nvme<subsys>c<ctrl>n<ns>) name them. Empty
// when sysfs does not say. The multipath layout is taken from kernel sources
// and not verified on a real multipath system.
func nvmeControllers(sysBlock, name string) []string {
	if sysBlock == "" {
		sysBlock = "/sys/block"
	}
	if link, err := os.Readlink(filepath.Join(sysBlock, name, "device")); err == nil {
		if base := filepath.Base(link); isController(base) {
			return []string{base}
		}
	}
	paths, _ := os.ReadDir(filepath.Join(sysBlock, name, "multipath"))
	var out []string
	for _, p := range paths {
		n := p.Name()
		if head := headName(n); head == n || !strings.HasPrefix(n, "nvme") {
			continue
		}
		// nvme<s>c<c>n<n>: the controller is the number after 'c'.
		i := skipDigits(n, len("nvme"))
		j := skipDigits(n, i+1)
		out = append(out, "nvme"+n[i+1:j])
	}
	return out
}

// isController reports whether name is "nvme<N>".
func isController(name string) bool {
	return strings.HasPrefix(name, "nvme") && len(name) > len("nvme") && skipDigits(name, len("nvme")) == len(name)
}
