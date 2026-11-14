package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// lvRow is one logical volume of lvSim.
type lvRow struct {
	VG, LV, Segtype, Stripes, StripeSize, Attr, Sync, Health string
}

// lvSim is a fake lvm2 over a temporary /dev directory. It keeps the PVs,
// VGs and LVs, answers the JSON reports from them, records every command
// and can be told to fail.
type lvSim struct {
	t       *testing.T
	dev     string
	calls   []string
	pvs     map[string]string // PV path -> VG name ("" orphan)
	vgs     map[string]bool
	lvs     map[string]*lvRow // lv uuid -> LV
	devices []string          // devices file entries; nil: no devices file
	syncSeq []string          // sync_percent answers of WaitSteady, in order
	fail    map[string]error  // command prefix -> error returned instead
	noLink  bool              // lvcreate makes no /dev/<vg>/<lv> link
}

func newLVSim(t *testing.T) *lvSim {
	t.Helper()
	return &lvSim{t: t, dev: t.TempDir(), pvs: map[string]string{}, vgs: map[string]bool{},
		lvs: map[string]*lvRow{}, fail: map[string]error{}}
}

func (s *lvSim) env() Env {
	return Env{Run: s.run, LookPath: lookAll, Sys: s.t.TempDir(), Dev: s.dev,
		Poll: time.Millisecond, Settle: 200 * time.Millisecond, InUse: freeDisks}
}

func report(kind string, rows ...map[string]string) []byte {
	if rows == nil {
		rows = []map[string]string{}
	}
	b, err := json.Marshal(map[string]any{"report": []any{map[string]any{kind: rows}}})
	if err != nil {
		panic(err)
	}
	return b
}

// flag is the value after name in args.
func flag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (s *lvSim) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, key)
	for p, err := range s.fail {
		if strings.HasPrefix(key, p) {
			return nil, err
		}
	}
	if name != "lvm" || len(args) == 0 {
		return nil, errors.New("unexpected " + key)
	}
	last := args[len(args)-1]
	switch args[0] {
	case "vgs":
		if s.vgs[last] {
			return report("vg", map[string]string{"vg_name": last}), nil
		}
		return nil, exitErr("  Volume group \"" + last + "\" not found\n")
	case "pvs":
		if sel := flag(args, "--select"); sel != "" {
			vg := strings.TrimPrefix(sel, "vg_name=")
			var rows []map[string]string
			for _, p := range slices.Sorted(mapsKeys(s.pvs)) {
				if s.pvs[p] == vg {
					rows = append(rows, map[string]string{"pv_name": p})
				}
			}
			return report("pv", rows...), nil
		}
		if vg, ok := s.pvs[last]; ok {
			return report("pv", map[string]string{"pv_name": last, "vg_name": vg}), nil
		}
		return nil, exitErr("  Failed to find physical volume \"" + last + "\".\n")
	case "pvcreate":
		for _, p := range args[2:] {
			s.pvs[p] = ""
			if s.devices != nil && !slices.Contains(s.devices, p) {
				s.devices = append(s.devices, p)
			}
		}
		return nil, nil
	case "vgcreate":
		vg := args[4]
		s.vgs[vg] = true
		for _, p := range args[5:] {
			s.pvs[p] = vg
		}
		return nil, nil
	case "lvcreate":
		return nil, s.lvcreate(args)
	case "lvs":
		if sel := flag(args, "--select"); sel != "" {
			lv, ok := s.lvs[strings.TrimPrefix(sel, "lv_uuid=")]
			if !ok {
				return report("lv"), nil
			}
			if flag(args, "-o") == "sync_percent" {
				if len(s.syncSeq) > 0 {
					lv.Sync, s.syncSeq = s.syncSeq[0], s.syncSeq[1:]
				}
				return report("lv", map[string]string{"sync_percent": lv.Sync}), nil
			}
			return report("lv", map[string]string{"vg_name": lv.VG, "lv_name": lv.LV, "segtype": lv.Segtype,
				"stripes": lv.Stripes, "stripe_size": lv.StripeSize, "lv_attr": lv.Attr,
				"sync_percent": lv.Sync, "lv_health_status": lv.Health}), nil
		}
		for u, lv := range s.lvs {
			if lv.VG+"/"+lv.LV == last {
				return report("lv", map[string]string{"lv_uuid": u, "lv_attr": lv.Attr}), nil
			}
		}
		return nil, exitErr("  Failed to find logical volume \"" + last + "\"\n")
	case "lvremove":
		for u, lv := range s.lvs {
			if lv.VG == last {
				delete(s.lvs, u)
			}
		}
		return nil, nil
	case "vgremove":
		delete(s.vgs, last)
		for p, vg := range s.pvs {
			if vg == last {
				s.pvs[p] = ""
			}
		}
		return nil, nil
	case "pvremove":
		delete(s.pvs, last)
		return nil, nil
	case "lvmdevices":
		if s.devices == nil {
			return nil, exitErr("  Devices file not enabled.\n")
		}
		if len(args) == 3 && args[1] == "--deldev" {
			s.devices = slices.DeleteFunc(s.devices, func(p string) bool { return p == args[2] })
			return nil, nil
		}
		var b strings.Builder
		for _, p := range s.devices {
			fmt.Fprintf(&b, "  Device %s IDTYPE=sys_serial IDNAME=X DEVNAME=%s PVID=abc\n", p, p)
		}
		return []byte(b.String()), nil
	}
	return nil, errors.New("unexpected " + key)
}

func mapsKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// lvcreate records the LV of the argv and makes <Dev>/<vg>/<lv> -> ../dm-3.
func (s *lvSim) lvcreate(args []string) error {
	name := flag(args, "--name")
	typ := flag(args, "--type")
	var paths []string
	for _, a := range args {
		if strings.HasPrefix(a, "/dev/") {
			paths = append(paths, a)
		}
	}
	lv := &lvRow{VG: name, LV: name, Segtype: typ, Stripes: "1", StripeSize: "0", Attr: "-wi-a-----"}
	if st := flag(args, "--stripesize"); st != "" {
		lv.StripeSize = strings.TrimSuffix(st, "k") + ".00"
	}
	switch typ {
	case "striped":
		lv.Stripes = flag(args, "--stripes")
	case "mirror":
		lv.Attr, lv.Sync = "mwi-a-m---", "100.00"
	case "linear":
	default:
		lv.Attr, lv.Sync, lv.Stripes = "rwi-a-r---", "100.00", fmt.Sprint(len(paths))
	}
	s.lvs["LVUUID-1"] = lv
	if s.noLink {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(s.dev, name), 0o755); err != nil {
		s.t.Fatal(err)
	}
	return os.Symlink("../dm-3", filepath.Join(s.dev, name, name))
}

func lvLayout(level string, opts ...string) Layout {
	l := Layout{Backend: "lvm", Level: level, Options: map[string]string{}}
	for i := 0; i+1 < len(opts); i += 2 {
		l.Options[opts[i]] = opts[i+1]
	}
	return l
}

// dataDisks are n data disks sdb, sdc, … with serials S1, S2, ….
func dataDisks(n int) []Disk {
	var ds []Disk
	for i := range n {
		ds = append(ds, dsk(fmt.Sprintf("sd%c", 'b'+i), fmt.Sprintf("S%d", i+1), RoleData))
	}
	return ds
}

// lvBuild prepares and applies l on ds and returns the record and targets.
func lvBuild(t *testing.T, s *lvSim, l Layout, ds []Disk) (*ArrayUndo, []BenchTarget) {
	t.Helper()
	b := NewLVM(s.env())
	v := variant("1_1", b.Label(l, ds), ds)
	rec, err := b.Prepare(context.Background(), v, l, "bbbench_1_1")
	if err != nil {
		t.Fatal(err)
	}
	rec.Backend = "lvm"
	ts, err := b.Apply(context.Background(), v, l, rec)
	if err != nil {
		t.Fatal(err)
	}
	return rec, ts
}

func TestLVM_Identity(t *testing.T) {
	b := NewLVM(Env{})
	if b.Name() != "lvm" {
		t.Errorf("name %q", b.Name())
	}
	want := []string{"linear", "striped", "mirror", "raid1", "raid5", "raid6", "raid10"}
	if !slices.Equal(b.Levels(), want) {
		t.Errorf("levels %v", b.Levels())
	}
	for _, lv := range want {
		if got := b.Roles(lv); !slices.Equal(got, []Role{RoleData}) {
			t.Errorf("roles %s %v", lv, got)
		}
	}
}

func TestLVM_Available(t *testing.T) {
	if ok, why := NewLVM(Env{LookPath: lookNone}).Available(); ok || why != "the lvm2 tools are not installed" {
		t.Errorf("%v %q", ok, why)
	}
	if ok, why := NewLVM(Env{LookPath: lookAll}).Available(); !ok || why != "" {
		t.Errorf("%v %q", ok, why)
	}
}

func TestLVM_Options(t *testing.T) {
	b := NewLVM(Env{})
	size := Option{Key: "stripesize", Label: "stripe size", Choices: []string{"64K", "128K", "256K", "512K", "1024K"}, Default: "64K"}
	stripes := Option{Key: "stripes", Label: "stripes", Choices: []string{"2", "3", "4"}, Default: "4"}
	if got := b.Options("striped", 4); !reflect.DeepEqual(got, []Option{stripes, size}) {
		t.Errorf("striped %#v", got)
	}
	one := Option{Key: "stripes", Label: "stripes", Choices: []string{"2"}, Default: "2"}
	if got := b.Options("striped", 1); !reflect.DeepEqual(got, []Option{one, size}) {
		t.Errorf("striped 1 disk %#v", got)
	}
	for _, lv := range []string{"raid5", "raid6", "raid10"} {
		if got := b.Options(lv, 5); !reflect.DeepEqual(got, []Option{size}) {
			t.Errorf("%s %#v", lv, got)
		}
	}
	for _, lv := range []string{"linear", "mirror", "raid1"} {
		if got := b.Options(lv, 4); len(got) != 0 {
			t.Errorf("%s %#v", lv, got)
		}
	}
}

