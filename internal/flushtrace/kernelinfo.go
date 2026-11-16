package flushtrace

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// CPUStats are the kernel's own counters of one CPU's ring buffer
// (per_cpu/cpuN/stats), read once tracing has stopped and the pipes are
// drained. They are the ground truth the reader's own counts are checked
// against: Overrun is what the kernel overwrote before anyone read it.
type CPUStats struct {
	CPU           int   `json:"cpu"`
	Entries       int64 `json:"entries"`
	Overrun       int64 `json:"overrun"`
	CommitOverrun int64 `json:"commit_overrun"`
	Bytes         int64 `json:"bytes"`
	Dropped       int64 `json:"dropped_events"`
	Read          int64 `json:"read_events"`
}

// FilterState is an event filter as bbbench wrote it and as the kernel shows
// it afterwards (events/<sys>/<event>/filter). A filter the kernel rejected
// is reported as none (or with an error): every event of that type, of every
// device, then reaches the buffer.
type FilterState struct {
	Event   string `json:"event"`
	Wanted  string `json:"wanted"`
	Applied string `json:"applied"`
	Err     string `json:"err,omitempty"` // error of the write
}

// NotApplied reports whether the kernel is certainly not filtering as wanted:
// no filter shown, or an error text.
func (f FilterState) NotApplied() bool {
	a := strings.TrimSpace(f.Applied)
	return f.Wanted != "" && (f.Err != "" || a == "" || a == "none" || strings.Contains(a, "Error"))
}

// KernelInfo is what the kernel says about an instance: the filters it
// applied, the buffer size it granted and each CPU's counters.
type KernelInfo struct {
	BufferKB string        `json:"buffer_kb,omitempty"` // buffer_size_kb as the kernel prints it
	Filters  []FilterState `json:"filters,omitempty"`
	CPUs     []CPUStats    `json:"cpus,omitempty"`
}

// Overrun is the events overwritten, summed over the CPUs.
func (k *KernelInfo) Overrun() int64 {
	var n int64
	for _, c := range k.CPUs {
		n += c.Overrun
	}
	return n
}

// Read is the events the kernel counts as read, summed over the CPUs.
func (k *KernelInfo) Read() int64 {
	var n int64
	for _, c := range k.CPUs {
		n += c.Read
	}
	return n
}

// Unfiltered returns the filters the kernel did not apply.
func (k *KernelInfo) Unfiltered() []FilterState {
	var out []FilterState
	for _, f := range k.Filters {
		if f.NotApplied() {
			out = append(out, f)
		}
	}
	return out
}

// parseCPUStats reads the "key: value" lines of a per_cpu stats file.
func parseCPUStats(cpu int, text string) CPUStats {
	c := CPUStats{CPU: cpu}
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(k) {
		case "entries":
			c.Entries = n
		case "overrun":
			c.Overrun = n
		case "commit overrun":
			c.CommitOverrun = n
		case "bytes":
			c.Bytes = n
		case "dropped events":
			c.Dropped = n
		case "read events":
			c.Read = n
		}
	}
	return c
}

// snapshot reads the kernel's view; it is called once tracing has stopped,
// before the instance directory is removed. Unreadable files are left out.
func (i *Instance) snapshot() {
	k := &KernelInfo{}
	if b, err := os.ReadFile(filepath.Join(i.dir, "buffer_size_kb")); err == nil {
		k.BufferKB = strings.TrimSpace(string(b))
	}
	for _, e := range i.events {
		w, ok := i.wanted[e]
		if !ok {
			continue
		}
		fs := FilterState{Event: e, Wanted: w, Err: i.filterErr[e]}
		if b, err := os.ReadFile(filepath.Join(i.dir, e, "filter")); err == nil {
			fs.Applied = strings.TrimSpace(string(b))
		}
		k.Filters = append(k.Filters, fs)
	}
	dirs, _ := filepath.Glob(filepath.Join(i.dir, "per_cpu", "cpu*"))
	for _, d := range dirs {
		n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(d), "cpu"))
		if err != nil {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(d, "stats")); err == nil {
			k.CPUs = append(k.CPUs, parseCPUStats(n, string(b)))
		}
	}
	sort.Slice(k.CPUs, func(a, b int) bool { return k.CPUs[a].CPU < k.CPUs[b].CPU })
	i.kernel = k
	// The pages carry a lost count only when the kernel could store one, so
	// the reader's count can fall short: the kernel's own counter is the
	// floor.
	if ko := k.Overrun(); ko > i.overrun.Load() {
		i.overrun.Store(ko)
	}
}

// Kernel returns the kernel's view recorded at Stop (nil when the instance
// was closed without a clean stop or nothing could be read). Call it after
// Stop or Close.
func (i *Instance) Kernel() *KernelInfo { return i.kernel }
