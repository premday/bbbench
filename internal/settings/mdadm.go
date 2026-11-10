package settings

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// NewMdadm is the Linux software RAID backend (md arrays built with mdadm).
func NewMdadm(env Env) Backend { return &mdadm{env: env.fill()} }

type mdadm struct{ env Env }

var (
	mdChunks = []string{"64K", "128K", "256K", "512K", "1024K"}
	// mdMinData is the least number of data disks of each level.
	mdMinData = map[string]int{"raid0": 2, "raid1": 2, "raid5": 3, "raid6": 4, "raid10": 4, "linear": 2}
	// mdRolesRe matches the roles part of a label.
	mdRolesRe = regexp.MustCompile(`^(\d+ disks?|\d+ data( \+ \d+ spares?)?( \+ \d+ journal)?)$`)
)

func mdHasChunk(level string) bool {
	return level == "raid0" || level == "raid5" || level == "raid6" || level == "raid10"
}

func mdHasBitmap(level string) bool {
	return level == "raid1" || level == "raid5" || level == "raid6" || level == "raid10"
}

func (m *mdadm) Name() string { return "mdadm" }

func (m *mdadm) Available() (bool, string) {
	if _, err := m.env.LookPath("mdadm"); err != nil {
		return false, "mdadm is not installed"
	}
	return true, ""
}

func (m *mdadm) Levels() []string {
	return []string{"raid0", "raid1", "raid5", "raid6", "raid10", "linear"}
}

func (m *mdadm) Roles(level string) []Role {
	switch level {
	case "raid0", "linear":
		return []Role{RoleData}
	case "raid1", "raid10":
		return []Role{RoleData, RoleSpare}
	case "raid5", "raid6":
		return []Role{RoleData, RoleSpare, RoleJournal}
	}
	return nil
}

func (m *mdadm) Options(level string, _ int) []Option {
	var opts []Option
	if mdHasChunk(level) {
		opts = append(opts, Option{Key: "chunk", Label: "chunk", Choices: slices.Clone(mdChunks), Default: "512K"})
	}
	if mdHasBitmap(level) {
		opts = append(opts, Option{Key: "bitmap", Label: "write-intent bitmap", Choices: []string{"no", "yes"}, Default: "no"})
	}
	return opts
}

// roleCounts counts the disks of each role.
func roleCounts(disks []Disk) (data, spare, journal int) {
	for _, d := range disks {
		switch d.Role {
		case RoleSpare:
			spare++
		case RoleJournal:
			journal++
		default:
			data++
		}
	}
	return
}

func (m *mdadm) Validate(l Layout, disks []Disk) error {
	data, _, journal := roleCounts(disks)
	if n, ok := mdMinData[l.Level]; ok && data < n {
		return fmt.Errorf("%s needs at least %d data disks, has %d", l.Level, n, data)
	}
	if journal > 1 {
		return errors.New("at most one journal disk")
	}
	if journal > 0 && l.Options["bitmap"] == "yes" {
		return errors.New("a write journal and a bitmap cannot be used together")
	}
	return nil
}

func (m *mdadm) Label(l Layout, disks []Disk) string {
	data, spare, journal := roleCounts(disks)
	return mdLabel(l.Level, data, spare, journal, l.Options["chunk"], l.Options["bitmap"] == "yes")
}

// mdLabel is "mdadm <level> · <roles>[ · <chunk>][ · bitmap]"; the chunk
// appears only for levels that have one.
func mdLabel(level string, data, spare, journal int, chunk string, bitmap bool) string {
	s := "mdadm " + level + " · " + mdRoles(data, spare, journal)
	if mdHasChunk(level) && chunk != "" {
		s += " · " + chunk
	}
	if bitmap {
		s += " · bitmap"
	}
	return s
}

