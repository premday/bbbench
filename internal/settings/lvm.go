package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// NewLVM is the LVM backend: one volume group over whole member disks and
// one logical volume of a linear, striped, mirror or raid type over all of
// it. The VG and the LV are both named after the variant
// ("bbbench_<group id>_<n>").
func NewLVM(env Env) Backend { return &lvm{env: env.fill()} }

type lvm struct{ env Env }

// devicesFileKey is the Extra key of the serials of the members whose
// devices-file entry pvcreate adds (comma-separated).
const devicesFileKey = "devicesfile_added"

var (
	lvStripeSizes = []string{"64K", "128K", "256K", "512K", "1024K"}
	// lvMinDisks is the least number of disks of each type.
	//
	// UNVERIFIED on hardware: the raid6 minimum of 3 data stripes (5 disks).
	lvMinDisks = map[string]int{"linear": 1, "striped": 2, "mirror": 2, "raid1": 2, "raid5": 3, "raid6": 5, "raid10": 4}
	lvDisksRe  = regexp.MustCompile(`^(\d+) disks?$`)
	lvStripsRe = regexp.MustCompile(`^(\d+) stripes$`)
	lvSizeRe   = regexp.MustCompile(`^(\d+K) stripe$`)
	// lvNameRe is a VG/LV name bbbench creates: "bbbench_" and the
	// characters LVM allows in a name.
	lvNameRe = regexp.MustCompile(`^bbbench_[A-Za-z0-9_.+-]+$`)
)

func lvHasStripeSize(level string) bool {
	return level == "striped" || level == "raid5" || level == "raid6" || level == "raid10"
}

// lvResyncs is whether an LV of this type resyncs after it is created.
func lvResyncs(level string) bool { return level == "mirror" || strings.HasPrefix(level, "raid") }

func (b *lvm) Name() string { return "lvm" }

func (b *lvm) Available() (bool, string) {
	if _, err := b.env.LookPath("lvm"); err != nil {
		return false, "the lvm2 tools are not installed"
	}
	return true, ""
}

func (b *lvm) Levels() []string {
	return []string{"linear", "striped", "mirror", "raid1", "raid5", "raid6", "raid10"}
}

func (b *lvm) Roles(level string) []Role {
	if _, ok := lvMinDisks[level]; ok {
		return []Role{RoleData}
	}
	return nil
}

func (b *lvm) Options(level string, n int) []Option {
	var opts []Option
	if level == "striped" {
		choices := []string{"2"}
		for i := 3; i <= n; i++ {
			choices = append(choices, strconv.Itoa(i))
		}
		opts = append(opts, Option{Key: "stripes", Label: "stripes", Choices: choices, Default: strconv.Itoa(max(n, 2))})
	}
	if lvHasStripeSize(level) {
		opts = append(opts, Option{Key: "stripesize", Label: "stripe size", Choices: slices.Clone(lvStripeSizes), Default: "64K"})
	}
	return opts
}

func (b *lvm) Validate(l Layout, disks []Disk) error {
	n := len(disks)
	if min, ok := lvMinDisks[l.Level]; ok && n < min {
		return fmt.Errorf("%s needs at least %d disks, has %d", l.Level, min, n)
	}
	if l.Level == "raid10" && n%2 != 0 {
		return fmt.Errorf("raid10 needs an even number of disks, has %d", n)
	}
	return nil
}

func (b *lvm) Label(l Layout, disks []Disk) string {
	stripes := 0
	if l.Level == "striped" {
		stripes, _ = strconv.Atoi(l.Options["stripes"])
	}
	return lvLabel(l.Level, len(disks), stripes, l.Options["stripesize"])
}

