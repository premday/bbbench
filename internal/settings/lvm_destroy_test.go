package settings

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"bbbench/internal/inuse"
)

const (
	vgU1  = "Vg0001-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	vgU2  = "Vg0002-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	lvU1  = "Lv0001-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	lvU2  = "Lv0002-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	lvU3  = "Lv0003-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	lvU4  = "Lv0004-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	pvU1  = "Pv0001-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	pvU2  = "Pv0002-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	pvU3  = "Pv0003-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	pvU4  = "Pv0004-aaaa-bbbb-cccc-dddd-eeee-ffffff"
	lvTiB = "1000000000000"
)

type lvdVG struct{ name, uuid, attr, size, lock, missing string }
type lvdLV struct{ vgUUID, name, uuid, attr string }
type lvdPV struct{ path, uuid, size, vg, vgUUID string }

// lvdSim is a fake lvm2 for the destroy provider: VGs, LVs and PVs it
// answers the JSON reports from, dm nodes and disks in a fake sysfs, an
// LVM devices file; it records every command and can fail or run a hook.
type lvdSim struct {
	t        *testing.T
	sys      string
	calls    []string
	vgs      []*lvdVG
	lvs      []*lvdLV
	pvs      []*lvdPV
	dms      map[string]string // dm node -> vg uuid (removed with the VG's LVs)
	devices  []string          // nil: no devices file
	pvremDev bool              // pvremove also drops the devices-file entry
	fail     map[string]error  // command prefix -> error returned instead
	after    map[string]func() // command prefix -> run after it succeeded
}

func newLvdSim(t *testing.T) *lvdSim {
	return &lvdSim{t: t, sys: t.TempDir(), dms: map[string]string{}, fail: map[string]error{}, after: map[string]func(){}}
}

func (s *lvdSim) env() Env {
	return Env{Run: s.run, LookPath: lookAll, Sys: s.sys, Dev: s.t.TempDir(),
		Poll: time.Millisecond, Settle: 200 * time.Millisecond, InUse: freeDisks}
}

func (s *lvdSim) lvm() *lvm { return &lvm{env: s.env().fill()} }

func (s *lvdSim) write(rel, content string) {
	s.t.Helper()
	p := filepath.Join(s.sys, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// disk makes a disk with serial S-<name>, model M and its partitions.
func (s *lvdSim) disk(name string, parts ...string) {
	s.write(filepath.Join("block", name, "size"), "2000")
	s.write(filepath.Join("block", name, "device", "serial"), "S-"+name)
	s.write(filepath.Join("block", name, "device", "model"), "M")
	for _, p := range parts {
		s.write(filepath.Join("block", name, p, "partition"), "1")
	}
}

// dm makes /sys/block/<node> a dm device with the LVM uuid of the LV lv of
// the VG vg, with suffix ("-tpool", "-real", ...).
func (s *lvdSim) dm(node, vg, lv, suffix string) {
	s.write(filepath.Join("block", node, "dm", "uuid"), "LVM-"+strings.ReplaceAll(vg, "-", "")+strings.ReplaceAll(lv, "-", "")+suffix)
	s.write(filepath.Join("block", node, "size"), "100")
	if err := os.MkdirAll(filepath.Join(s.sys, "block", node, "holders"), 0o755); err != nil {
		s.t.Fatal(err)
	}
	s.dms[node] = vg
}

func (s *lvdSim) vgByName(n string) *lvdVG {
	for _, v := range s.vgs {
		if v.name == n {
			return v
		}
	}
	return nil
}

func (s *lvdSim) pvByPath(p string) *lvdPV {
	for _, v := range s.pvs {
		if v.path == p {
			return v
		}
	}
	return nil
}

func (s *lvdSim) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, key)
	for p, err := range s.fail {
		if strings.HasPrefix(key, p) {
			return nil, err
		}
	}
	out, err := s.answer(name, args)
	if err == nil {
		for p, f := range s.after {
			if strings.HasPrefix(key, p) {
				f()
			}
		}
	}
	return out, err
}