// mdRoles is "1 disk", "<n> disks" or "<n> data[ + <k> spares][ + <j> journal]".
func mdRoles(data, spare, journal int) string {
	var roles string
	switch {
	case spare == 0 && journal == 0 && data == 1:
		roles = "1 disk"
	case spare == 0 && journal == 0:
		roles = fmt.Sprintf("%d disks", data)
	default:
		roles = fmt.Sprintf("%d data", data)
		if spare == 1 {
			roles += " + 1 spare"
		} else if spare > 1 {
			roles += fmt.Sprintf(" + %d spares", spare)
		}
		if journal > 0 {
			roles += fmt.Sprintf(" + %d journal", journal)
		}
	}
	return roles
}

// Parse is the inverse of Label; the disk counts are not part of a Layout.
func (m *mdadm) Parse(value string) (Layout, error) {
	parts := strings.Split(value, " · ")
	head := strings.Fields(parts[0])
	if len(head) != 2 || head[0] != m.Name() {
		return Layout{}, fmt.Errorf("not an mdadm layout: %q", value)
	}
	level := head[1]
	if !slices.Contains(m.Levels(), level) {
		return Layout{}, fmt.Errorf("unknown mdadm level %q", level)
	}
	if len(parts) < 2 || !mdRolesRe.MatchString(parts[1]) {
		return Layout{}, fmt.Errorf("no disks in %q", value)
	}
	l := Layout{Backend: m.Name(), Level: level, Options: map[string]string{}}
	rest := parts[2:]
	if mdHasChunk(level) {
		if len(rest) == 0 || !slices.Contains(mdChunks, rest[0]) {
			return Layout{}, fmt.Errorf("no chunk size in %q", value)
		}
		l.Options["chunk"] = rest[0]
		rest = rest[1:]
	}
	if mdHasBitmap(level) {
		l.Options["bitmap"] = "no"
		if len(rest) > 0 && rest[0] == "bitmap" {
			l.Options["bitmap"] = "yes"
			rest = rest[1:]
		}
	}
	if len(rest) > 0 {
		return Layout{}, fmt.Errorf("unexpected %q in %q", rest[0], value)
	}
	return l, nil
}

// Prepare checks that /dev/md/<name> is free and draws the uuid the array
// will be created with, so that the record can find it again whatever
// kernel name it gets. It runs no command.
func (m *mdadm) Prepare(_ context.Context, v Variant, _ Layout, name string) (*ArrayUndo, error) {
	if _, err := os.Lstat(filepath.Join(m.env.Dev, "md", name)); err == nil {
		return nil, fmt.Errorf("/dev/md/%s already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("check /dev/md/%s: %v", name, err)
	}
	var b [16]byte
	if _, err := io.ReadFull(m.env.Rand, b[:]); err != nil {
		return nil, fmt.Errorf("draw an array uuid: %v", err)
	}
	uuid := fmt.Sprintf("%08x:%08x:%08x:%08x",
		binary.BigEndian.Uint32(b[0:]), binary.BigEndian.Uint32(b[4:]),
		binary.BigEndian.Uint32(b[8:]), binary.BigEndian.Uint32(b[12:]))
	return &ArrayUndo{Name: name, UUID: uuid, Members: slices.Clone(v.Disks)}, nil
}

// run runs mdadm; an error names the command and carries mdadm's stderr.
func (m *mdadm) run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := m.env.Run(ctx, "mdadm", args...)
	if err != nil {
		msg := fmt.Sprintf("mdadm %s: %v", strings.Join(args, " "), err)
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if s := strings.TrimSpace(string(ee.Stderr)); s != "" {
				msg += ": " + s
			}
		}
		return out, errors.New(msg)
	}
	return out, nil
}