func TestLVM_Validate(t *testing.T) {
	b := NewLVM(Env{})
	cases := []struct {
		l    Layout
		n    int
		want string
	}{
		{lvLayout("raid6", "stripesize", "64K"), 4, "raid6 needs at least 5 disks, has 4"},
		{lvLayout("raid10", "stripesize", "64K"), 5, "raid10 needs an even number of disks, has 5"},
		{lvLayout("raid10", "stripesize", "64K"), 3, "raid10 needs at least 4 disks, has 3"},
		{lvLayout("striped", "stripes", "2", "stripesize", "64K"), 1, "striped needs at least 2 disks, has 1"},
		{lvLayout("mirror"), 1, "mirror needs at least 2 disks, has 1"},
		{lvLayout("raid1"), 1, "raid1 needs at least 2 disks, has 1"},
		{lvLayout("raid5", "stripesize", "64K"), 2, "raid5 needs at least 3 disks, has 2"},
		{lvLayout("linear"), 0, "linear needs at least 1 disks, has 0"},
		{lvLayout("linear"), 1, ""},
		{lvLayout("raid10", "stripesize", "64K"), 6, ""},
		{lvLayout("raid6", "stripesize", "64K"), 5, ""},
	}
	for _, c := range cases {
		err := b.Validate(c.l, dataDisks(c.n))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("Validate(%v, %d) = %q, want %q", c.l, c.n, got, c.want)
		}
	}
}

func TestLVM_LabelAndParse(t *testing.T) {
	b := NewLVM(Env{})
	cases := []struct {
		label string
		l     Layout
		n     int
	}{
		{"lvm linear · 1 disk", lvLayout("linear"), 1},
		{"lvm striped · 4 disks · 64K stripe", lvLayout("striped", "stripes", "4", "stripesize", "64K"), 4},
		{"lvm striped · 4 disks · 2 stripes · 128K stripe", lvLayout("striped", "stripes", "2", "stripesize", "128K"), 4},
		{"lvm mirror · 2 disks", lvLayout("mirror"), 2},
		{"lvm raid1 · 3 disks", lvLayout("raid1"), 3},
		{"lvm raid5 · 3 disks · 64K stripe", lvLayout("raid5", "stripesize", "64K"), 3},
		{"lvm raid10 · 4 disks · 256K stripe", lvLayout("raid10", "stripesize", "256K"), 4},
	}
	for _, c := range cases {
		if got := b.Label(c.l, dataDisks(c.n)); got != c.label {
			t.Errorf("Label(%v, %d) = %q, want %q", c.l, c.n, got, c.label)
		}
		got, err := b.Parse(c.label)
		if err != nil || !reflect.DeepEqual(got, c.l) {
			t.Errorf("Parse(%q) = %#v, %v; want %#v", c.label, got, err, c.l)
		}
	}
	for _, bad := range []string{
		"lvm raid7 · 2 disks", "mdadm raid5 · 3 disks · 64K stripe", "lvm raid5 · 3 disks",
		"lvm mirror · 2 disks · 64K stripe", "lvm striped · 4 disks · 5 stripes · 64K stripe",
		"lvm striped · 4 disks · 4 stripes · 64K stripe", "lvm linear · two disks", "lvm raid5 · 3 disks · 32K stripe",
		"lvm raid1 · 2 disks · fast",
	} {
		if l, err := b.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted: %#v", bad, l)
		}
	}
}

func TestLVM_PrepareRefusesExistingVG(t *testing.T) {
	s := newLVSim(t)
	s.vgs["bbbench_1_1"] = true
	_, err := NewLVM(s.env()).Prepare(context.Background(), variant("1_1", "x", dataDisks(2)), lvLayout("raid1"), "bbbench_1_1")
	if err == nil || err.Error() != "volume group bbbench_1_1 already exists" {
		t.Errorf("%v", err)
	}
}

func TestLVM_PrepareRefusesBadName(t *testing.T) {
	s := newLVSim(t)
	for _, name := range []string{"", "vg0", "bbbench_", "bbbench_1/1", "bbbench_1 1"} {
		if _, err := NewLVM(s.env()).Prepare(context.Background(), variant("1_1", "x", dataDisks(2)), lvLayout("raid1"), name); err == nil {
			t.Errorf("Prepare accepted name %q", name)
		}
	}
	if len(s.calls) != 0 {
		t.Errorf("ran %v", s.calls)
	}
}

func TestLVM_PrepareRecordsDevicesFile(t *testing.T) {
	s := newLVSim(t)
	s.devices = []string{"/dev/sdb"}
	ds := dataDisks(3)
	rec, err := NewLVM(s.env()).Prepare(context.Background(), variant("1_1", "x", ds), lvLayout("raid5"), "bbbench_1_1")
	if err != nil {
		t.Fatal(err)
	}
	want := &ArrayUndo{Name: "bbbench_1_1", Members: ds, Extra: map[string]string{"devicesfile_added": "S2,S3"}}
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("%+v", rec)
	}
	for _, c := range s.calls {
		if !strings.HasPrefix(c, "lvm vgs ") && c != "lvm lvmdevices" {
			t.Errorf("ran %s", c)
		}
	}

	s = newLVSim(t)
	rec, err = NewLVM(s.env()).Prepare(context.Background(), variant("1_1", "x", ds), lvLayout("raid5"), "bbbench_1_1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Extra != nil || rec.UUID != "" {
		t.Errorf("no devices file: %+v", rec)
	}
	if want := []string{"lvm vgs --reportformat json -o vg_name bbbench_1_1", "lvm lvmdevices"}; !slices.Equal(s.calls, want) {
		t.Errorf("calls %q", s.calls)
	}
}

