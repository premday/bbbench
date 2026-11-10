package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// NewLayout is the software-layout setting over backends, in order. It
// names no tool: building, reading and tearing down are the backends'.
func NewLayout(env Env, backends ...Backend) LayoutSetting {
	return &layout{env: env.fill(), backends: slices.Clone(backends)}
}

type layout struct {
	env      Env
	backends []Backend
}

func (s *layout) Name() string       { return "storage layout" }
func (s *layout) Kind() Kind         { return Applied }
func (s *layout) Scope() Scope       { return ScopeLayout }
func (s *layout) Input() Input       { return InputVariants }
func (s *layout) Lifetime() Lifetime { return LifetimeRun }

func (s *layout) Backends() []Backend { return slices.Clone(s.backends) }

// Values is none, then "<backend> <level>" for every backend and level.
func (s *layout) Values() []Value {
	vs := []Value{{ID: LevelNone, Label: LevelNone}}
	for _, b := range s.backends {
		for _, lv := range b.Levels() {
			id := b.Name() + " " + lv
			vs = append(vs, Value{ID: id, Label: id})
		}
	}
	return vs
}

func (s *layout) backend(name string) Backend {
	for _, b := range s.backends {
		if b.Name() == name {
			return b
		}
	}
	return nil
}

// Applicable is true when any backend is available; otherwise the reason
// of each backend.
func (s *layout) Applicable(Target) (bool, string) {
	if len(s.backends) == 0 {
		return false, "no layout provider is registered"
	}
	var why []string
	for _, b := range s.backends {
		ok, reason := b.Available()
		if ok {
			return true, ""
		}
		why = append(why, b.Name()+": "+reason)
	}
	return false, strings.Join(why, "; ")
}

// ValidateLayout checks the generic rules in order, then the backend's,
// then mixed media.
func (s *layout) ValidateLayout(l Layout, disks []Disk) error {
	var b Backend
	if l.Backend != "" {
		if b = s.backend(l.Backend); b == nil {
			return fmt.Errorf("unknown backend %q", l.Backend)
		}
		if ok, why := b.Available(); !ok {
			return fmt.Errorf("%s is not available: %s", b.Name(), why)
		}
		if !slices.Contains(b.Levels(), l.Level) {
			return fmt.Errorf("unknown %s level %q", b.Name(), l.Level)
		}
	} else if l.Level != LevelNone {
		return errors.New("a layout needs a backend")
	}
	seen := map[string]bool{}
	for _, d := range disks {
		if seen[d.Name] {
			return fmt.Errorf("%s is used twice", d.Name)
		}
		seen[d.Name] = true
	}
	if b == nil {
		for _, d := range disks {
			if d.Role != RoleData {
				return errors.New("none takes data disks only")
			}
		}
		if len(disks) == 0 {
			return errors.New("none needs at least 1 disk")
		}
		if ks := sortedOptionKeys(l.Options); len(ks) > 0 {
			return fmt.Errorf("%s has no %s option", LevelNone, ks[0])
		}
		return nil
	}
	roles := b.Roles(l.Level)
	data := 0
	for _, d := range disks {
		if !slices.Contains(roles, d.Role) {
			return fmt.Errorf("%s cannot have a %s disk", l.Level, d.Role)
		}
		if d.Role == RoleData {
			data++
		}
	}
	opts := b.Options(l.Level, data)
	for _, k := range sortedOptionKeys(l.Options) {
		if !slices.ContainsFunc(opts, func(o Option) bool { return o.Key == k }) {
			return fmt.Errorf("%s has no %s option", l.Level, k)
		}
	}
	for _, o := range opts {
		v := l.Options[o.Key]
		if v == "" {
			return fmt.Errorf("%s is not set", o.Label)
		}
		if len(o.Choices) > 0 && !slices.Contains(o.Choices, v) {
			return fmt.Errorf("%s must be one of %s", o.Label, strings.Join(o.Choices, ", "))
		}
	}
	if err := b.Validate(l, disks); err != nil {
		return err
	}
	var hdd, ssd string
	for _, d := range disks {
		if d.Role != RoleData {
			continue
		}
		if d.Rotational && hdd == "" {
			hdd = d.Name
		}
		if !d.Rotational && ssd == "" {
			ssd = d.Name
		}
	}
	if hdd != "" && ssd != "" {
		return fmt.Errorf("mixed media: data disks %s (HDD) and %s (SSD)", hdd, ssd)
	}
	return nil
}

