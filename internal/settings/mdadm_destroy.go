package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"bbbench/internal/blockdev"
)

var (
	_ Destroyer         = (*mdadm)(nil)
	_ LeftoverCommander = (*mdadm)(nil)
)

// Extra keys of the mdadm entries, next to ExtraKind and ExtraOwnedNodes.
const (
	// mdExtraAssembled is "yes" for an assembled array, "no" for a group of
	// superblocks that no assembled array carries.
	mdExtraAssembled = "assembled"
	// mdExtraMemberDevs is the comma list of the device nodes that carry
	// the superblocks ("/dev/sdb1,/dev/sdc1"): the only devices Destroy
	// zeroes.
	mdExtraMemberDevs = "member_devs"
)

// mdDevRe is a device node bbbench may zero: /dev/<kernel name>.
var mdDevRe = regexp.MustCompile(`^/dev/[A-Za-z0-9][A-Za-z0-9_.:+-]*$`)

// Existing lists every assembled md array (inactive ones and those with
// spares or a journal included) and every group of md superblocks, by
// uuid, that no assembled array carries. It changes nothing.
//
// What it recognises but must not destroy is listed with Blocked set: an
// array whose members cannot be read or whose uuid cannot be told,
// external metadata (IMSM, DDF: firmware RAID), metadata that is not md's
// own, superblocks without a readable uuid. An md node without members
// (nothing on any disk) is left out.
func (m *mdadm) Existing(ctx context.Context) ([]Existing, error) {
	nodes, err := m.mdNodes()
	if err != nil {
		return nil, err
	}
	var out []Existing
	assembled := map[string]int{} // normalised uuid -> index in out
	assembledDev := map[string]bool{}
	unreadable := ""
	for _, md := range nodes {
		ms, merr := m.members(md)
		if merr != nil {
			unreadable = md
		}
		if merr == nil && len(ms) == 0 {
			continue
		}
		for _, d := range ms {
			assembledDev["/dev/"+d.name] = true
		}
		var uuid, level string
		if merr == nil {
			uuid, level = m.arrayIdentity(ctx, md, ms)
		}
		if uuid != "" {
			assembled[normUUID(uuid)] = len(out)
		}
		out = append(out, m.existingArray(md, ms, merr, uuid, level))
	}

	groups, err := m.superblockGroups(ctx)
	if err != nil {
		return nil, err
	}
	var unassembled []Existing
	for _, g := range groups {
		if i, ok := assembled[normUUID(g.uuid)]; ok && g.uuid != "" {
			// Assembled wins; superblocks of its uuid outside it are stale.
			var stale []string
			for _, d := range g.devs {
				if !assembledDev[d] {
					stale = append(stale, d)
				}
			}
			if len(stale) == 1 {
				out[i].Summary += "; stale md superblock of " + out[i].Name + " on " + stale[0]
			} else if len(stale) > 1 {
				out[i].Summary += "; stale md superblocks of " + out[i].Name + " on " + strings.Join(stale, ", ")
			}
			continue
		}
		g.devs = slices.DeleteFunc(g.devs, func(d string) bool { return assembledDev[d] })
		if len(g.devs) == 0 {
			continue
		}
		if g.blocked == "" && unreadable != "" {
			g.blocked = fmt.Sprintf("the members of %s cannot be read; cannot tell whether these superblocks are in use", unreadable)
		}
		unassembled = append(unassembled, m.existingGroup(g))
	}
	nameGroups(unassembled)
	return append(out, unassembled...), nil
}

// nameGroups names each unassembled group "md-<uuid prefix>", the prefix
// at least 8 hex digits and long enough to be unique among the groups (it
// is what the user types to confirm). A group without uuid keeps its name.
func nameGroups(es []Existing) {
	for i := range es {
		u := normUUID(es[i].ID)
		if len(u) != 32 {
			continue
		}
		n := 8
		for j := range es {
			v := normUUID(es[j].ID)
			if j == i || len(v) != 32 {
				continue
			}
			for n < 32 && u[:n] == v[:n] {
				n++
			}
		}
		es[i].Name = "md-" + u[:n]
	}
}