// lvLabel is "lvm <type> · N disks[ · <s> stripes][ · <size> stripe]"; the
// stripe count appears only for striped when it is less than N, the size
// only for the types that have one.
func lvLabel(level string, n, stripes int, size string) string {
	s := "lvm " + level + " · " + strconv.Itoa(n) + " disks"
	if n == 1 {
		s = "lvm " + level + " · 1 disk"
	}
	if level == "striped" && stripes > 0 && stripes < n {
		s += fmt.Sprintf(" · %d stripes", stripes)
	}
	if lvHasStripeSize(level) && size != "" {
		s += " · " + size + " stripe"
	}
	return s
}

// Parse is the inverse of Label; a striped value without a stripe count
// stripes over every disk.
func (b *lvm) Parse(value string) (Layout, error) {
	parts := strings.Split(value, " · ")
	head := strings.Fields(parts[0])
	if len(head) != 2 || head[0] != b.Name() {
		return Layout{}, fmt.Errorf("not an lvm layout: %q", value)
	}
	level := head[1]
	if !slices.Contains(b.Levels(), level) {
		return Layout{}, fmt.Errorf("unknown lvm type %q", level)
	}
	var m []string
	if len(parts) < 2 {
		return Layout{}, fmt.Errorf("no disks in %q", value)
	}
	if m = lvDisksRe.FindStringSubmatch(parts[1]); m == nil {
		return Layout{}, fmt.Errorf("no disks in %q", value)
	}
	n, _ := strconv.Atoi(m[1])
	l := Layout{Backend: b.Name(), Level: level, Options: map[string]string{}}
	rest := parts[2:]
	if level == "striped" {
		l.Options["stripes"] = strconv.Itoa(n)
		if len(rest) > 0 {
			if m := lvStripsRe.FindStringSubmatch(rest[0]); m != nil {
				s, _ := strconv.Atoi(m[1])
				if s < 2 || s >= n {
					return Layout{}, fmt.Errorf("bad stripe count in %q", value)
				}
				l.Options["stripes"] = m[1]
				rest = rest[1:]
			}
		}
	}
	if lvHasStripeSize(level) {
		if len(rest) == 0 {
			return Layout{}, fmt.Errorf("no stripe size in %q", value)
		}
		m := lvSizeRe.FindStringSubmatch(rest[0])
		if m == nil || !slices.Contains(lvStripeSizes, m[1]) {
			return Layout{}, fmt.Errorf("no stripe size in %q", value)
		}
		l.Options["stripesize"] = m[1]
		rest = rest[1:]
	}
	if len(rest) > 0 {
		return Layout{}, fmt.Errorf("unexpected %q in %q", rest[0], value)
	}
	return l, nil
}

// run runs "lvm <args>"; an error names the command and carries lvm's
// stderr.
func (b *lvm) run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := b.env.Run(ctx, "lvm", args...)
	if err != nil {
		return out, execError("lvm", args, err)
	}
	return out, nil
}

// exists runs a report on one object. A non-zero exit whose stderr says
// the object is not there (one of notFound, case-insensitive; any non-zero
// exit when notFound is empty) means it does not exist; any other failure
// (another lvm error such as a lock failure, lvm not run, cancelled) is an
// error, so that an ambiguous read is never taken for "nothing there".
//
// UNVERIFIED on hardware: the exit status and stderr of pvs and vgs on a
// disk or VG that does not exist ("Volume group ... not found", "Failed to
// find physical volume ...", "No physical volume label read from ...").
func (b *lvm) exists(ctx context.Context, notFound []string, args ...string) ([]byte, bool, error) {
	out, err := b.env.Run(ctx, "lvm", args...)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr := strings.ToLower(string(ee.Stderr))
			if len(notFound) == 0 || slices.ContainsFunc(notFound, func(s string) bool { return strings.Contains(stderr, s) }) {
				return nil, false, nil
			}
		}
		return nil, false, execError("lvm", args, err)
	}
	return out, true, nil
}

var (
	vgNotFound = []string{"not found"}
	pvNotFound = []string{"failed to find", "not found", "no physical volume label"}
)

