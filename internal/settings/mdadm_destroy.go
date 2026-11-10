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

var _ Destroyer = (*mdadm)(nil)

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
// An array is left out when its uuid cannot be told (mdadm --detail gives
// none and its members do not agree) or its metadata is external (IMSM,
// DDF: firmware RAID, whose containers --zero-superblock does not undo
// cleanly); the superblocks of its members are then not offered either.
func (m *mdadm) Existing(ctx context.Context) ([]Existing, error) {
	nodes, err := m.mdNodes()
	if err != nil {
		return nil, err
	}
	var out []Existing
	assembledUUID := map[string]bool{}
	assembledDev := map[string]bool{}
	for _, md := range nodes {
		ms, err := m.members(md)
		if err != nil {
			slog.Warn("cannot list the members of an md array; left out", "md", md, "err", err)
			continue
		}
		for _, d := range ms {
			assembledDev["/dev/"+d.name] = true
		}
		uuid, level := m.arrayIdentity(ctx, md, ms)
		if uuid != "" {
			assembledUUID[normUUID(uuid)] = true
		}
		if e, ok := m.existingArray(md, ms, uuid, level); ok {
			out = append(out, e)
		}
	}

	groups, err := m.superblockGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if assembledUUID[normUUID(g.uuid)] {
			continue // assembled wins; a stale member is not a second entry
		}
		g.devs = slices.DeleteFunc(g.devs, func(d string) bool { return assembledDev[d] })
		if len(g.devs) == 0 {
			continue
		}
		out = append(out, m.existingGroup(g))
	}
	return out, nil
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

// existingArray is the entry of the assembled array md.
func (m *mdadm) existingArray(md string, ms []mdMember, uuid, detailLevel string) (Existing, bool) {
	if uuid == "" {
		slog.Warn("cannot tell the uuid of an md array; left out", "md", md)
		return Existing{}, false
	}
	if len(ms) == 0 {
		slog.Warn("md array has no members; left out", "md", md)
		return Existing{}, false
	}
	if meta, err := m.mdAttr(md, "metadata_version"); err != nil || !nativeMetadata(meta) {
		slog.Warn("md array without native metadata; left out", "md", md, "metadata", meta, "err", err)
		return Existing{}, false
	}
	level, _ := m.mdAttr(md, "level")
	if level == "" {
		level = detailLevel
	}
	state, _ := m.mdAttr(md, "array_state")
	if state == "" {
		state = "unknown state"
	}
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
	head := md + " (" + state + ")"
	if level != "" {
		head = md + " (" + level + ", " + state + ")"
	}
	owned := append([]string{md}, m.partitions(md)...)
	return Existing{Backend: m.Name(), ID: uuid, Name: md, Summary: sum,
		Disks: m.disksOf(devs),
		Stack: append([]string{head}, superblockLines(devs)...),
		Extra: map[string]string{ExtraKind: KindMDValue, ExtraOwnedNodes: strings.Join(owned, ","),
			mdExtraAssembled: "yes", mdExtraMemberDevs: strings.Join(devs, ",")}}, true
}