func (s *lvdSim) answer(name string, args []string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	if name != "lvm" || len(args) == 0 {
		return nil, errors.New("unexpected " + key)
	}
	last := args[len(args)-1]
	listing := flag(args, "-o") == last
	switch args[0] {
	case "vgs":
		var rows []map[string]string
		for _, v := range s.vgs {
			if !listing && v.name != last {
				continue
			}
			rows = append(rows, map[string]string{"vg_name": v.name, "vg_uuid": v.uuid, "vg_attr": v.attr,
				"vg_size": v.size, "vg_lock_type": v.lock, "vg_missing_pv_count": v.missing})
		}
		if !listing && len(rows) == 0 {
			return nil, exitErr("  Volume group \"" + last + "\" not found\n")
		}
		return report("vg", rows...), nil
	case "lvs":
		sel := strings.TrimPrefix(flag(args, "--select"), "vg_uuid=")
		var rows []map[string]string
		for _, l := range s.lvs {
			if sel != "" && l.vgUUID != sel {
				continue
			}
			rows = append(rows, map[string]string{"vg_uuid": l.vgUUID, "lv_name": l.name, "lv_uuid": l.uuid, "lv_attr": l.attr})
		}
		return report("lv", rows...), nil
	case "pvs":
		var rows []map[string]string
		for _, p := range s.pvs {
			if !listing && p.path != last {
				continue
			}
			rows = append(rows, map[string]string{"pv_name": p.path, "pv_uuid": p.uuid, "pv_size": p.size, "vg_name": p.vg, "vg_uuid": p.vgUUID})
		}
		if !listing && len(rows) == 0 {
			return nil, exitErr("  Failed to find physical volume \"" + last + "\".\n")
		}
		return report("pv", rows...), nil
	case "lvremove":
		v := s.vgByName(last)
		if v == nil {
			return nil, exitErr("  Volume group \"" + last + "\" not found\n")
		}
		s.lvs = slices.DeleteFunc(s.lvs, func(l *lvdLV) bool { return l.vgUUID == v.uuid })
		for n, u := range s.dms {
			if u == v.uuid {
				delete(s.dms, n)
				os.RemoveAll(filepath.Join(s.sys, "block", n))
			}
		}
		return nil, nil
	case "vgremove":
		v := s.vgByName(last)
		if v == nil {
			return nil, exitErr("  Volume group \"" + last + "\" not found\n")
		}
		s.vgs = slices.DeleteFunc(s.vgs, func(x *lvdVG) bool { return x == v })
		for _, p := range s.pvs {
			if p.vgUUID == v.uuid {
				p.vg, p.vgUUID = "", ""
			}
		}
		return nil, nil
	case "pvremove":
		s.pvs = slices.DeleteFunc(s.pvs, func(p *lvdPV) bool { return p.path == last })
		if s.pvremDev {
			s.devices = slices.DeleteFunc(s.devices, func(p string) bool { return p == last })
		}
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

// dataVG is VG data (uuid vgU1) with LVs home and root, both active (dm-0,
// dm-1), on the PVs /dev/sdb and /dev/sdc1, and an orphan PV /dev/sdd1.
func dataVG(t *testing.T) *lvdSim {
	s := newLvdSim(t)
	s.disk("sdb")
	s.disk("sdc", "sdc1")
	s.disk("sdd", "sdd1")
	s.vgs = []*lvdVG{{name: "data", uuid: vgU1, attr: "wz--n-", size: "2000000000000"}}
	s.lvs = []*lvdLV{{vgU1, "home", lvU1, "-wi-ao----"}, {vgU1, "root", lvU2, "-wi-a-----"}}
	s.pvs = []*lvdPV{{"/dev/sdb", pvU1, lvTiB, "data", vgU1}, {"/dev/sdc1", pvU2, lvTiB, "data", vgU1},
		{"/dev/sdd1", pvU3, lvTiB, "", ""}}
	s.dm("dm-0", vgU1, lvU1, "")
	s.dm("dm-1", vgU1, lvU2, "")
	return s
}

func listLVM(t *testing.T, s *lvdSim) []Existing {
	t.Helper()
	es, err := s.lvm().Existing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.calls = nil
	return es
}

func TestLVMIsADestroyer(t *testing.T) {
	var b Backend = NewLVM(Env{})
	if _, ok := b.(Destroyer); !ok {
		t.Fatal("the lvm backend is not a Destroyer")
	}
}

func TestLVMExistingVGAndOrphan(t *testing.T) {
	s := dataVG(t)
	s.dm("dm-9", vgU2, lvU3, "") // another VG's node
	es := listLVM(t, s)
	if len(es) != 2 {
		t.Fatalf("entries %+v", es)
	}
	vg := es[0]
	want := Existing{Backend: "lvm", ID: "data", Name: "data",
		Summary: "vg data: 2 LVs (2 active), 2 PVs, 2.0 TB",
		Disks:   []Target{tgt("sdb"), tgt("sdc")},
		Stack:   []string{"lv data/home", "lv data/root", "vg data", "pv /dev/sdb", "pv /dev/sdc1"},
		Extra: map[string]string{ExtraOwnedVG: "data", ExtraOwnedNodes: "dm-0,dm-1", lvmExtraVGUUID: vgU1,
			lvmExtraPVs: "/dev/sdb,/dev/sdc1", lvmExtraLVs: "home,root"}}
	if fmt.Sprint(vg) != fmt.Sprint(want) {
		t.Errorf("vg\n got %+v\nwant %+v", vg, want)
	}
	orphan := es[1]
	wantO := Existing{Backend: "lvm", ID: "pv:" + pvU3, Name: "sdd1",
		Summary: "orphan physical volume /dev/sdd1, 1.0 TB",
		Disks:   []Target{tgt("sdd")},
		Stack:   []string{"pv /dev/sdd1"},
		Extra:   map[string]string{ExtraOrphanPV: "yes", ExtraOwnedNodes: "sdd1", lvmExtraPVUUID: pvU3, lvmExtraPVPath: "/dev/sdd1"}}
	if fmt.Sprint(orphan) != fmt.Sprint(wantO) {
		t.Errorf("orphan\n got %+v\nwant %+v", orphan, wantO)
	}
}

func TestLVMExistingInactiveVG(t *testing.T) {
	s := dataVG(t)
	for _, l := range s.lvs {
		l.attr = "-wi-------"
	}
	os.RemoveAll(filepath.Join(s.sys, "block", "dm-0"))
	os.RemoveAll(filepath.Join(s.sys, "block", "dm-1"))
	vg := findExisting(t, listLVM(t, s), "data")
	if vg.Summary != "vg data: 2 LVs (0 active), 2 PVs, 2.0 TB" || vg.Extra[ExtraOwnedNodes] != "" || vg.Blocked != "" {
		t.Errorf("%+v", vg)
	}
}

// A thin pool's hidden sub-LVs and its -tpool node, and a snapshot's -real
// and -cow nodes, are all the VG's dm nodes.
func TestLVMExistingOwnsEveryDMNodeOfTheVG(t *testing.T) {
	s := dataVG(t)
	s.lvs = []*lvdLV{{vgU1, "pool", lvU1, "twi-aotz--"}, {vgU1, "[pool_tdata]", lvU2, "Twi-ao----"},
		{vgU1, "[pool_tmeta]", lvU3, "ewi-ao----"}, {vgU1, "thin1", lvU4, "Vwi-aotz--"}, {vgU1, "[lvol0_pmspare]", pvU4, "ewi-------"}}
	s.dm("dm-1", vgU1, lvU2, "")
	s.dm("dm-2", vgU1, lvU3, "")
	s.dm("dm-3", vgU1, lvU1, "-tpool")
	s.dm("dm-4", vgU1, lvU1, "-pool")
	s.dm("dm-5", vgU1, lvU4, "")
	s.dm("dm-6", vgU1, lvU4, "-real")
	s.dm("dm-7", vgU1, lvU4, "-cow")
	s.dm("dm-8", vgU2, lvU1, "")
	s.write("block/dm-10/dm/uuid", "CRYPT-LUKS2-abc")
	vg := findExisting(t, listLVM(t, s), "data")
	if got := vg.Extra[ExtraOwnedNodes]; got != "dm-0,dm-1,dm-2,dm-3,dm-4,dm-5,dm-6,dm-7" {
		t.Errorf("owned %s", got)
	}
	if !slices.Equal(vg.Stack[:2], []string{"lv data/pool", "lv data/thin1"}) || vg.Summary != "vg data: 2 LVs (2 active), 2 PVs, 2.0 TB" {
		t.Errorf("%+v", vg)
	}
}

func TestLVMExistingBlocked(t *testing.T) {
	cases := []struct {
		name string
		set  func(*lvdSim)
		want string
	}{
		{"partial", func(s *lvdSim) { s.vgs[0].attr = "wz-pn-" }, "missing physical volumes"},
		{"missing count", func(s *lvdSim) { s.vgs[0].missing = "1" }, "missing physical volumes"},
		{"unknown pv", func(s *lvdSim) { s.pvs[1].path = "[unknown]" }, "missing physical volumes"},
		{"clustered", func(s *lvdSim) { s.vgs[0].attr = "wz--nc" }, "shared/clustered"},
		{"shared lock", func(s *lvdSim) { s.vgs[0].lock = "sanlock" }, "shared/clustered"},
		{"exported", func(s *lvdSim) { s.vgs[0].attr = "wzx-n-" }, "exported"},
		{"duplicate name", func(s *lvdSim) {
			s.vgs = append(s.vgs, &lvdVG{name: "data", uuid: vgU2, attr: "wz--n-", size: "1"})
		}, "two volume groups"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := dataVG(t)
			tc.set(s)
			vg := findExisting(t, listLVM(t, s), "data")
			if !strings.Contains(vg.Blocked, tc.want) {
				t.Fatalf("blocked %q, want %q", vg.Blocked, tc.want)
			}
			l := s.lvm()
			if c := l.DestroyCommand(vg); c != "" {
				t.Errorf("command %q", c)
			}
			if err := l.Destroy(context.Background(), vg); err == nil || !strings.Contains(err.Error(), "nothing done") {
				t.Errorf("destroy %v", err)
			}
			if len(s.calls) != 0 {
				t.Errorf("ran %q", s.calls)
			}
		})
	}
	// A PV of a VG that vgs does not list (foreign system ID) is a blocked
	// entry, not an orphan.
	s := dataVG(t)
	s.pvs[2].vg, s.pvs[2].vgUUID = "far", vgU2
	far := findExisting(t, listLVM(t, s), "far")
	if !strings.Contains(far.Blocked, "not listed by vgs") || far.Disks[0].Name != "sdd" {
		t.Errorf("%+v", far)
	}
}

// A VG on an md device: the disk is named but has no serial, so the gate
// refuses it.
func TestLVMExistingOnMDIsRefusedByTheGate(t *testing.T) {
	s := dataVG(t)
	s.write("block/md0/size", "100")
	s.write("block/md0/md/level", "raid1")
	s.pvs[1].path = "/dev/md0"
	vg := findExisting(t, listLVM(t, s), "data")
	if vg.Disks[0] != (Target{Name: "md0"}) {
		t.Fatalf("disks %+v", vg.Disks)
	}
	l := NewLayout(s.env(), s.lvm())
	bs, err := l.DestroyBlockers(context.Background(), vg)
	if err != nil || len(bs) != 1 || !strings.Contains(bs[0], "no serial") {
		t.Errorf("%v %v", bs, err)
	}
}

func lvmMutations(calls []string) []string {
	var out []string
	for _, c := range calls {
		for _, m := range []string{"lvremove", "vgremove", "pvremove", "--deldev"} {
			if strings.Contains(c, m) {
				out = append(out, c)
			}
		}
	}
	return out
}

func noForce(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		for _, a := range strings.Fields(c) {
			if a == "--force" || a == "-f" || a == "-ff" || a == "-y" || (strings.HasPrefix(a, "-f") && !strings.HasPrefix(a, "--")) {
				t.Errorf("forced: %q", c)
			}
		}
	}
}

