package diskinfo

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Queue is the kernel's request queue and scheduling state of a block device,
// as sysfs shows it. Values are the sysfs strings; "" means not read (the
// file is not on this kernel or not readable). Only read, never written.
type Queue struct {
	// Scheduler is the active I/O scheduler (the bracketed word of
	// queue/scheduler, or its only word), Schedulers every one offered.
	Scheduler  string   `json:"scheduler,omitempty"`
	Schedulers []string `json:"schedulers,omitempty"`
	// MQ is true when the device is blk-mq (it has an mq/ directory);
	// NrHWQueues is the number of its hardware queues (mq/<n>).
	MQ           bool   `json:"mq"`
	NrHWQueues   int    `json:"nr_hw_queues,omitempty"`
	NrRequests   string `json:"nr_requests,omitempty"`
	Rotational   string `json:"rotational,omitempty"`
	ReadAheadKB  string `json:"read_ahead_kb,omitempty"`
	MaxSectorsKB string `json:"max_sectors_kb,omitempty"`
	WBTLatUsec   string `json:"wbt_lat_usec,omitempty"`
	RqAffinity   string `json:"rq_affinity,omitempty"`
	NoMerges     string `json:"nomerges,omitempty"`
	IOPoll       string `json:"io_poll,omitempty"`
	// DeviceQueueDepth is a SCSI device's queue_depth.
	DeviceQueueDepth string `json:"device_queue_depth,omitempty"`
	// NVMeQueueCount and NVMeSQSize are the NVMe controller's queue_count and
	// sqsize, as the kernel shows them.
	NVMeQueueCount string `json:"nvme_queue_count,omitempty"`
	NVMeSQSize     string `json:"nvme_sqsize,omitempty"`
	// MD is set for an md node.
	MD *MDQueue `json:"md,omitempty"`
	// DMMod is set whenever dm_mod is loaded, and for a dm node even when it
	// is not (Loaded false).
	DMMod *DMMod `json:"dm_mod,omitempty"`
}

// MDQueue is the md array's own configuration (block/<md>/md/).
type MDQueue struct {
	Level           string `json:"level,omitempty"`
	RaidDisks       string `json:"raid_disks,omitempty"`
	ChunkSize       string `json:"chunk_size,omitempty"`
	StripeCacheSize string `json:"stripe_cache_size,omitempty"`
	SyncAction      string `json:"sync_action,omitempty"`
	// Bitmap is md/bitmap/location: "none", "+8", "file", "" when unknown.
	Bitmap string `json:"bitmap,omitempty"`
}

// DMMod is three parameters of the device-mapper core module. A parameter
// that this kernel does not have (use_blk_mq is gone from newer kernels) is "".
type DMMod struct {
	Loaded       bool   `json:"loaded"`
	UseBlkMQ     string `json:"use_blk_mq,omitempty"`
	MQNrHWQueues string `json:"dm_mq_nr_hw_queues,omitempty"`
	MQQueueDepth string `json:"dm_mq_queue_depth,omitempty"`
	// Params is every readable file of /sys/module/dm_mod/parameters/.
	Params map[string]string `json:"params,omitempty"`
}

// dmModKeys are the dm_mod parameters named in the summary and always listed
// in the dump, present or not.
var dmModKeys = []string{"use_blk_mq", "dm_mq_nr_hw_queues", "dm_mq_queue_depth"}

// queueModules are the modules whose parameters the dump lists in full.
var queueModules = []string{
	"dm_mod", "md_mod", "nvme_core", "nvme", "scsi_mod", "sd_mod", "libata",
	"raid0", "raid1", "raid10", "raid456",
	"dm_raid", "dm_thin_pool", "dm_mirror", "dm_crypt", "dm_multipath",
}

