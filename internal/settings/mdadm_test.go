package settings

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mdSim is a fake mdadm over a temporary sysfs tree and /dev directory. It
// records every command, answers from its state and can be told to fail.
type mdSim struct {
	t        *testing.T
	sys, dev string
	calls    []string
	next     string            // kernel name of the next array created, default md127
	readAuto bool              // a created array starts read-auto
	arrays   map[string]string // kernel name -> uuid, for --detail
	supers   map[string]string // member path -> uuid of its superblock
	fail     map[string]error  // command prefix -> error returned instead
}

func newMdSim(t *testing.T) *mdSim {
	t.Helper()
	s := &mdSim{t: t, sys: t.TempDir(), dev: t.TempDir(), next: "md127",
		arrays: map[string]string{}, supers: map[string]string{}, fail: map[string]error{}}
	for _, d := range []string{"sdb", "sdc", "sdd", "sde", "sdf", "sdx", "nvme0n1"} {
		if err := os.MkdirAll(filepath.Join(s.sys, "block", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *mdSim) env() Env {
	return Env{Run: s.run, LookPath: lookAll, Sys: s.sys, Dev: s.dev,
		Rand: bytes.NewReader(seqBytes()), Poll: time.Millisecond, Settle: 200 * time.Millisecond,
		InUse: freeDisks}
}

func seqBytes() []byte {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func exitErr(stderr string) error { return &exec.ExitError{Stderr: []byte(stderr)} }

func (s *mdSim) write(rel, content string) {
	s.t.Helper()
	p := filepath.Join(s.sys, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *mdSim) remove(rel string) {
	s.t.Helper()
	if err := os.RemoveAll(filepath.Join(s.sys, rel)); err != nil {
		s.t.Fatal(err)
	}
}

// addDev makes disk a member of md with the given dev-*/state.
func (s *mdSim) addDev(md, disk, state string) {
	s.t.Helper()
	dir := filepath.Join(s.sys, "block", md, "md", "dev-"+disk)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(s.sys, "block", disk), filepath.Join(dir, "block")); err != nil {
		s.t.Fatal(err)
	}
	s.write(filepath.Join("block", md, "md", "dev-"+disk, "state"), state)
}

func (s *mdSim) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, key)
	for p, err := range s.fail {
		if strings.HasPrefix(key, p) {
			return nil, err
		}
	}
	if name != "mdadm" || len(args) == 0 {
		return nil, errors.New("unexpected " + key)
	}
	switch args[0] {
	case "--create":
		return nil, s.create(args[1:])
	case "--detail":
		md := strings.TrimPrefix(args[len(args)-1], "/dev/")
		if u, ok := s.arrays[md]; ok {
			return []byte("MD_LEVEL=raid5\nMD_DEVICES=3\nMD_NAME=host:x\nMD_UUID=" + u + "\n"), nil
		}
		return nil, exitErr("mdadm: cannot open " + args[len(args)-1] + ": No such file or directory")
	case "--stop":
		md := strings.TrimPrefix(args[1], "/dev/")
		s.remove(filepath.Join("block", md))
		delete(s.arrays, md)
		return nil, nil
	case "--readwrite":
		md := strings.TrimPrefix(args[1], "/dev/")
		s.write(filepath.Join("block", md, "md", "array_state"), "clean")
		return nil, nil
	case "--examine":
		p := args[len(args)-1]
		if u, ok := s.supers[p]; ok {
			return []byte("MD_LEVEL=raid5\nMD_UUID=" + u + "\nMD_DEVICES=3\n"), nil
		}
		return nil, exitErr("mdadm: No md superblock detected on " + p + ".")
	case "--zero-superblock":
		delete(s.supers, args[1])
		return nil, nil
	}
	return nil, errors.New("unexpected " + key)
}

// create builds the sysfs of the array from the --create argv.
func (s *mdSim) create(args []string) error {
	md := s.next
	opt := map[string]string{}
	var paths []string
	for _, a := range args[1:] {
		if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
			opt[k[2:]] = v
		} else if !strings.HasPrefix(a, "--") {
			paths = append(paths, a)
		}
	}
	if err := os.MkdirAll(filepath.Join(s.dev, "md"), 0o755); err != nil {
		return err
	}
	if err := os.Symlink("../"+md, filepath.Join(s.dev, "md", opt["name"])); err != nil {
		return err
	}
	level := opt["level"]
	dir := filepath.Join("block", md, "md")
	s.write(filepath.Join(dir, "level"), level)
	s.write(filepath.Join(dir, "raid_disks"), opt["raid-devices"])
	state := "clean"
	if s.readAuto {
		state = "read-auto"
	}
	s.write(filepath.Join(dir, "array_state"), state)
	if c, ok := opt["chunk"]; ok {
		n, _ := strconv.Atoi(strings.TrimSuffix(c, "K"))
		s.write(filepath.Join(dir, "chunk_size"), strconv.Itoa(n*1024))
	} else {
		s.write(filepath.Join(dir, "chunk_size"), "0")
	}
	if level != "raid0" && level != "linear" {
		s.write(filepath.Join(dir, "sync_action"), "idle")
		s.write(filepath.Join(dir, "sync_completed"), "none")
		s.write(filepath.Join(dir, "degraded"), "0")
		loc := "none"
		if opt["bitmap"] == "internal" {
			loc = "+8"
		}
		s.write(filepath.Join(dir, "bitmap", "location"), loc)
	}
	switch level {
	case "raid5", "raid6":
		s.write(filepath.Join(dir, "layout"), "2")
	case "raid10":
		s.write(filepath.Join(dir, "layout"), "258")
	default:
		s.write(filepath.Join(dir, "layout"), "0")
	}
	data, _ := strconv.Atoi(opt["raid-devices"])
	for i, p := range paths {
		state := "in_sync"
		if i >= data {
			state = "spare"
		}
		s.addDev(md, filepath.Base(p), state)
		s.supers[p] = opt["uuid"]
	}
	if j, ok := opt["write-journal"]; ok {
		s.addDev(md, filepath.Base(j), "journal")
		s.supers[j] = opt["uuid"]
	}
	s.arrays[md] = opt["uuid"]
	return nil
}

func dsk(name, serial string, r Role) Disk {
	return Disk{Target: Target{Name: name, Path: "/dev/" + name, Serial: serial, Model: "M"}, Role: r}
}

func mdLayout(level string, opts ...string) Layout {
	l := Layout{Backend: "mdadm", Level: level, Options: map[string]string{}}
	for i := 0; i+1 < len(opts); i += 2 {
		l.Options[opts[i]] = opts[i+1]
	}
	return l
}

func raid5Disks() []Disk {
	return []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData), dsk("sdd", "S3", RoleData), dsk("sde", "S4", RoleSpare)}
}

