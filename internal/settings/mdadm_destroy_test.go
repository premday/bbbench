package settings

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"bbbench/internal/inuse"
)

const (
	mdU1 = "a1b2c3d4:00000000:11111111:22222222"
	mdU2 = "e5f6a7b8:33333333:44444444:55555555"
)

// mdDestroySim is an mdSim that also answers mdadm --examine --scan
// --verbose from its superblocks (every one of them, members of assembled
// arrays included, as mdadm does) and can keep an array after --stop.
type mdDestroySim struct {
	*mdSim
	stopKeeps bool              // --stop succeeds but the array stays
	onStop    func()            // called on --stop
	levels    map[string]string // uuid -> level printed by the scan
}

func newMdDestroySim(t *testing.T) *mdDestroySim {
	return &mdDestroySim{mdSim: newMdSim(t), levels: map[string]string{}}
}

func (d *mdDestroySim) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "mdadm" && len(args) >= 2 && args[0] == "--examine" && args[1] == "--scan" {
		d.calls = append(d.calls, name+" "+strings.Join(args, " "))
		return []byte(d.scanOutput()), nil
	}
	if name == "mdadm" && len(args) > 0 && args[0] == "--stop" {
		if d.onStop != nil {
			d.onStop()
		}
		if d.stopKeeps {
			d.calls = append(d.calls, name+" "+strings.Join(args, " "))
			return nil, nil
		}
	}
	return d.mdSim.run(ctx, name, args...)
}

func (d *mdDestroySim) env() Env {
	e := d.mdSim.env()
	e.Run = d.run
	return e
}

func (d *mdDestroySim) mdadm() *mdadm { return &mdadm{env: d.env().fill()} }

// scanOutput prints the superblocks grouped by uuid in the shape of
// mdadm --examine --scan --verbose.
func (d *mdDestroySim) scanOutput() string {
	by := map[string][]string{}
	for p, u := range d.supers {
		by[u] = append(by[u], p)
	}
	var uuids []string
	for u := range by {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	var b strings.Builder
	for _, u := range uuids {
		devs := by[u]
		sort.Strings(devs)
		level := d.levels[u]
		if level == "" {
			level = "raid1"
		}
		b.WriteString("ARRAY /dev/md/host:x  level=" + level + " metadata=1.2 num-devices=" + strconv.Itoa(len(devs)) + " UUID=" + u + " name=host:x\n")
		b.WriteString("   devices=" + strings.Join(devs, ",") + "\n")
	}
	return b.String()
}

// disk gives disk a size, a serial and a model, and creates its partitions.
func (d *mdDestroySim) disk(name string, parts ...string) {
	d.write(filepath.Join("block", name, "size"), "2000")
	d.write(filepath.Join("block", name, "device", "serial"), "S-"+name)
	d.write(filepath.Join("block", name, "device", "model"), "M")
	for _, p := range parts {
		d.write(filepath.Join("block", name, p, "partition"), "1")
	}
}

// member makes node (a disk, or a partition of disk) a member of md with
// state, carrying the superblock of uuid.
func (d *mdDestroySim) member(md, disk, node, state, uuid string) {
	d.t.Helper()
	dir := filepath.Join(d.sys, "block", md, "md", "dev-"+node)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.t.Fatal(err)
	}
	dst := filepath.Join(d.sys, "block", disk)
	if node != disk {
		dst = filepath.Join(dst, node)
	}
	if err := os.Symlink(dst, filepath.Join(dir, "block")); err != nil {
		d.t.Fatal(err)
	}
	d.write(filepath.Join("block", md, "md", "dev-"+node, "state"), state)
	d.supers["/dev/"+node] = uuid
}

// array makes md an assembled array of uuid; detail says whether
// mdadm --detail reports it.
func (d *mdDestroySim) array(md, uuid, level, state string, detail bool) {
	dir := filepath.Join("block", md, "md")
	if level != "" {
		d.write(filepath.Join(dir, "level"), level)
	}
	d.write(filepath.Join(dir, "array_state"), state)
	d.write(filepath.Join(dir, "metadata_version"), "1.2")
	d.write(filepath.Join(dir, "degraded"), "0")
	d.write(filepath.Join("block", md, "size"), "4000000000") // 2.0 TB
	if err := os.MkdirAll(filepath.Join(d.sys, "block", md, "holders"), 0o755); err != nil {
		d.t.Fatal(err)
	}
	if detail {
		d.arrays[md] = uuid
	}
}

func tgt(name string) Target {
	return Target{Name: name, Path: "/dev/" + name, Serial: "S-" + name, Model: "M"}
}