func TestLVMDestroyVGArgvOrder(t *testing.T) {
	s := dataVG(t)
	s.devices = []string{"/dev/sdb", "/dev/sdc1", "/dev/sdx"}
	vg := findExisting(t, listLVM(t, s), "data")
	if err := s.lvm().Destroy(context.Background(), vg); err != nil {
		t.Fatal(err)
	}
	list := []string{
		"lvm vgs --reportformat json --units b --nosuffix -o vg_name,vg_uuid,vg_attr,vg_size,vg_lock_type,vg_missing_pv_count",
		"lvm lvs -a --reportformat json -o vg_uuid,lv_name,lv_attr",
		"lvm pvs --reportformat json --units b --nosuffix -o pv_name,pv_uuid,pv_size,vg_name,vg_uuid",
	}
	want := append(slices.Clone(list),
		"lvm lvremove --yes --autobackup n data",
		"lvm lvs -a --reportformat json -o lv_name --select vg_uuid="+vgU1,
		list[2],
		"lvm vgremove --yes --autobackup n data",
		"lvm vgs --reportformat json -o vg_name data")
	for _, p := range []string{"/dev/sdb", "/dev/sdc1"} {
		want = append(want, "lvm pvs --reportformat json -o pv_name,pv_uuid,vg_name "+p, "lvm pvremove --yes "+p,
			"lvm pvs --reportformat json -o pv_name,pv_uuid,vg_name "+p)
	}
	want = append(want, "lvm lvmdevices", "lvm lvmdevices --deldev /dev/sdb", "lvm lvmdevices --deldev /dev/sdc1", "lvm lvmdevices")
	if !slices.Equal(s.calls, want) {
		t.Errorf("calls:\n got %q\nwant %q", s.calls, want)
	}
	noForce(t, s.calls)
	if len(s.vgs) != 0 || len(s.lvs) != 0 || len(s.pvs) != 1 || s.pvs[0].path != "/dev/sdd1" {
		t.Errorf("left %+v %+v %+v", s.vgs, s.lvs, s.pvs)
	}
	if !slices.Equal(s.devices, []string{"/dev/sdx"}) {
		t.Errorf("devices file %q", s.devices)
	}
}

