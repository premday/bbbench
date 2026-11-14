// Package settings changes a configuration setting of the machine for a run
// group: the whole test plan runs once per value of the setting. Settings are
// volatile (never saved, never persisted); each is read before it is changed,
// applied, verified by reading it back, and restored afterwards.
package settings

import (
	"context"
	"crypto/rand"
	"io"
	"os/exec"
	"time"

	"bbbench/internal/inuse"
	"bbbench/internal/scsi"
)

// Kind says how a setting reaches its value.
type Kind string

const (
	// Applied settings are changed by bbbench itself.
	Applied Kind = "applied"
	// Manual settings are changed by the user between groups.
	Manual Kind = "manual"
)

// Scope is what a setting applies to.
type Scope string

const (
	// ScopeDrive is a property of each drive.
	ScopeDrive Scope = "drive"
	// ScopeLayout is how a set of disks is assembled (software RAID, LVM).
	ScopeLayout Scope = "layout"
)

// Input is how the user chooses the groups of a setting.
type Input string

const (
	// InputValues: the user ticks values over one common drive selection
	// (drive-scope settings).
	InputValues Input = "values"
	// InputVariants: the user lists variants one by one, each with its own
	// disks (layout-scope settings).
	InputVariants Input = "variants"
)

// Role is what a disk does in a layout.
type Role string

const (
	RoleData    Role = "data"
	RoleSpare   Role = "spare"
	RoleJournal Role = "journal"
)

// TargetKind says what a target is.
type TargetKind string

const (
	// TargetDrive is a physical drive.
	TargetDrive TargetKind = "drive"
	// TargetArray is any composite: an md array, an LV, later a
	// controller's virtual drive.
	TargetArray TargetKind = "array"
)

// Disk is a drive with its role in a layout.
type Disk struct {
	Target
	Role Role
}

// Target is the drive a setting is applied to.
type Target struct {
	Name       string // kernel name, "nvme0n1", "sda"
	Path       string // device node, "/dev/sda"
	Serial     string
	Model      string
	Vendor     string
	Interface  string // diskinfo.Info.Interface: "NVMe (PCIe)", "SATA", "SAS", "USB", "virtio"
	Rotational bool
}

// Value is one value a setting can take.
type Value struct{ ID, Label string }

// Reading is the state of a setting on a drive at a moment.
type Reading struct {
	Value  string    `json:"value"`
	Kernel string    `json:"kernel,omitempty"` // informational: what the kernel believes
	At     time.Time `json:"at"`
}

// Lifetime says when a setting is prepared and undone.
type Lifetime string

const (
	// LifetimeGroup is prepared once for every disk of the group and undone
	// when the group ends (drive values).
	LifetimeGroup Lifetime = "group"
	// LifetimeRun is prepared before each run and undone right after it
	// (layouts).
	LifetimeRun Lifetime = "run"
)

// Variant is one value of a setting on its disks: what one part of a run
// applies. Name is unique in the group, "<group id>_<run n>", and is set by
// the group loop.
type Variant struct {
	Name  string
	Value Value
	Disks []Disk
}

// BenchTarget is what a run benchmarks after Apply. For a drive, Target is
// the drive itself. For a composite, Target.Name/Path are its kernel name and
// node ("md127", "dm-3"), Serial is its uuid, Model the level, and the
// backend sets Vendor and Interface.
type BenchTarget struct {
	Kind   TargetKind
	Target Target
	// Backend is the layout backend that built a composite target, "" for
	// a drive target.
	Backend string
	UUID    string
	Members []Disk
	// Want is the Reading.Value that verifies the target.
	Want string
	// Err is set when Apply could not make this one target effective.
	Err error
}

// Undo is one record of what puts the machine back: for a drive, its
// original reading; for a layout, the array that was built.
type Undo struct {
	Drive    *Target
	Original *Reading
	Array    *ArrayUndo
}

// ArrayUndo is what tears down a built layout. UUID is empty when the
// backend has none before the build. Extra belongs to the backend: the core
// stores it and never reads it.
type ArrayUndo struct {
	Backend, Name, UUID string
	Members             []Disk
	Extra               map[string]string
}