func TestMdadm_Label(t *testing.T) {
	m := NewMdadm(Env{})
	data := func(n int) []Disk {
		var ds []Disk
		for i := range n {
			ds = append(ds, dsk(fmt.Sprintf("sd%c", 'b'+i), "S", RoleData))
		}
		return ds
	}
	cases := []struct {
		l     Layout
		disks []Disk
		want  string
	}{
		{mdLayout("raid10", "chunk", "512K", "bitmap", "no"), data(4), "mdadm raid10 · 4 disks · 512K"},
		{mdLayout("raid5", "chunk", "512K", "bitmap", "yes"), raid5Disks(), "mdadm raid5 · 3 data + 1 spare · 512K · bitmap"},
		{mdLayout("raid5", "chunk", "64K", "bitmap", "no"),
			append(data(3), dsk("sdf", "S", RoleSpare), dsk("sdx", "S", RoleSpare), dsk("nvme0n1", "S", RoleJournal)),
			"mdadm raid5 · 3 data + 2 spares + 1 journal · 64K"},
		{mdLayout("raid1", "bitmap", "yes"), data(2), "mdadm raid1 · 2 disks · bitmap"},
		{mdLayout("linear"), data(2), "mdadm linear · 2 disks"},
		{mdLayout("linear"), data(1), "mdadm linear · 1 disk"},
	}
	for _, c := range cases {
		if got := m.Label(c.l, c.disks); got != c.want {
			t.Errorf("Label(%v) = %q, want %q", c.l, got, c.want)
		}
	}
}