// pvremove that already dropped the entry leaves nothing to delete; no
// devices file means no lvmdevices --deldev.
func TestLVMDestroyDevicesFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		devices  []string
		pvremDev bool
	}{{"pvremove drops it", []string{"/dev/sdb", "/dev/sdc1"}, true}, {"no devices file", nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := dataVG(t)
			s.devices, s.pvremDev = tc.devices, tc.pvremDev
			vg := findExisting(t, listLVM(t, s), "data")
			if err := s.lvm().Destroy(context.Background(), vg); err != nil {
				t.Fatal(err)
			}
			for _, c := range s.calls {
				if strings.Contains(c, "--deldev") {
					t.Errorf("ran %q", c)
				}
			}
		})
	}
}

func TestLVMDestroyVGWithoutLVs(t *testing.T) {
	s := dataVG(t)
	s.lvs = nil
	os.RemoveAll(filepath.Join(s.sys, "block", "dm-0"))
	os.RemoveAll(filepath.Join(s.sys, "block", "dm-1"))
	vg := findExisting(t, listLVM(t, s), "data")
	if c := s.lvm().DestroyCommand(vg); c != "vgremove --yes data && pvremove --yes /dev/sdb /dev/sdc1" {
		t.Errorf("command %q", c)
	}
	if err := s.lvm().Destroy(context.Background(), vg); err != nil {
		t.Fatal(err)
	}
	if m := lvmMutations(s.calls); len(m) == 0 || strings.Contains(m[0], "lvremove") {
		t.Errorf("mutations %q", m)
	}
}

