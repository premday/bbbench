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
//
// Provider convention for Extra (the core reads only these keys; the gate
// compares the whole map, so a provider keeps only stable values there and
// anything that changes on its own, such as resync progress, in Summary):
//   - md array or superblock group: ExtraKind = KindMDValue, ID = the array
//     uuid, ExtraOwnedNodes = the md node ("md127") when assembled.
//   - volume group: ExtraOwnedVG = the VG name, ExtraOwnedNodes = every dm
//     node of the VG ("dm-0,dm-1"; lvs -a, hidden and internal sub-LVs
//     included: thin pool data and metadata, raid images, mirror logs,
//     snapshot -real/-cow), Disks = the disks of all its PVs.
//   - orphan physical volume: ExtraOrphanPV = "yes", ExtraOwnedNodes = the
//     PV's node ("sdb1", or "sdb" for a whole-disk PV).
type Existing struct {
	// Backend is the provider's Name; ID its stable key (md uuid, VG name).
	Backend, ID, Name, Summary string
	// Disks are the whole disks it sits on: kernel name, path, serial, model.
	Disks []Target
	// Stack is what is destroyed with it, top first, for the user.
	Stack []string
	// Extra is the provider's; see the convention above.
	Extra map[string]string
	// Blocked is set by a provider for a configuration it recognises but
	// must not or cannot destroy: the entry is listed, shown grayed with
	// this reason, and every gate path refuses it.
	Blocked string
}

const (
	// ExtraOwnedNodes is the comma list of the kernel nodes (md127, dm-3,
	// or the node of an orphan PV) that make up the configuration.
	ExtraOwnedNodes = "owned_nodes"
	// ExtraOwnedVG is the name of the configuration's volume group.
	ExtraOwnedVG = "owned_vg"
	// ExtraKind says what the entry is; KindMDValue for md, whose ID is
	// then the array uuid that its members' superblocks carry.
	ExtraKind   = "kind"
	KindMDValue = "md"
	// ExtraOrphanPV is "yes" on an orphan physical volume entry.
	ExtraOrphanPV = "orphan_pv"
)

// maxStackDepth bounds the walk over what is stacked on the owned nodes.
const maxStackDepth = 16

// Destroyer is the optional part of a Backend that lists and destroys
// configurations it did not build. Destroy is called only after the layout
// setting's safety gate, with the entry as just re-listed.
//
// The gate is a snapshot: it checks before Destroy is called, and something
// can open or stack on the configuration in between. A provider must fail
// closed itself: use the tool's own refusals (the provider's stop or remove
// tool refusing an open array or volume, never a force flag), act only on the
// members of the entry it was given and stop at the first step that fails.
// Each atomic tool step runs to completion: once a mutation has started, it
// and the read-back that verifies it are not abandoned when ctx is
// cancelled (run both with context.WithoutCancel: Env.Run kills a command
// whose ctx ends); ctx is checked only between steps.
//
// A Destroy error must say what is done, what is left and the command for
// what is left: the gate adds no command of its own to it. DestroyCommand
// is shown by the gate only when Destroy succeeded and the re-scan still
// finds part of the configuration, built from a fresh listing. A provider
// lists what it recognises but must not destroy with Blocked set.
type Destroyer interface {
	Existing(ctx context.Context) ([]Existing, error)
	Destroy(ctx context.Context, e Existing) error
	// DestroyCommand is the command that tears e down by hand, "" for a
	// Blocked entry.
	DestroyCommand(e Existing) string
}

// LeftoverCommander is the optional part of a Destroyer that gives the
// manual command for what a successful Destroy left of confirmed, as fresh
// now lists it, limited to what confirmed named (and "" for a Blocked
// entry or when nothing confirmed is left). The gate uses it instead of
// DestroyCommand(fresh), which could name what the user never confirmed.
type LeftoverCommander interface {
	LeftoverCommand(confirmed, fresh Existing) string
}