// mdNodes are the <Sys>/block/md* nodes that have an md directory, sorted.
func (m *mdadm) mdNodes() ([]string, error) {
	es, err := os.ReadDir(filepath.Join(m.env.Sys, "block"))
	if err != nil {
		return nil, fmt.Errorf("list the block devices: %w", err)
	}
	var out []string
	for _, e := range es {
		n := e.Name()
		if !strings.HasPrefix(n, "md") || !safeNode(n) {
			continue
		}
		if _, err := os.Stat(filepath.Join(m.env.Sys, "block", n, "md")); err == nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// arrayIdentity is the uuid (lower case, mdadm's colon form) and level of
// md: from mdadm --detail --export, else, for an array --detail cannot
// describe (inactive), the uuid all its members' superblocks agree on.
// "" when it cannot be told.
//
// UNVERIFIED on hardware: what --detail --export prints for an inactive
// array (MD_UUID or not).
func (m *mdadm) arrayIdentity(ctx context.Context, md string, ms []mdMember) (uuid, level string) {
	if out, err := m.run(ctx, "--detail", "--export", "/dev/"+md); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "MD_LEVEL="); ok {
				level = strings.TrimSpace(v)
			}
		}
		if u := exportUUID(out); u != "" {
			return strings.ToLower(u), level
		}
	}
	if len(ms) == 0 {
		return "", level
	}
	for _, d := range ms {
		u, err := m.examine(ctx, "/dev/"+d.name)
		if err != nil || u == "" || (uuid != "" && normUUID(u) != normUUID(uuid)) {
			return "", level
		}
		uuid = strings.ToLower(u)
	}
	return uuid, level
}

// externalName is how an array with external metadata is named.
func externalName(md, meta string) string {
	switch {
	case strings.HasPrefix(meta, "external:imsm"):
		return md + " (IMSM container)"
	case strings.HasPrefix(meta, "external:ddf"):
		return md + " (DDF container)"
	}
	return md + " (firmware RAID volume)"
}

// existingArray is the entry of the assembled array md. Stack holds only
// what does not change while the array is used (level, member devices);
// the state goes in Summary, which the gate does not compare.
func (m *mdadm) existingArray(md string, ms []mdMember, merr error, uuid, detailLevel string) Existing {
	devs := make([]string, 0, len(ms))
	data, spare, journal := 0, 0, 0
	for _, d := range ms {
		devs = append(devs, "/dev/"+d.name)
		switch d.role {
		case RoleSpare:
			spare++
		case RoleJournal:
			journal++
		default:
			data++
		}
	}
	slices.Sort(devs)
	level, _ := m.mdAttr(md, "level")
	if level == "" {
		level = detailLevel
	}
	state, _ := m.mdAttr(md, "array_state")
	if state == "" {
		state = "unknown state"
	}

	sum := md
	if level != "" {
		sum += " " + level
	}
	sum += " " + mdRoles(data, spare, journal)
	if sz, err := os.ReadFile(filepath.Join(m.env.Sys, "block", md, "size")); err == nil {
		if n, err := strconv.ParseUint(strings.TrimSpace(string(sz)), 10, 64); err == nil && n > 0 {
			sum += ", " + fmtBytes(n*512)
		}
	}
	sum += ", " + state
	if d, _ := m.mdAttr(md, "degraded"); d != "" && d != "0" {
		sum += ", degraded"
	}
	if a, err := m.mdAttr(md, "sync_action"); err == nil && a != "" && a != "idle" {
		sum += ", " + a
	}
	head := md
	if level != "" {
		head = md + " (" + level + ")"
	}
	owned := append([]string{md}, m.partitions(md)...)
	e := Existing{Backend: m.Name(), ID: uuid, Name: md, Summary: sum,
		Disks: m.disksOf(devs),
		Stack: append([]string{head}, superblockLines(devs)...),
		Extra: map[string]string{ExtraKind: KindMDValue, ExtraOwnedNodes: strings.Join(owned, ","),
			mdExtraAssembled: "yes", mdExtraMemberDevs: strings.Join(devs, ",")}}
	if e.ID == "" {
		e.ID = "node:" + md
	}
	meta, metaErr := m.mdAttr(md, "metadata_version")
	switch {
	case merr != nil:
		e.Blocked = fmt.Sprintf("cannot list the members of %s: %v", md, merr)
	case strings.HasPrefix(meta, "external"):
		e.Name = externalName(md, meta)
		e.Blocked = mdFirmwareBlocked
	case metaErr != nil || !nativeMetadata(meta):
		e.Blocked = fmt.Sprintf("its metadata %q is not md's own (0.90 or 1.x); destroy it by hand", meta)
	case uuid == "":
		e.Blocked = "cannot tell its uuid (mdadm --detail gives none and its members do not agree); destroy it by hand"
	}
	if e.Blocked != "" {
		slog.Info("md array listed but not destroyable", "md", md, "why", e.Blocked)
	}
	return e
}