func TestLVM_PrepareRefusesMemberWithoutPath(t *testing.T) {
	s := newLVSim(t)
	ds := dataDisks(2)
	ds[1].Path = ""
	if _, err := NewLVM(s.env()).Prepare(context.Background(), variant("1_1", "x", ds), lvLayout("raid1"), "bbbench_1_1"); err == nil {
		t.Error("accepted a member without a path")
	}
}

func TestLVM_CreateArgs(t *testing.T) {
	const pv = "lvm pvcreate --yes "
	const lvHead = "lvm lvcreate --yes --autobackup n --activate y --name bbbench_1_1 --extents 100%FREE "
	cases := []struct {
		l    Layout
		n    int
		want string
	}{
		{lvLayout("linear"), 1, "--type linear"},
		{lvLayout("striped", "stripes", "2", "stripesize", "128K"), 4, "--type striped --stripes 2 --stripesize 128k"},
		{lvLayout("striped", "stripes", "3", "stripesize", "1024K"), 3, "--type striped --stripes 3 --stripesize 1024k"},
		{lvLayout("mirror"), 2, "--type mirror --mirrors 1 --mirrorlog core"},
		{lvLayout("raid1"), 3, "--type raid1 --mirrors 2"},
		{lvLayout("raid5", "stripesize", "64K"), 3, "--type raid5 --stripes 2 --stripesize 64k"},
		{lvLayout("raid6", "stripesize", "256K"), 5, "--type raid6 --stripes 3 --stripesize 256k"},
		{lvLayout("raid10", "stripesize", "64K"), 4, "--type raid10 --stripes 2 --mirrors 1 --stripesize 64k"},
	}
	for _, c := range cases {
		s := newLVSim(t)
		ds := dataDisks(c.n)
		var paths []string
		for _, d := range ds {
			paths = append(paths, d.Path)
		}
		p := strings.Join(paths, " ")
		lvBuild(t, s, c.l, ds)
		want := []string{
			pv + p,
			"lvm vgcreate --yes --autobackup n bbbench_1_1 " + p,
			lvHead + c.want + " bbbench_1_1 " + p,
		}
		if len(s.calls) < 5 || !slices.Equal(s.calls[2:5], want) {
			t.Errorf("%s:\n got %q\nwant %q", c.l.Level, s.calls, want)
		}
	}
}

func TestLVM_ApplyResolvesDM(t *testing.T) {
	s := newLVSim(t)
	ds := dataDisks(3)
	l := lvLayout("raid5", "stripesize", "64K")
	_, ts := lvBuild(t, s, l, ds)
	want := []BenchTarget{{Kind: TargetArray,
		Target: Target{Name: "dm-3", Path: "/dev/dm-3", Serial: "LVUUID-1", Model: "raid5", Vendor: "lvm", Interface: "lvm"},
		UUID:   "LVUUID-1", Members: ds, Want: "lvm raid5 · 3 disks · 64K stripe"}}
	if !reflect.DeepEqual(ts, want) {
		t.Errorf("\n got %+v\nwant %+v", ts, want)
	}
	if !slices.Contains(s.calls, "lvm lvs --reportformat json --units k --nosuffix -o lv_uuid,lv_attr bbbench_1_1/bbbench_1_1") {
		t.Errorf("calls %q", s.calls)
	}
}

func TestLVM_ApplyRefusesInactiveLV(t *testing.T) {
	s := newLVSim(t)
	env := s.env()
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := s.run(ctx, name, args...)
		if len(args) > 0 && args[0] == "lvcreate" {
			s.lvs["LVUUID-1"].Attr = "rwi---r---"
		}
		return out, err
	}
	b := NewLVM(env)
	ds := dataDisks(2)
	l := lvLayout("raid1")
	v := variant("1_1", b.Label(l, ds), ds)
	rec, err := b.Prepare(context.Background(), v, l, "bbbench_1_1")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := b.Apply(context.Background(), v, l, rec)
	if ts != nil || err == nil || err.Error() != "logical volume bbbench_1_1/bbbench_1_1 is not active (lv_attr rwi---r---)" {
		t.Errorf("%v %v", ts, err)
	}
}

func TestLVM_ApplyCreateFails(t *testing.T) {
	s := newLVSim(t)
	s.fail["lvm lvcreate"] = exitErr("  Insufficient free space: 2000 extents needed, but only 1000 available\n")
	b := NewLVM(s.env())
	ds := dataDisks(2)
	l := lvLayout("raid1")
	v := variant("1_1", b.Label(l, ds), ds)
	rec, err := b.Prepare(context.Background(), v, l, "bbbench_1_1")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := b.Apply(context.Background(), v, l, rec)
	if ts != nil || err == nil || !strings.Contains(err.Error(), "lvcreate") || !strings.Contains(err.Error(), "Insufficient free space") {
		t.Errorf("%v %v", ts, err)
	}
	// What was built is left to Undo.
	if !s.vgs["bbbench_1_1"] {
		t.Error("the VG was removed by Apply")
	}
}