func TestMdadm_ParseRoundTrip(t *testing.T) {
	m := NewMdadm(Env{})
	cases := map[string]Layout{
		"mdadm raid10 · 4 disks · 512K":                     mdLayout("raid10", "chunk", "512K", "bitmap", "no"),
		"mdadm raid5 · 3 data + 1 spare · 512K · bitmap":    mdLayout("raid5", "chunk", "512K", "bitmap", "yes"),
		"mdadm raid5 · 3 data + 2 spares + 1 journal · 64K": mdLayout("raid5", "chunk", "64K", "bitmap", "no"),
		"mdadm raid1 · 2 disks · bitmap":                    mdLayout("raid1", "bitmap", "yes"),
		"mdadm linear · 2 disks":                            mdLayout("linear"),
	}
	for v, want := range cases {
		got, err := m.Parse(v)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q) = %#v, %v; want %#v", v, got, err, want)
		}
	}
	for _, bad := range []string{"mdadm raid7 · 2 disks", "lvm raid5 · 3 disks", "mdadm raid5 · 3 disks", "mdadm raid5 · 3 disks · 512K · fast", "mdadm raid0 · two disks · 512K", "mdadm raid1 · 2 disks · 512K"} {
		if _, err := m.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestMdadm_Validate(t *testing.T) {
	m := NewMdadm(Env{})
	d := func(n int, extra ...Disk) []Disk {
		var ds []Disk
		for i := range n {
			ds = append(ds, dsk(fmt.Sprintf("sd%c", 'b'+i), "S", RoleData))
		}
		return append(ds, extra...)
	}
	j1, j2 := dsk("nvme0n1", "J1", RoleJournal), dsk("nvme1n1", "J2", RoleJournal)
	cases := []struct {
		l     Layout
		disks []Disk
		want  string
	}{
		{mdLayout("raid0", "chunk", "512K"), d(1), "raid0 needs at least 2 data disks, has 1"},
		{mdLayout("raid1", "bitmap", "no"), d(1), "raid1 needs at least 2 data disks, has 1"},
		{mdLayout("raid5", "chunk", "512K", "bitmap", "no"), d(2), "raid5 needs at least 3 data disks, has 2"},
		{mdLayout("raid6", "chunk", "512K", "bitmap", "no"), d(3), "raid6 needs at least 4 data disks, has 3"},
		{mdLayout("raid10", "chunk", "512K", "bitmap", "no"), d(3), "raid10 needs at least 4 data disks, has 3"},
		{mdLayout("linear"), d(1), "linear needs at least 2 data disks, has 1"},
		{mdLayout("raid5", "chunk", "512K", "bitmap", "no"), d(3, j1, j2), "at most one journal disk"},
		{mdLayout("raid5", "chunk", "512K", "bitmap", "yes"), d(3, j1), "a write journal and a bitmap cannot be used together"},
		{mdLayout("raid5", "chunk", "512K", "bitmap", "yes"), raid5Disks(), ""},
		{mdLayout("raid6", "chunk", "64K", "bitmap", "no"), d(4, j1), ""},
		{mdLayout("raid0", "chunk", "512K"), d(2), ""},
	}
	for _, c := range cases {
		err := m.Validate(c.l, c.disks)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("Validate(%v, %d disks) = %q, want %q", c.l, len(c.disks), got, c.want)
		}
	}
}

func TestMdadm_ThroughLayout(t *testing.T) {
	env := Env{LookPath: lookAll}
	s := NewLayout(env, NewMdadm(env))
	two := []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)}
	cases := []struct {
		l     Layout
		disks []Disk
		want  string
	}{
		{mdLayout("raid0", "chunk", "512K"), append(two, dsk("sdd", "S3", RoleSpare)), "raid0 cannot have a spare disk"},
		{mdLayout("raid1", "bitmap", "no"), append(two, dsk("sdd", "S3", RoleJournal)), "raid1 cannot have a journal disk"},
		{mdLayout("raid0", "chunk", "512K", "bitmap", "no"), two, "raid0 has no bitmap option"},
		{mdLayout("raid0", "chunk", "32K"), two, "chunk must be one of 64K, 128K, 256K, 512K, 1024K"},
	}
	for _, c := range cases {
		err := s.ValidateLayout(c.l, c.disks)
		if err == nil || err.Error() != c.want {
			t.Errorf("ValidateLayout(%v) = %v, want %q", c.l, err, c.want)
		}
	}
	v, err := s.LayoutValue(mdLayout("raid5", "chunk", "512K", "bitmap", "yes"), raid5Disks())
	if err != nil || v.ID != "mdadm raid5 · 3 data + 1 spare · 512K · bitmap" {
		t.Errorf("LayoutValue = %+v, %v", v, err)
	}
}