func TestLVMDestroyCommand(t *testing.T) {
	s := dataVG(t)
	es := listLVM(t, s)
	l := s.lvm()
	if c := l.DestroyCommand(es[0]); c != "lvremove --yes data && vgremove --yes data && pvremove --yes /dev/sdb /dev/sdc1" {
		t.Errorf("vg %q", c)
	}
	if c := l.DestroyCommand(es[1]); c != "pvremove --yes /dev/sdd1" {
		t.Errorf("orphan %q", c)
	}
	odd := es[0]
	odd.Extra = map[string]string{ExtraOwnedVG: "my vg", lvmExtraPVs: "/dev/sdb", lvmExtraLVs: "a"}
	if c := l.DestroyCommand(odd); c != "lvremove --yes 'my vg' && vgremove --yes 'my vg' && pvremove --yes /dev/sdb" {
		t.Errorf("quoted %q", c)
	}
}

func TestLVMDestroyVGRemoveFailureLeavesThePVs(t *testing.T) {
	s := dataVG(t)
	s.fail["lvm vgremove"] = exitErr("  Volume group \"data\" lock failed\n")
	vg := findExisting(t, listLVM(t, s), "data")
	err := s.lvm().Destroy(context.Background(), vg)
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.HasPrefix(err.Error(), "data: logical volumes removed; volume group data not removed; no physical volume label removed; left: physical volumes /dev/sdb, /dev/sdc1; ") ||
		!strings.Contains(err.Error(), "lock failed") ||
		!strings.HasSuffix(err.Error(), "; rest by hand: vgremove --yes data && pvremove --yes /dev/sdb /dev/sdc1") {
		t.Errorf("error %q", err)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "pvremove") {
			t.Errorf("ran %q", c)
		}
	}
	if len(s.pvs) != 3 {
		t.Errorf("pvs %+v", s.pvs)
	}
}