// LayoutValue validates l and returns its canonical value.
func (s *layout) LayoutValue(l Layout, disks []Disk) (Value, error) {
	if err := s.ValidateLayout(l, disks); err != nil {
		return Value{}, err
	}
	if l.Backend == "" {
		label := fmt.Sprintf("%s · %d disks", LevelNone, len(disks))
		if len(disks) == 1 {
			label = LevelNone + " · 1 disk"
		}
		return Value{ID: label, Label: label}, nil
	}
	b := s.backend(l.Backend)
	label := b.Label(l, disks)
	if !strings.HasPrefix(label, b.Name()+" ") {
		return Value{}, fmt.Errorf("backend %s labels %q without its name", b.Name(), label)
	}
	return Value{ID: label, Label: label}, nil
}

// ParseLayout: "none · …" is the baseline, else the first word picks the
// backend that parses the value.
func (s *layout) ParseLayout(id string) (Layout, error) {
	if id == LevelNone || strings.HasPrefix(id, LevelNone+" · ") {
		return Layout{Level: LevelNone}, nil
	}
	name, _, _ := strings.Cut(id, " ")
	b := s.backend(name)
	if b == nil {
		return Layout{}, fmt.Errorf("unknown backend %q", name)
	}
	l, err := b.Parse(id)
	if err != nil {
		return Layout{}, err
	}
	if l.Backend == "" {
		l.Backend = b.Name()
	}
	if l.Backend != b.Name() {
		return Layout{}, fmt.Errorf("backend %s parses %q as backend %q", b.Name(), id, l.Backend)
	}
	return l, nil
}

// variantLayout is the layout of v, checked against its disks.
func (s *layout) variantLayout(v Variant) (Layout, error) {
	l, err := s.ParseLayout(v.Value.ID)
	if err != nil {
		return Layout{}, err
	}
	val, err := s.LayoutValue(l, v.Disks)
	if err != nil {
		return Layout{}, err
	}
	if val.ID != v.Value.ID {
		return Layout{}, fmt.Errorf("layout %q does not match its disks (%q)", v.Value.ID, val.ID)
	}
	return l, nil
}

func (s *layout) ApplicableVariant(v Variant) (bool, string) {
	if _, err := s.variantLayout(v); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// Prepare re-checks the variant and that none of its disks is in use (the
// baseline's benchmark overwrites its disks too), then asks the backend
// what tears the layout down. It changes nothing; the baseline has nothing
// to undo.
func (s *layout) Prepare(ctx context.Context, v Variant) ([]Undo, error) {
	l, err := s.variantLayout(v)
	if err != nil {
		return nil, err
	}
	rep, err := s.env.InUse()
	if err != nil {
		return nil, fmt.Errorf("cannot check which disks are in use: %v", err)
	}
	if rep == nil {
		return nil, errors.New("cannot check which disks are in use: no report")
	}
	if rep.All != "" {
		return nil, fmt.Errorf("no disk can be used: %s", rep.All)
	}
	for _, d := range v.Disks {
		if why := rep.Reason(d.Name); why != "" {
			return nil, fmt.Errorf("%s is in use: %s", d.Name, why)
		}
	}
	if l.Backend == "" {
		return nil, nil
	}
	name, err := ArrayName(v.Name)
	if err != nil {
		return nil, err
	}
	b := s.backend(l.Backend)
	rec, err := b.Prepare(ctx, v, l, name)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("backend %s returned no undo record", b.Name())
	}
	rec.Backend = b.Name()
	if rec.Name == "" {
		rec.Name = name
	}
	return []Undo{{Array: rec}}, nil
}