func findExisting(t *testing.T, es []Existing, name string) Existing {
	t.Helper()
	for _, e := range es {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %+v", name, es)
	return Existing{}
}

func TestMdadmIsADestroyer(t *testing.T) {
	var b Backend = NewMdadm(Env{})
	if _, ok := b.(Destroyer); !ok {
		t.Fatal("the mdadm backend is not a Destroyer")
	}
}

func TestMdExistingAssembledArrays(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb", "sdb1")
	s.disk("sdc", "sdc1")
	for _, n := range []string{"sdd", "sde", "sdf", "sdx"} {
		s.disk(n)
	}
	// raid1 on two partitions, with a partition of its own.
	s.array("md126", mdU2, "raid1", "clean", true)
	s.member("md126", "sdb", "sdb1", "in_sync", mdU2)
	s.member("md126", "sdc", "sdc1", "in_sync", mdU2)
	s.write("block/md126/md126p1/partition", "1")
	// degraded raid5 with a spare.
	s.array("md127", strings.ToUpper(mdU1), "raid5", "clean", true)
	s.write("block/md127/md/degraded", "1")
	s.member("md127", "sdd", "sdd", "in_sync", mdU1)
	s.member("md127", "sde", "sde", "in_sync", mdU1)
	s.member("md127", "sdf", "sdf", "in_sync", mdU1)
	s.member("md127", "sdx", "sdx", "spare", mdU1)

	es, err := s.mdadm().Existing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("want 2 entries, got %+v", es)
	}
	r1 := findExisting(t, es, "md126")
	want1 := Existing{Backend: "mdadm", ID: mdU2, Name: "md126",
		Summary: "md126 raid1 2 disks, 2.0 TB, clean",
		Disks:   []Target{tgt("sdb"), tgt("sdc")},
		Stack:   []string{"md126 (raid1)", "md superblock of /dev/sdb1", "md superblock of /dev/sdc1"},
		Extra: map[string]string{ExtraKind: KindMDValue, ExtraOwnedNodes: "md126,md126p1",
			"assembled": "yes", "member_devs": "/dev/sdb1,/dev/sdc1"}}
	if !reflect.DeepEqual(r1, want1) {
		t.Errorf("raid1:\n got %+v\nwant %+v", r1, want1)
	}
	r5 := findExisting(t, es, "md127")
	if r5.ID != mdU1 {
		t.Errorf("uuid not normalised to lower case: %q", r5.ID)
	}
	if r5.Summary != "md127 raid5 3 data + 1 spare, 2.0 TB, clean, degraded" {
		t.Errorf("raid5 summary %q", r5.Summary)
	}
	if !reflect.DeepEqual(r5.Disks, []Target{tgt("sdd"), tgt("sde"), tgt("sdf"), tgt("sdx")}) {
		t.Errorf("raid5 disks %+v", r5.Disks)
	}
	if r5.Extra["member_devs"] != "/dev/sdd,/dev/sde,/dev/sdf,/dev/sdx" || r5.Extra[ExtraOwnedNodes] != "md127" {
		t.Errorf("raid5 extra %+v", r5.Extra)
	}
	if r5.Stack[0] != "md127 (raid5)" {
		t.Errorf("raid5 stack %v", r5.Stack)
	}
	for _, c := range s.calls {
		for _, bad := range []string{"--stop", "--zero-superblock", "--force", "--create", "--assemble"} {
			if strings.Contains(c, bad) {
				t.Errorf("listing ran %q", c)
			}
		}
	}
}

func TestMdExistingInactiveArrayTakesTheUUIDOfItsMembers(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb")
	s.disk("sdc")
	s.array("md125", mdU1, "", "inactive", false) // --detail does not answer
	s.member("md125", "sdb", "sdb", "spare", mdU1)
	s.member("md125", "sdc", "sdc", "spare", mdU1)

	es, err := s.mdadm().Existing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 {
		t.Fatalf("want the inactive array only, got %+v", es)
	}
	e := es[0]
	if e.Name != "md125" || e.ID != mdU1 || e.Extra["assembled"] != "yes" {
		t.Errorf("inactive entry %+v", e)
	}
	if e.Stack[0] != "md125" || !strings.HasSuffix(e.Summary, "inactive") {
		t.Errorf("inactive stack %v summary %q", e.Stack, e.Summary)
	}
}

func TestMdExistingListsAnArrayWithoutAUUIDAsBlocked(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb")
	s.disk("sdc")
	s.array("md125", mdU1, "", "inactive", false)
	s.member("md125", "sdb", "sdb", "spare", mdU1)
	s.member("md125", "sdc", "sdc", "spare", mdU2) // members disagree
	es, err := s.mdadm().Existing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Listed, blocked; its members' superblocks are not a second entry.
	if len(es) != 1 {
		t.Fatalf("want the blocked array only, got %+v", es)
	}
	if e := es[0]; e.Name != "md125" || !strings.Contains(e.Blocked, "uuid") || e.ID == "" ||
		!reflect.DeepEqual(e.Disks, []Target{tgt("sdb"), tgt("sdc")}) {
		t.Errorf("blocked entry %+v", e)
	}
}