// Files listed by name: a missing one is said to be absent.
var (
	blockFiles = []string{"dev", "size", "ro", "removable", "hidden", "inflight", "stat", "capability",
		"ext_range", "range", "alignment_offset", "discard_alignment", "diskseq", "events", "events_async", "events_poll_msecs"}
	scsiDevFiles = []string{"queue_depth", "queue_type", "timeout", "eh_timeout", "state", "queue_ramp_up_period",
		"device_blocked", "device_busy", "iorequest_cnt", "iodone_cnt", "ioerr_cnt"}
	scsiHostFiles = []string{"can_queue", "cmd_per_lun", "sg_tablesize", "sg_prot_tablesize", "nr_hw_queues",
		"host_busy", "use_blk_mq", "proc_name", "state", "eh_deadline"}
	nvmeCtrlFiles = []string{"queue_count", "sqsize", "transport", "numa_node", "kato", "cntlid", "state",
		"ctrl_loss_tmo", "reconnect_delay", "fast_io_fail_tmo"}
	moduleFiles = []string{"version", "srcversion", "refcnt", "initstate"}
)

const (
	notPresent   = "not present (not on this kernel)"
	defMaxFile   = 64 << 10
	defMaxDevice = 4 << 20
)

// virtualKind is "dm" or "md" for a software node (it has dm/ or md/; a
// dm-raid node may have both and is "dm"), "" for a disk.
func (c *collector) virtualKind() string {
	for _, k := range []string{"dm", "md"} {
		if st, err := os.Stat(c.sys("block", c.name, k)); err == nil && st.IsDir() {
			return k
		}
	}
	return ""
}

// readq reads a sysfs value without logging it (the dump has its own section).
func readq(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// parseScheduler splits "mq-deadline kyber [bfq] none" into the active one and
// the list.
func parseScheduler(s string) (string, []string) {
	var all []string
	active := ""
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "[") && strings.HasSuffix(f, "]") {
			f = strings.Trim(f, "[]")
			active = f
		}
		all = append(all, f)
	}
	if active == "" && len(all) == 1 {
		active = all[0]
	}
	return active, all
}

// hwQueues lists the numeric mq/<n> directories in numeric order; ok is false
// without mq/ (a bio-based device).
func hwQueues(mqDir string) (ids []int, ok bool) {
	ents, err := os.ReadDir(mqDir)
	if err != nil {
		return nil, false
	}
	for _, e := range ents {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			ids = append(ids, n)
		}
	}
	slices.Sort(ids)
	return ids, true
}

// queueSummary reads Info.Queue; nil when the device has no queue directory.
func (c *collector) queueSummary(virt string) *Queue {
	dir := c.sys("block", c.name)
	qd := filepath.Join(dir, "queue")
	if !exists(qd) {
		return nil
	}
	q := &Queue{}
	r := func(f string) string { return readq(filepath.Join(qd, f)) }
	q.Scheduler, q.Schedulers = parseScheduler(r("scheduler"))
	ids, mq := hwQueues(filepath.Join(dir, "mq"))
	q.MQ, q.NrHWQueues = mq, len(ids)
	q.NrRequests, q.Rotational, q.ReadAheadKB = r("nr_requests"), r("rotational"), r("read_ahead_kb")
	q.MaxSectorsKB, q.WBTLatUsec, q.RqAffinity = r("max_sectors_kb"), r("wbt_lat_usec"), r("rq_affinity")
	q.NoMerges, q.IOPoll = r("nomerges"), r("io_poll")
	if virt == "" {
		q.DeviceQueueDepth = readq(filepath.Join(dir, "device", "queue_depth"))
		if m := nvmeNameRe.FindStringSubmatch(c.name); m != nil {
			q.NVMeQueueCount = readq(c.sys("class", "nvme", m[1], "queue_count"))
			q.NVMeSQSize = readq(c.sys("class", "nvme", m[1], "sqsize"))
		}
	}
	if virt != "" && exists(filepath.Join(dir, "md")) {
		md := func(f ...string) string { return readq(filepath.Join(append([]string{dir, "md"}, f...)...)) }
		q.MD = &MDQueue{Level: md("level"), RaidDisks: md("raid_disks"), ChunkSize: md("chunk_size"),
			StripeCacheSize: md("stripe_cache_size"), SyncAction: md("sync_action"), Bitmap: md("bitmap", "location")}
	}
	modDir := c.sys("module", "dm_mod")
	if loaded := exists(modDir); loaded || virt == "dm" {
		p := func(f string) string { return readq(filepath.Join(modDir, "parameters", f)) }
		q.DMMod = &DMMod{Loaded: loaded, UseBlkMQ: p("use_blk_mq"), MQNrHWQueues: p("dm_mq_nr_hw_queues"), MQQueueDepth: p("dm_mq_queue_depth")}
		if ents, err := os.ReadDir(filepath.Join(modDir, "parameters")); err == nil {
			for _, e := range ents {
				if b, err := os.ReadFile(filepath.Join(modDir, "parameters", e.Name())); err == nil && e.Type().IsRegular() {
					if q.DMMod.Params == nil {
						q.DMMod.Params = map[string]string{}
					}
					q.DMMod.Params[e.Name()] = strings.TrimSpace(string(b))
				}
			}
		}
	}
	return q
}

