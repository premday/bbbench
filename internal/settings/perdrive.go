package settings

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DriveOps is a setting of each drive on its own (a drive value, such as its
// write cache): PerDrive turns it into a Setting.
type DriveOps interface {
	Name() string
	Kind() Kind
	Values() []Value
	Applicable(t Target) (bool, string)
	Read(ctx context.Context, t Target) (Reading, error)
	Apply(ctx context.Context, t Target, v Value) error
	// Restore puts the drive back to original, writing only when it differs.
	Restore(ctx context.Context, t Target, original Reading) error
}

// DriveCommandHinter is implemented by DriveOps that can say which command
// sets a drive back by hand.
type DriveCommandHinter interface {
	// RestoreCommand is the command that sets t to original, or "".
	RestoreCommand(t Target, original Reading) string
}

// PerDrive is the Setting of ops: drive scope, values input, group lifetime.
// Each disk of a variant is one drive target; settle bounds the read-back
// after an undo.
func PerDrive(ops DriveOps, settle time.Duration) Setting {
	return &perDrive{ops: ops, settle: settle}
}

type perDrive struct {
	ops    DriveOps
	settle time.Duration
}

func (p *perDrive) Name() string       { return p.ops.Name() }
func (p *perDrive) Kind() Kind         { return p.ops.Kind() }
func (p *perDrive) Scope() Scope       { return ScopeDrive }
func (p *perDrive) Input() Input       { return InputValues }
func (p *perDrive) Lifetime() Lifetime { return LifetimeGroup }
func (p *perDrive) Values() []Value    { return p.ops.Values() }

func (p *perDrive) Applicable(t Target) (bool, string) { return p.ops.Applicable(t) }

// ApplicableVariant is false, naming the disk, when one disk is not applicable.
func (p *perDrive) ApplicableVariant(v Variant) (bool, string) {
	for _, d := range v.Disks {
		if ok, why := p.ops.Applicable(d.Target); !ok {
			return false, d.Name + ": " + why
		}
	}
	return true, ""
}

// Prepare reads the original value of every disk of v.
func (p *perDrive) Prepare(ctx context.Context, v Variant) ([]Undo, error) {
	undos := make([]Undo, 0, len(v.Disks))
	for _, d := range v.Disks {
		r, err := p.ops.Read(ctx, d.Target)
		if err != nil {
			return nil, fmt.Errorf("read the original %s of %s: %w", p.Name(), d.Name, err)
		}
		if r.Value == "" {
			return nil, fmt.Errorf("the %s of %s read back empty", p.Name(), d.Name)
		}
		t := d.Target
		undos = append(undos, Undo{Drive: &t, Original: &r})
	}
	return undos, nil
}

// Apply sets every disk of v to v.Value; a disk that fails carries its
// error in its target, the others are still applied.
func (p *perDrive) Apply(ctx context.Context, v Variant, _ []Undo) ([]BenchTarget, error) {
	if len(v.Disks) == 0 {
		return nil, errors.New("the variant has no disks")
	}
	ts := make([]BenchTarget, 0, len(v.Disks))
	for _, d := range v.Disks {
		bt := BenchTarget{Kind: TargetDrive, Target: d.Target, Want: v.Value.ID}
		if err := p.ops.Apply(ctx, d.Target, v.Value); err != nil {
			bt.Err = err
		}
		ts = append(ts, bt)
	}
	return ts, nil
}

func (p *perDrive) Read(ctx context.Context, t BenchTarget) (Reading, error) {
	return p.ops.Read(ctx, t.Target)
}

// Undo restores the drive and reads it back until it shows the original.
func (p *perDrive) Undo(ctx context.Context, u Undo) error {
	if u.Drive == nil || u.Original == nil {
		return errors.New("not a drive record")
	}
	if err := p.ops.Restore(ctx, *u.Drive, *u.Original); err != nil {
		return err
	}
	bt := BenchTarget{Kind: TargetDrive, Target: *u.Drive, Want: u.Original.Value}
	if r, ok := Verify(ctx, p, bt, p.settle); !ok {
		return fmt.Errorf("reads %q after restoring, want %q", r.Value, u.Original.Value)
	}
	return nil
}

// UndoCommand is the ops' restore command, "" when it has none.
func (p *perDrive) UndoCommand(u Undo) string {
	h, ok := p.ops.(DriveCommandHinter)
	if !ok || u.Drive == nil || u.Original == nil {
		return ""
	}
	return h.RestoreCommand(*u.Drive, *u.Original)
}
