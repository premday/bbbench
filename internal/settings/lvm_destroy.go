package settings

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	_ Destroyer         = (*lvm)(nil)
	_ LeftoverCommander = (*lvm)(nil)
)

// Extra keys of the lvm entries, next to ExtraOwnedVG, ExtraOwnedNodes and
// ExtraOrphanPV.
const (
	// lvmExtraVGUUID is the uuid of the volume group: Destroy requires it
	// unchanged.
	lvmExtraVGUUID = "vg_uuid"
	// lvmExtraPVs is the comma list of the VG's PV paths, sorted: the only
	// devices whose label Destroy removes.
	lvmExtraPVs = "pvs"
	// lvmExtraLVs is the comma list of the VG's visible LVs, sorted.
	lvmExtraLVs = "lvs"
	// lvmExtraPVUUID and lvmExtraPVPath identify an orphan PV.
	lvmExtraPVUUID = "pv_uuid"
	lvmExtraPVPath = "pv_path"
)

var (
	// lvmVGNameRe is a VG name Destroy passes to lvm: LVM's characters,
	// not starting with "-" (never read as a flag).
	lvmVGNameRe = regexp.MustCompile(`^[A-Za-z0-9_.+][A-Za-z0-9_.+-]*$`)
	// lvmPVPathRe is a PV path Destroy passes to lvm: /dev/ and segments
	// that do not start with "." or "-" (no "..", never read as a flag).
	lvmPVPathRe = regexp.MustCompile(`^/dev/[A-Za-z0-9_:+][A-Za-z0-9_.:+-]*(/[A-Za-z0-9_:+][A-Za-z0-9_.:+-]*)*$`)
)

// The listing commands of Existing. --foreign and --shared make lvm show
// the VGs of another host's system ID and shared (lvmlockd) VGs too, so
// that they are listed as Blocked instead of passing unseen (their PVs
// as orphans).
//
// UNVERIFIED on hardware: --foreign and --shared on vgs, lvs and pvs, the
// vg_systemid and vg_shared fields, and the "system ID:" line of
// lvm systemid.
var (
	lvmVGsArgs = []string{"vgs", "--foreign", "--shared", "--reportformat", "json", "--units", "b", "--nosuffix", "-o", "vg_name,vg_uuid,vg_attr,vg_size,vg_lock_type,vg_shared,vg_systemid,vg_missing_pv_count"}
	lvmLVsArgs = []string{"lvs", "-a", "--foreign", "--shared", "--reportformat", "json", "-o", "vg_uuid,lv_name,lv_attr"}
	lvmPVsArgs = []string{"pvs", "--foreign", "--shared", "--reportformat", "json", "--units", "b", "--nosuffix", "-o", "pv_name,pv_uuid,pv_size,vg_name,vg_uuid,vg_systemid"}
)

// lvmListing is one read of vgs, lvs -a, pvs and the local system ID
// (localKnown false when lvm systemid failed).
type lvmListing struct {
	vgs, lvs, pvs []map[string]string
	local         string
	localKnown    bool
}

func (b *lvm) listing(ctx context.Context) (*lvmListing, error) {
	l := &lvmListing{}
	for _, q := range []struct {
		args []string
		kind string
		dst  *[]map[string]string
	}{{lvmVGsArgs, "vg", &l.vgs}, {lvmLVsArgs, "lv", &l.lvs}, {lvmPVsArgs, "pv", &l.pvs}} {
		out, err := b.run(ctx, q.args...)
		if err != nil {
			return nil, err
		}
		rows, err := lvReport(out, q.kind)
		if err != nil {
			return nil, err
		}
		*q.dst = rows
	}
	l.local, l.localKnown = b.systemID(ctx)
	return l, nil
}