func TestLVM_ApplyRefusesUnrecordedDisk(t *testing.T) {
	s := newLVSim(t)
	b := NewLVM(s.env())
	ds := dataDisks(2)
	l := lvLayout("raid1")
	v := variant("1_1", b.Label(l, ds), ds)
	rec := &ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: ds[:1]}
	if _, err := b.Apply(context.Background(), v, l, rec); err == nil {
		t.Error("built on a disk the record does not hold")
	}
	if len(s.calls) != 0 {
		t.Errorf("ran %v", s.calls)
	}
}

func TestLVM_WaitSteady(t *testing.T) {
	s := newLVSim(t)
	_, ts := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	s.syncSeq = []string{"25.00", "100.00"}
	b := NewLVM(s.env())
	var got []float64
	if err := b.WaitSteady(context.Background(), ts, func(p float64) { got = append(got, p) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []float64{25}) {
		t.Errorf("progress %v", got)
	}
	if !slices.Contains(s.calls, "lvm lvs --reportformat json --units k --nosuffix --select lv_uuid=LVUUID-1 -o sync_percent") {
		t.Errorf("calls %q", s.calls)
	}

	s2 := newLVSim(t)
	_, ts2 := lvBuild(t, s2, lvLayout("striped", "stripes", "2", "stripesize", "64K"), dataDisks(2))
	s2.calls = nil
	if err := NewLVM(s2.env()).WaitSteady(context.Background(), ts2, nil); err != nil || len(s2.calls) != 0 {
		t.Errorf("striped: %v %v", err, s2.calls)
	}

	s.syncSeq = []string{"10.00"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.WaitSteady(ctx, ts, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestLVM_ReadLV(t *testing.T) {
	s := newLVSim(t)
	_, ts := lvBuild(t, s, lvLayout("raid5", "stripesize", "64K"), dataDisks(3))
	s.calls = nil
	r, err := NewLVM(s.env()).Read(context.Background(), ts[0])
	if err != nil || r.Value != ts[0].Want || r.Kernel != "segtype=raid5, lv_attr=rwi-a-r---" {
		t.Errorf("%+v %v", r, err)
	}
	want := []string{
		"lvm lvs --reportformat json --units k --nosuffix --select lv_uuid=LVUUID-1 -o vg_name,lv_name,segtype,stripes,stripe_size,lv_attr,sync_percent,lv_health_status",
		"lvm pvs --reportformat json --select vg_name=bbbench_1_1 -o pv_name",
	}
	if !slices.Equal(s.calls, want) {
		t.Errorf("calls %q", s.calls)
	}
	// raid5_ls reads as raid5.
	s.lvs["LVUUID-1"].Segtype = "raid5_ls"
	if r, err := NewLVM(s.env()).Read(context.Background(), ts[0]); err != nil || r.Value != ts[0].Want || r.Kernel != "segtype=raid5_ls, lv_attr=rwi-a-r---" {
		t.Errorf("raid5_ls: %+v %v", r, err)
	}
	// A striped LV reads its stripe count and size.
	s2 := newLVSim(t)
	_, ts2 := lvBuild(t, s2, lvLayout("striped", "stripes", "2", "stripesize", "128K"), dataDisks(4))
	if r, err := NewLVM(s2.env()).Read(context.Background(), ts2[0]); err != nil || r.Value != "lvm striped · 4 disks · 2 stripes · 128K stripe" {
		t.Errorf("striped: %+v %v", r, err)
	}
	delete(s.lvs, "LVUUID-1")
	if _, err := NewLVM(s.env()).Read(context.Background(), ts[0]); err == nil || err.Error() != "logical volume LVUUID-1 not found" {
		t.Errorf("gone: %v", err)
	}
}

func TestLVM_ReadDetectsHealthAndSync(t *testing.T) {
	s := newLVSim(t)
	_, ts := lvBuild(t, s, lvLayout("raid5", "stripesize", "64K"), dataDisks(3))
	lv := s.lvs["LVUUID-1"]
	lv.Health, lv.Sync = "refresh needed", "87.50"
	r, err := NewLVM(s.env()).Read(context.Background(), ts[0])
	if want := ts[0].Want + " (health refresh needed) (syncing 87.5%)"; err != nil || r.Value != want {
		t.Errorf("\n got %q\nwant %q", r.Value, want)
	}
	lv.Health, lv.Sync, lv.Attr = "", "100.00", "rwi---r---"
	r, _ = NewLVM(s.env()).Read(context.Background(), ts[0])
	if want := ts[0].Want + " (not active)"; r.Value != want {
		t.Errorf("\n got %q\nwant %q", r.Value, want)
	}
	lv.Attr, lv.Segtype = "rwi-a-r---", "raid6_zr"
	r, _ = NewLVM(s.env()).Read(context.Background(), ts[0])
	if r.Value == ts[0].Want {
		t.Errorf("raid6 reads as %q", r.Value)
	}
}

func TestLVM_ReadWrongMembers(t *testing.T) {
	s := newLVSim(t)
	_, ts := lvBuild(t, s, lvLayout("raid5", "stripesize", "64K"), dataDisks(3))
	delete(s.pvs, "/dev/sdc")
	s.pvs["/dev/sdx"] = "bbbench_1_1"
	r, err := NewLVM(s.env()).Read(context.Background(), ts[0])
	if want := ts[0].Want + " (members differ: want sdb, sdc, sdd got sdb, sdd, sdx)"; err != nil || r.Value != want {
		t.Errorf("\n got %q\nwant %q", r.Value, want)
	}
}

func TestLVM_UndoRemovesAll(t *testing.T) {
	s := newLVSim(t)
	rec, _ := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	s.calls = nil
	if err := NewLVM(s.env()).Undo(context.Background(), *rec); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"lvm vgs --reportformat json -o vg_name bbbench_1_1",
		"lvm pvs --reportformat json --select vg_name=bbbench_1_1 -o pv_name",
		"lvm lvremove --yes --autobackup n bbbench_1_1",
		"lvm vgremove --yes --autobackup n bbbench_1_1",
		"lvm vgs --reportformat json -o vg_name bbbench_1_1",
		"lvm pvs --reportformat json -o pv_name,vg_name /dev/sdb", "lvm pvremove --yes /dev/sdb", "lvm pvs --reportformat json -o pv_name,vg_name /dev/sdb",
		"lvm pvs --reportformat json -o pv_name,vg_name /dev/sdc", "lvm pvremove --yes /dev/sdc", "lvm pvs --reportformat json -o pv_name,vg_name /dev/sdc",
	}
	if !slices.Equal(s.calls, want) {
		t.Errorf("\n got %q\nwant %q", s.calls, want)
	}
	if len(s.pvs) != 0 || len(s.vgs) != 0 || len(s.lvs) != 0 {
		t.Errorf("left pvs %v vgs %v lvs %v", s.pvs, s.vgs, s.lvs)
	}
}

// destructive are the calls that change something.
func destructive(calls []string) []string {
	var out []string
	for _, c := range calls {
		for _, w := range []string{"lvremove", "vgremove", "pvremove", "--deldev", "pvcreate", "vgcreate", "lvcreate"} {
			if strings.Contains(c, w) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func TestLVM_UndoRefusesForeignPV(t *testing.T) {
	s := newLVSim(t)
	rec, _ := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	s.pvs["/dev/sdx"] = "bbbench_1_1"
	s.calls = nil
	err := NewLVM(s.env()).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "not touched") || !strings.Contains(err.Error(), "sdx") {
		t.Errorf("%v", err)
	}
	if d := destructive(s.calls); len(d) != 0 {
		t.Errorf("ran %q", d)
	}
}

func TestLVM_UndoRefusesPVOfAbsentMember(t *testing.T) {
	s := newLVSim(t)
	rec, _ := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	// sdc is not the recorded member any more (it was not found by serial),
	// so the PV the VG lists as /dev/sdc cannot be proven to be bbbench's.
	rec.Members[1].Path = ""
	s.calls = nil
	err := NewLVM(s.env()).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "not touched") || !strings.Contains(err.Error(), "sdc") {
		t.Errorf("%v", err)
	}
	if d := destructive(s.calls); len(d) != 0 {
		t.Errorf("ran %q", d)
	}
}

func TestLVM_UndoRefusesVGWithoutPVs(t *testing.T) {
	s := newLVSim(t)
	s.vgs["bbbench_1_1"] = true
	rec := ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: dataDisks(2)}
	err := NewLVM(s.env()).Undo(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "not touched") {
		t.Errorf("%v", err)
	}
	if d := destructive(s.calls); len(d) != 0 {
		t.Errorf("ran %q", d)
	}
}

func TestLVM_UndoRefusesForeignName(t *testing.T) {
	for _, name := range []string{"", "vg0", "bbbench_", "bbbench_1 1"} {
		s := newLVSim(t)
		s.vgs[name] = true
		rec := ArrayUndo{Backend: "lvm", Name: name, Members: dataDisks(2)}
		if err := NewLVM(s.env()).Undo(context.Background(), rec); err == nil || !strings.Contains(err.Error(), "nothing touched") {
			t.Errorf("%q: %v", name, err)
		}
		if len(s.calls) != 0 {
			t.Errorf("%q: ran %q", name, s.calls)
		}
	}
}

func TestLVM_UndoAfterPartialBuild(t *testing.T) {
	s := newLVSim(t)
	s.pvs["/dev/sdb"] = ""
	rec := ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: dataDisks(2)}
	if err := NewLVM(s.env()).Undo(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if d := destructive(s.calls); !slices.Equal(d, []string{"lvm pvremove --yes /dev/sdb"}) {
		t.Errorf("ran %q", d)
	}
}

func TestLVM_UndoLeavesOtherVGsPV(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)
	s := newLVSim(t)
	s.pvs["/dev/sdb"] = ""
	s.pvs["/dev/sdc"] = "vg0"
	s.vgs["vg0"] = true
	rec := ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: dataDisks(2)}
	if err := NewLVM(s.env()).Undo(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if d := destructive(s.calls); !slices.Equal(d, []string{"lvm pvremove --yes /dev/sdb"}) {
		t.Errorf("ran %q", d)
	}
	if s.pvs["/dev/sdc"] != "vg0" || !s.vgs["vg0"] {
		t.Error("vg0 touched")
	}
}

func TestLVM_UndoLvremoveFails(t *testing.T) {
	s := newLVSim(t)
	rec, _ := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	s.fail["lvm lvremove"] = exitErr("  Logical volume bbbench_1_1/bbbench_1_1 contains a filesystem in use.\n")
	s.calls = nil
	err := NewLVM(s.env()).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "lvremove") || !strings.Contains(err.Error(), "in use") {
		t.Errorf("%v", err)
	}
	if d := destructive(s.calls); !slices.Equal(d, []string{"lvm lvremove --yes --autobackup n bbbench_1_1"}) {
		t.Errorf("ran %q", d)
	}
}

func TestLVM_UndoVgremoveFails(t *testing.T) {
	s := newLVSim(t)
	rec, _ := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	s.fail["lvm vgremove"] = exitErr("  boom\n")
	s.calls = nil
	err := NewLVM(s.env()).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "vgremove") {
		t.Errorf("%v", err)
	}
	if d := destructive(s.calls); len(d) != 2 || strings.Contains(strings.Join(d, ";"), "pvremove") {
		t.Errorf("ran %q", d)
	}
}

