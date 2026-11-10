package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"bbbench/internal/inuse"
)

// Existing is a configuration a provider found on the machine (an md array,
// an md superblock group, a volume group, an orphan physical volume) that
// the user may select for destruction.
type Existing struct {
	// Backend is the provider's Name; ID its stable key (md uuid, VG name).
	Backend, ID, Name, Summary string
	// Disks are the whole disks it sits on: kernel name, path, serial, model.
	Disks []Target
	// Stack is what is destroyed with it, top first, for the user.
	Stack []string
	// Extra is the provider's; the core reads only ExtraOwned and
	// ExtraOrphanPV.
	Extra map[string]string
}

const (
	// ExtraOwned is the comma list of the kernel nodes (md127, dm-3) and
	// the volume group name that make up the configuration. The safety gate
	// accepts holders and physical volumes only of these.
	ExtraOwned = "owned"
	// ExtraOrphanPV is "yes" on an orphan physical volume entry: an LVM
	// signature without a volume group on its disk is then its own.
	ExtraOrphanPV = "orphan_pv"
)

// Destroyer is the optional part of a Backend that lists and destroys
// configurations it did not build. Destroy is called only after the layout
// setting's safety gate, with the entry as just re-listed.
type Destroyer interface {
	Existing(ctx context.Context) ([]Existing, error)
	Destroy(ctx context.Context, e Existing) error
	// DestroyCommand is the command that tears e down by hand.
	DestroyCommand(e Existing) string
}

// Existing lists the configurations of every available backend that is a
// Destroyer. A provider that fails is reported as "<name>: <err>" and the
// others are still listed.
func (s *layout) Existing(ctx context.Context) ([]Existing, []error) {
	var out []Existing
	var errs []error
	for _, b := range s.backends {
		d, ok := b.(Destroyer)
		if !ok {
			continue
		}
		if ok, _ := b.Available(); !ok {
			continue
		}
		es, err := d.Existing(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", b.Name(), err))
			continue
		}
		for _, e := range es {
			if e.Backend != b.Name() {
				errs = append(errs, fmt.Errorf("%s: listed %q as backend %q; left out", b.Name(), e.ID, e.Backend))
				continue
			}
			out = append(out, e)
		}
	}
	return out, errs
}

// DestroyBlockers is the dry safety gate: why e may not be destroyed now,
// empty when it may. An error means it could not be checked (and so may not
// be destroyed either). Destroy runs exactly this check first.
func (s *layout) DestroyBlockers(ctx context.Context, e Existing) ([]string, error) {
	_, _, blockers, err := s.destroyGate(ctx, e)
	return blockers, err
}

// Destroy destroys the configuration the user selected, after the safety
// gate, then waits for udev and re-scans to prove the disks are free of it.
func (s *layout) Destroy(ctx context.Context, e Existing) error {
	d, fresh, blockers, err := s.destroyGate(ctx, e)
	if err != nil {
		return fmt.Errorf("%s not destroyed: %w", e.Name, err)
	}
	if len(blockers) > 0 {
		return fmt.Errorf("%s not destroyed: %s", e.Name, strings.Join(blockers, "; "))
	}
	byHand := func(what string) error {
		return fmt.Errorf("%s; to finish by hand: %s", what, d.DestroyCommand(fresh))
	}
	slog.Info("destroying an existing configuration", "backend", fresh.Backend, "name", fresh.Name, "id", fresh.ID, "disks", diskNames(fresh.Disks), "stack", fresh.Stack)
	if err := d.Destroy(ctx, fresh); err != nil {
		slog.Warn("destroying an existing configuration failed", "name", fresh.Name, "err", err)
		return byHand(fmt.Sprintf("destroying %s failed: %v", fresh.Name, err))
	}
	if _, err := s.env.LookPath("udevadm"); err == nil {
		secs := max(int(s.env.Settle/time.Second), 1)
		if _, err := s.env.Run(ctx, "udevadm", "settle", "--timeout="+strconv.Itoa(secs)); err != nil {
			slog.Warn("udevadm settle failed after destroying a configuration", "name", fresh.Name, "err", err)
		}
	}
	rep, err := s.env.InUse()
	if err != nil {
		return byHand(fmt.Sprintf("%s: cannot check that it is gone: %v", fresh.Name, err))
	}
	if rep.All != "" {
		return byHand(fmt.Sprintf("%s: cannot check that it is gone: %s", fresh.Name, rep.All))
	}
	owned := ownedSet(fresh)
	var left []string
	for _, c := range rep.Blockers(diskNames(fresh.Disks), func(c inuse.Cause) bool { return !isOwned(fresh, owned, c) }) {
		left = append(left, "disk "+c.Disk+": "+c.Text)
	}
	if len(left) > 0 {
		return byHand(fmt.Sprintf("%s is not fully gone: %s", fresh.Name, strings.Join(left, "; ")))
	}
	slog.Info("destroyed an existing configuration", "backend", fresh.Backend, "name", fresh.Name, "disks", diskNames(fresh.Disks))
	return nil
}