func TestMdExistingUnassembledPair(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb", "sdb1")
	s.disk("sdc", "sdc1")
	s.supers["/dev/sdb1"] = mdU2
	s.supers["/dev/sdc1"] = mdU2
	s.levels[mdU2] = "raid1"

	es, err := s.mdadm().Existing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Existing{{Backend: "mdadm", ID: mdU2, Name: "md-e5f6a7b8",
		Summary: "unassembled raid1 md superblocks (uuid " + mdU2 + ") on /dev/sdb1, /dev/sdc1",
		Disks:   []Target{tgt("sdb"), tgt("sdc")},
		Stack:   []string{"md superblock of /dev/sdb1", "md superblock of /dev/sdc1"},
		Extra:   map[string]string{ExtraKind: KindMDValue, "assembled": "no", "member_devs": "/dev/sdb1,/dev/sdc1"}}}
	if !reflect.DeepEqual(es, want) {
		t.Errorf("\n got %+v\nwant %+v", es, want)
	}
}

func TestMdExistingAssembledWinsOverAStaleMember(t *testing.T) {
	s := newMdDestroySim(t)
	for _, n := range []string{"sdb", "sdc", "sdd", "sde", "sdf"} {
		s.disk(n)
	}
	s.array("md127", mdU1, "raid1", "clean", true)
	s.member("md127", "sdb", "sdb", "in_sync", mdU1)
	s.member("md127", "sdc", "sdc", "in_sync", mdU1)
	s.supers["/dev/sdd"] = mdU1 // a stale member of md127, not in it
	s.supers["/dev/sde"] = mdU2
	s.supers["/dev/sdf"] = mdU2

	es, err := s.mdadm().Existing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("want md127 and one unassembled group, got %+v", es)
	}
	a := findExisting(t, es, "md127")
	if !reflect.DeepEqual(a.Disks, []Target{tgt("sdb"), tgt("sdc")}) {
		t.Errorf("md127 disks %+v", a.Disks)
	}
	u := findExisting(t, es, "md-e5f6a7b8")
	if !strings.Contains(a.Summary, "stale md superblock of md127 on /dev/sdd") {
		t.Errorf("md127 summary does not name the stale member: %q", a.Summary)
	}
	if u.Extra["member_devs"] != "/dev/sde,/dev/sdf" {
		t.Errorf("unassembled devs %q", u.Extra["member_devs"])
	}
}

func TestMdExistingUnresolvedDiskHasNoSerial(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb")
	s.supers["/dev/sdb"] = mdU2
	s.supers["/dev/mapper/odd"] = mdU2
	es, err := s.mdadm().Existing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 {
		t.Fatalf("got %+v", es)
	}
	var unresolved bool
	for _, d := range es[0].Disks {
		if d.Serial == "" {
			unresolved = true
		}
	}
	if !unresolved {
		t.Errorf("a device that is not a known disk must give a disk without serial (the gate refuses it): %+v", es[0].Disks)
	}
}

// examineScanSample is shaped like mdadm --examine --scan --verbose:
// a native array, a 0.90 array, an IMSM container and its member array.
const examineScanSample = `ARRAY /dev/md/data  level=raid5 metadata=1.2 num-devices=3 UUID=a1b2c3d4:00000000:11111111:22222222 name=host:data
   devices=/dev/sdc1,/dev/sdb1,/dev/sdd1
ARRAY /dev/md0 level=raid1 num-devices=2 UUID=e5f6a7b8:33333333:44444444:55555555
   devices=/dev/sde,/dev/sdf
ARRAY metadata=imsm UUID=11111111:22222222:33333333:44444444
   devices=/dev/sdg,/dev/sdh
ARRAY /dev/md/Volume0 container=11111111:22222222:33333333:44444444 member=0 UUID=55555555:66666666:77777777:88888888
ARRAY /dev/md/odd metadata=1.2 UUID=garbage
   devices=/dev/sdi
some line mdadm might print
`