func TestLVM_UndoVerifiesVGGone(t *testing.T) {
	s := newLVSim(t)
	rec, _ := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	s.fail["lvm vgremove"] = errNoop
	env := s.env()
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := s.run(ctx, name, args...)
		if errors.Is(err, errNoop) {
			return nil, nil
		}
		return out, err
	}
	err := NewLVM(env).Undo(context.Background(), *rec)
	if err == nil || err.Error() != "volume group bbbench_1_1 still exists after vgremove" {
		t.Errorf("%v", err)
	}
	if slices.ContainsFunc(s.calls, func(c string) bool { return strings.Contains(c, "pvremove") }) {
		t.Errorf("ran pvremove: %q", s.calls)
	}
}

func TestLVM_UndoVerifiesPVRemove(t *testing.T) {
	s := newLVSim(t)
	rec, _ := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	s.fail["lvm pvremove --yes /dev/sdc"] = errNoop
	env := s.env()
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := s.run(ctx, name, args...)
		if errors.Is(err, errNoop) {
			return nil, nil
		}
		return out, err
	}
	err := NewLVM(env).Undo(context.Background(), *rec)
	if err == nil || err.Error() != "/dev/sdc still carries an LVM label after pvremove" {
		t.Errorf("%v", err)
	}
	if _, ok := s.pvs["/dev/sdb"]; ok {
		t.Error("sdb not removed")
	}
}