// Name is the name of the drive or the array the record puts back.
func (u Undo) Name() string {
	switch {
	case u.Drive != nil:
		return u.Drive.Name
	case u.Array != nil:
		return u.Array.Name
	}
	return ""
}

// Settler is implemented by settings whose targets need time to become
// steady after Apply (an array resyncing), before a run may start.
type Settler interface {
	WaitSteady(ctx context.Context, ts []BenchTarget, progress func(pct float64)) error
}

// Setting is one thing bbbench can vary between groups.
type Setting interface {
	Name() string
	Kind() Kind
	Scope() Scope
	Input() Input
	Lifetime() Lifetime
	Values() []Value
	// Applicable says whether the setting can be controlled on one disk,
	// and if not, why.
	Applicable(t Target) (bool, string)
	// ApplicableVariant says the same for a whole variant.
	ApplicableVariant(v Variant) (bool, string)
	// Prepare reads what Apply will change and returns what undoes it; it
	// changes nothing.
	Prepare(ctx context.Context, v Variant) ([]Undo, error)
	// Apply makes v effective and returns the targets to benchmark.
	Apply(ctx context.Context, v Variant, prepared []Undo) ([]BenchTarget, error)
	Read(ctx context.Context, t BenchTarget) (Reading, error)
	// Undo puts back one record and checks it; it writes only when
	// something differs.
	Undo(ctx context.Context, u Undo) error
}

// CommandHinter is implemented by settings that can say which command puts
// a record back by hand, for the report of a failed restore.
type CommandHinter interface {
	// UndoCommand is the command that undoes u, or "" when there is none
	// (unknown value or interface).
	UndoCommand(u Undo) string
}

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Env lets tests replace the machine.
type Env struct {
	Run      Runner                             // default: exec
	LookPath func(string) (string, error)       // default exec.LookPath
	Sys      string                             // sysfs root, default /sys
	GetWCE   func(devPath string) (bool, error) // default scsi.GetWCE
	// Settle is how long a read-back may take to show a restored value
	// (default 10 s).
	Settle time.Duration
	Dev    string    // device directory, default /dev
	Rand   io.Reader // default crypto/rand.Reader
	// Poll is the interval between two readings while waiting for a
	// target to become steady (default 2 s).
	Poll time.Duration
	// InUse reports the disks that must not be erased (default
	// inuse.Scan(inuse.Options{})).
	InUse func() (*inuse.Report, error)
}

func (e Env) fill() Env {
	if e.Run == nil {
		e.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	if e.LookPath == nil {
		e.LookPath = exec.LookPath
	}
	if e.Sys == "" {
		e.Sys = "/sys"
	}
	if e.GetWCE == nil {
		e.GetWCE = scsi.GetWCE
	}
	if e.Settle == 0 {
		e.Settle = 10 * time.Second
	}
	if e.Dev == "" {
		e.Dev = "/dev"
	}
	if e.Rand == nil {
		e.Rand = rand.Reader
	}
	if e.Poll == 0 {
		e.Poll = 2 * time.Second
	}
	if e.InUse == nil {
		e.InUse = func() (*inuse.Report, error) { return inuse.Scan(inuse.Options{}) }
	}
	return e
}

// All lists the settings bbbench knows: the drive write cache, then the
// storage layout with its backends.
func All(env Env) []Setting {
	return []Setting{NewWriteCache(env), NewLayout(env, NewMdadm(env), NewLVM(env))}
}

// Verify reads the setting every 200 ms until it equals t.Want or settle has
// elapsed; it returns the last reading and whether it matched.
func Verify(ctx context.Context, s Setting, t BenchTarget, settle time.Duration) (Reading, bool) {
	deadline := time.Now().Add(settle)
	var last Reading
	for {
		if r, err := s.Read(ctx, t); err == nil {
			last = r
			if r.Value == t.Want {
				return r, true
			}
		}
		wait := min(200*time.Millisecond, time.Until(deadline))
		if wait <= 0 {
			return last, false
		}
		select {
		case <-ctx.Done():
			return last, false
		case <-time.After(wait):
		}
	}
}