// existingGroup is the entry of superblocks no assembled array carries;
// nameGroups names it.
func (m *mdadm) existingGroup(g mdSuperGroup) Existing {
	sum := "unassembled "
	if g.level != "" {
		sum += g.level + " "
	}
	id := g.uuid
	if g.uuid != "" {
		sum += "md superblocks (uuid " + g.uuid + ") on " + strings.Join(g.devs, ", ")
	} else {
		sum += "md superblocks without a readable uuid on " + strings.Join(g.devs, ", ")
		id = "devs:" + strings.Join(g.devs, ",")
	}
	return Existing{Backend: m.Name(), ID: id, Name: "md superblocks on " + strings.Join(g.devs, ", "), Summary: sum,
		Disks:   m.disksOf(g.devs),
		Stack:   superblockLines(g.devs),
		Blocked: g.blocked,
		Extra: map[string]string{ExtraKind: KindMDValue, mdExtraAssembled: "no",
			mdExtraMemberDevs: strings.Join(g.devs, ",")}}
}

func superblockLines(devs []string) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = "md superblock of " + d
	}
	return out
}

// nativeMetadata is whether an md/metadata_version is md's own (0.90, 1.x).
func nativeMetadata(v string) bool {
	return strings.HasPrefix(v, "0.") || strings.HasPrefix(v, "00.") || strings.HasPrefix(v, "1.")
}

func fmtBytes(b uint64) string {
	switch f := float64(b); {
	case f >= 1e12:
		return fmt.Sprintf("%.1f TB", f/1e12)
	case f >= 1e9:
		return fmt.Sprintf("%.1f GB", f/1e9)
	default:
		return fmt.Sprintf("%.0f MB", f/1e6)
	}
}