func TestLVM_UndoDevicesFile(t *testing.T) {
	s := newLVSim(t)
	s.devices = []string{"/dev/sdb"}
	rec, _ := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	if rec.Extra["devicesfile_added"] != "S2" || !slices.Equal(s.devices, []string{"/dev/sdb", "/dev/sdc"}) {
		t.Fatalf("extra %v devices %v", rec.Extra, s.devices)
	}
	s.calls = nil
	if err := NewLVM(s.env()).Undo(context.Background(), *rec); err != nil {
		t.Fatal(err)
	}
	if d := destructive(s.calls); !slices.Contains(d, "lvm lvmdevices --deldev /dev/sdc") || slices.Contains(d, "lvm lvmdevices --deldev /dev/sdb") {
		t.Errorf("ran %q", d)
	}
	if !slices.Equal(s.devices, []string{"/dev/sdb"}) {
		t.Errorf("devices %v", s.devices)
	}

	// After a reboot the member is sdd; its entry is found by serial.
	s = newLVSim(t)
	s.devices = []string{"/dev/sdb", "/dev/sdd"}
	moved := dataDisks(2)
	moved[1].Name, moved[1].Path = "sdd", "/dev/sdd"
	rec2 := ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: moved,
		Extra: map[string]string{"devicesfile_added": "S2,S9"}}
	if err := NewLVM(s.env()).Undo(context.Background(), rec2); err != nil {
		t.Fatal(err)
	}
	if d := destructive(s.calls); !slices.Equal(d, []string{"lvm lvmdevices --deldev /dev/sdd"}) {
		t.Errorf("ran %q", d)
	}
}

func TestLVM_UndoMissingMember(t *testing.T) {
	s := newLVSim(t)
	s.pvs["/dev/sdb"] = ""
	s.pvs["/dev/sdc"] = ""
	ds := dataDisks(2)
	ds[1].Path = ""
	rec := ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: ds}
	err := NewLVM(s.env()).Undo(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "member S/N S2 is not present; its LVM label was not removed") {
		t.Errorf("%v", err)
	}
	if d := destructive(s.calls); !slices.Equal(d, []string{"lvm pvremove --yes /dev/sdb"}) {
		t.Errorf("ran %q", d)
	}
}

func TestLVM_UndoCommand(t *testing.T) {
	u := ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: dataDisks(2)}
	if got := NewLVM(Env{}).UndoCommand(u); got != "lvremove -y bbbench_1_1; vgremove -y bbbench_1_1; pvremove -y /dev/sdb /dev/sdc" {
		t.Errorf("%q", got)
	}
}