// lvReport parses "report[0].<kind>[]" of a JSON report.
//
// UNVERIFIED on hardware: the JSON report fields of lvs/vgs/pvs with
// --reportformat json: segtype values (raid5 or raid5_ls), stripes of a
// raid LV (data stripes or all images), stripe_size units with --units k
// --nosuffix, sync_percent of a mirror, lv_health_status.
func lvReport(out []byte, kind string) ([]map[string]string, error) {
	var r struct {
		Report []map[string][]map[string]string `json:"report"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("parse the %s report: %v", kind, err)
	}
	if len(r.Report) == 0 {
		return nil, fmt.Errorf("the %s report is empty", kind)
	}
	return r.Report[0][kind], nil
}

// vgExists is whether the VG name exists.
func (b *lvm) vgExists(ctx context.Context, name string) (bool, error) {
	out, ok, err := b.exists(ctx, vgNotFound, "vgs", "--reportformat", "json", "-o", "vg_name", name)
	if err != nil || !ok {
		return false, err
	}
	rows, err := lvReport(out, "vg")
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r["vg_name"] == name {
			return true, nil
		}
	}
	return false, fmt.Errorf("vgs %s exits 0 but does not list it", name)
}

// vgPVs are the kernel names of the PVs of vg, sorted.
func (b *lvm) vgPVs(ctx context.Context, vg string) ([]string, error) {
	out, err := b.run(ctx, "pvs", "--reportformat", "json", "--select", "vg_name="+vg, "-o", "pv_name")
	if err != nil {
		return nil, err
	}
	rows, err := lvReport(out, "pv")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, filepath.Base(r["pv_name"]))
	}
	slices.Sort(names)
	return names, nil
}

// devicesFile lists the DEVNAME= paths of the LVM devices file; ok is
// false when lvmdevices fails (the host has no devices file).
//
// UNVERIFIED on hardware: the LVM devices file (lvmdevices,
// use_devicesfile=1): the DEVNAME= field of its listing, whether pvcreate
// adds an entry and whether pvremove already removes it.
func (b *lvm) devicesFile(ctx context.Context) (map[string]bool, bool, error) {
	out, ok, err := b.exists(ctx, nil, "lvmdevices")
	if err != nil || !ok {
		return nil, false, err
	}
	paths := map[string]bool{}
	for _, f := range strings.Fields(string(out)) {
		if p, ok := strings.CutPrefix(f, "DEVNAME="); ok {
			paths[p] = true
		}
	}
	return paths, true, nil
}

// Prepare checks that the VG name is free and records, when the host uses
// an LVM devices file, the members pvcreate will add to it. It runs only
// vgs and lvmdevices; the record holds everything Undo needs, as LVM has
// no uuid before the build.
func (b *lvm) Prepare(ctx context.Context, v Variant, _ Layout, name string) (*ArrayUndo, error) {
	if !lvNameRe.MatchString(name) {
		return nil, fmt.Errorf("%q is not a volume group name bbbench creates", name)
	}
	for _, d := range v.Disks {
		if d.Path == "" {
			return nil, fmt.Errorf("%s has no device path", d.Name)
		}
	}
	exists, err := b.vgExists(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("check volume group %s: %v", name, err)
	}
	if exists {
		return nil, fmt.Errorf("volume group %s already exists", name)
	}
	rec := &ArrayUndo{Name: name, Members: slices.Clone(v.Disks)}
	listed, ok, err := b.devicesFile(ctx)
	if err != nil {
		return nil, err
	}
	if ok {
		var added []string
		for _, d := range v.Disks {
			if listed[d.Path] {
				continue
			}
			if d.Serial == "" || strings.Contains(d.Serial, ",") {
				return nil, fmt.Errorf("%s has no usable serial to record its LVM devices file entry", d.Name)
			}
			added = append(added, d.Serial)
		}
		if len(added) > 0 {
			rec.Extra = map[string]string{devicesFileKey: strings.Join(added, ",")}
		}
	}
	return rec, nil
}

// lvTypeArgs are the lvcreate arguments of the type of l over n disks.
//
// UNVERIFIED on hardware: the lvcreate argv per type: --extents 100%FREE
// for raid and mirror LVs (metadata sub-LVs), --mirrorlog core for mirror,
// --stripes for raid10 being the number of mirrored pairs, and that --yes
// never leaves a prompt when members carry old signatures.
func lvTypeArgs(l Layout, n int) []string {
	size := strings.TrimSuffix(l.Options["stripesize"], "K") + "k"
	switch l.Level {
	case "linear":
		return []string{"--type", "linear"}
	case "striped":
		return []string{"--type", "striped", "--stripes", l.Options["stripes"], "--stripesize", size}
	case "mirror":
		return []string{"--type", "mirror", "--mirrors", strconv.Itoa(n - 1), "--mirrorlog", "core"}
	case "raid1":
		return []string{"--type", "raid1", "--mirrors", strconv.Itoa(n - 1)}
	case "raid5":
		return []string{"--type", "raid5", "--stripes", strconv.Itoa(n - 1), "--stripesize", size}
	case "raid6":
		return []string{"--type", "raid6", "--stripes", strconv.Itoa(n - 2), "--stripesize", size}
	case "raid10":
		return []string{"--type", "raid10", "--stripes", strconv.Itoa(n / 2), "--mirrors", "1", "--stripesize", size}
	}
	return nil
}

// Apply creates the PVs, the VG and the LV recorded in rec, finds the LV's
// dm device and checks that it is active. On a failure what was built is
// left to Undo.
//
// UNVERIFIED on hardware: whether --autobackup n leaves files under
// /etc/lvm/archive.
func (b *lvm) Apply(ctx context.Context, v Variant, l Layout, rec *ArrayUndo) ([]BenchTarget, error) {
	if rec == nil || !lvNameRe.MatchString(rec.Name) {
		return nil, errors.New("no lvm record with a volume group name bbbench creates")
	}
	recorded := map[string]bool{}
	for _, d := range rec.Members {
		if d.Path != "" {
			recorded[d.Path] = true
		}
	}
	typeArgs := lvTypeArgs(l, len(v.Disks))
	if typeArgs == nil {
		return nil, fmt.Errorf("unknown lvm type %q", l.Level)
	}
	var paths []string
	for _, d := range v.Disks {
		if d.Path == "" {
			return nil, fmt.Errorf("%s has no device path", d.Name)
		}
		if !recorded[d.Path] {
			return nil, fmt.Errorf("%s is not in the record of %s; nothing built", d.Path, rec.Name)
		}
		paths = append(paths, d.Path)
	}
	name := rec.Name
	if _, err := b.run(ctx, append([]string{"pvcreate", "--yes"}, paths...)...); err != nil {
		return nil, err
	}
	if _, err := b.run(ctx, append([]string{"vgcreate", "--yes", "--autobackup", "n", name}, paths...)...); err != nil {
		return nil, err
	}
	args := []string{"lvcreate", "--yes", "--autobackup", "n", "--activate", "y", "--name", name, "--extents", "100%FREE"}
	args = append(append(append(args, typeArgs...), name), paths...)
	if _, err := b.run(ctx, args...); err != nil {
		return nil, err
	}
	// The activity check comes first: an inactive LV gets no /dev link.
	out, err := b.run(ctx, "lvs", "--reportformat", "json", "--units", "k", "--nosuffix", "-o", "lv_uuid,lv_attr", name+"/"+name)
	if err != nil {
		return nil, err
	}
	rows, err := lvReport(out, "lv")
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || rows[0]["lv_uuid"] == "" {
		return nil, fmt.Errorf("logical volume %s/%s not found after lvcreate", name, name)
	}
	uuid, attr := rows[0]["lv_uuid"], rows[0]["lv_attr"]
	if !lvActive(attr) {
		return nil, fmt.Errorf("logical volume %s/%s is not active (lv_attr %s)", name, name, attr)
	}
	dm, err := b.waitDM(ctx, name)
	if err != nil {
		return nil, err
	}
	return []BenchTarget{{Kind: TargetArray,
		Target: Target{Name: dm, Path: "/dev/" + dm, Serial: uuid, Model: l.Level, Vendor: "lvm", Interface: "lvm"},
		UUID:   uuid, Members: slices.Clone(v.Disks), Want: v.Value.ID}}, nil
}

// lvActive is whether the fifth lv_attr character (state) is "a".
func lvActive(attr string) bool { return len(attr) >= 5 && attr[4] == 'a' }

// waitDM polls the link <Dev>/<vg>/<lv> until Settle and returns the dm
// kernel name it points to.
//
// UNVERIFIED on hardware: whether udev creates /dev/<vg>/<lv> within
// Env.Settle after lvcreate --activate y.
func (b *lvm) waitDM(ctx context.Context, name string) (string, error) {
	link := filepath.Join(b.env.Dev, name, name)
	deadline := time.Now().Add(b.env.Settle)
	for {
		if dst, err := os.Readlink(link); err == nil {
			if dm := filepath.Base(dst); strings.HasPrefix(dm, "dm-") {
				return dm, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("/dev/%s/%s did not appear after lvcreate", name, name)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(b.env.Poll):
		}
	}
}

// lvSelect runs lvs on the LV with uuid and returns its one row.
func (b *lvm) lvSelect(ctx context.Context, uuid, fields string) (map[string]string, error) {
	if uuid == "" {
		return nil, errors.New("the target has no logical volume uuid")
	}
	out, err := b.run(ctx, "lvs", "--reportformat", "json", "--units", "k", "--nosuffix", "--select", "lv_uuid="+uuid, "-o", fields)
	if err != nil {
		return nil, err
	}
	rows, err := lvReport(out, "lv")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("logical volume %s not found", uuid)
	}
	return rows[0], nil
}

// WaitSteady polls the sync percentage of the mirror and raid LVs until
// every one is at 100; linear and striped LVs are steady at once. It has no
// timeout.
func (b *lvm) WaitSteady(ctx context.Context, ts []BenchTarget, progress func(pct float64)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		steady, sum := true, 0.0
		for _, t := range ts {
			if !lvResyncs(t.Target.Model) {
				sum += 100
				continue
			}
			row, err := b.lvSelect(ctx, t.UUID, "sync_percent")
			if err != nil {
				return err
			}
			pct, err := strconv.ParseFloat(strings.TrimSpace(row["sync_percent"]), 64)
			if err != nil {
				pct = 0
			}
			if pct < 100 {
				steady = false
			}
			sum += min(pct, 100)
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
		case <-time.After(b.env.Poll):
		}
	}
}

// lvType is the layout type of a segtype: raid5_* reads as raid5, raid6_*
// as raid6.
func lvType(segtype string) string {
	for _, p := range []string{"raid5", "raid6"} {
		if strings.HasPrefix(segtype, p+"_") {
			return p
		}
	}
	return segtype
}

// stripeSizeK is a stripe_size in KiB ("64.00") as a label size ("64K").
func stripeSizeK(s string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64) + "K"
}

// Read builds the label of the LV from its report, with every difference
// from an active, healthy, synced LV on the recorded members appended.
func (b *lvm) Read(ctx context.Context, t BenchTarget) (Reading, error) {
	row, err := b.lvSelect(ctx, t.UUID, "vg_name,lv_name,segtype,stripes,stripe_size,lv_attr,sync_percent,lv_health_status")
	if err != nil {
		return Reading{}, err
	}
	got, err := b.vgPVs(ctx, row["vg_name"])
	if err != nil {
		return Reading{}, err
	}
	seg := row["segtype"]
	typ := lvType(seg)
	stripes := 0
	if typ == "striped" {
		stripes, _ = strconv.Atoi(strings.TrimSpace(row["stripes"]))
	}
	size := ""
	if lvHasStripeSize(typ) {
		size = stripeSizeK(row["stripe_size"])
	}
	v := lvLabel(typ, len(got), stripes, size)

	want := make([]string, 0, len(t.Members))
	for _, d := range t.Members {
		want = append(want, d.Name)
	}
	slices.Sort(want)
	if w, g := strings.Join(want, ", "), strings.Join(got, ", "); w != g {
		v += fmt.Sprintf(" (members differ: want %s got %s)", w, g)
	}
	attr := row["lv_attr"]
	if !lvActive(attr) {
		v += " (not active)"
	}
	if h := strings.TrimSpace(row["lv_health_status"]); h != "" {
		v += " (health " + h + ")"
	}
	if lvResyncs(typ) {
		sp := strings.TrimSpace(row["sync_percent"])
		if p, err := strconv.ParseFloat(sp, 64); err != nil {
			v += fmt.Sprintf(" (sync_percent %q)", sp)
		} else if p < 100 {
			v += " (syncing " + strconv.FormatFloat(p, 'f', -1, 64) + "%)"
		}
	}
	return Reading{Value: v, Kernel: "segtype=" + seg + ", lv_attr=" + attr, At: time.Now()}, nil
}

// Undo removes the LV and the VG of u when every PV of the VG is a present
// recorded member, then the LVM label of every recorded member that is an
// orphan PV, then the devices-file entries pvcreate added; each step is
// checked. It touches nothing it cannot prove bbbench created.
func (b *lvm) Undo(ctx context.Context, u ArrayUndo) error {
	if !lvNameRe.MatchString(u.Name) {
		return fmt.Errorf("%q is not a volume group name bbbench creates; nothing touched", u.Name)
	}
	name := u.Name
	present := map[string]bool{}
	for _, d := range u.Members {
		if d.Path != "" {
			present[d.Name] = true
		}
	}
	exists, err := b.vgExists(ctx, name)
	if err != nil {
		return fmt.Errorf("check volume group %s: %v; not touched", name, err)
	}
	if exists {
		pvs, err := b.vgPVs(ctx, name)
		if err != nil {
			return fmt.Errorf("list the physical volumes of %s: %v; not touched", name, err)
		}
		if len(pvs) == 0 {
			return fmt.Errorf("volume group %s lists no physical volume bbbench recorded; not touched", name)
		}
		var foreign []string
		for _, p := range pvs {
			if !present[p] {
				foreign = append(foreign, p)
			}
		}
		if len(foreign) > 0 {
			return fmt.Errorf("volume group %s has physical volumes bbbench did not record (%s); not touched", name, strings.Join(foreign, ", "))
		}
		if _, err := b.run(ctx, "lvremove", "--yes", "--autobackup", "n", name); err != nil {
			return err
		}
		if _, err := b.run(ctx, "vgremove", "--yes", "--autobackup", "n", name); err != nil {
			return err
		}
		still, err := b.vgExists(ctx, name)
		if err != nil {
			return fmt.Errorf("check volume group %s after vgremove: %v", name, err)
		}
		if still {
			return fmt.Errorf("volume group %s still exists after vgremove", name)
		}
	}
	var errs []error
	failed := map[string]bool{}
	for _, d := range u.Members {
		if d.Path == "" {
			errs = append(errs, fmt.Errorf("member S/N %s is not present; its LVM label was not removed", d.Serial))
			continue
		}
		removed, err := b.removeLabel(ctx, d.Path, name)
		if err != nil {
			errs = append(errs, err)
		}
		if !removed {
			// Left alone or failed: its devices-file entry is not
			// bbbench's to remove.
			failed[d.Path] = true
		}
	}
	if err := b.removeDevicesEntries(ctx, u, failed); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// pvVG reports whether path is a PV and the VG it belongs to.
func (b *lvm) pvVG(ctx context.Context, path string) (bool, string, error) {
	out, ok, err := b.exists(ctx, pvNotFound, "pvs", "--reportformat", "json", "-o", "pv_name,vg_name", path)
	if err != nil || !ok {
		return false, "", err
	}
	rows, err := lvReport(out, "pv")
	if err != nil {
		return false, "", err
	}
	if len(rows) != 1 {
		return false, "", fmt.Errorf("pvs %s lists %d physical volumes", path, len(rows))
	}
	return true, rows[0]["vg_name"], nil
}

// removeLabel runs pvremove on path when it is an orphan PV and checks the
// label is gone. removed is true when path carries no LVM label at the end
// (removed here or none to begin with); it is false when the path was left
// alone (a PV of another volume group) or on an error. A PV still in the
// volume group own is an error: bbbench's VG is not gone.
func (b *lvm) removeLabel(ctx context.Context, path, own string) (removed bool, err error) {
	isPV, vg, err := b.pvVG(ctx, path)
	if err != nil {
		return false, fmt.Errorf("check the LVM label of %s: %v", path, err)
	}
	if !isPV {
		return true, nil
	}
	if vg == own {
		return false, fmt.Errorf("%s is still a physical volume of volume group %s", path, own)
	}
	if vg != "" {
		slog.Info("member is a physical volume of another volume group; left alone", "path", path, "vg", vg)
		return false, nil
	}
	if _, err := b.run(ctx, "pvremove", "--yes", path); err != nil {
		return false, err
	}
	still, _, err := b.pvVG(ctx, path)
	if err != nil {
		return false, fmt.Errorf("cannot check the pvremove of %s: %v", path, err)
	}
	if still {
		return false, fmt.Errorf("%s still carries an LVM label after pvremove", path)
	}
	return true, nil
}

// removeDevicesEntries removes the devices-file entries of the recorded
// serials whose member is present and has no LVM label left (skip: left
// alone or failed) and checks they are gone. A serial that is not a recorded member is ignored.
func (b *lvm) removeDevicesEntries(ctx context.Context, u ArrayUndo, skip map[string]bool) error {
	added := u.Extra[devicesFileKey]
	if added == "" {
		return nil
	}
	var paths []string
	for _, serial := range strings.Split(added, ",") {
		for _, d := range u.Members {
			if serial != "" && d.Serial == serial && d.Path != "" && !skip[d.Path] {
				paths = append(paths, d.Path)
				break
			}
		}
	}
	return b.delDevicesEntries(ctx, paths)
}

// delDevicesEntries removes the devices-file entries of paths that are
// listed (an unlisted path is skipped: pvremove may already have removed
// it) and checks they are gone. No devices file means nothing to do.
func (b *lvm) delDevicesEntries(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	listed, ok, err := b.devicesFile(ctx)
	if err != nil {
		return fmt.Errorf("list the LVM devices file: %v", err)
	}
	if !ok {
		return nil // no devices file any more
	}
	var removed []string
	for _, p := range paths {
		if !listed[p] {
			continue
		}
		if _, err := b.run(ctx, "lvmdevices", "--deldev", p); err != nil {
			return err
		}
		removed = append(removed, p)
	}
	if len(removed) == 0 {
		return nil
	}
	listed, _, err = b.devicesFile(ctx)
	if err != nil {
		return fmt.Errorf("check the LVM devices file: %v", err)
	}
	for _, p := range removed {
		if listed[p] {
			return fmt.Errorf("%s is still in the LVM devices file after lvmdevices --deldev", p)
		}
	}
	return nil
}

func (b *lvm) UndoCommand(u ArrayUndo) string {
	var paths []string
	for _, d := range u.Members {
		if d.Path != "" {
			paths = append(paths, d.Path)
		}
	}
	return "lvremove -y " + u.Name + "; vgremove -y " + u.Name + "; pvremove -y " + strings.Join(paths, " ")
}