// An LV open: lvremove refuses, nothing else runs.
func TestLVMDestroyLVRemoveFailureStops(t *testing.T) {
	s := dataVG(t)
	s.fail["lvm lvremove"] = exitErr("  Logical volume data/home in use.\n")
	vg := findExisting(t, listLVM(t, s), "data")
	err := s.lvm().Destroy(context.Background(), vg)
	if err == nil || !strings.HasPrefix(err.Error(), "data: logical volumes not all removed (left: home, root); volume group data not removed; no physical volume label removed; left: physical volumes /dev/sdb, /dev/sdc1; ") ||
		!strings.HasSuffix(err.Error(), "; rest by hand: lvremove --yes data && vgremove --yes data && pvremove --yes /dev/sdb /dev/sdc1") {
		t.Errorf("error %v", err)
	}
	if m := lvmMutations(s.calls); len(m) != 1 {
		t.Errorf("mutations %q", m)
	}
}

func TestLVMDestroyRefusesAChangedEntry(t *testing.T) {
	cases := []struct {
		name string
		set  func(*lvdSim)
		want string
	}{
		{"lv added", func(s *lvdSim) { s.lvs = append(s.lvs, &lvdLV{vgU1, "new", lvU3, "-wi-------"}) }, "changed"},
		{"vg uuid changed", func(s *lvdSim) {
			s.vgs[0].uuid = vgU2
			for _, p := range s.pvs[:2] {
				p.vgUUID = vgU2
			}
			for _, l := range s.lvs {
				l.vgUUID = vgU2
			}
		}, "uuid"},
		{"pv moved to another vg", func(s *lvdSim) {
			s.vgs = append(s.vgs, &lvdVG{name: "other", uuid: vgU2, attr: "wz--n-", size: "1"})
			s.pvs[1].vg, s.pvs[1].vgUUID = "other", vgU2
		}, "changed"},
		{"gone", func(s *lvdSim) {
			s.vgs, s.lvs = nil, nil
			for _, p := range s.pvs {
				p.vg, p.vgUUID = "", ""
			}
		}, "no longer"},
		{"pvs left without their vg", func(s *lvdSim) { s.vgs = nil }, "not listed by vgs"},
		{"now blocked", func(s *lvdSim) { s.vgs[0].attr = "wz-pn-" }, "missing physical volumes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := dataVG(t)
			vg := findExisting(t, listLVM(t, s), "data")
			tc.set(s)
			err := s.lvm().Destroy(context.Background(), vg)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing done") {
				t.Errorf("error %v, want %q", err, tc.want)
			}
			if m := lvmMutations(s.calls); len(m) != 0 {
				t.Errorf("ran %q", m)
			}
		})
	}
}