func TestMdadm_Identity(t *testing.T) {
	m := NewMdadm(Env{})
	if m.Name() != "mdadm" {
		t.Errorf("name %q", m.Name())
	}
	if want := []string{"raid0", "raid1", "raid5", "raid6", "raid10", "linear"}; !slices.Equal(m.Levels(), want) {
		t.Errorf("levels %v", m.Levels())
	}
	roles := map[string][]Role{
		"raid0": {RoleData}, "linear": {RoleData},
		"raid1": {RoleData, RoleSpare}, "raid10": {RoleData, RoleSpare},
		"raid5": {RoleData, RoleSpare, RoleJournal}, "raid6": {RoleData, RoleSpare, RoleJournal},
	}
	for lv, want := range roles {
		if got := m.Roles(lv); !slices.Equal(got, want) {
			t.Errorf("roles %s %v", lv, got)
		}
	}
	chunk := Option{Key: "chunk", Label: "chunk", Choices: []string{"64K", "128K", "256K", "512K", "1024K"}, Default: "512K"}
	bitmap := Option{Key: "bitmap", Label: "write-intent bitmap", Choices: []string{"no", "yes"}, Default: "no"}
	opts := map[string][]Option{
		"raid0": {chunk}, "raid1": {bitmap}, "raid5": {chunk, bitmap},
		"raid6": {chunk, bitmap}, "raid10": {chunk, bitmap}, "linear": nil,
	}
	for lv, want := range opts {
		if got := m.Options(lv, 4); !reflect.DeepEqual(got, want) {
			t.Errorf("options %s %#v", lv, got)
		}
	}
}

func TestMdadm_Available(t *testing.T) {
	if ok, why := NewMdadm(Env{LookPath: lookNone}).Available(); ok || why != "mdadm is not installed" {
		t.Errorf("%v %q", ok, why)
	}
	if ok, why := NewMdadm(Env{LookPath: lookAll}).Available(); !ok || why != "" {
		t.Errorf("%v %q", ok, why)
	}
}

func TestAll_RegistersLayout(t *testing.T) {
	all := All(Env{})
	if len(all) != 2 || all[0].Name() != "disk write cache policy" || all[1].Name() != "software layout" {
		t.Fatalf("%d settings", len(all))
	}
	ls, ok := all[1].(LayoutSetting)
	if !ok {
		t.Fatal("not a LayoutSetting")
	}
	var names []string
	for _, b := range ls.Backends() {
		names = append(names, b.Name())
	}
	if !slices.Equal(names, []string{"mdadm", "lvm"}) {
		t.Errorf("backends %v", names)
	}
}

func variant(name string, value string, ds []Disk) Variant {
	return Variant{Name: name, Value: Value{ID: value, Label: value}, Disks: ds}
}

func TestMdadm_PrepareRecordsNameAndUUID(t *testing.T) {
	s := newMdSim(t)
	m := NewMdadm(s.env())
	ds := raid5Disks()
	rec, err := m.Prepare(context.Background(), variant("1_1", "x", ds), mdLayout("raid5"), "bbbench_1_1")
	if err != nil {
		t.Fatal(err)
	}
	want := &ArrayUndo{Name: "bbbench_1_1", UUID: "00010203:04050607:08090a0b:0c0d0e0f", Members: ds}
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("%+v", rec)
	}
	if len(s.calls) != 0 {
		t.Errorf("ran %v", s.calls)
	}
}