// createArgs is the mdadm --create argv of l on v under rec.
//
// UNVERIFIED on hardware: the --uuid=xxxxxxxx:xxxxxxxx:xxxxxxxx:xxxxxxxx
// form, --bitmap=none (needed: mdadm adds an internal bitmap on its own to
// large redundant arrays), leaving --bitmap out when --write-journal is
// given, and that --run with stdin at /dev/null (exec's default) never
// prompts when members carry old signatures.
func createArgs(v Variant, l Layout, rec *ArrayUndo) ([]string, error) {
	var data, spares []string
	journal := ""
	for _, d := range v.Disks {
		if d.Path == "" {
			return nil, fmt.Errorf("%s has no device path", d.Name)
		}
		switch d.Role {
		case RoleSpare:
			spares = append(spares, d.Path)
		case RoleJournal:
			journal = d.Path
		default:
			data = append(data, d.Path)
		}
	}
	args := []string{"--create", "/dev/md/" + rec.Name, "--run", "--metadata=1.2",
		"--name=" + rec.Name, "--uuid=" + rec.UUID, "--level=" + l.Level,
		"--raid-devices=" + strconv.Itoa(len(data))}
	if mdHasChunk(l.Level) {
		args = append(args, "--chunk="+l.Options["chunk"])
	}
	if mdHasBitmap(l.Level) && journal == "" {
		if l.Options["bitmap"] == "yes" {
			args = append(args, "--bitmap=internal")
		} else {
			args = append(args, "--bitmap=none")
		}
	}
	if len(spares) > 0 {
		args = append(args, "--spare-devices="+strconv.Itoa(len(spares)))
	}
	if journal != "" {
		args = append(args, "--write-journal="+journal)
	}
	args = append(args, data...)
	return append(args, spares...), nil
}

// Apply creates the array recorded in rec, finds its kernel name and makes
// it writable.
func (m *mdadm) Apply(ctx context.Context, v Variant, l Layout, rec *ArrayUndo) ([]BenchTarget, error) {
	if rec == nil || rec.Name == "" || rec.UUID == "" {
		return nil, errors.New("no mdadm record with a name and a uuid")
	}
	args, err := createArgs(v, l, rec)
	if err != nil {
		return nil, err
	}
	if _, err := m.run(ctx, args...); err != nil {
		return nil, err
	}
	md, err := m.waitKernelName(ctx, rec)
	if err != nil {
		return nil, err
	}
	// UNVERIFIED on hardware: a new array may start read-auto (no write
	// yet, resync pending); --readwrite starts it.
	if st, _ := m.mdAttr(md, "array_state"); st == "read-auto" {
		if _, err := m.run(ctx, "--readwrite", "/dev/"+md); err != nil {
			return nil, err
		}
	}
	return []BenchTarget{{Kind: TargetArray,
		Target: Target{Name: md, Path: "/dev/" + md, Serial: rec.UUID, Model: l.Level, Vendor: "md", Interface: "md"},
		UUID:   rec.UUID, Members: slices.Clone(v.Disks), Want: v.Value.ID}}, nil
}

// waitKernelName polls the link /dev/md/<name> until Settle; past it, the
// md devices are scanned for the uuid.
//
// UNVERIFIED on hardware: how long udev takes to create /dev/md/<name>,
// and whether the 32-character name limit holds once mdadm adds the
// homehost prefix.
func (m *mdadm) waitKernelName(ctx context.Context, rec *ArrayUndo) (string, error) {
	deadline := time.Now().Add(m.env.Settle)
	for {
		if md, ok := m.linkName(rec.Name); ok {
			return md, nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(m.env.Poll):
		}
	}
	if md, ok := m.scanUUID(ctx, rec.UUID, ""); ok {
		return md, nil
	}
	return "", fmt.Errorf("/dev/md/%s did not appear after mdadm --create, and no md device has uuid %s", rec.Name, rec.UUID)
}

// linkName is the kernel name /dev/md/<name> points to.
func (m *mdadm) linkName(name string) (string, bool) {
	dst, err := os.Readlink(filepath.Join(m.env.Dev, "md", name))
	if err != nil {
		return "", false
	}
	md := filepath.Base(dst)
	if !strings.HasPrefix(md, "md") {
		return "", false
	}
	return md, true
}