// A PV that joins another VG between vgremove and its pvremove is left
// alone and out of the manual command.
func TestLVMDestroyPVTakenAfterVGRemove(t *testing.T) {
	s := dataVG(t)
	vg := findExisting(t, listLVM(t, s), "data")
	s.after["lvm vgremove"] = func() {
		s.vgs = append(s.vgs, &lvdVG{name: "other", uuid: vgU2, attr: "wz--n-", size: "1"})
		s.pvs[0].vg, s.pvs[0].vgUUID = "other", vgU2
	}
	err := s.lvm().Destroy(context.Background(), vg)
	want := "data: logical volumes removed; volume group data removed; no physical volume label removed; " +
		"left: physical volumes /dev/sdb, /dev/sdc1; /dev/sdb is now a physical volume of volume group other; do not remove it; " +
		"rest by hand, after checking with pvs that each is a physical volume of no volume group: pvremove --yes /dev/sdc1"
	if err == nil || err.Error() != want {
		t.Errorf("error\n got %v\nwant %s", err, want)
	}
	if s.pvByPath("/dev/sdb") == nil {
		t.Error("the taken PV was removed")
	}
	for _, c := range s.calls {
		if strings.Contains(c, "pvremove") {
			t.Errorf("ran %q", c)
		}
	}
}

func TestLVMDestroyPVRemoveFailure(t *testing.T) {
	s := dataVG(t)
	s.devices = []string{"/dev/sdb", "/dev/sdc1"}
	s.fail["lvm pvremove --yes /dev/sdc1"] = exitErr("  boom\n")
	vg := findExisting(t, listLVM(t, s), "data")
	err := s.lvm().Destroy(context.Background(), vg)
	if err == nil || !strings.HasPrefix(err.Error(), "data: logical volumes removed; volume group data removed; physical volume labels removed from /dev/sdb; left: physical volumes /dev/sdc1; ") ||
		!strings.HasSuffix(err.Error(), "rest by hand, after checking with pvs that each is a physical volume of no volume group: pvremove --yes /dev/sdc1") {
		t.Errorf("error %v", err)
	}
	// Only the entry of the PV this call removed is dropped.
	if !slices.Equal(s.devices, []string{"/dev/sdc1"}) {
		t.Errorf("devices %q", s.devices)
	}
}

func TestLVMDestroyCancelledStillVerifies(t *testing.T) {
	s := dataVG(t)
	vg := findExisting(t, listLVM(t, s), "data")
	ctx, cancel := context.WithCancel(context.Background())
	s.after["lvm lvremove"] = cancel
	err := s.lvm().Destroy(ctx, vg)
	if err == nil || !strings.Contains(err.Error(), "logical volumes removed") || !strings.Contains(err.Error(), "canceled") ||
		!strings.HasSuffix(err.Error(), "rest by hand: vgremove --yes data && pvremove --yes /dev/sdb /dev/sdc1") {
		t.Errorf("error %v", err)
	}
	if !slices.Contains(s.calls, "lvm lvs -a --reportformat json -o lv_name --select vg_uuid="+vgU1) {
		t.Errorf("lvremove not verified: %q", s.calls)
	}
}

func TestLVMDestroyOrphanPV(t *testing.T) {
	s := dataVG(t)
	s.devices = []string{"/dev/sdd1"}
	o := findExisting(t, listLVM(t, s), "sdd1")
	if err := s.lvm().Destroy(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	m := lvmMutations(s.calls)
	if !slices.Equal(m, []string{"lvm pvremove --yes /dev/sdd1", "lvm lvmdevices --deldev /dev/sdd1"}) {
		t.Errorf("mutations %q", m)
	}
	noForce(t, s.calls)
	if s.pvByPath("/dev/sdd1") != nil || len(s.devices) != 0 || len(s.pvs) != 2 {
		t.Errorf("left %+v %q", s.pvs, s.devices)
	}
}

func TestLVMDestroyOrphanRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*lvdSim)
		want string
	}{
		{"joined a vg", func(s *lvdSim) { s.pvs[2].vg, s.pvs[2].vgUUID = "data", vgU1 }, "no longer"},
		{"other uuid", func(s *lvdSim) { s.pvs[2].uuid = pvU4 }, "no longer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dataVG(t)
			o := findExisting(t, listLVM(t, s), "sdd1")
			tc.set(s)
			err := s.lvm().Destroy(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing done") {
				t.Errorf("error %v", err)
			}
			if m := lvmMutations(s.calls); len(m) != 0 {
				t.Errorf("ran %q", m)
			}
		})
	}
}