// Existing lists the configurations of every backend that is a Destroyer. A
// provider that fails or is not available is reported as "<name>: <why>" and
// the others are still listed.
func (s *layout) Existing(ctx context.Context) ([]Existing, []error) {
	var out []Existing
	var errs []error
	for _, b := range s.backends {
		d, ok := b.(Destroyer)
		if !ok {
			continue
		}
		if ok, why := b.Available(); !ok {
			errs = append(errs, fmt.Errorf("%s: not available: %s", b.Name(), why))
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
// empty when it may. An error means it could not be checked (a listing or
// scan failed), so it may not be destroyed either. Destroy runs exactly this
// check first.
func (s *layout) DestroyBlockers(ctx context.Context, e Existing) ([]string, error) {
	_, blockers, err := s.destroyGate(ctx, e)
	return blockers, err
}

// gated is what the gate proved: the provider, the entry as re-listed and
// the nodes the gate looked at (disks, owned nodes and their partitions).
type gated struct {
	d     Destroyer
	fresh Existing
	nodes map[string]bool // owned nodes
	vg    string          // owned VG
	names []string
}

// Destroy destroys the configuration the user selected, after the safety
// gate, then waits for udev and re-scans to prove that nothing of it is left.
func (s *layout) Destroy(ctx context.Context, e Existing) error {
	g, blockers, err := s.destroyGate(ctx, e)
	if err != nil {
		return fmt.Errorf("%s not destroyed: %w", e.Name, err)
	}
	if len(blockers) > 0 {
		return fmt.Errorf("%s not destroyed: %s", e.Name, strings.Join(blockers, "; "))
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s not destroyed: %w", e.Name, err)
	}
	fresh := g.fresh
	slog.Info("destroying an existing configuration", "backend", fresh.Backend, "name", fresh.Name, "id", fresh.ID, "disks", diskNames(fresh.Disks), "stack", fresh.Stack)
	perr := g.d.Destroy(ctx, fresh)
	if perr != nil {
		slog.Warn("destroying an existing configuration failed", "name", fresh.Name, "err", perr)
	}

	// From here on the outcome must be reported even if ctx is cancelled.
	bg := context.WithoutCancel(ctx)
	if _, err := s.env.LookPath("udevadm"); err == nil {
		secs := max(int(s.env.Settle/time.Second), 1)
		if _, err := s.env.Run(bg, "udevadm", "settle", "--timeout="+strconv.Itoa(secs)); err != nil {
			slog.Warn("udevadm settle failed after destroying a configuration", "name", fresh.Name, "err", err)
		}
	}
	left, lerr := s.leftOver(g)
	var what []string
	if perr != nil {
		what = append(what, fmt.Sprintf("destroying %s failed: %v", fresh.Name, perr))
	}
	if lerr != nil {
		what = append(what, fmt.Sprintf("%s: cannot check that it is gone: %v", fresh.Name, lerr))
	} else if len(left) > 0 {
		what = append(what, fmt.Sprintf("%s is not fully gone: %s", fresh.Name, strings.Join(left, "; ")))
	}
	if len(what) == 0 {
		slog.Info("destroyed an existing configuration", "backend", fresh.Backend, "name", fresh.Name, "disks", diskNames(fresh.Disks))
		return nil
	}
	// A provider that failed says itself what is done and what to run by
	// hand; a command built here would repeat steps already done.
	if perr != nil {
		return errors.New(strings.Join(what, "; "))
	}
	// The provider succeeded but something is left: the command comes from
	// what is listed now, not from what was there before.
	return fmt.Errorf("%s; %s", strings.Join(what, "; "), s.finishCommand(bg, g.d, fresh))
}

// finishCommand is the manual command for what is left of e, the entry
// the user confirmed, from a fresh listing. It never names more than was
// confirmed: a provider that is a LeftoverCommander limits the command
// itself; otherwise a fresh entry on other disks gets no command. A fresh
// entry that is now Blocked gets its reason and no command.
func (s *layout) finishCommand(ctx context.Context, d Destroyer, e Existing) string {
	const again = "list the configurations again to see what is left"
	es, err := d.Existing(ctx)
	if err != nil {
		return fmt.Sprintf("cannot list the configurations again (%v); %s", err, again)
	}
	for _, x := range es {
		if x.Backend != e.Backend || x.ID != e.ID {
			continue
		}
		if x.Blocked != "" {
			return fmt.Sprintf("%s is now listed as not destroyable: %s; nothing to run by hand", x.Name, x.Blocked)
		}
		if lc, ok := d.(LeftoverCommander); ok {
			if c := lc.LeftoverCommand(e, x); c != "" {
				return "to finish by hand, " + c
			}
			return "nothing of what was confirmed is left to run by hand; " + again
		}
		if !disksWithin(x.Disks, e.Disks) {
			return "it is now listed on disks that were not confirmed; " + again
		}
		if c := d.DestroyCommand(x); c != "" {
			return "to finish by hand: " + c
		}
	}
	return "it is no longer listed; " + again
}

// disksWithin is whether every disk of a is one of b (name, path, serial,
// model).
func disksWithin(a, b []Target) bool {
	for _, t := range a {
		if !slices.ContainsFunc(b, func(u Target) bool {
			return u.Name == t.Name && u.Path == t.Path && u.Serial == t.Serial && u.Model == t.Model
		}) {
			return false
		}
	}
	return true
}

// leftOver re-scans and lists what the configuration owned that is still
// there: holders that are its nodes, its md superblocks, its PV labels, and
// its nodes in sysfs that still have members or holders (nodeLeft).
func (s *layout) leftOver(g *gated) ([]string, error) {
	rep, err := s.env.InUse()
	if err != nil {
		return nil, err
	}
	if rep.All != "" {
		return nil, errors.New(rep.All)
	}
	e := g.fresh
	var left []string
	for _, c := range rep.Blockers(g.names, func(c inuse.Cause) bool { return !wasOwned(e, g.nodes, g.vg, c) }) {
		left = append(left, "disk "+c.Disk+": "+c.Text)
	}
	if e.Extra[ExtraOrphanPV] != "yes" {
		for _, n := range slices.Sorted(maps.Keys(g.nodes)) {
			if why := s.nodeLeft(n); why != "" {
				left = append(left, why)
			}
		}
	}
	return left, nil
}

// nodeLeft says how owned node n is still there after a destroy, "" when it
// is gone. The kernel may keep the node of a stopped device for a while: a
// node with no member under slaves/ and nothing under holders/ is gone. One
// whose slaves/ cannot be read is counted as left.
func (s *layout) nodeLeft(n string) string {
	dir := filepath.Join(s.env.Sys, "block", n)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return ""
	}
	slaves, err := os.ReadDir(filepath.Join(dir, "slaves"))
	if err != nil {
		return n + " still exists"
	}
	holders, err := os.ReadDir(filepath.Join(dir, "holders"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return n + " still exists"
	}
	var on []string
	for _, e := range slaves {
		on = append(on, e.Name())
	}
	var held []string
	for _, e := range holders {
		held = append(held, e.Name())
	}
	switch {
	case len(on) > 0 && len(held) > 0:
		return fmt.Sprintf("%s still exists (on %s; %s sits on it)", n, strings.Join(on, ", "), strings.Join(held, ", "))
	case len(on) > 0:
		return fmt.Sprintf("%s still exists (on %s)", n, strings.Join(on, ", "))
	case len(held) > 0:
		return fmt.Sprintf("%s still exists (%s sits on it)", n, strings.Join(held, ", "))
	}
	return ""
}

// destroyGate is steps (1) and (2) of the safety gate: re-list e and prove it
// is the one the user saw, then scan what uses its disks and what is stacked
// on its nodes. Blockers are reasons to refuse; an error means the check
// could not be made.
func (s *layout) destroyGate(ctx context.Context, e Existing) (*gated, []string, error) {
	refuse := func(format string, a ...any) (*gated, []string, error) {
		return nil, []string{fmt.Sprintf(format, a...)}, nil
	}
	if e.Blocked != "" {
		return nil, []string{e.Blocked}, nil
	}
	b := s.backend(e.Backend)
	if b == nil {
		return refuse("unknown provider %q", e.Backend)
	}
	d, ok := b.(Destroyer)
	if !ok {
		return refuse("provider %q cannot destroy a configuration", e.Backend)
	}
	if ok, why := b.Available(); !ok {
		return refuse("provider %q is not available: %s", e.Backend, why)
	}
	if why := checkExisting(e); why != "" {
		return refuse("%s", why)
	}

	// (1) Re-list: the same entry, on the same disks, made of the same.
	es, err := d.Existing(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot list the configurations again: %w", err)
	}
	var fresh *Existing
	for i := range es {
		if es[i].Backend == e.Backend && es[i].ID == e.ID {
			if fresh != nil {
				return refuse("%s is listed twice; cannot tell which one was shown", e.Name)
			}
			fresh = &es[i]
		}
	}
	if fresh == nil {
		return refuse("%s is no longer present", e.Name)
	}
	if fresh.Blocked != "" {
		return nil, []string{fresh.Blocked}, nil
	}
	if why := sameExisting(e, *fresh); why != "" {
		return refuse("%s changed since it was shown (%s); list it again", e.Name, why)
	}
	if why := checkExisting(*fresh); why != "" {
		return refuse("%s", why)
	}
	g := &gated{d: d, fresh: *fresh, nodes: ownedNodes(*fresh), vg: ownedVG(*fresh)}

	// (2) Scan: nothing but the configuration itself may use its disks,
	// its nodes or their partitions, or sit on them.
	rep, err := s.env.InUse()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot check what uses the disks: %w", err)
	}
	var blockers []string
	if rep.PVsIncomplete && (g.vg != "" || g.fresh.Extra[ExtraOrphanPV] == "yes") {
		// A PV known only from udev has no volume group: a PV just moved
		// into another VG could pass as this one's or as an orphan.
		blockers = append(blockers, "the physical volumes could not all be listed (see the scan warnings); which volume group each physical volume belongs to cannot be proven")
	}
	stacked, walked, err := s.walkOwned(g.nodes)
	if err != nil {
		return nil, nil, err
	}
	g.names = append(diskNames(g.fresh.Disks), walked...)
	for _, c := range rep.Blockers(g.names, func(c inuse.Cause) bool { return isOwned(g.fresh, g.nodes, g.vg, c) }) {
		if c.Kind == inuse.KindAll {
			blockers = append(blockers, c.Text)
			continue
		}
		blockers = append(blockers, "disk "+c.Disk+": "+c.Text)
	}
	return g, append(blockers, stacked...), nil
}

// walkOwned walks what is stacked on each owned node and its partitions,
// through owned holders, recursively. A holder that is not owned is a
// blocker. It returns the nodes and partitions it saw.
func (s *layout) walkOwned(owned map[string]bool) (blockers, names []string, err error) {
	visited := map[string]bool{}
	var walk func(node string, depth int) error
	walk = func(node string, depth int) error {
		if visited[node] {
			return nil
		}
		visited[node] = true
		if depth > maxStackDepth {
			return fmt.Errorf("what sits on %s is stacked deeper than %d levels", node, maxStackDepth)
		}
		dir := filepath.Join(s.env.Sys, "block", node)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return nil // not assembled, or not a whole-disk node
		} else if err != nil {
			return fmt.Errorf("cannot read %s: %w", node, err)
		}
		names = append(names, node)
		dirs := []string{dir}
		subs, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", node, err)
		}
		for _, sub := range subs {
			if _, err := os.Stat(filepath.Join(dir, sub.Name(), "partition")); err == nil {
				dirs = append(dirs, filepath.Join(dir, sub.Name()))
				names = append(names, sub.Name())
			}
		}
		for _, dd := range dirs {
			hs, err := os.ReadDir(filepath.Join(dd, "holders"))
			if err != nil {
				return fmt.Errorf("cannot read what sits on %s: %w", filepath.Base(dd), err)
			}
			for _, h := range hs {
				if !owned[h.Name()] {
					blockers = append(blockers, fmt.Sprintf("stacked: %s sits on %s", h.Name(), filepath.Base(dd)))
					continue
				}
				if err := walk(h.Name(), depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, n := range slices.Sorted(maps.Keys(owned)) {
		if err := walk(n, 0); err != nil {
			return nil, nil, err
		}
	}
	return blockers, names, nil
}

// checkExisting is why the gate cannot judge an entry, "" when it can.
func checkExisting(e Existing) string {
	if len(e.Disks) == 0 {
		return fmt.Sprintf("%s lists no disks", e.Name)
	}
	seen := map[string]bool{}
	for _, t := range e.Disks {
		if !safeNode(t.Name) {
			return fmt.Sprintf("%s lists a disk with a bad name %q", e.Name, t.Name)
		}
		if t.Serial == "" {
			return fmt.Sprintf("%s: disk %s has no serial; cannot prove it is the disk shown", e.Name, t.Name)
		}
		if seen[t.Name] {
			return fmt.Sprintf("%s lists disk %s twice", e.Name, t.Name)
		}
		seen[t.Name] = true
	}
	for n := range ownedNodes(e) {
		if !safeNode(n) {
			return fmt.Sprintf("%s: bad owned name %q", e.Name, n)
		}
	}
	return ""
}

func safeNode(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/ \t\n")
}

// sameExisting says how fresh differs from what the user saw, "" when it
// does not: name, disk set (kernel name, path, serial, model), what it is
// made of (owned nodes, VG, kind, orphan flag), the whole Extra, Blocked and
// the stack the user confirmed. Summary is not compared (resync progress
// changes it): a provider puts nothing in Extra that changes on its own.
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
	if !maps.Equal(ownedNodes(saw), ownedNodes(fresh)) || ownedVG(saw) != ownedVG(fresh) {
		return "what it is made of differs"
	}
	if saw.Extra[ExtraKind] != fresh.Extra[ExtraKind] || (saw.Extra[ExtraOrphanPV] == "yes") != (fresh.Extra[ExtraOrphanPV] == "yes") {
		return "its kind differs"
	}
	// The provider's own keys too (a uuid, the member devices): a
	// configuration made again with the same names is another one.
	if !maps.Equal(saw.Extra, fresh.Extra) {
		return "what the provider records of it differs"
	}
	if saw.Blocked != fresh.Blocked {
		return "whether it may be destroyed differs"
	}
	if !slices.Equal(saw.Stack, fresh.Stack) {
		return fmt.Sprintf("what goes with it is now %s", strings.Join(fresh.Stack, ", "))
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

// ownedNodes is Extra[ExtraOwnedNodes] as a set.
func ownedNodes(e Existing) map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Split(e.Extra[ExtraOwnedNodes], ",") {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

func ownedVG(e Existing) string { return strings.TrimSpace(e.Extra[ExtraOwnedVG]) }

// normUUID makes a tool's "a1b2c3d4:..." and udev's "A1B2C3D4-..." compare.
func normUUID(u string) string {
	return strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(u)))
}

func onDisks(e Existing, disk string) bool {
	return slices.ContainsFunc(e.Disks, func(t Target) bool { return t.Name == disk })
}

// isOwned is the gate's rule, deny by default: only a holder that is one of
// e's own nodes, an md superblock with e's uuid on one of e's disks (md
// entries only), a physical volume of e's volume group on one of e's disks,
// and, for an orphan PV entry, the orphan PV on its own node belong to e. A
// mount, swap, bbbench path, other signature or unjudgeable report never
// does, wherever it sits. The md and PV kinds are the in-use scan's cause
// kinds (signatures any provider's disks can carry), not a provider: a
// provider whose ownership they do not describe needs its own rule here.
func isOwned(e Existing, nodes map[string]bool, vg string, c inuse.Cause) bool {
	switch c.Kind {
	case inuse.KindHolder:
		return c.Device != "" && nodes[c.Device]
	case inuse.KindMD:
		id := normUUID(e.ID)
		return e.Extra[ExtraKind] == KindMDValue && onDisks(e, c.Disk) &&
			id != "" && c.Via != "" && normUUID(c.Via) == id
	case inuse.KindPV:
		if !onDisks(e, c.Disk) {
			return false
		}
		if c.Via != "" {
			return vg != "" && c.Via == vg
		}
		return e.Extra[ExtraOrphanPV] == "yes" && c.Device != "" && nodes[c.Device]
	}
	return false
}

// wasOwned is the post-destroy rule: a cause that means part of e is still
// there. It is wider than isOwned where the scan cannot tell (an md
// superblock without a uuid, a PV label without a VG), so that a leftover is
// never reported as gone.
func wasOwned(e Existing, nodes map[string]bool, vg string, c inuse.Cause) bool {
	switch c.Kind {
	case inuse.KindHolder:
		return c.Device != "" && nodes[c.Device]
	case inuse.KindMD:
		return e.Extra[ExtraKind] == KindMDValue && onDisks(e, c.Disk) &&
			(c.Via == "" || normUUID(c.Via) == normUUID(e.ID))
	case inuse.KindPV:
		if !onDisks(e, c.Disk) {
			return false
		}
		if e.Extra[ExtraOrphanPV] == "yes" {
			return true
		}
		return vg != "" && (c.Via == vg || c.Via == "")
	}
	return false
}