func TestParseExamineScan(t *testing.T) {
	got := parseExamineScan([]byte(examineScanSample))
	want := []mdSuperGroup{
		{uuid: "11111111:22222222:33333333:44444444", devs: []string{"/dev/sdg", "/dev/sdh"}, blocked: mdFirmwareBlocked},
		{uuid: mdU1, level: "raid5", devs: []string{"/dev/sdb1", "/dev/sdc1", "/dev/sdd1"}},
		{uuid: mdU2, level: "raid1", devs: []string{"/dev/sde", "/dev/sdf"}},
		{devs: []string{"/dev/sdi"}, blocked: "its superblock has no readable uuid"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

// raid5Sim is md127, raid5 on sdb, sdc, sdd, listed.
func raid5Sim(t *testing.T) (*mdDestroySim, Existing) {
	s := newMdDestroySim(t)
	for _, n := range []string{"sdb", "sdc", "sdd"} {
		s.disk(n)
	}
	s.array("md127", mdU1, "raid5", "clean", true)
	for _, n := range []string{"sdb", "sdc", "sdd"} {
		s.member("md127", n, n, "in_sync", mdU1)
	}
	es, err := s.mdadm().Existing(context.Background())
	if err != nil || len(es) != 1 {
		t.Fatalf("listing: %v %+v", err, es)
	}
	s.calls = nil
	return s, es[0]
}

func TestMdDestroyAssembledArgvOrder(t *testing.T) {
	s, e := raid5Sim(t)
	if err := s.mdadm().Destroy(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mdadm --detail --export /dev/md127",
		"mdadm --stop /dev/md127",
	}
	for _, d := range []string{"/dev/sdb", "/dev/sdc", "/dev/sdd"} {
		want = append(want, "mdadm --examine --export "+d, "mdadm --zero-superblock "+d, "mdadm --examine --export "+d)
	}
	if !slices.Equal(s.calls, want) {
		t.Errorf("calls:\n got %q\nwant %q", s.calls, want)
	}
	if len(s.supers) != 0 {
		t.Errorf("superblocks left: %v", s.supers)
	}
}

func TestMdDestroyStillAssembledAfterStopZeroesNothing(t *testing.T) {
	s, e := raid5Sim(t)
	s.stopKeeps = true
	err := s.mdadm().Destroy(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "still") {
		t.Fatalf("want a still-assembled error, got %v", err)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "--zero-superblock") {
			t.Errorf("zeroed after a failed stop: %q", s.calls)
		}
	}
	if !strings.Contains(err.Error(), "mdadm --zero-superblock /dev/sdb /dev/sdc /dev/sdd") {
		t.Errorf("error does not give the remaining command: %v", err)
	}
}

func TestMdDestroyUUIDMismatchLeavesTheMember(t *testing.T) {
	s, e := raid5Sim(t)
	s.onStop = func() { s.supers["/dev/sdc"] = mdU2 } // sdc now carries another array
	err := s.mdadm().Destroy(context.Background(), e)
	if err == nil {
		t.Fatal("want an error")
	}
	if slices.Contains(s.calls, "mdadm --zero-superblock /dev/sdc") || slices.Contains(s.calls, "mdadm --zero-superblock /dev/sdd") {
		t.Errorf("zeroed a member it could not prove, or went on after: %q", s.calls)
	}
	if !slices.Contains(s.calls, "mdadm --zero-superblock /dev/sdb") {
		t.Errorf("did not zero the first member: %q", s.calls)
	}
	msg := err.Error()
	for _, w := range []string{"/dev/sdc carries", mdU2, "do not zero it", "mdadm --zero-superblock /dev/sdd", "mdadm --examine"} {
		if !strings.Contains(msg, w) {
			t.Errorf("error %q lacks %q", msg, w)
		}
	}
	if strings.Contains(msg, "--zero-superblock /dev/sdc") {
		t.Errorf("the manual command zeroes the foreign device: %q", msg)
	}
	if s.supers["/dev/sdc"] != mdU2 {
		t.Errorf("the other array's superblock was touched")
	}
}

func TestMdDestroyZeroFailureReportsTheRest(t *testing.T) {
	s, e := raid5Sim(t)
	s.fail["mdadm --zero-superblock /dev/sdc"] = exitErr("mdadm: Couldn't open /dev/sdc for write")
	err := s.mdadm().Destroy(context.Background(), e)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, w := range []string{"stopped md127", "/dev/sdb", "mdadm --zero-superblock /dev/sdc /dev/sdd", "Couldn't open"} {
		if !strings.Contains(msg, w) {
			t.Errorf("error %q lacks %q", msg, w)
		}
	}
	if strings.Contains(msg, "--stop") || strings.Contains(msg, "--zero-superblock /dev/sdb") {
		t.Errorf("the manual command repeats what is done: %q", msg)
	}
	if slices.Contains(s.calls, "mdadm --zero-superblock /dev/sdd") {
		t.Errorf("went on after a failure: %q", s.calls)
	}
}

func TestMdDestroyRefusesWhenMembersDiffer(t *testing.T) {
	cases := map[string]func(s *mdDestroySim, e *Existing){
		"disk left out of the entry": func(_ *mdDestroySim, e *Existing) { e.Disks = e.Disks[:2] },
		"disk added to the entry": func(s *mdDestroySim, e *Existing) {
			s.disk("sde")
			e.Disks = append(e.Disks, tgt("sde"))
		},
		"member added to the array": func(s *mdDestroySim, _ *Existing) {
			s.disk("sde")
			s.member("md127", "sde", "sde", "spare", mdU1)
		},
		"device not of the entry": func(_ *mdDestroySim, e *Existing) {
			e.Extra = map[string]string{ExtraKind: KindMDValue, ExtraOwnedNodes: "md127", "assembled": "yes", "member_devs": "/dev/sdb,/dev/sdc,/dev/sde"}
		},
		"serial changed": func(s *mdDestroySim, _ *Existing) { s.write("block/sdc/device/serial", "OTHER") },
		"no kind": func(_ *mdDestroySim, e *Existing) {
			e.Extra = map[string]string{"assembled": "yes", "member_devs": e.Extra["member_devs"]}
		},
		"no uuid":      func(_ *mdDestroySim, e *Existing) { e.ID = "" },
		"uuid changed": func(s *mdDestroySim, _ *Existing) { s.arrays["md127"] = mdU2 },
		"bad device path": func(_ *mdDestroySim, e *Existing) {
			e.Extra = map[string]string{ExtraKind: KindMDValue, ExtraOwnedNodes: "md127", "assembled": "yes", "member_devs": "/dev/sdb,/dev/sdc,/dev/../sdd"}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s, e := raid5Sim(t)
			change(s, &e)
			err := s.mdadm().Destroy(context.Background(), e)
			if err == nil {
				t.Fatal("want a refusal")
			}
			for _, c := range s.calls {
				if strings.Contains(c, "--stop") || strings.Contains(c, "--zero-superblock") {
					t.Errorf("acted: %q", s.calls)
				}
			}
			if !strings.Contains(err.Error(), "nothing done") {
				t.Errorf("refusal does not say nothing was done: %v", err)
			}
		})
	}
}

func TestMdDestroyCancelledDuringStopStillVerifies(t *testing.T) {
	s, e := raid5Sim(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.onStop = cancel
	err := s.mdadm().Destroy(ctx, e)
	if err == nil {
		t.Fatal("want an error: cancelled before any superblock")
	}
	if !strings.Contains(err.Error(), "stopped md127") || !strings.Contains(err.Error(), "mdadm --zero-superblock /dev/sdb /dev/sdc /dev/sdd") {
		t.Errorf("error %v", err)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "--zero-superblock") {
			t.Errorf("zeroed after cancel: %q", s.calls)
		}
	}
}

func TestMdDestroyUnassembled(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb", "sdb1")
	s.disk("sdc", "sdc1")
	s.supers["/dev/sdb1"] = mdU2
	s.supers["/dev/sdc1"] = mdU2
	m := s.mdadm()
	es, err := m.Existing(context.Background())
	if err != nil || len(es) != 1 {
		t.Fatalf("listing: %v %+v", err, es)
	}
	s.calls = nil
	if err := m.Destroy(context.Background(), es[0]); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, d := range []string{"/dev/sdb1", "/dev/sdc1"} {
		want = append(want, "mdadm --examine --export "+d, "mdadm --zero-superblock "+d, "mdadm --examine --export "+d)
	}
	if !slices.Equal(s.calls, want) {
		t.Errorf("calls:\n got %q\nwant %q", s.calls, want)
	}
}

func TestMdDestroyUnassembledRefusesWhenAssembledMeanwhile(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb")
	s.disk("sdc")
	s.supers["/dev/sdb"] = mdU2
	s.supers["/dev/sdc"] = mdU2
	m := s.mdadm()
	es, err := m.Existing(context.Background())
	if err != nil || len(es) != 1 {
		t.Fatalf("listing: %v %+v", err, es)
	}
	s.array("md127", mdU2, "raid1", "clean", true)
	s.member("md127", "sdb", "sdb", "in_sync", mdU2)
	s.member("md127", "sdc", "sdc", "in_sync", mdU2)
	s.calls = nil
	err = m.Destroy(context.Background(), es[0])
	if err == nil || !strings.Contains(err.Error(), "md127") {
		t.Fatalf("want a refusal naming md127, got %v", err)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "--zero-superblock") || strings.Contains(c, "--stop") {
			t.Errorf("acted: %q", s.calls)
		}
	}
}