func TestMdadm_PrepareRefusesExistingName(t *testing.T) {
	s := newMdSim(t)
	if err := os.MkdirAll(filepath.Join(s.dev, "md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../md0", filepath.Join(s.dev, "md", "bbbench_1_1")); err != nil {
		t.Fatal(err)
	}
	_, err := NewMdadm(s.env()).Prepare(context.Background(), variant("1_1", "x", raid5Disks()), mdLayout("raid5"), "bbbench_1_1")
	if err == nil || err.Error() != "/dev/md/bbbench_1_1 already exists" {
		t.Errorf("%v", err)
	}
}

// build prepares and applies l on ds and returns the record and targets.
func build(t *testing.T, s *mdSim, l Layout, ds []Disk) (*ArrayUndo, []BenchTarget) {
	t.Helper()
	m := NewMdadm(s.env())
	v := variant("1_1", m.Label(l, ds), ds)
	rec, err := m.Prepare(context.Background(), v, l, "bbbench_1_1")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := m.Apply(context.Background(), v, l, rec)
	if err != nil {
		t.Fatal(err)
	}
	return rec, ts
}

func TestMdadm_CreateArgs(t *testing.T) {
	const head = "mdadm --create /dev/md/bbbench_1_1 --run --metadata=1.2 --name=bbbench_1_1 --uuid=00010203:04050607:08090a0b:0c0d0e0f "
	two := []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)}
	four := append(slices.Clone(two), dsk("sdd", "S3", RoleData), dsk("sde", "S4", RoleData))
	cases := []struct {
		l     Layout
		disks []Disk
		want  string
	}{
		{mdLayout("raid5", "chunk", "512K", "bitmap", "yes"), raid5Disks(),
			"--level=raid5 --raid-devices=3 --chunk=512K --bitmap=internal --spare-devices=1 /dev/sdb /dev/sdc /dev/sdd /dev/sde"},
		{mdLayout("raid1", "bitmap", "no"), two, "--level=raid1 --raid-devices=2 --bitmap=none /dev/sdb /dev/sdc"},
		{mdLayout("raid0", "chunk", "512K"), two, "--level=raid0 --raid-devices=2 --chunk=512K /dev/sdb /dev/sdc"},
		{mdLayout("raid6", "chunk", "64K", "bitmap", "no"), append(slices.Clone(four), dsk("nvme0n1", "J", RoleJournal)),
			"--level=raid6 --raid-devices=4 --chunk=64K --write-journal=/dev/nvme0n1 /dev/sdb /dev/sdc /dev/sdd /dev/sde"},
		{mdLayout("linear"), two, "--level=linear --raid-devices=2 /dev/sdb /dev/sdc"},
	}
	for _, c := range cases {
		s := newMdSim(t)
		build(t, s, c.l, c.disks)
		if len(s.calls) == 0 || s.calls[0] != head+c.want {
			t.Errorf("%s:\n got %v\nwant %s", c.l.Level, s.calls, head+c.want)
		}
	}
}

func TestMdadm_ApplyResolvesKernelName(t *testing.T) {
	s := newMdSim(t)
	ds := raid5Disks()
	l := mdLayout("raid5", "chunk", "512K", "bitmap", "yes")
	_, ts := build(t, s, l, ds)
	uuid := "00010203:04050607:08090a0b:0c0d0e0f"
	want := []BenchTarget{{Kind: TargetArray,
		Target: Target{Name: "md127", Path: "/dev/md127", Serial: uuid, Model: "raid5", Vendor: "md", Interface: "md"},
		UUID:   uuid, Members: ds, Want: "mdadm raid5 · 3 data + 1 spare · 512K · bitmap"}}
	if !reflect.DeepEqual(ts, want) {
		t.Errorf("\n got %+v\nwant %+v", ts, want)
	}
	if slices.ContainsFunc(s.calls, func(c string) bool { return strings.Contains(c, "--readwrite") }) {
		t.Errorf("readwrite on a clean array: %v", s.calls)
	}
}