func TestLVMDestroyRefusesAForgedEntry(t *testing.T) {
	s := dataVG(t)
	vg := findExisting(t, listLVM(t, s), "data")
	l := s.lvm()
	bad := vg
	bad.Extra = map[string]string{ExtraOwnedVG: "-ff", lvmExtraVGUUID: vgU1}
	bad.Name, bad.ID = "-ff", "-ff"
	if err := l.Destroy(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "nothing done") {
		t.Errorf("error %v", err)
	}
	if l.DestroyCommand(bad) != "" {
		t.Error("command for a bad name")
	}
	if m := lvmMutations(s.calls); len(m) != 0 {
		t.Errorf("ran %q", m)
	}
}

func TestLVMLeftoverCommand(t *testing.T) {
	s := dataVG(t)
	vg := findExisting(t, listLVM(t, s), "data")
	l := s.lvm()
	fresh := vg
	fresh.Extra = map[string]string{ExtraOwnedVG: "data", lvmExtraVGUUID: vgU1, lvmExtraPVs: "/dev/sdc1", lvmExtraLVs: ""}
	if c := l.LeftoverCommand(vg, fresh); c != "vgremove --yes data && pvremove --yes /dev/sdc1" {
		t.Errorf("%q", c)
	}
	fresh.Extra[lvmExtraPVs] = "/dev/sdc1,/dev/sdz"
	if c := l.LeftoverCommand(vg, fresh); c != "" {
		t.Errorf("unconfirmed pv: %q", c)
	}
	fresh.Extra[lvmExtraPVs], fresh.Extra[lvmExtraLVs] = "/dev/sdc1", "home,new"
	if c := l.LeftoverCommand(vg, fresh); c != "" {
		t.Errorf("unconfirmed lv: %q", c)
	}
	fresh.Extra[lvmExtraLVs], fresh.Extra[lvmExtraVGUUID] = "home", vgU2
	if c := l.LeftoverCommand(vg, fresh); c != "" {
		t.Errorf("other uuid: %q", c)
	}
}

// Through the gate: a whole VG destroyed with its holders and PV causes.
func TestLVMDestroyThroughTheGate(t *testing.T) {
	s := dataVG(t)
	scans := 0
	env := s.env()
	env.InUse = func() (*inuse.Report, error) {
		scans++
		if scans > 1 {
			return &inuse.Report{}, nil
		}
		return reportOf(map[string][]inuse.Cause{
			"sdb": {{Kind: inuse.KindHolder, Disk: "sdb", Via: "dm-0", Device: "dm-0", Text: "used by dm-0"},
				{Kind: inuse.KindPV, Disk: "sdb", Via: "data", Device: "sdb", Text: "is an LVM physical volume of volume group data"}},
			"sdc": {{Kind: inuse.KindPV, Disk: "sdc", Via: "data", Device: "sdc1", Text: "is an LVM physical volume of volume group data"}},
		}), nil
	}
	l := NewLayout(env, &lvm{env: env.fill()})
	es, errs := l.Existing(context.Background())
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	vg := findExisting(t, es, "data")
	if err := l.Destroy(context.Background(), vg); err != nil {
		t.Fatal(err)
	}
	if len(s.vgs) != 0 {
		t.Errorf("vgs %+v", s.vgs)
	}
}

// A PV added to the VG after it was listed stops the teardown before
// vgremove, with no command (the VG now spans an unconfirmed disk).
func TestLVMDestroyVGGrewBeforeVGRemove(t *testing.T) {
	s := dataVG(t)
	vg := findExisting(t, listLVM(t, s), "data")
	s.after["lvm lvremove"] = func() { s.pvs[2].vg, s.pvs[2].vgUUID = "data", vgU1 }
	err := s.lvm().Destroy(context.Background(), vg)
	if err == nil || !strings.Contains(err.Error(), "vgremove not run") || !strings.HasSuffix(err.Error(), "; nothing proven is left to run by hand") {
		t.Errorf("error %v", err)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "vgremove") || strings.Contains(c, "pvremove") {
			t.Errorf("ran %q", c)
		}
	}
}