func TestMdDestroyCommand(t *testing.T) {
	m := &mdadm{}
	e := Existing{Name: "md127", Extra: map[string]string{"assembled": "yes", "member_devs": "/dev/sdb,/dev/sdc"}}
	if got := m.DestroyCommand(e); got != "mdadm --stop /dev/md127 && mdadm --zero-superblock /dev/sdb /dev/sdc" {
		t.Errorf("assembled: %q", got)
	}
	e = Existing{Name: "md-e5f6a7b8", Extra: map[string]string{"assembled": "no", "member_devs": "/dev/sdb1,/dev/it's"}}
	if got := m.DestroyCommand(e); got != `mdadm --zero-superblock /dev/sdb1 '/dev/it'"'"'s'` {
		t.Errorf("unassembled: %q", got)
	}
}

func TestMdDestroyUnexaminableMemberIsLeftOutOfTheCommand(t *testing.T) {
	s, e := raid5Sim(t)
	s.fail["mdadm --examine --export /dev/sdc"] = exitErr("mdadm: cannot open /dev/sdc: Input/output error")
	err := s.mdadm().Destroy(context.Background(), e)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, w := range []string{"/dev/sdc could not be examined", "mdadm --zero-superblock /dev/sdd"} {
		if !strings.Contains(msg, w) {
			t.Errorf("error %q lacks %q", msg, w)
		}
	}
	if strings.Contains(msg, "--zero-superblock /dev/sdc") || slices.Contains(s.calls, "mdadm --zero-superblock /dev/sdc") {
		t.Errorf("zeroes, or tells to zero, an unproven device: %q", msg)
	}
}