// destroyGate is steps (1) and (2) of the safety gate: re-list e and prove it
// is the one the user saw, then scan what uses its disks. It returns the
// provider and the re-listed entry. Blockers are reasons to refuse; an error
// means the check could not be made.
func (s *layout) destroyGate(ctx context.Context, e Existing) (Destroyer, Existing, []string, error) {
	b := s.backend(e.Backend)
	if b == nil {
		return nil, Existing{}, nil, fmt.Errorf("unknown provider %q", e.Backend)
	}
	d, ok := b.(Destroyer)
	if !ok {
		return nil, Existing{}, nil, fmt.Errorf("provider %q cannot destroy a configuration", e.Backend)
	}
	if ok, why := b.Available(); !ok {
		return nil, Existing{}, nil, fmt.Errorf("provider %q is not available: %s", e.Backend, why)
	}
	if err := checkExisting(e); err != nil {
		return nil, Existing{}, nil, err
	}

	// (1) Re-list: the same entry, on the same disks.
	es, err := d.Existing(ctx)
	if err != nil {
		return nil, Existing{}, nil, fmt.Errorf("cannot list the configurations again: %w", err)
	}
	var fresh *Existing
	for i := range es {
		if es[i].Backend == e.Backend && es[i].ID == e.ID {
			if fresh != nil {
				return nil, Existing{}, nil, fmt.Errorf("%s is listed twice; cannot tell which one was shown", e.Name)
			}
			fresh = &es[i]
		}
	}
	if fresh == nil {
		return d, Existing{}, []string{fmt.Sprintf("%s is no longer present", e.Name)}, nil
	}
	if why := sameExisting(e, *fresh); why != "" {
		return d, *fresh, []string{fmt.Sprintf("%s changed since it was shown (%s); list it again", e.Name, why)}, nil
	}
	if err := checkExisting(*fresh); err != nil {
		return nil, Existing{}, nil, err
	}

	// (2) Scan: nothing but the configuration itself may use its disks.
	rep, err := s.env.InUse()
	if err != nil {
		return nil, Existing{}, nil, fmt.Errorf("cannot check what uses the disks: %w", err)
	}
	owned := ownedSet(*fresh)
	var blockers []string
	for _, c := range rep.Blockers(diskNames(fresh.Disks), func(c inuse.Cause) bool { return isOwned(*fresh, owned, c) }) {
		if c.Kind == inuse.KindAll {
			blockers = append(blockers, c.Text)
			continue
		}
		blockers = append(blockers, "disk "+c.Disk+": "+c.Text)
	}
	for _, node := range slices.Sorted(maps.Keys(owned)) {
		hs, err := os.ReadDir(filepath.Join(s.env.Sys, "block", node, "holders"))
		if errors.Is(err, os.ErrNotExist) {
			if _, serr := os.Stat(filepath.Join(s.env.Sys, "block", node)); errors.Is(serr, os.ErrNotExist) {
				continue // not a block node (a VG name) or not assembled
			}
		}
		if err != nil {
			blockers = append(blockers, fmt.Sprintf("cannot read what sits on %s: %v", node, err))
			continue
		}
		for _, h := range hs {
			if !owned[h.Name()] {
				blockers = append(blockers, fmt.Sprintf("stacked: %s sits on %s", h.Name(), node))
			}
		}
	}
	return d, *fresh, blockers, nil
}