// systemID is the local LVM system ID ("" when the host has none); ok is
// false when it cannot be read.
func (b *lvm) systemID(ctx context.Context) (string, bool) {
	out, err := b.env.Run(ctx, "lvm", "systemid")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if _, v, ok := strings.Cut(line, "system ID:"); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// foreignVG is why a VG with system ID sys is another host's, "" when it
// is this host's. When the local ID cannot be read, any system ID is.
func (l *lvmListing) foreignVG(sys string) string {
	sys = strings.TrimSpace(sys)
	if sys == "" || (l.localKnown && sys == l.local) {
		return ""
	}
	return fmt.Sprintf("volume group of another host (system ID %s); destroy it by hand on that host", sys)
}

// pvOrphan is whether a pvs row names no VG at all (name, uuid, system ID).
func pvOrphan(r map[string]string) bool {
	return r["vg_name"] == "" && r["vg_uuid"] == "" && r["vg_systemid"] == ""
}

// dmByVG maps the dash-less VG uuid to the dm nodes whose dm uuid is
// "LVM-<vg uuid><lv uuid>[-suffix]", sorted. That covers every dm node of a
// VG, hidden and internal ones included (thin pool _tdata/_tmeta and
// -tpool, raid _rimage/_rmeta, mirror logs, snapshot -real/-cow).
//
// UNVERIFIED on hardware: the dm uuid form of LVM ("LVM-" + 32 characters
// of the VG uuid without dashes + 32 of the LV uuid + an optional suffix).
func (b *lvm) dmByVG() (map[string][]string, error) {
	block := filepath.Join(b.env.Sys, "block")
	es, err := os.ReadDir(block)
	if err != nil {
		return nil, fmt.Errorf("list the block devices: %w", err)
	}
	out := map[string][]string{}
	for _, e := range es {
		n := e.Name()
		if !strings.HasPrefix(n, "dm-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(block, n, "dm", "uuid"))
		if err != nil {
			continue // not attributable: the gate sees it as a foreign holder
		}
		u, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "LVM-")
		if !ok || len(u) < 32 {
			continue
		}
		out[u[:32]] = append(out[u[:32]], n)
	}
	for k := range out {
		slices.SortFunc(out[k], cmpNode)
	}
	return out, nil
}

// cmpNode orders dm-2 before dm-10.
func cmpNode(a, b string) int {
	na, ea := strconv.Atoi(strings.TrimPrefix(a, "dm-"))
	nb, eb := strconv.Atoi(strings.TrimPrefix(b, "dm-"))
	if ea == nil && eb == nil && na != nb {
		return na - nb
	}
	return strings.Compare(a, b)
}

func dashless(u string) string { return strings.ReplaceAll(strings.TrimSpace(u), "-", "") }

// Existing lists every volume group (active or not) and every orphan
// physical volume. It changes nothing.
//
// What it recognises but must not destroy is listed with Blocked set: a VG
// with missing PVs, a shared or clustered VG, an exported VG, two VGs of the
// same name, a VG whose PVs are listed but that vgs does not show (foreign
// system ID), a name or path that cannot be passed safely to lvm.
func (b *lvm) Existing(ctx context.Context) ([]Existing, error) {
	l, err := b.listing(ctx)
	if err != nil {
		return nil, err
	}
	dms, err := b.dmByVG()
	if err != nil {
		return nil, err
	}
	names := map[string]int{}
	listed := map[string]bool{}
	for _, v := range l.vgs {
		names[v["vg_name"]]++
		listed[v["vg_uuid"]] = true
	}
	vgs := slices.Clone(l.vgs)
	slices.SortFunc(vgs, func(a, c map[string]string) int {
		return strings.Compare(a["vg_name"]+"\x00"+a["vg_uuid"], c["vg_name"]+"\x00"+c["vg_uuid"])
	})
	var out []Existing
	for _, v := range vgs {
		var lvs, pvs []map[string]string
		for _, r := range l.lvs {
			if r["vg_uuid"] == v["vg_uuid"] {
				lvs = append(lvs, r)
			}
		}
		for _, r := range l.pvs {
			if r["vg_uuid"] == v["vg_uuid"] && r["vg_name"] == v["vg_name"] {
				pvs = append(pvs, r)
			}
		}
		e := b.existingVG(v, lvs, pvs, dms[dashless(v["vg_uuid"])], names[v["vg_name"]] > 1)
		if why := l.foreignVG(v["vg_systemid"]); why != "" {
			e.Blocked = why
		}
		out = append(out, e)
	}

	// PVs of a VG vgs does not show, then orphan PVs. A PV that names a
	// VG in any way (name, uuid, system ID) is never an orphan.
	foreign := map[string][]string{}
	var orphans []map[string]string
	for _, r := range l.pvs {
		switch {
		case pvOrphan(r):
			orphans = append(orphans, r)
		case r["vg_name"] == "" || !listed[r["vg_uuid"]] || names[r["vg_name"]] == 0:
			key := r["vg_name"]
			if key == "" {
				key = r["vg_uuid"]
			}
			if key == "" {
				key = filepath.Base(r["pv_name"])
			}
			foreign[key] = append(foreign[key], r["pv_name"])
		}
	}
	for _, name := range slices.Sorted(mapKeys(foreign)) {
		paths := slices.Sorted(slices.Values(foreign[name]))
		// Its ID never equals a VG name: it is tied to no VG uuid.
		e := Existing{Backend: b.Name(), ID: "foreign:" + name, Name: name,
			Summary: fmt.Sprintf("vg %s: not listed by vgs, %s", name, plural(len(paths), "PV", "PVs")),
			Disks:   b.disksOfPVs(paths), Stack: append([]string{"vg " + name}, pvLines(paths)...),
			Extra:   map[string]string{ExtraOwnedVG: name, lvmExtraPVs: strings.Join(paths, ",")},
			Blocked: "its physical volumes are listed but the volume group is not listed by vgs (foreign system ID or unreadable metadata); destroy it by hand"}
		out = append(out, e)
	}
	slices.SortFunc(orphans, func(a, c map[string]string) int { return strings.Compare(a["pv_name"], c["pv_name"]) })
	for _, r := range orphans {
		out = append(out, b.existingOrphan(r))
	}
	return out, nil
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func pvLines(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = "pv " + p
	}
	return out
}

func sizeOf(s string) string {
	n, err := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(s, "B")), 10, 64)
	if err != nil || n == 0 {
		return ""
	}
	return fmtBytes(n)
}

