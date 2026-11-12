package iotrace

import (
	"strings"

	"bbbench/internal/flushtrace"
)

// Sources of the device latency of a traced device.
const (
	SourceNVMe  = "nvme"  // nvme_setup_cmd -> nvme_complete_rq
	SourceBlock = "block" // block_rq_issue -> block_rq_complete
)

// blockRq is the part of a block_rq_* payload the collectors use.
type blockRq struct {
	Maj, Min int
	RWBS     string
	Sector   uint64
	Nr       uint32
}

// parseBlockRq parses a block_rq_insert/issue/merge payload (withBytes) or a
// block_rq_complete payload:
//
//	8,16 R 4096 () 1000 + 8 none,0,0 [fio]     (insert, issue, merge)
//	8,16 R () 1000 + 8 [0]                     (complete)
//
// The ioprio field of newer kernels and anything after nr_sector are
// ignored; the (cmd) may contain spaces, so sector and nr are read after the
// first ") " that follows it.
func parseBlockRq(p string, withBytes bool) (blockRq, bool) {
	var r blockRq
	var ok bool
	if r.Maj, p, ok = atoi(p); !ok || p == "" || p[0] != ',' {
		return r, false
	}
	if r.Min, p, ok = atoi(p[1:]); !ok || p == "" || p[0] != ' ' {
		return r, false
	}
	p = p[1:]
	sp := strings.IndexByte(p, ' ')
	if sp <= 0 {
		return r, false
	}
	r.RWBS, p = p[:sp], p[sp+1:]
	if withBytes {
		if _, p, ok = atoi(p); !ok || p == "" || p[0] != ' ' {
			return r, false
		}
		p = p[1:]
	}
	if p == "" || p[0] != '(' {
		return r, false
	}
	k := strings.Index(p, ") ")
	if k < 0 {
		return r, false
	}
	p = p[k+2:]
	var sector uint64
	if sector, p, ok = atou(p); !ok || !strings.HasPrefix(p, " + ") {
		return r, false
	}
	nr, _, ok := atou(p[3:])
	if !ok || nr > 1<<32-1 {
		return r, false
	}
	r.Sector, r.Nr = sector, uint32(nr)
	return r, true
}

// parseNVMeSetup parses an nvme_setup_cmd payload:
//
//	nvme0: disk=nvme0n1, qid=1, cmdid=40, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_read slba=0, ...)
//
// disk= is absent for admin commands (and on older kernels); op is the
// command name after "cmd=(".
func parseNVMeSetup(p string) (ctrl, disk string, qid, cmdid int, op string, ok bool) {
	ctrl, disk, qid, cmdid, p, ok = parseNVMeHead(p)
	if !ok {
		return
	}
	k := strings.Index(p, "cmd=(")
	if k < 0 {
		return "", "", 0, 0, "", false
	}
	p = p[k+len("cmd=("):]
	e := 0
	for e < len(p) && p[e] != ' ' && p[e] != ')' {
		e++
	}
	if e == 0 {
		return "", "", 0, 0, "", false
	}
	return ctrl, disk, qid, cmdid, p[:e], true
}

// parseNVMeDone parses an nvme_complete_rq payload:
//
//	nvme0: disk=nvme0n1, qid=2, cmdid=40, res=0x0, retries=0, flags=0x0, status=0x0
func parseNVMeDone(p string) (ctrl string, qid, cmdid int, ok bool) {
	ctrl, _, qid, cmdid, _, ok = parseNVMeHead(p)
	return
}

// parseNVMeHead reads "nvmeN: [disk=X, ]qid=Q, cmdid=C" and returns the rest.
func parseNVMeHead(p string) (ctrl, disk string, qid, cmdid int, rest string, ok bool) {
	c := strings.Index(p, ": ")
	if c <= 0 {
		return
	}
	ctrl, p = p[:c], p[c+2:]
	if strings.HasPrefix(p, "disk=") {
		e := strings.Index(p, ", ")
		if e < 0 {
			return "", "", 0, 0, "", false
		}
		disk, p = p[len("disk="):e], p[e+2:]
	}
	if !strings.HasPrefix(p, "qid=") {
		return "", "", 0, 0, "", false
	}
	if qid, p, ok = atoi(p[len("qid="):]); !ok || !strings.HasPrefix(p, ", cmdid=") {
		return "", "", 0, 0, "", false
	}
	if cmdid, p, ok = atoi(p[len(", cmdid="):]); !ok {
		return "", "", 0, 0, "", false
	}
	return ctrl, disk, qid, cmdid, p, true
}