func TestMdDestroyStopErrorSaysWhatTheArrayIs(t *testing.T) {
	s, e := raid5Sim(t)
	s.fail["mdadm --stop"] = exitErr("mdadm: Cannot get exclusive access to /dev/md127")
	err := s.mdadm().Destroy(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "md127 is still assembled") || !strings.Contains(err.Error(), "exclusive access") {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(err.Error(), "nothing done") {
		t.Errorf("claims nothing done without looking: %v", err)
	}
	// mdadm failed but the array is gone after all: say so.
	s, e = raid5Sim(t)
	inner := s.mdSim.run
	m := s.mdadm()
	m.env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--stop" {
			s.calls = append(s.calls, name+" "+strings.Join(args, " "))
			s.remove("block/md127")
			delete(s.arrays, "md127")
			return nil, exitErr("mdadm: odd")
		}
		return inner(ctx, name, args...)
	}
	err = m.Destroy(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "md127 is no longer assembled") || !strings.Contains(err.Error(), "no superblock zeroed") {
		t.Errorf("stopped despite the error: %v", err)
	}
	if slices.ContainsFunc(s.calls, func(c string) bool { return strings.Contains(c, "--zero-superblock") }) {
		t.Errorf("zeroed after a failed stop: %q", s.calls)
	}
}

