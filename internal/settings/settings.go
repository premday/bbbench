// Package settings changes a configuration setting of the machine for a run
// group: the whole test plan runs once per value of the setting. Settings are
// volatile (never saved, never persisted); each is read before it is changed,
// applied, verified by reading it back, and restored afterwards.
package settings

import (
	"context"
	"os/exec"
	"time"

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
)

// Input is how the user chooses the groups of a setting.
type Input string

const (
	// InputValues: the user ticks values over one common drive selection
	// (drive-scope settings).
	InputValues Input = "values"
	// InputVariants: the user lists variants one by one, each with its own
	// disks (layout-scope settings; none exists yet).
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

// Setting is one thing bbbench can vary between groups.
type Setting interface {
	Name() string
	Kind() Kind
	Scope() Scope
	Input() Input
	Values() []Value
	// Applicable says whether the setting can be controlled on t, and if not, why.
	Applicable(t Target) (bool, string)
	Read(ctx context.Context, t Target) (Reading, error)
	Apply(ctx context.Context, t Target, v Value) error
	// Restore puts the setting back to original, writing only when it differs.
	Restore(ctx context.Context, t Target, original Reading) error
}

// CommandHinter is implemented by settings that can say which command sets
// a drive back by hand, for the report of a failed restore.
type CommandHinter interface {
	// RestoreCommand is the command that sets t to original, or "" when
	// there is none (unknown value or interface).
	RestoreCommand(t Target, original Reading) string
}

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Env lets tests replace the machine.
type Env struct {
	Run      Runner                             // default: exec
	LookPath func(string) (string, error)       // default exec.LookPath
	Sys      string                             // sysfs root, default /sys
	GetWCE   func(devPath string) (bool, error) // default scsi.GetWCE
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
	return e
}

// All lists the settings bbbench knows.
func All(env Env) []Setting {
	return []Setting{NewWriteCache(env)}
}

// Verify reads the setting every 200 ms until it equals want or settle has
// elapsed; it returns the last reading and whether it matched.
func Verify(ctx context.Context, s Setting, t Target, want Value, settle time.Duration) (Reading, bool) {
	deadline := time.Now().Add(settle)
	var last Reading
	for {
		if r, err := s.Read(ctx, t); err == nil {
			last = r
			if r.Value == want.ID {
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