// qdump writes the queue sections of a dump within its bounds.
type qdump struct {
	b         strings.Builder
	maxFile   int
	maxDevice int
	start     int  // where the current device section starts
	full      bool // the current device section reached maxDevice
	dev       string
}

func (d *qdump) over() bool {
	if d.full {
		return true
	}
	if d.b.Len()-d.start >= d.maxDevice {
		fmt.Fprintf(&d.b, "... truncated: the section of %s reached %d bytes\n", d.dev, d.maxDevice)
		d.full = true
	}
	return d.full
}

// entry writes "label = value", or a multi-line value indented below it.
func (d *qdump) entry(label, value string) {
	if d.over() {
		return
	}
	if !strings.Contains(value, "\n") {
		fmt.Fprintf(&d.b, "%s = %s\n", label, value)
		return
	}
	fmt.Fprintf(&d.b, "%s:\n", label)
	for _, l := range strings.Split(value, "\n") {
		fmt.Fprintf(&d.b, "    %s\n", l)
	}
}

func (d *qdump) line(format string, a ...any) {
	if d.over() {
		return
	}
	fmt.Fprintf(&d.b, format+"\n", a...)
}

// value reads a file within maxFile, saying what is missing, unreadable,
// binary, empty or cut.
func (d *qdump) value(path string) string {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return notPresent
	}
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(d.maxFile)+1))
	if err != nil && len(b) == 0 {
		return "unreadable: " + err.Error()
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return fmt.Sprintf("(binary, %d bytes, skipped)", len(b))
	}
	cut := len(b) > d.maxFile
	if cut {
		b = b[:d.maxFile]
	}
	v := strings.TrimRight(string(b), "\n ")
	if cut {
		return v + fmt.Sprintf(" ... (truncated at %d bytes)", d.maxFile)
	}
	if strings.TrimSpace(v) == "" {
		return "(empty)"
	}
	return v
}

// walk writes every file under dir in name order, labelled prefix+rel;
// symlinks are shown, not followed.
func (d *qdump) walk(dir, label string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			d.line("%s/: %s", label, notPresent)
		} else {
			d.line("%s/: unreadable: %v", label, err)
		}
		return
	}
	for _, e := range ents {
		if d.over() {
			return
		}
		p, l := filepath.Join(dir, e.Name()), label+"/"+e.Name()
		switch {
		case e.Type()&os.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			d.line("%s -> %s", l, t)
		case e.IsDir():
			d.walk(p, l)
		default:
			d.entry(l, d.value(p))
		}
	}
}

func names(dir string) (string, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	if len(out) == 0 {
		return "(none)", true
	}
	return strings.Join(out, ", "), true
}

func (c *collector) rel(path string) string {
	r, err := filepath.Rel(c.opts.Sys, path)
	if err != nil {
		return path
	}
	return r
}

// queueDump renders the queue sections: the device, then for an md or dm
// node every device below it (slaves, their slaves, down to the disks), each
// once, nearest first.
func (c *collector) queueDump(virt string) string {
	d := &qdump{maxFile: c.opts.MaxFile, maxDevice: c.opts.MaxDevice}
	type node struct{ name, virt, of string }
	seen := map[string]bool{c.name: true}
	queue := []node{{c.name, virt, ""}}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		c.deviceSection(d, n.name, n.virt, n.of)
		if n.virt == "" {
			continue
		}
		ents, _ := os.ReadDir(c.sys("block", n.name, "slaves"))
		for _, e := range ents {
			disk := c.diskOf(e.Name())
			if disk == "" || seen[disk] {
				continue
			}
			seen[disk] = true
			queue = append(queue, node{disk, c.virtualKindOf(disk), n.name})
		}
	}
	return d.b.String()
}

