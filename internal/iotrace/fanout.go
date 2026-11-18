package iotrace

import (
	"strings"
	"sync"

	"bbbench/internal/flushtrace"
)

const (
	fanBatch = 1024 // events per message to a shard
	fanQueue = 64   // messages a shard may have queued before the feed waits
)

// fanout spreads the events of a session over one collector per device, each
// on its own goroutine: the Instance feeds from one goroutine, and pairing and
// aggregating there caps the event rate. A device's events never meet another
// device's pending requests, except NVMe namespaces of one controller, whose
// completions carry only (controller, queue, cmdid): those share a shard.
//
// feed runs with the Instance mutex held, so the routing state needs no lock;
// stop and finish run after the feeding ended (stop under Locked).
type fanout struct {
	shards  []*shard
	byNum   map[devNum]*shard // block and bio events by device number
	byDisk  map[string]*shard // nvme setup by namespace
	byCtrl  map[string]*shard // nvme setup/completion by controller
	startNs int64             // last start marker passed on
	stopped bool
	wg      sync.WaitGroup
}

type shard struct {
	col     *collector
	in      chan fanMsg
	pending []flushtrace.Event
}

// fanMsg is a batch of events and the start marker time they were fed with; an
// empty batch only carries a new start marker.
type fanMsg struct {
	evs     []flushtrace.Event
	startNs int64
}

// newFanout builds the shards of the devices and starts their goroutines.
// devs is not empty.
func newFanout(devs []*device, windowMs, offsetMs int) *fanout {
	// Group devices that must share a collector: NVMe namespaces with a
	// controller in common, and those whose controllers are unknown.
	parent := make([]int, len(devs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) { parent[find(a)] = find(b) }
	owner, unknown := map[string]int{}, -1
	for i, d := range devs {
		if d.source != SourceNVMe {
			continue
		}
		if len(d.ctrls) == 0 {
			if unknown < 0 {
				unknown = i
			} else {
				union(i, unknown)
			}
		}
		for _, c := range d.ctrls {
			if j, ok := owner[c]; ok {
				union(i, j)
			} else {
				owner[c] = i
			}
		}
	}
	f := &fanout{byNum: map[devNum]*shard{}, byDisk: map[string]*shard{}, byCtrl: map[string]*shard{}}
	byRoot := map[int]*shard{}
	for i, d := range devs {
		r := find(i)
		sh := byRoot[r]
		if sh == nil {
			sh = &shard{col: newCollector(windowMs, offsetMs), in: make(chan fanMsg, fanQueue)}
			byRoot[r] = sh
			f.shards = append(f.shards, sh)
		}
		sh.col.addDevice(d)
		if d.usesBlock() || d.source == SourceBio {
			f.byNum[devNum{d.maj, d.min}] = sh
		}
		if d.source == SourceNVMe {
			f.byDisk[d.name] = sh
			for _, c := range d.ctrls {
				f.byCtrl[c] = sh
			}
		}
	}
	for _, sh := range f.shards {
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			for m := range sh.in {
				sh.col.setStart(m.startNs)
				for _, ev := range m.evs {
					sh.col.feed(ev, m.startNs)
				}
			}
		}()
	}
	return f
}

// route returns the shard that handles the event, nil when no traced device
// does (the collector would drop it as well).
func (f *fanout) route(ev flushtrace.Event) *shard {
	switch ev.Name {
	case "block_rq_insert", "block_rq_issue", "block_rq_merge", "block_rq_complete":
		if r, ok := blockOf(ev, ev.Name != "block_rq_complete"); ok {
			return f.byNum[devNum{r.Maj, r.Min}]
		}
	case "block_bio_queue", "block_bio_complete":
		if r, ok := bioOf(ev); ok {
			return f.byNum[devNum{r.Maj, r.Min}]
		}
	case "nvme_setup_cmd":
		ctrl, disk, qid, _, op, ok := nvmeSetupOf(ev)
		if !ok || qid == 0 || op == "" {
			return nil
		}
		var sh *shard
		if disk == "" {
			sh = f.byCtrl[ctrl]
		} else if sh = f.byDisk[headName(disk)]; sh != nil && f.byCtrl[ctrl] == nil {
			// The completion has no disk: remember where this controller's
			// commands go, should sysfs not have told.
			f.byCtrl[strings.Clone(ctrl)] = sh
		}
		return sh
	case "nvme_complete_rq":
		if ctrl, qid, _, ok := nvmeDoneOf(ev); ok && qid != 0 {
			return f.byCtrl[ctrl]
		}
	}
	return nil
}

func (f *fanout) feed(ev flushtrace.Event, startNs int64) {
	if f.stopped {
		return
	}
	if startNs != f.startNs {
		// In order with the events: what was fed before belongs to the old
		// time zero. Every shard must see the change, also one with no events.
		f.flush()
		f.startNs = startNs
		for _, sh := range f.shards {
			sh.in <- fanMsg{startNs: startNs}
		}
	}
	sh := f.route(ev)
	if sh == nil {
		return
	}
	if sh.pending == nil {
		sh.pending = make([]flushtrace.Event, 0, fanBatch)
	}
	sh.pending = append(sh.pending, ev)
	if len(sh.pending) >= fanBatch {
		f.send(sh)
	}
}

func (f *fanout) send(sh *shard) {
	sh.in <- fanMsg{evs: sh.pending, startNs: f.startNs}
	sh.pending = nil
}

func (f *fanout) flush() {
	for _, sh := range f.shards {
		if len(sh.pending) > 0 {
			f.send(sh)
		}
	}
}

// stop hands over what is pending, ends the shards and waits for them. It is
// idempotent, and later events are ignored.
func (f *fanout) stop() {
	if f.stopped {
		return
	}
	f.flush()
	f.stopped = true
	for _, sh := range f.shards {
		close(sh.in)
	}
	f.wg.Wait()
}

// finish merges the shards' results; stop must have run.
func (f *fanout) finish() ([]Window, map[string]map[Kind]map[Dir]Stats) {
	var ws []Window
	stats := map[string]map[Kind]map[Dir]Stats{}
	for _, sh := range f.shards {
		w, st := sh.col.finish()
		ws = append(ws, w...)
		for dev, v := range st {
			stats[dev] = v
		}
	}
	sortWindows(ws)
	return ws, stats
}