func TestLayout_MdadmAndLVMValues(t *testing.T) {
	env := Env{LookPath: lookAll}
	s := NewLayout(env, NewMdadm(env), NewLVM(env))
	ds := dataDisks(3)
	md, err := s.LayoutValue(mdLayout("raid5", "chunk", "512K", "bitmap", "no"), ds)
	if err != nil {
		t.Fatal(err)
	}
	lv, err := s.LayoutValue(lvLayout("raid5", "stripesize", "64K"), ds)
	if err != nil {
		t.Fatal(err)
	}
	if md.ID == lv.ID {
		t.Errorf("same value %q", md.ID)
	}
	for id, backend := range map[string]string{md.ID: "mdadm", lv.ID: "lvm"} {
		l, err := s.ParseLayout(id)
		if err != nil || l.Backend != backend || l.Level != "raid5" {
			t.Errorf("ParseLayout(%q) = %+v, %v", id, l, err)
		}
	}
}

func TestLVM_ApplyInactiveBeforeLink(t *testing.T) {
	// An inactive LV has no /dev link: the error says it is not active,
	// not that the link did not appear.
	s := newLVSim(t)
	s.noLink = true
	env := s.env()
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := s.run(ctx, name, args...)
		if len(args) > 0 && args[0] == "lvcreate" {
			s.lvs["LVUUID-1"].Attr = "rwi---r---"
		}
		return out, err
	}
	b := NewLVM(env)
	ds := dataDisks(2)
	l := lvLayout("raid1")
	v := variant("1_1", b.Label(l, ds), ds)
	rec, err := b.Prepare(context.Background(), v, l, "bbbench_1_1")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := b.Apply(context.Background(), v, l, rec)
	if ts != nil || err == nil || err.Error() != "logical volume bbbench_1_1/bbbench_1_1 is not active (lv_attr rwi---r---)" {
		t.Errorf("%v %v", ts, err)
	}
	// An active LV whose link never appears still fails.
	s2 := newLVSim(t)
	s2.noLink = true
	b2 := NewLVM(s2.env())
	rec2, _ := b2.Prepare(context.Background(), v, l, "bbbench_1_1")
	if _, err := b2.Apply(context.Background(), v, l, rec2); err == nil || !strings.Contains(err.Error(), "did not appear") {
		t.Errorf("no link: %v", err)
	}
}

func TestLVM_UndoKeepsDevicesEntryOfOtherVGsPV(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)
	s := newLVSim(t)
	// sdc was recorded as added to the devices file, but is now a PV of
	// vg0, a volume group bbbench did not create.
	s.devices = []string{"/dev/sdb", "/dev/sdc"}
	s.pvs["/dev/sdb"] = ""
	s.pvs["/dev/sdc"] = "vg0"
	s.vgs["vg0"] = true
	rec := ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: dataDisks(2),
		Extra: map[string]string{"devicesfile_added": "S1,S2"}}
	if err := NewLVM(s.env()).Undo(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if d := destructive(s.calls); !slices.Equal(d, []string{"lvm pvremove --yes /dev/sdb", "lvm lvmdevices --deldev /dev/sdb"}) {
		t.Errorf("ran %q", d)
	}
	if !slices.Equal(s.devices, []string{"/dev/sdc"}) {
		t.Errorf("devices %v", s.devices)
	}
}

func TestLVM_UndoErrorsWhenMemberStillInOwnVG(t *testing.T) {
	// vgs does not list the VG, but a member is still a PV of it: the
	// record must be kept.
	s := newLVSim(t)
	s.devices = []string{"/dev/sdc"}
	s.pvs["/dev/sdb"] = ""
	s.pvs["/dev/sdc"] = "bbbench_1_1"
	rec := ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: dataDisks(2),
		Extra: map[string]string{"devicesfile_added": "S2"}}
	err := NewLVM(s.env()).Undo(context.Background(), rec)
	if err == nil || err.Error() != "/dev/sdc is still a physical volume of volume group bbbench_1_1" {
		t.Errorf("%v", err)
	}
	if d := destructive(s.calls); !slices.Equal(d, []string{"lvm pvremove --yes /dev/sdb"}) {
		t.Errorf("ran %q", d)
	}
}

func TestLVM_UndoAmbiguousVgsIsAnError(t *testing.T) {
	s := newLVSim(t)
	rec, _ := lvBuild(t, s, lvLayout("raid1"), dataDisks(2))
	s.fail["lvm vgs"] = exitErr("  Global lock failed: check that global lockspace is started\n")
	s.calls = nil
	err := NewLVM(s.env()).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "Global lock failed") || !strings.Contains(err.Error(), "not touched") {
		t.Errorf("%v", err)
	}
	if d := destructive(s.calls); len(d) != 0 {
		t.Errorf("ran %q", d)
	}
	// The same for a pvs of a member that fails for another reason.
	s2 := newLVSim(t)
	s2.pvs["/dev/sdb"] = ""
	s2.fail["lvm pvs --reportformat json -o pv_name,vg_name /dev/sdb"] = exitErr("  Cannot open /dev/sdb: Device or resource busy\n")
	err = NewLVM(s2.env()).Undo(context.Background(), ArrayUndo{Backend: "lvm", Name: "bbbench_1_1", Members: dataDisks(1)})
	if err == nil || !strings.Contains(err.Error(), "resource busy") {
		t.Errorf("pvs: %v", err)
	}
	if d := destructive(s2.calls); len(d) != 0 {
		t.Errorf("pvs: ran %q", d)
	}
}