// diskOf is the block device whose queue serves name: itself, or the disk of
// a partition. "" when it is not in sysfs.
func (c *collector) diskOf(name string) string {
	if exists(c.sys("block", name)) {
		return name
	}
	cls := c.sys("class", "block", name)
	if exists(filepath.Join(cls, "partition")) {
		if real, err := filepath.EvalSymlinks(cls); err == nil {
			return filepath.Base(filepath.Dir(real))
		}
	}
	return ""
}

func (c *collector) virtualKindOf(name string) string {
	sub := &collector{opts: c.opts, name: name, sys: c.sys}
	return sub.virtualKind()
}

// deviceSection writes one device's queue section; of is the node it is a
// member of ("" for the device itself).
func (c *collector) deviceSection(d *qdump, name, virt, of string) {
	d.start, d.full, d.dev = d.b.Len(), false, name
	title := name
	if of != "" {
		title += " (member of " + of + ")"
	}
	fmt.Fprintf(&d.b, "\n== Queue and scheduling: %s ==\n", title)
	dir := c.sys("block", name)
	{
		// Partitions among the node's slaves are named here.
		if ents, err := os.ReadDir(filepath.Join(dir, "slaves")); err == nil {
			for _, e := range ents {
				if disk := c.diskOf(e.Name()); disk != "" && disk != e.Name() {
					d.line("%s is a partition of %s: its queue is the disk's", e.Name(), disk)
				}
			}
		}
	}
	for _, f := range blockFiles {
		d.entry(c.rel(filepath.Join(dir, f)), d.value(filepath.Join(dir, f)))
	}
	d.walk(filepath.Join(dir, "queue"), c.rel(filepath.Join(dir, "queue")))

	ids, mq := hwQueues(filepath.Join(dir, "mq"))
	if !mq {
		d.line("blk-mq: no (bio-based: no mq/ directory)")
	} else {
		d.line("blk-mq: yes, %d hardware queues", len(ids))
		for _, n := range ids {
			d.line("hctx%d: cpus %s", n, readq(filepath.Join(dir, "mq", strconv.Itoa(n), "cpu_list")))
		}
		d.walk(filepath.Join(dir, "mq"), c.rel(filepath.Join(dir, "mq")))
	}

	switch virt {
	case "md", "dm":
		// dm/ of a dm node, md/ of an md node, both of a dm-raid node that
		// shows its md state.
		for _, sub := range []string{"dm", "md"} {
			if p := filepath.Join(dir, sub); sub == virt || exists(p) {
				d.walk(p, c.rel(p))
			}
		}
		for _, sub := range []string{"slaves", "holders"} {
			if v, ok := names(filepath.Join(dir, sub)); ok {
				d.line("%s: %s", sub, v)
			} else {
				d.line("%s: %s", sub, notPresent)
			}
		}
	default:
		if m := nvmeNameRe.FindStringSubmatch(name); m != nil {
			ctrl := c.sys("class", "nvme", m[1])
			if exists(ctrl) {
				for _, f := range nvmeCtrlFiles {
					d.entry(c.rel(filepath.Join(ctrl, f)), d.value(filepath.Join(ctrl, f)))
				}
			}
		} else {
			dev := filepath.Join(dir, "device")
			if exists(dev) {
				for _, f := range scsiDevFiles {
					d.entry(c.rel(filepath.Join(dev, f)), d.value(filepath.Join(dev, f)))
				}
			}
			real, _ := filepath.EvalSymlinks(dir)
			if m := hostRe.FindStringSubmatch(real); m != nil {
				host := c.sys("class", "scsi_host", "host"+m[1])
				for _, f := range scsiHostFiles {
					d.entry(c.rel(filepath.Join(host, f)), d.value(filepath.Join(host, f)))
				}
			}
		}
	}

	dbg := filepath.Join(c.opts.Debug, "block", name)
	if _, err := os.ReadDir(dbg); err != nil {
		d.line("debugfs: %s is not readable (debugfs not mounted, or not root): these are the sysfs values only", dbg)
	} else {
		r, _ := filepath.Rel(c.opts.Debug, dbg)
		d.walk(dbg, "debugfs: "+r)
	}
}