// partitions are the partition nodes of node in sysfs, sorted.
func (m *mdadm) partitions(node string) []string {
	es, err := os.ReadDir(filepath.Join(m.env.Sys, "block", node))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if _, err := os.Stat(filepath.Join(m.env.Sys, "block", node, e.Name(), "partition")); err == nil {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// wholeDisk is the /sys/block node node is (a disk) or is a partition of,
// "" when it is neither.
func (m *mdadm) wholeDisk(node string) string { return wholeDiskIn(m.env.Sys, node) }

// wholeDiskIn is wholeDisk over the sysfs root sys.
func wholeDiskIn(sys, node string) string {
	if !safeNode(node) {
		return ""
	}
	block := filepath.Join(sys, "block")
	if _, err := os.Stat(filepath.Join(block, node)); err == nil {
		return node
	}
	es, err := os.ReadDir(block)
	if err != nil {
		return ""
	}
	for _, e := range es {
		if _, err := os.Stat(filepath.Join(block, e.Name(), node, "partition")); err == nil {
			return e.Name()
		}
	}
	return ""
}

// diskTarget identifies disk from sysfs. A disk that cannot be read keeps
// only its name, so the gate refuses it for having no serial.
func (m *mdadm) diskTarget(disk string) Target { return diskTargetIn(m.env.Sys, disk) }

// diskTargetIn is diskTarget over the sysfs root sys.
func diskTargetIn(sys, disk string) Target {
	d, err := blockdev.Lookup(blockdev.OSFS{}, filepath.Join(sys, "block"), disk)
	if err != nil {
		return Target{Name: disk}
	}
	return Target{Name: d.Name, Path: d.Path, Serial: d.Serial, Model: d.Model, Vendor: d.Vendor}
}

// disksOf are the whole disks of devs, once each, in order. A device that
// is not /dev/<node> or not a known disk or partition gives a disk with no
// serial (the gate then refuses the entry).
func (m *mdadm) disksOf(devs []string) []Target {
	var out []Target
	seen := map[string]bool{}
	for _, dev := range devs {
		node := filepath.Base(dev)
		disk := ""
		if mdDevRe.MatchString(dev) {
			disk = m.wholeDisk(node)
		}
		t := Target{Name: node}
		if disk != "" {
			t = m.diskTarget(disk)
		}
		if !seen[t.Name] {
			seen[t.Name] = true
			out = append(out, t)
		}
	}
	return out
}

// mdSuperGroup is one ARRAY line of mdadm --examine --scan --verbose.
type mdSuperGroup struct {
	uuid, level string
	devs        []string
	blocked     string
}

// mdFirmwareBlocked is why IMSM/DDF metadata is not destroyed.
const mdFirmwareBlocked = "Intel/DDF firmware RAID metadata: destroy it with the vendor tool or mdadm by hand"

// superblockGroups runs mdadm --examine --scan --verbose.
//
// UNVERIFIED on hardware: the exit status when no disk carries a
// superblock (taken as success when mdadm prints nothing at all).
func (m *mdadm) superblockGroups(ctx context.Context) ([]mdSuperGroup, error) {
	args := []string{"--examine", "--scan", "--verbose"}
	out, err := m.env.Run(ctx, "mdadm", args...)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.TrimSpace(string(out)) == "" && strings.TrimSpace(string(ee.Stderr)) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("mdadm %s: %w", strings.Join(args, " "), err)
	}
	return parseExamineScan(out), nil
}

// parseExamineScan groups the devices of mdadm --examine --scan --verbose
// by uuid (lower case), devices sorted, groups by uuid, then those without
// a readable uuid. Lines indented under an ARRAY line continue it; other
// lines are ignored. A member array of a container (container=, member=,
// no devices of its own) is left out: its container carries the disks.
// IMSM/DDF metadata and superblocks without a readable uuid are kept as
// blocked groups, so their disks are shown with the reason.
//
// UNVERIFIED on hardware: the output shape, taken to be
//
//	ARRAY /dev/md/data level=raid5 metadata=1.2 num-devices=3 UUID=a:b:c:d name=host:data
//	   devices=/dev/sdb1,/dev/sdc1,/dev/sdd1
func parseExamineScan(out []byte) []mdSuperGroup {
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if (l[0] == ' ' || l[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] += " " + strings.TrimSpace(l)
			continue
		}
		lines = append(lines, strings.TrimSpace(l))
	}
	by := map[string]*mdSuperGroup{}
	var order []string
	var noUUID []mdSuperGroup
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 || f[0] != "ARRAY" {
			continue
		}
		kv := map[string]string{}
		for _, w := range f[1:] {
			if k, v, ok := strings.Cut(w, "="); ok {
				kv[k] = v
			}
		}
		var devs []string
		for _, d := range strings.Split(kv["devices"], ",") {
			if d = strings.TrimSpace(d); d != "" && !slices.Contains(devs, d) {
				devs = append(devs, d)
			}
		}
		if len(devs) == 0 || kv["container"] != "" || kv["member"] != "" {
			continue
		}
		slices.Sort(devs)
		blocked := ""
		if meta, ok := kv["metadata"]; ok && !nativeMetadata(meta) {
			blocked = mdFirmwareBlocked
			if !strings.Contains(meta, "imsm") && !strings.Contains(meta, "ddf") {
				blocked = fmt.Sprintf("its metadata %q is not md's own (0.90 or 1.x); destroy it by hand", meta)
			}
		}
		uuid := strings.ToLower(kv["UUID"])
		if len(normUUID(uuid)) != 32 || strings.Trim(normUUID(uuid), "0123456789abcdef") != "" {
			noUUID = append(noUUID, mdSuperGroup{devs: devs, level: kv["level"], blocked: "its superblock has no readable uuid"})
			continue
		}
		g, ok := by[normUUID(uuid)]
		if !ok {
			g = &mdSuperGroup{uuid: uuid, level: kv["level"]}
			by[normUUID(uuid)] = g
			order = append(order, normUUID(uuid))
		}
		if blocked != "" {
			g.blocked = blocked
		}
		for _, d := range devs {
			if !slices.Contains(g.devs, d) {
				g.devs = append(g.devs, d)
			}
		}
	}
	slices.Sort(order)
	res := make([]mdSuperGroup, 0, len(order)+len(noUUID))
	for _, k := range order {
		g := by[k]
		slices.Sort(g.devs)
		res = append(res, *g)
	}
	return append(res, noUUID...)
}