func TestMdadm_ApplyReadAutoSwitchesReadWrite(t *testing.T) {
	s := newMdSim(t)
	s.readAuto = true
	build(t, s, mdLayout("raid1", "bitmap", "no"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	if !slices.Contains(s.calls, "mdadm --readwrite /dev/md127") {
		t.Errorf("%v", s.calls)
	}
}

func TestMdadm_ApplyCreateFails(t *testing.T) {
	s := newMdSim(t)
	s.fail["mdadm --create"] = exitErr("mdadm: cannot open /dev/sdb: Device or resource busy\n")
	m := NewMdadm(s.env())
	ds := raid5Disks()
	l := mdLayout("raid5", "chunk", "512K", "bitmap", "yes")
	v := variant("1_1", m.Label(l, ds), ds)
	rec, err := m.Prepare(context.Background(), v, l, "bbbench_1_1")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := m.Apply(context.Background(), v, l, rec)
	if ts != nil || err == nil || !strings.Contains(err.Error(), "mdadm: cannot open /dev/sdb: Device or resource busy") ||
		!strings.Contains(err.Error(), "mdadm --create") {
		t.Errorf("%v %v", ts, err)
	}
}

func TestMdadm_WaitSteady(t *testing.T) {
	s := newMdSim(t)
	_, ts := build(t, s, mdLayout("raid1", "bitmap", "no"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	s.write("block/md127/md/sync_action", "resync")
	s.write("block/md127/md/sync_completed", "50 / 200")
	m := NewMdadm(s.env())
	var got []float64
	err := m.WaitSteady(context.Background(), ts, func(p float64) {
		got = append(got, p)
		s.write("block/md127/md/sync_action", "idle")
	})
	if err != nil || !slices.Equal(got, []float64{25}) {
		t.Errorf("%v %v", got, err)
	}

	s0 := newMdSim(t)
	_, ts0 := build(t, s0, mdLayout("raid0", "chunk", "512K"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	called := false
	if err := NewMdadm(s0.env()).WaitSteady(context.Background(), ts0, func(float64) { called = true }); err != nil || called {
		t.Errorf("raid0: %v %v", err, called)
	}

	s.write("block/md127/md/sync_action", "resync")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.WaitSteady(ctx, ts, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestMdadm_ReadArray(t *testing.T) {
	s := newMdSim(t)
	_, ts := build(t, s, mdLayout("raid5", "chunk", "512K", "bitmap", "yes"), raid5Disks())
	m := NewMdadm(s.env())
	r, err := m.Read(context.Background(), ts[0])
	if err != nil || r.Value != ts[0].Want || r.Kernel != "array_state=clean, layout=left-symmetric" {
		t.Errorf("%+v %v", r, err)
	}

	s10 := newMdSim(t)
	four := []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData), dsk("sdd", "S3", RoleData), dsk("sde", "S4", RoleData)}
	_, ts10 := build(t, s10, mdLayout("raid10", "chunk", "512K", "bitmap", "no"), four)
	r, err = NewMdadm(s10.env()).Read(context.Background(), ts10[0])
	if err != nil || r.Value != "mdadm raid10 · 4 disks · 512K" || r.Kernel != "array_state=clean, layout=n2" {
		t.Errorf("raid10 %+v %v", r, err)
	}
}

func TestMdadm_ReadDetectsDegradedAndCheck(t *testing.T) {
	s := newMdSim(t)
	_, ts := build(t, s, mdLayout("raid5", "chunk", "512K", "bitmap", "yes"), raid5Disks())
	m := NewMdadm(s.env())
	s.write("block/md127/md/degraded", "1")
	r, _ := m.Read(context.Background(), ts[0])
	if r.Value != ts[0].Want+" (degraded)" {
		t.Errorf("degraded: %q", r.Value)
	}
	s.write("block/md127/md/degraded", "0")
	s.write("block/md127/md/sync_action", "check")
	r, _ = m.Read(context.Background(), ts[0])
	if r.Value != ts[0].Want+" (sync_action check)" {
		t.Errorf("check: %q", r.Value)
	}
	s.write("block/md127/md/sync_action", "idle")
	s.write("block/md127/md/array_state", "read-auto")
	r, _ = m.Read(context.Background(), ts[0])
	if r.Value != ts[0].Want+" (array_state read-auto)" || r.Kernel != "array_state=read-auto, layout=left-symmetric" {
		t.Errorf("read-auto: %+v", r)
	}
	s.write("block/md127/md/array_state", "active-idle")
	if r, _ = m.Read(context.Background(), ts[0]); r.Value != ts[0].Want {
		t.Errorf("active-idle: %q", r.Value)
	}
}

func TestMdadm_ReadDetectsUnexpectedBitmap(t *testing.T) {
	s := newMdSim(t)
	_, ts := build(t, s, mdLayout("raid1", "bitmap", "no"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	s.write("block/md127/md/bitmap/location", "+8")
	r, err := NewMdadm(s.env()).Read(context.Background(), ts[0])
	if err != nil || !strings.HasSuffix(r.Value, "· bitmap") || r.Value == ts[0].Want {
		t.Errorf("%+v %v", r, err)
	}
}

func TestMdadm_ReadWrongMember(t *testing.T) {
	s := newMdSim(t)
	_, ts := build(t, s, mdLayout("raid1", "bitmap", "no"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	s.remove("block/md127/md/dev-sdc")
	s.addDev("md127", "sdx", "in_sync")
	r, err := NewMdadm(s.env()).Read(context.Background(), ts[0])
	want := ts[0].Want + " (members differ: want sdb(data), sdc(data) got sdb(data), sdx(data))"
	if err != nil || r.Value != want {
		t.Errorf("\n got %q\nwant %q", r.Value, want)
	}
}

func TestMdadm_UndoStopsAndWipes(t *testing.T) {
	s := newMdSim(t)
	ds := []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)}
	rec, _ := build(t, s, mdLayout("raid1", "bitmap", "no"), ds)
	s.calls = nil
	if err := NewMdadm(s.env()).Undo(context.Background(), *rec); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mdadm --detail --export /dev/md127",
		"mdadm --stop /dev/md127",
		"mdadm --examine --export /dev/sdb", "mdadm --zero-superblock /dev/sdb", "mdadm --examine --export /dev/sdb",
		"mdadm --examine --export /dev/sdc", "mdadm --zero-superblock /dev/sdc", "mdadm --examine --export /dev/sdc",
	}
	if !slices.Equal(s.calls, want) {
		t.Errorf("\n got %q\nwant %q", s.calls, want)
	}
	if len(s.supers) != 0 {
		t.Errorf("superblocks left %v", s.supers)
	}
}

func TestMdadm_UndoFindsArrayByUUID(t *testing.T) {
	s := newMdSim(t)
	s.next = "md126"
	rec, _ := build(t, s, mdLayout("raid0", "chunk", "512K"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	if err := os.Remove(filepath.Join(s.dev, "md", "bbbench_1_1")); err != nil {
		t.Fatal(err)
	}
	s.write("block/md125/md/array_state", "clean")
	s.arrays["md125"] = "ffffffff:ffffffff:ffffffff:ffffffff"
	// MD_UUID upper case still matches.
	s.arrays["md126"] = strings.ToUpper(rec.UUID)
	s.calls = nil
	if err := NewMdadm(s.env()).Undo(context.Background(), *rec); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.calls, "mdadm --stop /dev/md126") || slices.Contains(s.calls, "mdadm --stop /dev/md125") {
		t.Errorf("%v", s.calls)
	}
}

func TestMdadm_UndoRefusesForeignMember(t *testing.T) {
	s := newMdSim(t)
	rec, _ := build(t, s, mdLayout("raid0", "chunk", "512K"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	s.addDev("md127", "sdx", "in_sync")
	s.calls = nil
	err := NewMdadm(s.env()).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "not touched") || !strings.Contains(err.Error(), "sdx") {
		t.Errorf("%v", err)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "--stop") || strings.Contains(c, "--zero-superblock") {
			t.Errorf("ran %s", c)
		}
	}
}

func TestMdadm_UndoArrayAlreadyStopped(t *testing.T) {
	s := newMdSim(t)
	ds := []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData), dsk("sdd", "S3", RoleData)}
	rec, _ := build(t, s, mdLayout("raid5", "chunk", "512K", "bitmap", "no"), ds)
	s.remove("block/md127")
	delete(s.arrays, "md127")
	if err := os.Remove(filepath.Join(s.dev, "md", "bbbench_1_1")); err != nil {
		t.Fatal(err)
	}
	delete(s.supers, "/dev/sdd")
	s.calls = nil
	if err := NewMdadm(s.env()).Undo(context.Background(), *rec); err != nil {
		t.Fatal(err)
	}
	var zeroed []string
	for _, c := range s.calls {
		if strings.Contains(c, "--stop") {
			t.Errorf("ran %s", c)
		}
		if p, ok := strings.CutPrefix(c, "mdadm --zero-superblock "); ok {
			zeroed = append(zeroed, p)
		}
	}
	if !slices.Equal(zeroed, []string{"/dev/sdb", "/dev/sdc"}) {
		t.Errorf("zeroed %v", zeroed)
	}
}

func TestMdadm_UndoSkipsForeignSuperblock(t *testing.T) {
	s := newMdSim(t)
	rec, _ := build(t, s, mdLayout("raid0", "chunk", "512K"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	s.supers["/dev/sdc"] = "aaaaaaaa:bbbbbbbb:cccccccc:dddddddd"
	s.calls = nil
	if err := NewMdadm(s.env()).Undo(context.Background(), *rec); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(s.calls, "mdadm --zero-superblock /dev/sdc") || !slices.Contains(s.calls, "mdadm --zero-superblock /dev/sdb") {
		t.Errorf("%v", s.calls)
	}
	if s.supers["/dev/sdc"] != "aaaaaaaa:bbbbbbbb:cccccccc:dddddddd" {
		t.Errorf("foreign superblock touched")
	}
}

func TestMdadm_UndoStopFails(t *testing.T) {
	s := newMdSim(t)
	rec, _ := build(t, s, mdLayout("raid0", "chunk", "512K"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	s.fail["mdadm --stop"] = exitErr("mdadm: Cannot get exclusive access to /dev/md127\n")
	s.calls = nil
	err := NewMdadm(s.env()).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "mdadm --stop") || !strings.Contains(err.Error(), "exclusive access") {
		t.Errorf("%v", err)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "--zero-superblock") || strings.Contains(c, "--examine") {
			t.Errorf("ran %s", c)
		}
	}
}

func TestMdadm_UndoStopLeavesArrayActive(t *testing.T) {
	s := newMdSim(t)
	rec, _ := build(t, s, mdLayout("raid0", "chunk", "512K"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	// --stop exits 0 but the array stays active.
	s.fail["mdadm --stop"] = errNoop
	env := s.env()
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := s.run(ctx, name, args...)
		if errors.Is(err, errNoop) {
			return nil, nil
		}
		return out, err
	}
	err := NewMdadm(env).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "still") {
		t.Errorf("%v", err)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "--zero-superblock") {
			t.Errorf("ran %s", c)
		}
	}
}

var errNoop = errors.New("noop")

func TestMdadm_UndoMissingMember(t *testing.T) {
	s := newMdSim(t)
	rec, _ := build(t, s, mdLayout("raid0", "chunk", "512K"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	s.remove("block/md127")
	delete(s.arrays, "md127")
	rec.Members[1].Path = ""
	err := NewMdadm(s.env()).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "member S/N S2 is not present; its superblock was not wiped") {
		t.Errorf("%v", err)
	}
	if !slices.Contains(s.calls, "mdadm --zero-superblock /dev/sdb") {
		t.Errorf("present member not wiped: %v", s.calls)
	}
}

func TestMdadm_UndoVerifiesWipe(t *testing.T) {
	s := newMdSim(t)
	rec, _ := build(t, s, mdLayout("raid0", "chunk", "512K"), []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData)})
	// --zero-superblock exits 0 but leaves the superblock.
	s.fail["mdadm --zero-superblock /dev/sdc"] = errNoop
	env := s.env()
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := s.run(ctx, name, args...)
		if errors.Is(err, errNoop) {
			return nil, nil
		}
		return out, err
	}
	err := NewMdadm(env).Undo(context.Background(), *rec)
	if err == nil || !strings.Contains(err.Error(), "/dev/sdc") || !strings.Contains(err.Error(), rec.UUID) {
		t.Errorf("%v", err)
	}
}

func TestMdadm_UndoCommand(t *testing.T) {
	u := ArrayUndo{Backend: "mdadm", Name: "bbbench_1_1", UUID: "x",
		Members: []Disk{dsk("sdb", "S1", RoleData), dsk("sdc", "S2", RoleData), dsk("sdd", "S3", RoleSpare)}}
	if got := NewMdadm(Env{}).UndoCommand(u); got != "mdadm --stop /dev/md/bbbench_1_1; mdadm --zero-superblock /dev/sdb /dev/sdc /dev/sdd" {
		t.Errorf("%q", got)
	}
}