func TestMdDestroyAcceptsAClearedArrayNode(t *testing.T) {
	s, e := raid5Sim(t)
	s.stopKeeps = true
	s.onStop = func() {
		for _, n := range []string{"sdb", "sdc", "sdd"} {
			s.remove("block/md127/md/dev-" + n)
		}
		s.write("block/md127/md/array_state", "clear")
		delete(s.arrays, "md127")
	}
	if err := s.mdadm().Destroy(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(s.supers) != 0 {
		t.Errorf("superblocks left: %v", s.supers)
	}
}

func TestMdDestroyUnassembledFailsClosedOnAnUnreadableArray(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb")
	s.disk("sdc")
	s.supers["/dev/sdb"] = mdU2
	s.supers["/dev/sdc"] = mdU2
	m := s.mdadm()
	es, err := m.Existing(context.Background())
	if err != nil || len(es) != 1 {
		t.Fatalf("listing: %v %+v", err, es)
	}
	// An md node whose members cannot be read: it may carry the uuid.
	if err := os.MkdirAll(filepath.Join(s.sys, "block", "md9", "md", "dev-sdz"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.calls = nil
	err = m.Destroy(context.Background(), es[0])
	if err == nil || !strings.Contains(err.Error(), "md9") {
		t.Fatalf("want a refusal naming md9, got %v", err)
	}
	if slices.ContainsFunc(s.calls, func(c string) bool { return strings.Contains(c, "--zero-superblock") }) {
		t.Errorf("acted: %q", s.calls)
	}
}

func TestMdExistingUnassembledNamesAreUnique(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb")
	s.disk("sdc")
	s.supers["/dev/sdb"] = "e5f6a7b8:11111111:00000000:00000000"
	s.supers["/dev/sdc"] = "e5f6a7b8:22222222:00000000:00000000"
	es, err := s.mdadm().Existing(context.Background())
	if err != nil || len(es) != 2 {
		t.Fatalf("listing: %v %+v", err, es)
	}
	if es[0].Name != "md-e5f6a7b81" || es[1].Name != "md-e5f6a7b82" {
		t.Errorf("names %q %q", es[0].Name, es[1].Name)
	}
}

func TestMdExistingBlocksFirmwareAndUnknownMetadata(t *testing.T) {
	s := newMdDestroySim(t)
	for _, n := range []string{"sdb", "sdc", "sdd", "sde", "sdf"} {
		s.disk(n)
	}
	s.array("md126", "11111111:22222222:33333333:44444444", "container", "inactive", true)
	s.write("block/md126/md/metadata_version", "external:imsm")
	s.member("md126", "sdb", "sdb", "spare", "11111111:22222222:33333333:44444444")
	s.member("md126", "sdc", "sdc", "spare", "11111111:22222222:33333333:44444444")
	s.array("md125", mdU1, "raid1", "clean", true)
	s.write("block/md125/md/metadata_version", "none")
	s.member("md125", "sdd", "sdd", "in_sync", mdU1)
	es, err := s.mdadm().Existing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := findExisting(t, es, "md126 (IMSM container)")
	if c.Blocked != mdFirmwareBlocked || !reflect.DeepEqual(c.Disks, []Target{tgt("sdb"), tgt("sdc")}) {
		t.Errorf("container %+v", c)
	}
	n := findExisting(t, es, "md125")
	if !strings.Contains(n.Blocked, "metadata") {
		t.Errorf("metadata none: %+v", n)
	}
	if len(es) != 2 {
		t.Errorf("want only the two blocked arrays, got %+v", es)
	}
}

// layoutOver is the layout setting with the mdadm backend of s.
func layoutOver(s *mdDestroySim) LayoutSetting {
	env := s.env()
	return NewLayout(env, &mdadm{env: env.fill()})
}

func TestMdDestroyBlockedEntryThroughTheGate(t *testing.T) {
	s := newMdDestroySim(t)
	s.disk("sdb")
	s.disk("sdc")
	s.array("md126", "11111111:22222222:33333333:44444444", "container", "inactive", true)
	s.write("block/md126/md/metadata_version", "external:imsm")
	s.member("md126", "sdb", "sdb", "spare", "11111111:22222222:33333333:44444444")
	s.member("md126", "sdc", "sdc", "spare", "11111111:22222222:33333333:44444444")
	l := layoutOver(s)
	es, errs := l.Existing(context.Background())
	if len(errs) != 0 || len(es) != 1 || es[0].Blocked == "" {
		t.Fatalf("listing %+v %v", es, errs)
	}
	bs, err := l.DestroyBlockers(context.Background(), es[0])
	if err != nil || len(bs) != 1 || bs[0] != mdFirmwareBlocked {
		t.Errorf("blockers %v %v", bs, err)
	}
	s.calls = nil
	if err := l.Destroy(context.Background(), es[0]); err == nil || !strings.Contains(err.Error(), mdFirmwareBlocked) {
		t.Errorf("destroy %v", err)
	}
	if len(s.calls) != 0 {
		t.Errorf("ran %q", s.calls)
	}
}

func TestMdDestroyThroughTheGateIgnoresTheArrayState(t *testing.T) {
	s, e := raid5Sim(t)
	l := layoutOver(s)
	s.write("block/md127/md/array_state", "active") // a write since it was shown
	if bs, err := l.DestroyBlockers(context.Background(), e); err != nil || len(bs) != 0 {
		t.Fatalf("clean -> active refused: %v %v", bs, err)
	}
	if err := l.Destroy(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(s.supers) != 0 {
		t.Errorf("superblocks left: %v", s.supers)
	}

	// A member added since it was shown still is a change.
	s, e = raid5Sim(t)
	l = layoutOver(s)
	s.disk("sde")
	s.member("md127", "sde", "sde", "spare", mdU1)
	bs, err := l.DestroyBlockers(context.Background(), e)
	if err != nil || len(bs) != 1 || !strings.Contains(bs[0], "changed") {
		t.Errorf("member added: %v %v", bs, err)
	}
}

func TestMdExistingNestedOnLVHasNoSerial(t *testing.T) {
	s := newMdDestroySim(t)
	for _, n := range []string{"dm-0", "dm-1"} {
		s.write(filepath.Join("block", n, "size"), "2000") // an LV: no device/serial
	}
	s.array("md127", mdU1, "raid1", "clean", true)
	s.member("md127", "dm-0", "dm-0", "in_sync", mdU1)
	s.member("md127", "dm-1", "dm-1", "in_sync", mdU1)
	l := layoutOver(s)
	es, errs := l.Existing(context.Background())
	if len(errs) != 0 || len(es) != 1 {
		t.Fatalf("listing %+v %v", es, errs)
	}
	if d := es[0].Disks; len(d) != 2 || d[0].Name != "dm-0" || d[0].Serial != "" {
		t.Errorf("disks %+v", d)
	}
	bs, err := l.DestroyBlockers(context.Background(), es[0])
	if err != nil || len(bs) != 1 || !strings.Contains(bs[0], "no serial") {
		t.Errorf("blockers %v %v", bs, err)
	}
}

func TestMdDestroyCommandOfABlockedEntryIsEmpty(t *testing.T) {
	m := &mdadm{}
	for _, e := range []Existing{
		{Name: "md-11111111", ID: "11111111:22222222:33333333:44444444", Blocked: mdFirmwareBlocked,
			Extra: map[string]string{ExtraKind: KindMDValue, "assembled": "no", "member_devs": "/dev/sdg,/dev/sdh"}},
		{Name: "md126 (IMSM container)", ID: "11111111:22222222:33333333:44444444", Blocked: mdFirmwareBlocked,
			Extra: map[string]string{ExtraKind: KindMDValue, "assembled": "yes", "member_devs": "/dev/sdg,/dev/sdh"}},
	} {
		if c := m.DestroyCommand(e); c != "" {
			t.Errorf("%s: %q", e.Name, c)
		}
		if c := m.LeftoverCommand(e, e); c != "" {
			t.Errorf("%s leftover: %q", e.Name, c)
		}
	}
}

func TestMdLeftoverCommandKeepsToTheConfirmedDevices(t *testing.T) {
	m := &mdadm{}
	confirmed := Existing{Name: "md127", ID: mdU1,
		Extra: map[string]string{ExtraKind: KindMDValue, "assembled": "yes", "member_devs": "/dev/sdb,/dev/sdc"}}
	fresh := Existing{Name: "md-a1b2c3d4", ID: mdU1,
		Extra: map[string]string{ExtraKind: KindMDValue, "assembled": "no", "member_devs": "/dev/sdc,/dev/sde"}}
	want := "after checking with mdadm --examine that each device carries uuid " + mdU1 + ": mdadm --zero-superblock /dev/sdc"
	if got := m.LeftoverCommand(confirmed, fresh); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	// Still assembled under the confirmed node: stop it, then zero.
	fresh = confirmed
	want = "after checking with mdadm --examine that each device carries uuid " + mdU1 + ": mdadm --stop /dev/md127 && mdadm --zero-superblock /dev/sdb /dev/sdc"
	if got := m.LeftoverCommand(confirmed, fresh); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	// Assembled under another node: never a --stop of a node not confirmed.
	fresh = Existing{Name: "md9", ID: mdU1,
		Extra: map[string]string{ExtraKind: KindMDValue, "assembled": "yes", "member_devs": "/dev/sdb"}}
	if got := m.LeftoverCommand(confirmed, fresh); got != "" {
		t.Errorf("other node: %q", got)
	}
	// Only unconfirmed devices left: nothing.
	fresh = Existing{Name: "md-a1b2c3d4", ID: mdU1,
		Extra: map[string]string{ExtraKind: KindMDValue, "assembled": "no", "member_devs": "/dev/sde"}}
	if got := m.LeftoverCommand(confirmed, fresh); got != "" {
		t.Errorf("unconfirmed only: %q", got)
	}
}

// After a successful teardown a stale superblock of the same uuid on a disk
// that was never confirmed is listed again under that uuid: the gate's
// leftover command must not name it.
func TestMdDestroyLeftoverNeverNamesAStaleDevice(t *testing.T) {
	s, e := raid5Sim(t)
	s.disk("sde")
	s.supers["/dev/sde"] = mdU1
	env := s.env()
	scans := 0
	env.InUse = func() (*inuse.Report, error) {
		if scans++; scans == 1 {
			return &inuse.Report{Busy: map[string][]string{}}, nil
		}
		return reportOf(map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindMD, Disk: "sdb", Via: mdU1, Device: "sdb", Text: "has an md RAID superblock"}}}), nil
	}
	l := NewLayout(env, &mdadm{env: env.fill()})
	err := l.Destroy(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "has an md RAID superblock") {
		t.Fatalf("want the leftover reported, got %v", err)
	}
	if strings.Contains(err.Error(), "/dev/sde") {
		t.Errorf("names the unconfirmed stale device: %v", err)
	}
}

func TestMdPartialWithNothingProvenLeft(t *testing.T) {
	s, e := raid5Sim(t)
	s.fail["mdadm --examine --export /dev/sdd"] = exitErr("mdadm: cannot open /dev/sdd")
	err := s.mdadm().Destroy(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "nothing proven is left to run by hand") {
		t.Errorf("error %v", err)
	}
}