// memberDevs is Extra[mdExtraMemberDevs] as a list.
func memberDevs(e Existing) []string {
	var out []string
	for _, d := range strings.Split(e.Extra[mdExtraMemberDevs], ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// destroyCheck is why Destroy may not touch e, "" when it may: an md entry
// with a uuid, whose member devices are /dev/<node> nodes, each on one of
// e's disks, every disk carrying one, each disk still the one listed
// (serial and model read again).
func (m *mdadm) destroyCheck(e Existing) string {
	if e.Extra[ExtraKind] != KindMDValue {
		return "not an md entry"
	}
	if len(normUUID(e.ID)) != 32 {
		return fmt.Sprintf("no array uuid (%q)", e.ID)
	}
	switch e.Extra[mdExtraAssembled] {
	case "yes":
		if !strings.HasPrefix(e.Name, "md") || !safeNode(e.Name) || !ownedNodes(e)[e.Name] {
			return fmt.Sprintf("bad md node %q", e.Name)
		}
	case "no":
	default:
		return "not said whether it is assembled"
	}
	devs := memberDevs(e)
	if len(devs) == 0 {
		return "lists no member device"
	}
	disks := map[string]Target{}
	for _, t := range e.Disks {
		disks[t.Name] = t
	}
	used := map[string]bool{}
	seen := map[string]bool{}
	for _, dev := range devs {
		if !mdDevRe.MatchString(dev) {
			return fmt.Sprintf("bad member device %q", dev)
		}
		if seen[dev] {
			return fmt.Sprintf("member device %s listed twice", dev)
		}
		seen[dev] = true
		disk := m.wholeDisk(filepath.Base(dev))
		if _, ok := disks[disk]; disk == "" || !ok {
			return fmt.Sprintf("member device %s is not on one of its disks", dev)
		}
		used[disk] = true
	}
	for _, t := range e.Disks {
		if !used[t.Name] {
			return fmt.Sprintf("disk %s carries none of its member devices", t.Name)
		}
		now := m.diskTarget(t.Name)
		if t.Serial == "" || now.Serial != t.Serial || now.Model != t.Model {
			return fmt.Sprintf("disk %s is not the one listed (serial %q model %q, now %q %q)", t.Name, t.Serial, t.Model, now.Serial, now.Model)
		}
	}
	return ""
}

// Destroy tears e down: an assembled array is stopped, then the superblock
// of each member device listed in e is zeroed; an unassembled group only
// has its superblocks zeroed. Before anything it proves that the entry
// still describes the machine (members, disks, uuid); before each zero, that
// the device carries e's uuid; after each step it reads back. It stops at
// the first step that fails and says what is done, what is left and the
// command for what is left (never a device it could not prove). No force
// flag is used: mdadm refuses to stop an array that is open.
func (m *mdadm) Destroy(ctx context.Context, e Existing) error {
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("%s: %s; nothing done", e.Name, fmt.Sprintf(format, a...))
	}
	if e.Blocked != "" {
		return refuse("%s", e.Blocked)
	}
	if why := m.destroyCheck(e); why != "" {
		return refuse("%s", why)
	}
	devs := memberDevs(e)
	uuid := e.ID
	assembled := e.Extra[mdExtraAssembled] == "yes"
	md := e.Name
	// After a mutation, its checks run to completion even if ctx ends.
	bg := context.WithoutCancel(ctx)

	if assembled {
		ms, err := m.members(md)
		if err != nil {
			return refuse("cannot list the members of %s: %v", md, err)
		}
		var now []string
		for _, d := range ms {
			now = append(now, "/dev/"+d.name)
		}
		slices.Sort(now)
		want := slices.Sorted(slices.Values(devs))
		if !slices.Equal(now, want) {
			return refuse("its members are now %s, not %s", strings.Join(now, ", "), strings.Join(want, ", "))
		}
		got, _ := m.arrayIdentity(ctx, md, ms)
		if got == "" || normUUID(got) != normUUID(uuid) {
			return refuse("%s does not carry uuid %s (it has %q)", md, uuid, got)
		}
		if err := ctx.Err(); err != nil {
			return refuse("%v", err)
		}
		// A started step runs to completion: only the checks between steps
		// see ctx.
		if _, err := m.run(bg, "--stop", "/dev/"+md); err != nil {
			if m.stopped(md) {
				return m.partial(e, true, nil, devs, fmt.Errorf("mdadm --stop reported an error (%v) but %s is no longer assembled", err, md))
			}
			return m.partial(e, false, nil, devs, fmt.Errorf("%v; %s is still assembled", err, md))
		}
		if err := m.waitDestroyStopped(bg, md); err != nil {
			return m.partial(e, false, nil, devs, err)
		}
		other, ok, err := m.findUUID(bg, uuid)
		if err != nil {
			return m.partial(e, true, nil, devs, fmt.Errorf("cannot check that uuid %s is not assembled elsewhere: %v", uuid, err))
		}
		if ok {
			return m.partial(e, false, nil, devs, fmt.Errorf("uuid %s is still assembled as %s after mdadm --stop", uuid, other))
		}
	} else {
		other, ok, err := m.findUUID(ctx, uuid)
		if err != nil {
			return refuse("cannot check that uuid %s is not assembled: %v", uuid, err)
		}
		if ok {
			return refuse("uuid %s is now assembled as %s", uuid, other)
		}
	}

	var done []string
	for i, dev := range devs {
		if err := ctx.Err(); err != nil {
			return m.partial(e, assembled, done, devs[i:], err)
		}
		got, err := m.examine(ctx, dev)
		if err != nil && ctx.Err() != nil {
			// The cancel killed the read-only examine: dev is not judged
			// either way, so it stays in the command, behind the check.
			return m.partial(e, assembled, done, devs[i:], fmt.Errorf("cancelled before %s was examined: %w", dev, ctx.Err()))
		}
		if err != nil {
			// Unproven: left out of the command for the rest.
			return m.partial(e, assembled, done, devs[i+1:],
				fmt.Errorf("%s could not be examined (%v); do not zero it before mdadm --examine shows uuid %s on it", dev, err, uuid))
		}
		if got == "" {
			slog.Info("member device carries no md superblock any more; nothing to zero", "dev", dev, "uuid", uuid)
			done = append(done, dev)
			continue
		}
		if normUUID(got) != normUUID(uuid) {
			return m.partial(e, assembled, done, devs[i+1:],
				fmt.Errorf("%s carries the superblock of another array (uuid %s), not of %s; do not zero it", dev, got, uuid))
		}
		if _, err := m.run(bg, "--zero-superblock", dev); err != nil {
			return m.partial(e, assembled, done, devs[i:], err)
		}
		after, err := m.examine(bg, dev)
		if err != nil {
			return m.partial(e, assembled, done, devs[i:], fmt.Errorf("cannot check that %s was zeroed: %v", dev, err))
		}
		if after != "" && normUUID(after) == normUUID(uuid) {
			return m.partial(e, assembled, done, devs[i:], fmt.Errorf("%s still has the superblock of %s after mdadm --zero-superblock", dev, uuid))
		}
		done = append(done, dev)
	}
	slog.Info("destroyed an md configuration", "name", e.Name, "uuid", uuid, "zeroed", done)
	return nil
}

// partial is the error of a Destroy that stopped part way: what is done,
// what is left, why, and the command for left, which holds only devices
// proven to carry e's uuid or not examined yet.
func (m *mdadm) partial(e Existing, stopped bool, done, left []string, cause error) error {
	var parts []string
	if e.Extra[mdExtraAssembled] == "yes" {
		if stopped {
			parts = append(parts, "stopped "+e.Name)
		} else {
			parts = append(parts, e.Name+" not stopped")
		}
	}
	if len(done) > 0 {
		parts = append(parts, "superblocks zeroed on "+strings.Join(done, ", "))
	} else {
		parts = append(parts, "no superblock zeroed")
	}
	if len(left) > 0 {
		parts = append(parts, "left: superblocks of "+strings.Join(left, ", "))
	}
	rest := Existing{Name: e.Name, Extra: maps.Clone(e.Extra)}
	rest.Extra[mdExtraMemberDevs] = strings.Join(left, ",")
	if stopped {
		rest.Extra[mdExtraAssembled] = "no"
	}
	hand := "nothing proven is left to run by hand"
	if c := m.DestroyCommand(rest); c != "" {
		hand = "rest by hand, " + examineFirst(e.ID, c)
	}
	return fmt.Errorf("%s: %s; %v; %s", e.Name, strings.Join(parts, "; "), cause, hand)
}

// stopped is whether md is no longer an assembled array: its node is gone,
// or it has no md directory, or it is clear or inactive with no member.
func (m *mdadm) stopped(md string) bool {
	if _, err := os.Stat(filepath.Join(m.env.Sys, "block", md)); errors.Is(err, os.ErrNotExist) {
		return true
	}
	ms, err := m.members(md)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || len(ms) > 0 {
		return false
	}
	st, _ := m.mdAttr(md, "array_state")
	return st == "clear" || st == "inactive"
}

// waitDestroyStopped waits up to Settle until md is stopped.
func (m *mdadm) waitDestroyStopped(ctx context.Context, md string) error {
	deadline := time.Now().Add(m.env.Settle)
	for {
		if m.stopped(md) {
			return nil
		}
		if time.Now().After(deadline) {
			st, _ := m.mdAttr(md, "array_state")
			return fmt.Errorf("%s is still assembled (array_state %q) after mdadm --stop; no superblock was zeroed", md, st)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.env.Poll):
		}
	}
}