// existingVG is the entry of the volume group v. Stack holds only what
// does not change while the VG is used (its visible LVs, its PVs); the
// activity goes in Summary, which the gate does not compare. The owned
// nodes are the active dm nodes, so activating or deactivating an LV after
// the listing makes the gate refuse the entry as changed: list it again.
//
// UNVERIFIED on hardware: the vg_attr characters read (3 x exported, 4 p
// partial, 6 c clustered or s shared), vg_lock_type of a local VG ("" or
// "none") and the "[unknown]" pv_name of a missing PV.
func (b *lvm) existingVG(v map[string]string, lvs, pvs []map[string]string, dms []string, dup bool) Existing {
	name := v["vg_name"]
	var visible []string
	active := 0
	for _, r := range lvs {
		n := r["lv_name"]
		if strings.HasPrefix(n, "[") {
			continue // hidden sub-LV: goes with its LV
		}
		visible = append(visible, n)
		if lvActive(r["lv_attr"]) {
			active++
		}
	}
	slices.Sort(visible)
	var paths, known []string
	missing := false
	for _, r := range pvs {
		p := r["pv_name"]
		paths = append(paths, p)
		if p == "" || strings.Contains(p, "unknown") {
			missing = true
			continue
		}
		known = append(known, p)
	}
	slices.Sort(paths)
	slices.Sort(known)

	sum := fmt.Sprintf("vg %s: %s (%d active), %s", name, plural(len(visible), "LV", "LVs"), active, plural(len(paths), "PV", "PVs"))
	if sz := sizeOf(v["vg_size"]); sz != "" {
		sum += ", " + sz
	}
	stack := make([]string, 0, len(visible)+1+len(paths))
	for _, n := range visible {
		stack = append(stack, "lv "+name+"/"+n)
	}
	stack = append(append(stack, "vg "+name), pvLines(paths)...)
	e := Existing{Backend: b.Name(), ID: name, Name: name, Summary: sum,
		Disks: b.disksOfPVs(known), Stack: stack,
		Extra: map[string]string{ExtraOwnedVG: name, ExtraOwnedNodes: strings.Join(dms, ","),
			lvmExtraVGUUID: v["vg_uuid"], lvmExtraPVs: strings.Join(known, ","), lvmExtraLVs: strings.Join(visible, ",")}}

	attr := v["vg_attr"]
	at := func(i int) byte {
		if len(attr) > i {
			return attr[i]
		}
		return 0
	}
	lock := strings.TrimSpace(v["vg_lock_type"])
	shared := strings.TrimSpace(v["vg_shared"])
	miss, _ := strconv.Atoi(strings.TrimSpace(v["vg_missing_pv_count"]))
	switch {
	case !lvmVGNameRe.MatchString(name):
		e.Blocked = fmt.Sprintf("its name %q cannot be passed safely to lvm; destroy it by hand", name)
	case dup:
		e.Blocked = fmt.Sprintf("two volume groups are named %s; rename one by its uuid (vgrename <uuid> <new name>) first", name)
	case missing || miss > 0 || at(3) == 'p':
		e.Blocked = "volume group has missing physical volumes; fix it or remove it by hand (vgreduce --removemissing, then vgremove)"
	case at(5) == 'c' || at(5) == 's' || (lock != "" && lock != "none") || (shared != "" && shared != "0"):
		e.Blocked = "shared/clustered volume group; remove it by hand with its lock manager running"
	case at(2) == 'x':
		e.Blocked = "exported volume group (vgexport); import it with vgimport first or remove it by hand"
	case v["vg_uuid"] == "":
		e.Blocked = "cannot tell its uuid; destroy it by hand"
	case len(known) == 0:
		e.Blocked = "lists no physical volume; destroy it by hand"
	}
	for _, p := range known {
		if e.Blocked == "" && !lvmPVPathRe.MatchString(p) {
			e.Blocked = fmt.Sprintf("its physical volume path %q cannot be passed safely to lvm; destroy it by hand", p)
		}
	}
	if e.Blocked != "" {
		slog.Info("volume group listed but not destroyable", "vg", name, "why", e.Blocked)
	}
	return e
}