// dirOf is the direction of a request from its rwbs: false for requests
// without data (a Flush), discards (D), secure erases (E) and other
// operations (N, e.g. write zeroes); w when it writes, r when it reads.
func dirOf(rwbs string, nr uint32) (Dir, bool) {
	if nr == 0 || rwbs == "" {
		return "", false
	}
	switch rwbs[0] {
	case 'D', 'E', 'N':
		return "", false
	}
	if strings.IndexByte(rwbs, 'W') >= 0 {
		return DirWrite, true
	}
	if strings.IndexByte(rwbs, 'R') >= 0 {
		return DirRead, true
	}
	return "", false
}

func atoi(p string) (int, string, bool) {
	n, rest, ok := atou(p)
	if !ok || n > 1<<31-1 {
		return 0, p, false
	}
	return int(n), rest, true
}

func atou(p string) (uint64, string, bool) {
	var n uint64
	i := 0
	for i < len(p) && p[i] >= '0' && p[i] <= '9' {
		if n > (1<<64-1)/10 {
			return 0, p, false
		}
		n = n*10 + uint64(p[i]-'0')
		i++
	}
	if i == 0 {
		return 0, p, false
	}
	return n, p[i:], true
}

// ---- pairing ----------------------------------------------------------------

// device is one traced block device.
type device struct {
	name       string
	source     string // SourceNVMe or SourceBlock
	maj, min   int
	blockLayer bool // block_rq_insert is traced: block-layer latency
	unpaired   int64
}

// usesBlock reports whether the device's block_rq_* events are paired.
func (d *device) usesBlock() bool { return d.source == SourceBlock || d.blockLayer }

type devNum struct{ maj, min int }

type blockKey struct {
	maj, min int
	sector   uint64
}

// blockEntry is a request pending at (dev, sector): inserted and/or issued.
// insertNs 0 = issued directly, never queued; issueNs 0 = not issued yet.
type blockEntry struct {
	insertNs, issueNs int64
	dir               Dir
}

type nvmeKey struct {
	ctrl       string
	qid, cmdid int
}

type nvmeEntry struct {
	dev     *device
	issueNs int64
	dir     Dir
}

// collector pairs the events of the traced devices and aggregates the
// latencies. It is not safe for concurrent use: the Instance calls feed with
// its mutex held, and Stop reads it through Locked.
type collector struct {
	agg      *aggregator
	devs     []*device
	byNum    map[devNum]*device
	byDisk   map[string]*device // nvme namespaces by name
	byCtrl   map[string]*device // nvme controller -> its only traced namespace (nil = ambiguous)
	block    map[blockKey][]blockEntry
	nvme     map[nvmeKey]nvmeEntry
	finished bool
}

func newCollector(windowMs, offsetMs int) *collector {
	return &collector{
		agg:    newAggregator(windowMs, offsetMs),
		byNum:  map[devNum]*device{},
		byDisk: map[string]*device{},
		byCtrl: map[string]*device{},
		block:  map[blockKey][]blockEntry{},
		nvme:   map[nvmeKey]nvmeEntry{},
	}
}

func (c *collector) addDevice(d *device) {
	c.devs = append(c.devs, d)
	if d.usesBlock() {
		c.byNum[devNum{d.maj, d.min}] = d
	}
	if d.source == SourceNVMe {
		c.byDisk[d.name] = d
		// Without a disk= field the namespace is known only when its
		// controller has a single traced namespace ("nvme0" for "nvme0n1").
		if k := strings.LastIndexByte(d.name, 'n'); k > 0 {
			ctrl := d.name[:k]
			if _, seen := c.byCtrl[ctrl]; seen {
				c.byCtrl[ctrl] = nil
			} else {
				c.byCtrl[ctrl] = d
			}
		}
	}
}

// feed takes one event; startNs is the trace-clock time of the start marker
// (0 = not seen yet). Completions before it are not counted.
func (c *collector) feed(ev flushtrace.Event, startNs int64) {
	switch ev.Name {
	case "block_rq_insert", "block_rq_issue", "block_rq_merge":
		c.blockQueued(ev)
	case "block_rq_complete":
		c.blockComplete(ev, startNs)
	case "nvme_setup_cmd":
		c.nvmeSetup(ev)
	case "nvme_complete_rq":
		c.nvmeComplete(ev, startNs)
	}
}

func (c *collector) blockDev(r blockRq) *device { return c.byNum[devNum{r.Maj, r.Min}] }