// findUUID is the assembled md node that carries uuid. It fails closed: an
// unreadable listing, or an array with members whose uuid cannot be told
// or whose members cannot be read, is an error, not "not assembled".
func (m *mdadm) findUUID(ctx context.Context, uuid string) (string, bool, error) {
	nodes, err := m.mdNodes()
	if err != nil {
		return "", false, err
	}
	for _, md := range nodes {
		ms, err := m.members(md)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("cannot list the members of %s: %v", md, err)
		}
		u, _ := m.arrayIdentity(ctx, md, ms)
		if u == "" {
			if len(ms) > 0 {
				return "", false, fmt.Errorf("cannot tell the uuid of %s", md)
			}
			continue
		}
		if normUUID(u) == normUUID(uuid) {
			return md, true, nil
		}
	}
	return "", false, nil
}

// DestroyCommand is the shell command that tears e down by hand:
// "mdadm --stop /dev/md127 && mdadm --zero-superblock /dev/sdb /dev/sdc".
// It is "" for a Blocked entry: what the reason says not to run is never
// printed.
func (m *mdadm) DestroyCommand(e Existing) string {
	if e.Blocked != "" {
		return ""
	}
	var cmds []string
	if e.Extra[mdExtraAssembled] == "yes" && e.Name != "" {
		cmds = append(cmds, "mdadm --stop "+shQuote("/dev/"+e.Name))
	}
	if devs := memberDevs(e); len(devs) > 0 {
		q := make([]string, len(devs))
		for i, d := range devs {
			q[i] = shQuote(d)
		}
		cmds = append(cmds, "mdadm --zero-superblock "+strings.Join(q, " "))
	}
	return strings.Join(cmds, " && ")
}