// checkExisting refuses an entry the gate cannot judge.
func checkExisting(e Existing) error {
	if len(e.Disks) == 0 {
		return fmt.Errorf("%s lists no disks", e.Name)
	}
	seen := map[string]bool{}
	for _, t := range e.Disks {
		if t.Name == "" || strings.ContainsAny(t.Name, "/ ") {
			return fmt.Errorf("%s lists a disk with a bad name %q", e.Name, t.Name)
		}
		if t.Serial == "" {
			return fmt.Errorf("%s: disk %s has no serial; cannot prove it is the disk shown", e.Name, t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("%s lists disk %s twice", e.Name, t.Name)
		}
		seen[t.Name] = true
	}
	for n := range ownedSet(e) {
		if strings.ContainsAny(n, "/ ") || n == "." || n == ".." {
			return fmt.Errorf("%s: bad owned name %q", e.Name, n)
		}
	}
	return nil
}

// sameExisting says how fresh differs from what the user saw, "" when it
// does not: name, disk set (kernel name, path, serial, model) and owned set.
func sameExisting(saw, fresh Existing) string {
	if saw.Name != fresh.Name {
		return fmt.Sprintf("name %s is now %s", saw.Name, fresh.Name)
	}
	key := func(t Target) string { return t.Name + "\x00" + t.Path + "\x00" + t.Serial + "\x00" + t.Model }
	a := make([]string, 0, len(saw.Disks))
	for _, t := range saw.Disks {
		a = append(a, key(t))
	}
	b := make([]string, 0, len(fresh.Disks))
	for _, t := range fresh.Disks {
		b = append(b, key(t))
	}
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return fmt.Sprintf("disks %s are now %s", describeDisks(saw.Disks), describeDisks(fresh.Disks))
	}
	if !maps.Equal(ownedSet(saw), ownedSet(fresh)) {
		return "what it is made of differs"
	}
	if (saw.Extra[ExtraOrphanPV] == "yes") != (fresh.Extra[ExtraOrphanPV] == "yes") {
		return "its kind differs"
	}
	return ""
}

func describeDisks(ts []Target) string {
	var s []string
	for _, t := range ts {
		s = append(s, fmt.Sprintf("%s (%s %s)", t.Name, t.Model, t.Serial))
	}
	return strings.Join(s, ", ")
}

func diskNames(ts []Target) []string {
	n := make([]string, 0, len(ts))
	for _, t := range ts {
		n = append(n, t.Name)
	}
	return n
}

// ownedSet is Extra[ExtraOwned] as a set.
func ownedSet(e Existing) map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Split(e.Extra[ExtraOwned], ",") {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

// isOwned is the gate's rule, deny by default: only a holder that is one of
// e's own nodes, an md superblock on one of e's disks, and a physical volume
// of e's volume group (or an orphan one when e is that orphan PV) belong to
// e. A mount, swap, bbbench path, other signature or unjudgeable report never
// does, wherever it sits.
func isOwned(e Existing, owned map[string]bool, c inuse.Cause) bool {
	switch c.Kind {
	case inuse.KindHolder:
		return c.Device != "" && owned[c.Device]
	case inuse.KindMD:
		return slices.ContainsFunc(e.Disks, func(t Target) bool { return t.Name == c.Disk })
	case inuse.KindPV:
		if c.Via != "" {
			return owned[c.Via]
		}
		return e.Extra[ExtraOrphanPV] == "yes" && slices.ContainsFunc(e.Disks, func(t Target) bool { return t.Name == c.Disk })
	}
	return false
}