// exportUUID is MD_UUID in the output of mdadm --detail/--examine --export.
//
// UNVERIFIED on hardware: the keys of --detail --export and --examine
// --export (MD_UUID).
func exportUUID(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "MD_UUID="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func sameUUID(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

// hasUUID asks mdadm --detail whether md is the array uuid.
func (m *mdadm) hasUUID(ctx context.Context, md, uuid string) bool {
	out, err := m.run(ctx, "--detail", "--export", "/dev/"+md)
	return err == nil && sameUUID(exportUUID(out), uuid)
}

// scanUUID looks for the md device with uuid among <Sys>/block/md*, skip
// left out.
func (m *mdadm) scanUUID(ctx context.Context, uuid, skip string) (string, bool) {
	es, err := os.ReadDir(filepath.Join(m.env.Sys, "block"))
	if err != nil {
		return "", false
	}
	for _, e := range es {
		if n := e.Name(); strings.HasPrefix(n, "md") && n != skip && m.hasUUID(ctx, n, uuid) {
			return n, true
		}
	}
	return "", false
}

// findArray finds the array of u by its uuid: first the kernel name its
// /dev/md link names, then every md device.
func (m *mdadm) findArray(ctx context.Context, u ArrayUndo) (string, bool) {
	md, ok := m.linkName(u.Name)
	if ok && m.hasUUID(ctx, md, u.UUID) {
		return md, true
	}
	if !ok {
		md = ""
	}
	return m.scanUUID(ctx, u.UUID, md)
}

// mdAttr reads <Sys>/block/<md>/md/<attr>, trimmed.
//
// UNVERIFIED on hardware: the attributes and values md/level, raid_disks,
// chunk_size, degraded, array_state (read-auto), sync_action,
// sync_completed (none, "a / b"), layout, bitmap/location and
// dev-*/state (containing spare / journal).
func (m *mdadm) mdAttr(md, attr string) (string, error) {
	if md == "" || strings.Contains(md, "/") {
		return "", fmt.Errorf("bad md name %q", md)
	}
	b, err := os.ReadFile(filepath.Join(m.env.Sys, "block", md, "md", attr))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// syncProgress is whether md is steady, else its resync percentage.
func (m *mdadm) syncProgress(md string) (bool, float64) {
	a, err := m.mdAttr(md, "sync_action")
	if err != nil || a == "idle" {
		return true, 100
	}
	c, _ := m.mdAttr(md, "sync_completed")
	done, total, ok := strings.Cut(c, "/")
	if !ok {
		return false, 0 // none, delayed
	}
	x, err1 := strconv.ParseFloat(strings.TrimSpace(done), 64)
	y, err2 := strconv.ParseFloat(strings.TrimSpace(total), 64)
	if err1 != nil || err2 != nil || y <= 0 {
		return false, 0
	}
	return false, 100 * x / y
}

// WaitSteady polls the arrays until none is resyncing; it has no timeout.
func (m *mdadm) WaitSteady(ctx context.Context, ts []BenchTarget, progress func(pct float64)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		steady, sum := true, 0.0
		for _, t := range ts {
			ok, pct := m.syncProgress(t.Target.Name)
			steady = steady && ok
			sum += pct
		}
		if steady {
			return nil
		}
		if progress != nil {
			progress(sum / float64(len(ts)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.env.Poll):
		}
	}
}

// mdMember is one dev-* entry of an array.
type mdMember struct {
	name string
	role Role
}

func (d mdMember) String() string { return fmt.Sprintf("%s(%s)", d.name, d.role) }

// members lists the dev-* entries of md: the kernel name of dev-*/block
// and the role from dev-*/state.
func (m *mdadm) members(md string) ([]mdMember, error) {
	dir := filepath.Join(m.env.Sys, "block", md, "md")
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []mdMember
	for _, e := range es {
		if !strings.HasPrefix(e.Name(), "dev-") {
			continue
		}
		dst, err := os.Readlink(filepath.Join(dir, e.Name(), "block"))
		if err != nil {
			return nil, fmt.Errorf("read %s/block of %s: %v", e.Name(), md, err)
		}
		st, _ := os.ReadFile(filepath.Join(dir, e.Name(), "state"))
		role := RoleData
		switch s := string(st); {
		case strings.Contains(s, "journal"):
			role = RoleJournal
		case strings.Contains(s, "spare"):
			role = RoleSpare
		}
		out = append(out, mdMember{name: filepath.Base(dst), role: role})
	}
	return out, nil
}

func memberList(ms []mdMember) string {
	ss := make([]string, len(ms))
	for i, d := range ms {
		ss[i] = d.String()
	}
	slices.Sort(ss)
	return strings.Join(ss, ", ")
}

// raid5Layouts are the md raid5/6 layout numbers.
var raid5Layouts = map[string]string{"0": "left-asymmetric", "1": "right-asymmetric", "2": "left-symmetric", "3": "right-symmetric"}

// raid10Layout is n<k>, f<k>, o<k> from the near, far and offset fields of
// md/layout.
//
// UNVERIFIED on hardware: the raid10 and raid5/6 layout numbers.
func raid10Layout(s string) string {
	n, err := strconv.ParseUint(s, 0, 32)
	if err != nil {
		return s
	}
	near, far, offset := n&0xff, (n>>8)&0xff, n&0x10000 != 0
	out := ""
	if near > 1 {
		out += "n" + strconv.FormatUint(near, 10)
	}
	if far > 1 {
		if offset {
			out += "o" + strconv.FormatUint(far, 10)
		} else {
			out += "f" + strconv.FormatUint(far, 10)
		}
	}
	if out == "" {
		out = "n1"
	}
	return out
}

// Read builds the label of the array from sysfs, with every difference from
// a healthy, idle array of the recorded members appended.
func (m *mdadm) Read(_ context.Context, t BenchTarget) (Reading, error) {
	md := t.Target.Name
	level, err := m.mdAttr(md, "level")
	if err != nil {
		return Reading{}, fmt.Errorf("read the level of %s: %v", md, err)
	}
	got, err := m.members(md)
	if err != nil {
		return Reading{}, err
	}
	data, spare, journal := 0, 0, 0
	for _, d := range got {
		switch d.role {
		case RoleSpare:
			spare++
		case RoleJournal:
			journal++
		default:
			data++
		}
	}
	if n, err := m.mdAttr(md, "raid_disks"); err == nil {
		if k, err := strconv.Atoi(n); err == nil && k > 0 {
			data = k
		}
	}
	chunk := ""
	if c, err := m.mdAttr(md, "chunk_size"); err == nil {
		if b, err := strconv.Atoi(c); err == nil && b > 0 {
			chunk = strconv.Itoa(b/1024) + "K"
		}
	}
	loc, err := m.mdAttr(md, "bitmap/location")
	bitmap := err == nil && loc != "" && loc != "none"
	v := mdLabel(level, data, spare, journal, chunk, bitmap)

	want := make([]mdMember, len(t.Members))
	for i, d := range t.Members {
		want[i] = mdMember{name: d.Name, role: d.Role}
	}
	if w, g := memberList(want), memberList(got); w != g {
		v += fmt.Sprintf(" (members differ: want %s got %s)", w, g)
	}
	if d, _ := m.mdAttr(md, "degraded"); d != "" && d != "0" {
		v += " (degraded)"
	}
	state, _ := m.mdAttr(md, "array_state")
	if state != "clean" && state != "active" && state != "active-idle" {
		v += fmt.Sprintf(" (array_state %s)", state)
	}
	if a, err := m.mdAttr(md, "sync_action"); err == nil && a != "idle" {
		v += fmt.Sprintf(" (sync_action %s)", a)
	}
	kernel := "array_state=" + state
	if lay, err := m.mdAttr(md, "layout"); err == nil {
		switch level {
		case "raid5", "raid6":
			name, ok := raid5Layouts[lay]
			if !ok {
				name = lay
			}
			kernel += ", layout=" + name
		case "raid10":
			kernel += ", layout=" + raid10Layout(lay)
		}
	}
	return Reading{Value: v, Kernel: kernel, At: time.Now()}, nil
}

// Undo stops the array of u, found by its uuid, and wipes the superblock of
// every recorded member that carries that uuid, checking each wipe. It
// touches nothing when the array has a member bbbench did not record.
func (m *mdadm) Undo(ctx context.Context, u ArrayUndo) error {
	if u.UUID == "" {
		return fmt.Errorf("the record of %s has no uuid; nothing touched", u.Name)
	}
	present := map[string]bool{}
	for _, d := range u.Members {
		if d.Path != "" {
			present[d.Name] = true
		}
	}
	if md, ok := m.findArray(ctx, u); ok {
		ms, err := m.members(md)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("list the members of %s: %v; not touched", md, err)
		}
		var foreign []string
		for _, d := range ms {
			if !present[d.name] {
				foreign = append(foreign, d.name)
			}
		}
		if len(foreign) > 0 {
			slices.Sort(foreign)
			return fmt.Errorf("array %s has members bbbench did not record (%s); not touched", md, strings.Join(foreign, ", "))
		}
		if _, err := m.run(ctx, "--stop", "/dev/"+md); err != nil {
			return err
		}
		if err := m.waitStopped(ctx, md); err != nil {
			return err
		}
	}
	var errs []error
	for _, d := range u.Members {
		if d.Path == "" {
			errs = append(errs, fmt.Errorf("member S/N %s is not present; its superblock was not wiped", d.Serial))
			continue
		}
		if err := m.wipe(ctx, d.Path, u.UUID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// waitStopped waits up to Settle until md has no md directory or is clear
// or inactive.
func (m *mdadm) waitStopped(ctx context.Context, md string) error {
	deadline := time.Now().Add(m.env.Settle)
	for {
		if _, err := os.Stat(filepath.Join(m.env.Sys, "block", md, "md")); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		st, _ := m.mdAttr(md, "array_state")
		if st == "clear" || st == "inactive" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("array %s is still %s after mdadm --stop; no superblock was wiped", md, st)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.env.Poll):
		}
	}
}

// examine is the uuid of the md superblock on path, "" when it has none.
//
// UNVERIFIED on hardware: the exit status and output of --examine on a disk
// with no superblock (taken as an error whose stderr says "No md
// superblock"); any other error fails safe.
func (m *mdadm) examine(ctx context.Context, path string) (string, error) {
	out, err := m.env.Run(ctx, "mdadm", "--examine", "--export", path)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(strings.ToLower(string(ee.Stderr)), "no md superblock") {
			return "", nil
		}
		msg := fmt.Sprintf("mdadm --examine --export %s: %v", path, err)
		if ee != nil {
			if s := strings.TrimSpace(string(ee.Stderr)); s != "" {
				msg += ": " + s
			}
		}
		return "", errors.New(msg)
	}
	return exportUUID(out), nil
}

// wipe zeroes the superblock of path when it carries uuid, then checks that
// it is gone. A superblock of another array is left alone.
func (m *mdadm) wipe(ctx context.Context, path, uuid string) error {
	got, err := m.examine(ctx, path)
	if err != nil {
		return err
	}
	if got == "" {
		return nil
	}
	if !sameUUID(got, uuid) {
		slog.Info("member carries the superblock of another array; left alone", "path", path, "uuid", got, "want", uuid)
		return nil
	}
	if _, err := m.run(ctx, "--zero-superblock", path); err != nil {
		return err
	}
	after, err := m.examine(ctx, path)
	if err != nil {
		return fmt.Errorf("cannot check the wipe of %s: %v", path, err)
	}
	if sameUUID(after, uuid) {
		return fmt.Errorf("%s still has the superblock of array %s after mdadm --zero-superblock", path, uuid)
	}
	return nil
}

func (m *mdadm) UndoCommand(u ArrayUndo) string {
	var paths []string
	for _, d := range u.Members {
		if d.Path != "" {
			paths = append(paths, d.Path)
		}
	}
	return "mdadm --stop /dev/md/" + u.Name + "; mdadm --zero-superblock " + strings.Join(paths, " ")
}