// existingOrphan is the entry of the orphan PV r, named after its node.
func (b *lvm) existingOrphan(r map[string]string) Existing {
	path := r["pv_name"]
	node := filepath.Base(path)
	sum := "orphan physical volume " + path
	if sz := sizeOf(r["pv_size"]); sz != "" {
		sum += ", " + sz
	}
	e := Existing{Backend: b.Name(), ID: "pv:" + r["pv_uuid"], Name: node, Summary: sum,
		Disks: b.disksOfPVs([]string{path}), Stack: []string{"pv " + path},
		Extra: map[string]string{ExtraOrphanPV: "yes", ExtraOwnedNodes: node,
			lvmExtraPVUUID: r["pv_uuid"], lvmExtraPVPath: path}}
	switch {
	case !mdDevRe.MatchString(path):
		e.Blocked = fmt.Sprintf("the physical volume %s is not a /dev/<kernel name> device; remove it by hand", path)
	case r["pv_uuid"] == "":
		e.Blocked = "cannot tell its uuid; remove it by hand"
	}
	return e
}

// disksOfPVs are the whole disks of the PV paths, once each, in order. A PV
// on an md or dm device, or a path that is not /dev/<node> of a disk or
// partition, gives a disk with only a name (the gate then refuses the entry
// for having no serial).
func (b *lvm) disksOfPVs(paths []string) []Target {
	var out []Target
	seen := map[string]bool{}
	for _, p := range paths {
		node := filepath.Base(p)
		t := Target{Name: node}
		if mdDevRe.MatchString(p) {
			if disk := wholeDiskIn(b.env.Sys, node); disk != "" && !b.composite(disk) {
				t = diskTargetIn(b.env.Sys, disk)
			}
		}
		if !seen[t.Name] {
			seen[t.Name] = true
			out = append(out, t)
		}
	}
	return out
}

// composite is whether the /sys/block node is an md or dm device.
func (b *lvm) composite(node string) bool {
	for _, sub := range []string{"md", "dm"} {
		if _, err := os.Stat(filepath.Join(b.env.Sys, "block", node, sub)); err == nil {
			return true
		}
	}
	return false
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// relist finds e again in a fresh listing and says why it is not the same
// configuration, "" when it is: same ID, not Blocked, same uuid, PVs and
// LVs, and what the gate compares (disks, stack, owned nodes).
func (b *lvm) relist(ctx context.Context, e Existing) string {
	es, err := b.Existing(ctx)
	if err != nil {
		return fmt.Sprintf("cannot list the volume groups again: %v", err)
	}
	var fresh *Existing
	for i := range es {
		if es[i].ID == e.ID {
			if fresh != nil {
				return fmt.Sprintf("%s is listed twice", e.ID)
			}
			fresh = &es[i]
		}
	}
	if fresh == nil {
		return fmt.Sprintf("%s is no longer listed", e.Name)
	}
	if fresh.Blocked != "" {
		return fresh.Blocked
	}
	if fresh.Extra[lvmExtraVGUUID] != e.Extra[lvmExtraVGUUID] {
		return fmt.Sprintf("its uuid is now %s, not %s", fresh.Extra[lvmExtraVGUUID], e.Extra[lvmExtraVGUUID])
	}
	if why := sameExisting(e, *fresh); why != "" {
		same := *fresh
		same.Extra = maps.Clone(fresh.Extra)
		same.Extra[ExtraOwnedNodes] = e.Extra[ExtraOwnedNodes]
		if sameExisting(e, same) == "" {
			return "its logical volumes were activated or deactivated since it was listed; list it again"
		}
		return fmt.Sprintf("it changed since it was listed (%s)", why)
	}
	for _, k := range []string{lvmExtraPVs, lvmExtraLVs, lvmExtraPVUUID, lvmExtraPVPath} {
		if fresh.Extra[k] != e.Extra[k] {
			return fmt.Sprintf("it changed since it was listed (%s %q is now %q)", k, e.Extra[k], fresh.Extra[k])
		}
	}
	return ""
}

// Destroy tears e down. A volume group: lvremove of all its LVs, check none
// is left, check its PVs are still the ones listed, vgremove, check it is
// gone, then per listed PV: check it is a PV of no VG with the same uuid,
// pvremove, check the label is gone; last, the devices-file entries of the
// PVs this call removed. An orphan PV: check, pvremove, check, its entry.
// Before anything it proves that the entry still describes the machine
// (same uuid, LVs, PVs, disks). It stops at the first step that fails and
// says what is done, what is left and the command for what is left (never
// a PV it could not prove). No force flag is used: lvremove refuses an
// open LV.
//
// UNVERIFIED on hardware: lvremove --yes of a whole VG (thin pools,
// snapshots, raid and mirror LVs, active LVs deactivated without a
// prompt), vgremove --yes, pvremove --yes on an orphan, and whether
// pvremove already removes the PV's devices-file entry (an entry still
// listed is removed with lvmdevices --deldev).
func (b *lvm) Destroy(ctx context.Context, e Existing) error {
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("%s: %s; nothing done", e.Name, fmt.Sprintf(format, a...))
	}
	if e.Blocked != "" {
		return refuse("%s", e.Blocked)
	}
	if e.Extra[ExtraOrphanPV] == "yes" {
		path, uuid := e.Extra[lvmExtraPVPath], e.Extra[lvmExtraPVUUID]
		if !mdDevRe.MatchString(path) || uuid == "" || e.ID != "pv:"+uuid {
			return refuse("not an orphan physical volume entry")
		}
		if why := b.relist(ctx, e); why != "" {
			return refuse("%s", why)
		}
		return b.destroyOrphan(ctx, e, path, uuid, refuse)
	}
	vg := ownedVG(e)
	if vg == "" || vg != e.ID || !lvmVGNameRe.MatchString(vg) || e.Extra[lvmExtraVGUUID] == "" {
		return refuse("not a volume group entry")
	}
	pvs := splitList(e.Extra[lvmExtraPVs])
	if len(pvs) == 0 {
		return refuse("lists no physical volume")
	}
	for _, p := range pvs {
		if !lvmPVPathRe.MatchString(p) {
			return refuse("bad physical volume path %q", p)
		}
	}
	if why := b.relist(ctx, e); why != "" {
		return refuse("%s", why)
	}
	if err := ctx.Err(); err != nil {
		return refuse("%v", err)
	}
	return b.destroyVG(ctx, e, vg, e.Extra[lvmExtraVGUUID], pvs, splitList(e.Extra[lvmExtraLVs]))
}

