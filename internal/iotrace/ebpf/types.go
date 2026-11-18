package ebpf

import "errors"

// Sizes shared with iolat.bpf.c (checked against it by a test).
const (
	NBuckets = 2240 // iotrace's log-bucket histogram
	MaxDevs  = 10   // devices traced at once
	Kinds    = 3    // device, block layer, bio
	maxSlots = MaxDevs * Kinds * 2
)

// Source says which tracepoints time a device.
type Source uint32

const (
	// SourceBlock: block_rq_insert/issue -> block_rq_complete of requests on
	// the device (NVMe included).
	SourceBlock Source = 0
	// SourceBio: block_bio_queue -> block_bio_complete of the bios submitted
	// to the device (an md or dm node).
	SourceBio Source = 1
)

// Kind indexes the histograms of a device.
const (
	KindDevice     = 0
	KindBlockLayer = 1
	KindBio        = 2
)

// DevSpec is one device to time. Dev is the kernel's dev_t (major<<20|minor).
type DevSpec struct {
	Dev    uint32
	Idx    int // 0..MaxDevs-1, the key of its slots in a Snapshot
	Source Source
}

// SlotKey names one histogram: device index, kind and direction.
type SlotKey struct {
	Idx   int
	Kind  int
	Write bool
}

// Slot is a cumulative histogram summed over the CPUs.
type Slot struct {
	Count   uint64
	SumNs   uint64
	Buckets [NBuckets]uint64
}

// Snapshot is the cumulative state of the program: histograms (only the
// devices set) and counters. Windows are differences of two snapshots.
type Snapshot struct {
	Slots     map[SlotKey]*Slot
	Started   uint64 // requests and bios whose start was stored
	Completed uint64 // completions that found their start
	NoStart   uint64 // completions of traced devices without a stored start
	Full      uint64 // starts dropped because the start map was full
}

// Program is the loaded, attached collector.
type Program interface {
	// SetDevices replaces the devices to time (at most MaxDevs).
	SetDevices([]DevSpec) error
	// Read returns the cumulative state.
	Read() (Snapshot, error)
	// Attached lists the tracepoints that are attached.
	Attached() []string
	// Close detaches and frees everything.
	Close() error
}

// ErrUnsupported is returned by Open where eBPF cannot be used at all (the
// platform, or no BTF); the text says why.
var ErrUnsupported = errors.New("ebpf not supported")