// moduleDump lists the parameters of the queue-related modules, in full.
func (c *collector) moduleDump() string {
	d := &qdump{maxFile: c.opts.MaxFile, maxDevice: c.opts.MaxDevice, dev: "the module parameters"}
	d.b.WriteString("\n== Kernel module parameters ==\n")
	for _, m := range queueModules {
		dir := c.sys("module", m)
		label := c.rel(dir)
		if !exists(dir) {
			d.line("%s: not loaded (or built in without parameters)", label)
			if m == "dm_mod" {
				for _, k := range dmModKeys {
					d.entry(label+"/parameters/"+k, notPresent)
				}
			}
			continue
		}
		for _, f := range moduleFiles {
			if p := filepath.Join(dir, f); exists(p) {
				d.entry(label+"/"+f, d.value(p))
			}
		}
		if v, ok := names(filepath.Join(dir, "holders")); ok {
			d.line("%s/holders: %s", label, v)
		}
		if m == "dm_mod" {
			for _, k := range dmModKeys {
				if p := filepath.Join(dir, "parameters", k); !exists(p) {
					d.entry(label+"/parameters/"+k, notPresent)
				}
			}
		}
		if exists(filepath.Join(dir, "parameters")) {
			d.walk(filepath.Join(dir, "parameters"), label+"/parameters")
		}
	}
	return d.b.String()
}

// formatQueue is the summary lines of the queue state.
func formatQueue(q *Queue, line func(k, v string)) {
	if q == nil {
		return
	}
	line("Queue", QueueMode(q))
	var t []string
	for _, kv := range [][2]string{{"nr_requests", q.NrRequests}, {"rotational", q.Rotational}, {"read_ahead_kb", q.ReadAheadKB},
		{"max_sectors_kb", q.MaxSectorsKB}, {"wbt_lat_usec", q.WBTLatUsec}, {"rq_affinity", q.RqAffinity},
		{"nomerges", q.NoMerges}, {"io_poll", q.IOPoll}, {"queue_depth", q.DeviceQueueDepth},
		{"nvme queue_count", q.NVMeQueueCount}, {"nvme sqsize", q.NVMeSQSize}} {
		if kv[1] != "" {
			t = append(t, kv[0]+" "+kv[1])
		}
	}
	line("Queue params", strings.Join(t, ", "))
	if m := q.MD; m != nil {
		line("md", strings.Join(nonEmpty(m.Level, prefixed(m.RaidDisks, "", " disks"), prefixed(m.ChunkSize, "chunk ", ""),
			prefixed(m.StripeCacheSize, "stripe cache ", ""), prefixed(m.SyncAction, "sync ", ""), prefixed(m.Bitmap, "bitmap ", "")), ", "))
	}
	if q.DMMod != nil {
		line("dm_mod", DMModText(q.DMMod))
	}
}

func prefixed(v, pre, post string) string {
	if v == "" {
		return ""
	}
	return pre + v + post
}

// QueueMode is "blk-mq, 4 hardware queues, scheduler none" or "bio-based,
// scheduler none".
func QueueMode(q *Queue) string {
	s := "bio-based"
	if q.MQ {
		s = fmt.Sprintf("blk-mq, %d hardware queues", q.NrHWQueues)
	}
	if q.Scheduler != "" {
		s += ", scheduler " + q.Scheduler
	}
	return s
}

// DMModText is "use_blk_mq Y, dm_mq_nr_hw_queues 0, dm_mq_queue_depth 2048",
// with "n/a (not on this kernel)" for a parameter this kernel lacks.
func DMModText(d *DMMod) string {
	if !d.Loaded {
		return "not loaded"
	}
	var out []string
	for i, v := range []string{d.UseBlkMQ, d.MQNrHWQueues, d.MQQueueDepth} {
		if v == "" {
			v = "n/a (not on this kernel)"
		}
		out = append(out, dmModKeys[i]+" "+v)
	}
	return strings.Join(out, ", ")
}