// Apply builds the layout of v with the record Prepare returned; the
// baseline benchmarks every disk on its own.
func (s *layout) Apply(ctx context.Context, v Variant, prepared []Undo) ([]BenchTarget, error) {
	l, err := s.variantLayout(v)
	if err != nil {
		return nil, err
	}
	if l.Backend == "" {
		ts := make([]BenchTarget, 0, len(v.Disks))
		for _, d := range v.Disks {
			ts = append(ts, BenchTarget{Kind: TargetDrive, Target: d.Target, Want: LevelNone})
		}
		return ts, nil
	}
	b := s.backend(l.Backend)
	if len(prepared) != 1 || prepared[0].Array == nil || prepared[0].Array.Backend != b.Name() {
		return nil, fmt.Errorf("no prepared %s record for %s", b.Name(), v.Value.ID)
	}
	ts, err := b.Apply(ctx, v, l, prepared[0].Array)
	for i := range ts {
		ts[i].Backend = b.Name()
	}
	return ts, err
}

// WaitSteady hands the targets of each backend to that backend; drive
// targets are steady.
func (s *layout) WaitSteady(ctx context.Context, ts []BenchTarget, progress func(pct float64)) error {
	var order []string
	groups := map[string][]BenchTarget{}
	for _, t := range ts {
		if t.Backend == "" {
			continue
		}
		if _, ok := groups[t.Backend]; !ok {
			order = append(order, t.Backend)
		}
		groups[t.Backend] = append(groups[t.Backend], t)
	}
	for _, name := range order {
		b := s.backend(name)
		if b == nil {
			return fmt.Errorf("unknown backend %q", name)
		}
	}
	for _, name := range order {
		if err := s.backend(name).WaitSteady(ctx, groups[name], progress); err != nil {
			return err
		}
	}
	return nil
}

// Read: a drive target reads "none" when neither the disk nor its
// partitions have holders, else "member of <holder>"; a composite is read
// by its backend.
func (s *layout) Read(ctx context.Context, t BenchTarget) (Reading, error) {
	if t.Backend == "" {
		hs, err := s.holders(t.Target.Name)
		if err != nil {
			return Reading{}, err
		}
		v := LevelNone
		if len(hs) > 0 {
			v = "member of " + strings.Join(hs, ", ")
		}
		return Reading{Value: v, At: time.Now()}, nil
	}
	b := s.backend(t.Backend)
	if b == nil {
		return Reading{}, fmt.Errorf("unknown backend %q", t.Backend)
	}
	return b.Read(ctx, t)
}

// holders are the holders of disk and of its partitions, sorted.
func (s *layout) holders(disk string) ([]string, error) {
	if disk == "" || strings.ContainsAny(disk, "/") {
		return nil, fmt.Errorf("bad disk name %q", disk)
	}
	dir := filepath.Join(s.env.Sys, "block", disk)
	list := func(d string) ([]string, error) {
		es, err := os.ReadDir(filepath.Join(d, "holders"))
		if err != nil {
			return nil, err
		}
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		return out, nil
	}
	hs, err := list(dir)
	if err != nil {
		return nil, fmt.Errorf("read the holders of %s: %w", disk, err)
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the partitions of %s: %w", disk, err)
	}
	for _, e := range es {
		p := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(p, "partition")); err != nil {
			continue
		}
		ph, err := list(p)
		if err != nil {
			return nil, fmt.Errorf("read the holders of %s: %w", e.Name(), err)
		}
		hs = append(hs, ph...)
	}
	slices.Sort(hs)
	return slices.Compact(hs), nil
}

// Undo tears the layout down with its backend, then lets udev settle so
// that the members' device nodes are back before the next run.
func (s *layout) Undo(ctx context.Context, u Undo) error {
	if u.Array == nil {
		return errors.New("not a layout record")
	}
	b := s.backend(u.Array.Backend)
	if b == nil {
		return fmt.Errorf("unknown backend %q; nothing torn down", u.Array.Backend)
	}
	if err := b.Undo(ctx, *u.Array); err != nil {
		return err
	}
	if _, err := s.env.LookPath("udevadm"); err == nil {
		secs := max(int(s.env.Settle/time.Second), 1)
		if _, err := s.env.Run(ctx, "udevadm", "settle", "--timeout="+strconv.Itoa(secs)); err != nil {
			slog.Warn("udevadm settle failed after a layout teardown", "array", u.Array.Name, "err", err)
		}
	}
	return nil
}

// UndoCommand is the backend's teardown command, "" for an unknown backend.
func (s *layout) UndoCommand(u Undo) string {
	if u.Array == nil {
		return ""
	}
	b := s.backend(u.Array.Backend)
	if b == nil {
		return ""
	}
	return b.UndoCommand(*u.Array)
}