var shSafeRe = regexp.MustCompile(`^[A-Za-z0-9@%_+=:,./-]+$`)

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string {
	if shSafeRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// examineFirst is cmd prefixed with the check to make before running it.
func examineFirst(uuid, cmd string) string {
	return "after checking with mdadm --examine that each device carries uuid " + uuid + ": " + cmd
}

// LeftoverCommand is the command for what is left of confirmed, as fresh
// lists it after a successful Destroy: only the member devices confirmed
// named, a --stop only of the node confirmed named, "" for a Blocked
// entry, another uuid, or nothing confirmed left.
func (m *mdadm) LeftoverCommand(confirmed, fresh Existing) string {
	if confirmed.Blocked != "" || fresh.Blocked != "" || fresh.Extra[ExtraKind] != KindMDValue ||
		normUUID(fresh.ID) == "" || normUUID(fresh.ID) != normUUID(confirmed.ID) {
		return ""
	}
	stop := fresh.Extra[mdExtraAssembled] == "yes"
	if stop && (confirmed.Extra[mdExtraAssembled] != "yes" || fresh.Name != confirmed.Name) {
		return ""
	}
	allowed := map[string]bool{}
	for _, d := range memberDevs(confirmed) {
		allowed[d] = true
	}
	var devs []string
	for _, d := range memberDevs(fresh) {
		if allowed[d] {
			devs = append(devs, d)
		}
	}
	if len(devs) == 0 && !stop {
		return ""
	}
	assembled := "no"
	if stop {
		assembled = "yes"
	}
	c := m.DestroyCommand(Existing{Name: fresh.Name, Extra: map[string]string{
		mdExtraAssembled: assembled, mdExtraMemberDevs: strings.Join(devs, ",")}})
	if c == "" {
		return ""
	}
	return examineFirst(confirmed.ID, c)
}