// existingGroup is the entry of superblocks no assembled array carries.
func (m *mdadm) existingGroup(g mdSuperGroup) Existing {
	short := normUUID(g.uuid)
	if len(short) > 8 {
		short = short[:8]
	}
	sum := "unassembled "
	if g.level != "" {
		sum += g.level + " "
	}
	sum += "md superblocks (uuid " + g.uuid + ") on " + strings.Join(g.devs, ", ")
	return Existing{Backend: m.Name(), ID: g.uuid, Name: "unassembled " + short, Summary: sum,
		Disks: m.disksOf(g.devs),
		Stack: superblockLines(g.devs),
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
func (m *mdadm) wholeDisk(node string) string {
	if !safeNode(node) {
		return ""
	}
	block := filepath.Join(m.env.Sys, "block")
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
func (m *mdadm) diskTarget(disk string) Target {
	d, err := blockdev.Lookup(blockdev.OSFS{}, filepath.Join(m.env.Sys, "block"), disk)
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
}

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
// by uuid (lower case), devices sorted. Lines indented under an ARRAY line
// continue it. Only native metadata (0.90, 1.x, or none printed) with a
// devices= list is kept: IMSM/DDF containers and their member arrays
// (metadata=imsm, container=) are left out.
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
		uuid := strings.ToLower(kv["UUID"])
		if len(normUUID(uuid)) != 32 || kv["container"] != "" || kv["member"] != "" {
			continue
		}
		if meta, ok := kv["metadata"]; ok && !nativeMetadata(meta) {
			continue
		}
		if kv["devices"] == "" {
			continue
		}
		g, ok := by[normUUID(uuid)]
		if !ok {
			g = &mdSuperGroup{uuid: uuid, level: kv["level"]}
			by[normUUID(uuid)] = g
			order = append(order, normUUID(uuid))
		}
		for _, d := range strings.Split(kv["devices"], ",") {
			if d = strings.TrimSpace(d); d != "" && !slices.Contains(g.devs, d) {
				g.devs = append(g.devs, d)
			}
		}
	}
	slices.Sort(order)
	res := make([]mdSuperGroup, 0, len(order))
	for _, k := range order {
		g := by[k]
		slices.Sort(g.devs)
		res = append(res, *g)
	}
	return res
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
// the first step that fails and says what is done and what is left. No
// force flag is used: mdadm refuses to stop an array that is open.
func (m *mdadm) Destroy(ctx context.Context, e Existing) error {
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("%s: %s; nothing done", e.Name, fmt.Sprintf(format, a...))
	}
	if why := m.destroyCheck(e); why != "" {
		return refuse("%s", why)
	}
	devs := memberDevs(e)
	uuid := e.ID
	assembled := e.Extra[mdExtraAssembled] == "yes"
	md := e.Name

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
		if _, err := m.run(ctx, "--stop", "/dev/"+md); err != nil {
			return refuse("%v", err)
		}
	} else if other, ok := m.findUUID(ctx, uuid); ok {
		return refuse("uuid %s is now assembled as %s", uuid, other)
	}

	// After a successful mutation, its check runs to completion.
	bg := context.WithoutCancel(ctx)
	if assembled {
		if err := m.waitGone(bg, md); err != nil {
			return m.partial(e, false, nil, devs, err)
		}
		if other, ok := m.findUUID(bg, uuid); ok {
			return m.partial(e, false, nil, devs, fmt.Errorf("uuid %s is still assembled as %s after mdadm --stop", uuid, other))
		}
	}
	var done []string
	for i, dev := range devs {
		rest := devs[i:]
		if err := ctx.Err(); err != nil {
			return m.partial(e, assembled, done, rest, err)
		}
		got, err := m.examine(ctx, dev)
		if err != nil {
			return m.partial(e, assembled, done, rest, err)
		}
		if got == "" {
			slog.Info("member device carries no md superblock any more; nothing to zero", "dev", dev, "uuid", uuid)
			done = append(done, dev)
			continue
		}
		if normUUID(got) != normUUID(uuid) {
			return m.partial(e, assembled, done, rest, fmt.Errorf("%s carries the superblock of another array (uuid %s); not zeroed", dev, got))
		}
		if _, err := m.run(ctx, "--zero-superblock", dev); err != nil {
			return m.partial(e, assembled, done, rest, err)
		}
		after, err := m.examine(bg, dev)
		if err != nil {
			return m.partial(e, assembled, done, rest, fmt.Errorf("cannot check that %s was zeroed: %v", dev, err))
		}
		if after != "" && normUUID(after) == normUUID(uuid) {
			return m.partial(e, assembled, done, rest, fmt.Errorf("%s still has the superblock of %s after mdadm --zero-superblock", dev, uuid))
		}
		done = append(done, dev)
	}
	slog.Info("destroyed an md configuration", "name", e.Name, "uuid", uuid, "zeroed", done)
	return nil
}

// partial is the error of a Destroy that stopped part way: what is done,
// what is left, why, and the command for the rest.
func (m *mdadm) partial(e Existing, stopped bool, done, rest []string, cause error) error {
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
	parts = append(parts, "left: superblocks of "+strings.Join(rest, ", "))
	left := Existing{Name: e.Name, Extra: maps.Clone(e.Extra)}
	left.Extra[mdExtraMemberDevs] = strings.Join(rest, ",")
	if stopped {
		left.Extra[mdExtraAssembled] = "no"
	}
	return fmt.Errorf("%s: %s: %w; rest by hand: %s", e.Name, strings.Join(parts, "; "), cause, m.DestroyCommand(left))
}

// waitGone waits up to Settle until <Sys>/block/<md> is gone.
func (m *mdadm) waitGone(ctx context.Context, md string) error {
	deadline := time.Now().Add(m.env.Settle)
	for {
		if _, err := os.Stat(filepath.Join(m.env.Sys, "block", md)); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if time.Now().After(deadline) {
			st, _ := m.mdAttr(md, "array_state")
			return fmt.Errorf("%s still exists (array_state %q) after mdadm --stop; no superblock was zeroed", md, st)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.env.Poll):
		}
	}
}

// findUUID is the assembled md node that carries uuid.
func (m *mdadm) findUUID(ctx context.Context, uuid string) (string, bool) {
	nodes, err := m.mdNodes()
	if err != nil {
		return "", false
	}
	for _, md := range nodes {
		ms, _ := m.members(md)
		if u, _ := m.arrayIdentity(ctx, md, ms); u != "" && normUUID(u) == normUUID(uuid) {
			return md, true
		}
	}
	return "", false
}

// DestroyCommand is the shell command that tears e down by hand:
// "mdadm --stop /dev/md127 && mdadm --zero-superblock /dev/sdb /dev/sdc".
func (m *mdadm) DestroyCommand(e Existing) string {
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