// Every mutating step below runs with context.WithoutCancel: a step that
// has started (lvremove, vgremove, pvremove) and its read-back finish even
// when ctx is cancelled; ctx is checked only between steps.

// lvmProgress is how far a VG teardown got.
type lvmProgress struct {
	lvsDone bool
	lvsLeft []string // nil when unknown
	vgDone  bool
	pvsDone []string
	pvsLeft []string // not yet removed
	pvsHand []string // of pvsLeft, those proven safe to name
	noHand  bool     // nothing may be named (the VG grew)
}

func (b *lvm) destroyVG(ctx context.Context, e Existing, vg, uuid string, pvs, lvs []string) error {
	// After a mutation, its checks run to completion even if ctx ends.
	bg := context.WithoutCancel(ctx)
	st := &lvmProgress{lvsLeft: lvs, pvsLeft: pvs, pvsHand: pvs}
	var removed []string
	fail := func(cause error) error {
		err := b.vgPartial(e, vg, st, cause)
		if derr := b.delDevicesEntries(bg, removed); derr != nil {
			err = fmt.Errorf("%w; %v", err, derr)
		}
		return err
	}

	if len(lvs) > 0 {
		if _, err := b.run(bg, "lvremove", "--yes", "--autobackup", "n", vg); err != nil {
			left, lerr := b.lvsLeft(bg, uuid)
			if lerr == nil {
				st.lvsLeft = left
			} else {
				st.lvsLeft = nil
			}
			return fail(err)
		}
	}
	left, err := b.lvsLeft(bg, uuid)
	if err != nil {
		st.lvsLeft = nil
		return fail(fmt.Errorf("cannot check that the logical volumes of %s are gone: %v", vg, err))
	}
	if len(left) > 0 {
		st.lvsLeft = left
		return fail(fmt.Errorf("logical volumes %s of %s are still there", strings.Join(left, ", "), vg))
	}
	st.lvsDone, st.lvsLeft = true, nil
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	// The PVs vgremove will release must be exactly the listed ones.
	pvRows, err := b.pvRows(ctx)
	if err != nil && ctx.Err() != nil {
		return fail(fmt.Errorf("cancelled before the physical volumes of %s were checked: %w", vg, ctx.Err()))
	}
	if err != nil {
		return fail(fmt.Errorf("cannot list the physical volumes of %s: %v", vg, err))
	}
	uuids := map[string]string{}
	var now []string
	for _, r := range pvRows {
		if r["vg_uuid"] == uuid {
			now = append(now, r["pv_name"])
			uuids[r["pv_name"]] = r["pv_uuid"]
		}
	}
	slices.Sort(now)
	if !slices.Equal(now, pvs) {
		st.noHand = true
		return fail(fmt.Errorf("the physical volumes of %s are now %s, not %s; vgremove not run", vg, strings.Join(now, ", "), strings.Join(pvs, ", ")))
	}

	// vgremove takes a name: the VG of that name must be this uuid now (a
	// rename and a new VG of the same name since the listing).
	if err := b.vgIs(ctx, vg, uuid); err != nil && ctx.Err() != nil {
		// Killed by the cancel: not a sign the VG changed; the command
		// checks the name and uuid first.
		return fail(fmt.Errorf("cancelled before the name of %s was checked: %w", vg, ctx.Err()))
	} else if err != nil {
		st.noHand = true
		return fail(fmt.Errorf("%v; vgremove not run", err))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if _, err := b.run(bg, "vgremove", "--yes", "--autobackup", "n", vg); err != nil {
		if still, cerr := b.vgUUIDExists(bg, uuid); cerr == nil && !still {
			st.vgDone = true
		}
		return fail(err)
	}
	still, err := b.vgUUIDExists(bg, uuid)
	if err != nil {
		return fail(fmt.Errorf("cannot check that volume group %s is gone: %v", vg, err))
	}
	if still {
		return fail(fmt.Errorf("volume group %s still exists after vgremove", vg))
	}
	st.vgDone = true

	for i, p := range pvs {
		st.pvsLeft, st.pvsHand = pvs[i:], pvs[i:]
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		isPV, other, pu, err := b.pvInfo(ctx, p)
		if err != nil && ctx.Err() != nil {
			// The cancel killed the read-only check: the PVs from p on are
			// not judged either way, so they stay in the command, behind
			// the check.
			return fail(fmt.Errorf("cancelled before %s was checked: %w", p, ctx.Err()))
		}
		if err != nil {
			// lvm cannot be read: no PV is named that was not checked.
			st.pvsHand = nil
			return fail(fmt.Errorf("%s could not be checked (%v); do not remove it before pvs shows it in no volume group", p, err))
		}
		if !isPV {
			// Gone, or the path now names another device: not proven
			// removed, so not counted and its devices entry kept.
			st.pvsHand = pvs[i+1:]
			return fail(fmt.Errorf("%s carries no LVM label: it may have been removed or the path may now name another device; check it by hand", p))
		}
		if other != "" {
			st.pvsHand = pvs[i+1:]
			return fail(fmt.Errorf("%s is now a physical volume of volume group %s; do not remove it", p, other))
		}
		if pu != uuids[p] {
			st.pvsHand = pvs[i+1:]
			return fail(fmt.Errorf("%s now carries physical volume %s, not %s; do not remove it", p, pu, uuids[p]))
		}
		if _, err := b.run(bg, "pvremove", "--yes", p); err != nil {
			return fail(err)
		}
		gone, err := b.pvGone(bg, p)
		if err != nil {
			return fail(err)
		}
		if !gone {
			return fail(fmt.Errorf("%s still carries an LVM label after pvremove", p))
		}
		removed = append(removed, p)
		st.pvsDone = append(st.pvsDone, p)
	}
	st.pvsLeft, st.pvsHand = nil, nil
	if err := b.delDevicesEntries(bg, removed); err != nil {
		return fmt.Errorf("%s: volume group removed and physical volume labels removed from %s; %v", e.Name, strings.Join(removed, ", "), err)
	}
	slog.Info("destroyed a volume group", "vg", vg, "uuid", uuid, "pvs", pvs)
	return nil
}

func (b *lvm) destroyOrphan(ctx context.Context, e Existing, path, uuid string, refuse func(string, ...any) error) error {
	bg := context.WithoutCancel(ctx)
	if err := ctx.Err(); err != nil {
		return refuse("%v", err)
	}
	isPV, vg, pu, err := b.pvInfo(ctx, path)
	switch {
	case err != nil:
		return refuse("cannot check %s: %v", path, err)
	case !isPV:
		return refuse("%s is no longer a physical volume", path)
	case vg != "":
		return refuse("%s is now a physical volume of volume group %s", path, vg)
	case pu != uuid:
		return refuse("%s now carries physical volume %s, not %s", path, pu, uuid)
	}
	hand := "rest by hand, " + orphanFirst(e.Name, "pvremove --yes "+shQuote(path))
	if _, err := b.run(bg, "pvremove", "--yes", path); err != nil {
		return fmt.Errorf("%s: label not removed; %v; %s", e.Name, err, hand)
	}
	gone, err := b.pvGone(bg, path)
	if err != nil {
		return fmt.Errorf("%s: %v; %s", e.Name, err, hand)
	}
	if !gone {
		return fmt.Errorf("%s: %s still carries an LVM label after pvremove; %s", e.Name, path, hand)
	}
	if err := b.delDevicesEntries(bg, []string{path}); err != nil {
		return fmt.Errorf("%s: label removed from %s; %v", e.Name, path, err)
	}
	slog.Info("destroyed an orphan physical volume", "pv", path, "uuid", uuid)
	return nil
}

// pvRows is the pvs listing of Existing.
func (b *lvm) pvRows(ctx context.Context) ([]map[string]string, error) {
	out, err := b.run(ctx, lvmPVsArgs...)
	if err != nil {
		return nil, err
	}
	return lvReport(out, "pv")
}

// vgIs checks that exactly one VG is named vg and that it has uuid.
func (b *lvm) vgIs(ctx context.Context, vg, uuid string) error {
	rows, err := b.vgRows(ctx)
	if err != nil {
		return fmt.Errorf("cannot list the volume groups: %v", err)
	}
	var got []string
	for _, r := range rows {
		if r["vg_name"] == vg {
			got = append(got, r["vg_uuid"])
		}
	}
	if len(got) != 1 || got[0] != uuid {
		return fmt.Errorf("the volume group named %s is not %s (uuids now: %s); it may have been renamed", vg, uuid, strings.Join(got, ", "))
	}
	return nil
}

// vgUUIDExists is whether a VG with uuid is listed.
func (b *lvm) vgUUIDExists(ctx context.Context, uuid string) (bool, error) {
	rows, err := b.vgRows(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(rows, func(r map[string]string) bool { return r["vg_uuid"] == uuid }), nil
}

func (b *lvm) vgRows(ctx context.Context) ([]map[string]string, error) {
	out, err := b.run(ctx, lvmVGsArgs...)
	if err != nil {
		return nil, err
	}
	return lvReport(out, "vg")
}

// pvInfo reports whether path is a PV, what VG it belongs to ("" for none:
// no VG name, uuid or system ID) and its uuid.
func (b *lvm) pvInfo(ctx context.Context, path string) (bool, string, string, error) {
	out, ok, err := b.exists(ctx, pvNotFound, "pvs", "--foreign", "--shared", "--reportformat", "json", "-o", "pv_name,pv_uuid,vg_name,vg_uuid,vg_systemid", path)
	if err != nil || !ok {
		return false, "", "", err
	}
	rows, err := lvReport(out, "pv")
	if err != nil {
		return false, "", "", err
	}
	if len(rows) != 1 {
		return false, "", "", fmt.Errorf("pvs %s lists %d physical volumes", path, len(rows))
	}
	r := rows[0]
	owner := ""
	switch {
	case r["vg_name"] != "":
		owner = r["vg_name"]
	case r["vg_uuid"] != "":
		owner = "with uuid " + r["vg_uuid"]
	case r["vg_systemid"] != "":
		owner = "of system ID " + r["vg_systemid"]
	}
	return true, owner, r["pv_uuid"], nil
}

// pvGone checks that path carries no PV after pvremove.
func (b *lvm) pvGone(ctx context.Context, path string) (bool, error) {
	still, _, _, err := b.pvInfo(ctx, path)
	if err != nil {
		return false, fmt.Errorf("cannot check the pvremove of %s: %v", path, err)
	}
	return !still, nil
}

// lvsLeft are the LVs (hidden ones included) of the VG with uuid.
func (b *lvm) lvsLeft(ctx context.Context, uuid string) ([]string, error) {
	out, err := b.run(ctx, "lvs", "-a", "--foreign", "--shared", "--reportformat", "json", "-o", "lv_name", "--select", "vg_uuid="+uuid)
	if err != nil {
		return nil, err
	}
	rows, err := lvReport(out, "lv")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range rows {
		names = append(names, r["lv_name"])
	}
	return names, nil
}

// vgPartial is the error of a VG teardown that stopped part way.
func (b *lvm) vgPartial(e Existing, vg string, st *lvmProgress, cause error) error {
	var parts []string
	switch {
	case st.lvsDone:
		parts = append(parts, "logical volumes removed")
	case st.lvsLeft != nil:
		parts = append(parts, "logical volumes not all removed (left: "+strings.Join(st.lvsLeft, ", ")+")")
	default:
		parts = append(parts, "logical volumes may not all be removed")
	}
	if st.vgDone {
		parts = append(parts, "volume group "+vg+" removed")
	} else {
		parts = append(parts, "volume group "+vg+" not removed")
	}
	if len(st.pvsDone) > 0 {
		parts = append(parts, "physical volume labels removed from "+strings.Join(st.pvsDone, ", "))
	} else {
		parts = append(parts, "no physical volume label removed")
	}
	if len(st.pvsLeft) > 0 {
		parts = append(parts, "left: physical volumes "+strings.Join(st.pvsLeft, ", "))
	}
	hand := "nothing proven is left to run by hand"
	if !st.noHand {
		if c := vgCommand(vg, !st.lvsDone, !st.vgDone, st.pvsHand); c != "" {
			if st.vgDone {
				hand = "rest by hand, " + orphanFirst("each", c)
			} else {
				hand = "rest by hand, " + vgFirst(vg, e.Extra[lvmExtraVGUUID], c)
			}
		}
	}
	return fmt.Errorf("%s: %s; %v; %s", e.Name, strings.Join(parts, "; "), cause, hand)
}

// vgFirst is cmd, which names vg, prefixed with the check that the VG of
// that name is still the one with uuid.
func vgFirst(vg, uuid, cmd string) string {
	return "after checking with vgs -o vg_name,vg_uuid that " + vg + " is " + uuid + ": " + cmd
}

// orphanFirst is cmd prefixed with the check to make before running it.
func orphanFirst(what, cmd string) string {
	if what == "each" {
		return "after checking with pvs that each is a physical volume of no volume group: " + cmd
	}
	return "after checking with pvs that it is a physical volume of no volume group: " + cmd
}

// vgCommand is "lvremove --yes <vg> && vgremove --yes <vg> && pvremove
// --yes <pvs>", with only the steps asked for; "" for a name that could be
// read as a flag or a path that is not under /dev.
func vgCommand(vg string, lvs, vgr bool, pvs []string) string {
	if (lvs || vgr) && (vg == "" || strings.HasPrefix(vg, "-")) {
		return ""
	}
	var cmds []string
	if lvs {
		cmds = append(cmds, "lvremove --yes "+shQuote(vg))
	}
	if vgr {
		cmds = append(cmds, "vgremove --yes "+shQuote(vg))
	}
	if len(pvs) > 0 {
		q := make([]string, len(pvs))
		for i, p := range pvs {
			if !strings.HasPrefix(p, "/dev/") {
				return ""
			}
			q[i] = shQuote(p)
		}
		cmds = append(cmds, "pvremove --yes "+strings.Join(q, " "))
	}
	return strings.Join(cmds, " && ")
}

// DestroyCommand is the shell command that tears e down by hand:
// "lvremove --yes data && vgremove --yes data && pvremove --yes /dev/sdb
// /dev/sdc1", or "pvremove --yes /dev/sdd1" for an orphan PV. It is "" for
// a Blocked entry: what the reason says not to run is never printed.
func (b *lvm) DestroyCommand(e Existing) string {
	if e.Blocked != "" {
		return ""
	}
	if e.Extra[ExtraOrphanPV] == "yes" {
		return vgCommand("", false, false, splitList(e.Extra[lvmExtraPVPath]))
	}
	vg := ownedVG(e)
	if vg == "" {
		return ""
	}
	return vgCommand(vg, len(splitList(e.Extra[lvmExtraLVs])) > 0, true, splitList(e.Extra[lvmExtraPVs]))
}

// LeftoverCommand is the command for what is left of confirmed, as fresh
// lists it after a successful Destroy: the same VG (name and uuid) or the
// same orphan PV (uuid and path), only PVs confirmed named, and lvremove
// only when every LV left was confirmed; "" otherwise or when Blocked.
func (b *lvm) LeftoverCommand(confirmed, fresh Existing) string {
	if confirmed.Blocked != "" || fresh.Blocked != "" {
		return ""
	}
	if confirmed.Extra[ExtraOrphanPV] == "yes" {
		if fresh.Extra[ExtraOrphanPV] != "yes" || fresh.Extra[lvmExtraPVUUID] == "" ||
			fresh.Extra[lvmExtraPVUUID] != confirmed.Extra[lvmExtraPVUUID] ||
			fresh.Extra[lvmExtraPVPath] != confirmed.Extra[lvmExtraPVPath] {
			return ""
		}
		return b.DestroyCommand(fresh)
	}
	vg := ownedVG(confirmed)
	if vg == "" || ownedVG(fresh) != vg || fresh.Extra[lvmExtraVGUUID] == "" ||
		fresh.Extra[lvmExtraVGUUID] != confirmed.Extra[lvmExtraVGUUID] {
		return ""
	}
	within := func(key string) bool {
		ok := splitList(confirmed.Extra[key])
		for _, x := range splitList(fresh.Extra[key]) {
			if !slices.Contains(ok, x) {
				return false
			}
		}
		return true
	}
	if !within(lvmExtraPVs) || !within(lvmExtraLVs) {
		return ""
	}
	c := vgCommand(vg, len(splitList(fresh.Extra[lvmExtraLVs])) > 0, true, splitList(fresh.Extra[lvmExtraPVs]))
	if c == "" {
		return ""
	}
	return vgFirst(vg, confirmed.Extra[lvmExtraVGUUID], c)
}