func (c *collector) blockQueued(ev flushtrace.Event) {
	r, ok := parseBlockRq(ev.Payload, true)
	if !ok {
		return
	}
	d := c.blockDev(r)
	if d == nil {
		return
	}
	dir, ok := dirOf(r.RWBS, r.Nr)
	if !ok {
		return
	}
	key := blockKey{r.Maj, r.Min, r.Sector}
	q := c.block[key]
	switch ev.Name {
	case "block_rq_insert":
		if !d.blockLayer {
			return
		}
		c.block[key] = append(q, blockEntry{insertNs: ev.TimeNs, dir: dir})
	case "block_rq_issue":
		for i := range q {
			if q[i].issueNs == 0 {
				q[i].issueNs = ev.TimeNs
				return
			}
		}
		c.block[key] = append(q, blockEntry{issueNs: ev.TimeNs, dir: dir})
	case "block_rq_merge":
		// The absorbed request: it is never issued nor completed on its own.
		if !d.blockLayer || len(q) == 0 {
			return
		}
		if len(q) == 1 {
			delete(c.block, key)
		} else {
			c.block[key] = q[:len(q)-1]
		}
	}
}

func (c *collector) blockComplete(ev flushtrace.Event, startNs int64) {
	r, ok := parseBlockRq(ev.Payload, false)
	if !ok {
		return
	}
	d := c.blockDev(r)
	if d == nil {
		return
	}
	dir, ok := dirOf(r.RWBS, r.Nr)
	if !ok {
		return
	}
	key := blockKey{r.Maj, r.Min, r.Sector}
	q := c.block[key]
	if len(q) == 0 {
		if startNs != 0 && ev.TimeNs >= startNs {
			d.unpaired++
		}
		return
	}
	e := q[0]
	if len(q) == 1 {
		delete(c.block, key)
	} else {
		c.block[key] = q[1:]
	}
	if startNs == 0 || ev.TimeNs < startNs {
		return
	}
	rel := ev.TimeNs - startNs
	if d.source == SourceBlock && e.issueNs != 0 {
		c.agg.add(d.name, KindDevice, dir, rel, ev.TimeNs-e.issueNs)
	}
	if d.blockLayer {
		from := e.insertNs
		if from == 0 {
			from = e.issueNs
		}
		if from != 0 {
			c.agg.add(d.name, KindBlockLayer, dir, rel, ev.TimeNs-from)
		}
	}
}

func (c *collector) nvmeSetup(ev flushtrace.Event) {
	ctrl, disk, qid, cmdid, op, ok := parseNVMeSetup(ev.Payload)
	if !ok || qid == 0 {
		return
	}
	var dir Dir
	switch op {
	case "nvme_cmd_read":
		dir = DirRead
	case "nvme_cmd_write":
		dir = DirWrite
	default:
		return
	}
	d := c.byDisk[disk]
	if disk == "" {
		d = c.byCtrl[ctrl]
	}
	if d == nil {
		return
	}
	key := nvmeKey{ctrl, qid, cmdid}
	if old, ok := c.nvme[key]; ok { // its completion was never seen
		old.dev.unpaired++
	}
	c.nvme[key] = nvmeEntry{dev: d, issueNs: ev.TimeNs, dir: dir}
}

// nvmeComplete pairs a completion with its command. A completion without a
// pending command is not counted as unpaired: completions cannot be filtered
// by opcode, so those of Flushes, discards and admin commands (whose setup
// the kernel filter drops) look the same.
func (c *collector) nvmeComplete(ev flushtrace.Event, startNs int64) {
	ctrl, qid, cmdid, ok := parseNVMeDone(ev.Payload)
	if !ok || qid == 0 {
		return
	}
	key := nvmeKey{ctrl, qid, cmdid}
	e, ok := c.nvme[key]
	if !ok {
		return
	}
	delete(c.nvme, key)
	if startNs == 0 || ev.TimeNs < startNs {
		return
	}
	c.agg.add(e.dev.name, KindDevice, e.dir, ev.TimeNs-startNs, ev.TimeNs-e.issueNs)
}

// finish counts the requests still pending as unpaired and returns the
// windows and the phase statistics. It is called once.
func (c *collector) finish() ([]Window, map[string]map[Kind]map[Dir]Stats) {
	if !c.finished {
		c.finished = true
		for key, q := range c.block {
			if d := c.byNum[devNum{key.maj, key.min}]; d != nil {
				d.unpaired += int64(len(q))
			}
		}
		for _, e := range c.nvme {
			e.dev.unpaired++
		}
		c.block, c.nvme = map[blockKey][]blockEntry{}, map[nvmeKey]nvmeEntry{}
	}
	return c.agg.finish()
}
