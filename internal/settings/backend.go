package settings

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// LevelNone is the baseline layout: every disk on its own, nothing built.
const LevelNone = "none"

// Option is one editor row of a layout level: a key with its choices.
type Option struct {
	Key, Label string
	Choices    []string
	Default    string
}

// Layout is one software layout: a backend, its level and the options of
// that level. The baseline is Layout{Level: LevelNone}.
type Layout struct {
	Backend, Level string
	Options        map[string]string
}

// Backend builds, reads and tears down layouts with one tool. It is the
// plug-in point of the software-layout setting: everything specific to a
// tool lives in its backend.
//
// Safety contract (every backend must meet it):
//   - Prepare changes nothing and refuses a name that already exists on the
//     machine. The record it returns is saved to the recovery file before
//     Apply and is not saved again afterwards, so it must already hold
//     everything Undo needs to find and remove what Apply creates (name,
//     uuid when the tool takes one up front, members, Extra).
//   - Apply builds only on the recorded members; on a failure it returns
//     and leaves what was built to Undo.
//   - Undo touches nothing it cannot prove belongs to the record: it
//     refuses an array, volume group or name it cannot prove is its own
//     (one that has a member or physical volume that is not a present
//     recorded member, or that it cannot list), it never touches a member
//     whose Path is empty (not found again; reported as not present), and it
//     only wipes signatures that the record says it created. Each removal is
//     verified by reading back.
type Backend interface {
	// Name is the first word of every value; it is recorded on targets
	// and undo records.
	Name() string
	// Available is false with the reason when its tool is missing.
	Available() (bool, string)
	Levels() []string
	// Roles are the disk roles a level takes, RoleData first.
	Roles(level string) []Role
	// Options are the editor rows for this level, in order.
	Options(level string, dataDisks int) []Option
	// Validate checks the backend rules, after the generic ones.
	Validate(l Layout, disks []Disk) error
	// Label is the canonical value; it starts with Name()+" ".
	Label(l Layout, disks []Disk) string
	// Parse is the inverse of Label, with every option set.
	Parse(value string) (Layout, error)
	// Prepare checks that the layout can be built under name and returns
	// what tears it down; it changes nothing.
	Prepare(ctx context.Context, v Variant, l Layout, name string) (*ArrayUndo, error)
	// Apply builds the layout recorded in rec and returns its targets.
	Apply(ctx context.Context, v Variant, l Layout, rec *ArrayUndo) ([]BenchTarget, error)
	// WaitSteady returns once the targets are steady (resync done).
	WaitSteady(ctx context.Context, ts []BenchTarget, progress func(pct float64)) error
	// Read reads a composite target built by this backend.
	Read(ctx context.Context, t BenchTarget) (Reading, error)
	// Undo tears the layout down and verifies it is gone.
	Undo(ctx context.Context, u ArrayUndo) error
	// UndoCommand is the command that tears u down by hand.
	UndoCommand(u ArrayUndo) string
}

// LayoutSetting is the software-layout setting: a Setting that dispatches to
// its backends.
type LayoutSetting interface {
	Setting
	Settler
	CommandHinter
	Backends() []Backend
	// ValidateLayout checks l on disks: the generic rules, then the
	// backend's.
	ValidateLayout(l Layout, disks []Disk) error
	// LayoutValue validates l and returns its value; ID == Label.
	LayoutValue(l Layout, disks []Disk) (Value, error)
	ParseLayout(id string) (Layout, error)
}

// WithDefaults returns l with exactly the options b lists for its level:
// options not listed are dropped, missing ones and ones no longer among the
// choices are set to the default. l itself is not changed.
func WithDefaults(b Backend, l Layout, dataDisks int) Layout {
	in := l.Options
	l.Options = map[string]string{}
	if b == nil || l.Level == LevelNone {
		return l
	}
	for _, o := range b.Options(l.Level, dataDisks) {
		v, ok := in[o.Key]
		if !ok || v == "" || (len(o.Choices) > 0 && !slices.Contains(o.Choices, v)) {
			v = o.Default
		}
		l.Options[o.Key] = v
	}
	return l
}

// maxArrayName is the longest array name every backend can record.
const maxArrayName = 32

// ArrayName is the private name of the array of a variant,
// "bbbench_<variant name>".
func ArrayName(variantName string) (string, error) {
	if variantName == "" {
		return "", errors.New("the variant has no name")
	}
	n := "bbbench_" + variantName
	if len(n) > maxArrayName {
		return "", fmt.Errorf("array name %q is longer than %d characters", n, maxArrayName)
	}
	return n, nil
}

// sortedOptionKeys are the keys of m in order, for stable messages.
func sortedOptionKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}
