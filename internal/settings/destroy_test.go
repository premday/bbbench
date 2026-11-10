package settings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"bbbench/internal/inuse"
)

// fakeDestroyer is a fakeBackend that also lists and destroys existing
// configurations. Every call is appended to *log.
type fakeDestroyer struct {
	*fakeBackend
	existing    []Existing
	existingErr error
	destroyErr  error
	destroyed   []Existing
	onDestroy   func()
	log         *[]string
}

func (f *fakeDestroyer) Existing(context.Context) ([]Existing, error) {
	*f.log = append(*f.log, f.name+" existing")
	return f.existing, f.existingErr
}

func (f *fakeDestroyer) Destroy(_ context.Context, e Existing) error {
	*f.log = append(*f.log, f.name+" destroy "+e.ID)
	f.destroyed = append(f.destroyed, e)
	if f.onDestroy != nil {
		f.onDestroy()
	}
	return f.destroyErr
}

func (f *fakeDestroyer) DestroyCommand(e Existing) string {
	return "tear down " + e.ID + " by hand"
}

// md127 on sdb and sdc, owned node md127.
func md127() Existing {
	return Existing{Backend: "aa", ID: mdUUID, Name: "md127", Summary: "raid1",
		Disks: []Target{
			{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M"},
			{Name: "sdc", Path: "/dev/sdc", Serial: "S2", Model: "M"},
		},
		Stack: []string{"md127 (raid1, clean)"},
		Extra: map[string]string{ExtraOwnedNodes: "md127", ExtraKind: KindMDValue}}
}

// mdUUID is md127's uuid as mdadm prints it; udevUUID the same as udev does.
const (
	mdUUID   = "a1b2c3d4:00000000:11111111:22222222"
	udevUUID = "A1B2C3D4-0000-0000-1111-111122222222"
)

// heldBy is the in-use report of an assembled md127 on sdb, sdc.
func heldBy() map[string][]inuse.Cause {
	return map[string][]inuse.Cause{
		"sdb": {{Kind: inuse.KindHolder, Disk: "sdb", Via: "md127", Device: "md127", Text: "member of md127 (md RAID)"},
			{Kind: inuse.KindMD, Disk: "sdb", Via: udevUUID, Device: "sdb", Text: "has an md RAID superblock"}},
		"sdc": {{Kind: inuse.KindHolder, Disk: "sdc", Via: "md127", Device: "md127", Text: "member of md127 (md RAID)"}},
	}
}

func reportOf(causes map[string][]inuse.Cause) *inuse.Report {
	r := &inuse.Report{Busy: map[string][]string{}, Causes: causes}
	for d, cs := range causes {
		for _, c := range cs {
			r.Busy[d] = append(r.Busy[d], c.Text)
		}
	}
	return r
}

// sysHolders builds a sysfs tree with /block/<node>/holders/<h> entries; a
// key "<node>/<part>" is a partition of node.
func sysHolders(t *testing.T, holders map[string][]string) string {
	t.Helper()
	root := t.TempDir()
	for node, hs := range holders {
		dir := filepath.Join(root, "block", node, "holders")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(node, "/") {
			if err := os.WriteFile(filepath.Join(root, "block", node, "partition"), []byte("1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, h := range hs {
			if err := os.WriteFile(filepath.Join(dir, h), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

type destroyRig struct {
	s       LayoutSetting
	d       *fakeDestroyer
	log     []string
	reports []*inuse.Report // returned by InUse in turn; the last one repeats
	scanErr error
}

func newDestroyRig(t *testing.T, sys string, listed ...Existing) *destroyRig {
	t.Helper()
	r := &destroyRig{}
	r.d = &fakeDestroyer{fakeBackend: aaBackend(), existing: listed, log: &r.log}
	env := Env{
		Sys:      sys,
		LookPath: lookAll,
		Settle:   time.Second,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			r.log = append(r.log, name+" "+strings.Join(args, " "))
			return nil, nil
		},
		InUse: func() (*inuse.Report, error) {
			r.log = append(r.log, "inuse")
			if r.scanErr != nil {
				return nil, r.scanErr
			}
			rep := r.reports[0]
			if len(r.reports) > 1 {
				r.reports = r.reports[1:]
			}
			return rep, nil
		},
	}
	r.s = NewLayout(env, r.d, bbBackend())
	return r
}

func TestLayout_ExistingAggregatesProviders(t *testing.T) {
	var log []string
	good := &fakeDestroyer{fakeBackend: aaBackend(), existing: []Existing{md127()}, log: &log}
	badB := aaBackend()
	badB.name = "cc"
	bad := &fakeDestroyer{fakeBackend: badB, existingErr: errors.New("tool crashed"), log: &log}
	offB := aaBackend()
	offB.name, offB.avail, offB.why = "dd", false, "tool d missing"
	off := &fakeDestroyer{fakeBackend: offB, existing: []Existing{{Backend: "dd", ID: "x"}}, log: &log}
	// bbBackend is not a Destroyer: it is not asked.
	s := NewLayout(Env{InUse: freeDisks}, bad, bbBackend(), off, good)
	es, errs := s.Existing(context.Background())
	if len(es) != 1 || es[0].ID != mdUUID {
		t.Errorf("existing %+v", es)
	}
	if len(errs) != 2 || errs[0].Error() != "cc: tool crashed" || errs[1].Error() != "dd: not available: tool d missing" {
		t.Errorf("errors %v", errs)
	}
	if slices.Contains(log, "dd existing") {
		t.Error("an unavailable provider was asked")
	}
	// An entry that names another backend is not passed on as this one's.
	good.existing = []Existing{{Backend: "zz", ID: "x", Disks: md127().Disks}}
	if es, errs := s.Existing(context.Background()); len(es) != 0 || len(errs) != 3 {
		t.Errorf("foreign backend entry: %+v %v", es, errs)
	}
}

func TestLayout_DestroySuccessOrder(t *testing.T) {
	sys := sysHolders(t, map[string][]string{"md127": nil})
	r := newDestroyRig(t, sys, md127())
	r.reports = []*inuse.Report{reportOf(heldBy()), reportOf(nil)}
	if bs, err := r.s.DestroyBlockers(context.Background(), md127()); err != nil || len(bs) != 0 {
		t.Fatalf("blockers %v %v", bs, err)
	}
	r.log = nil
	r.reports = []*inuse.Report{reportOf(heldBy()), reportOf(nil)}
	// md127 is gone from sysfs once destroyed.
	r.d.onDestroy = func() { os.RemoveAll(filepath.Join(sys, "block", "md127")) }
	if err := r.s.Destroy(context.Background(), md127()); err != nil {
		t.Fatal(err)
	}
	want := []string{"aa existing", "inuse", "aa destroy " + mdUUID, "udevadm settle --timeout=1", "inuse"}
	if !slices.Equal(r.log, want) {
		t.Errorf("order\n got %v\nwant %v", r.log, want)
	}
}

func vgData() Existing {
	return Existing{Backend: "aa", ID: "data", Name: "data",
		Disks: []Target{{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M"}},
		Stack: []string{"lv data/home", "vg data", "pv /dev/sdb"},
		Extra: map[string]string{ExtraOwnedVG: "data", ExtraOwnedNodes: "dm-0,dm-1"}}
}

func orphanSdb1() Existing {
	return Existing{Backend: "aa", ID: "pv-sdb1", Name: "/dev/sdb1",
		Disks: []Target{{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M"}},
		Stack: []string{"pv /dev/sdb1"},
		Extra: map[string]string{ExtraOrphanPV: "yes", ExtraOwnedNodes: "sdb1"}}
}

func TestLayout_DestroyOwnedVGAndOrphanPV(t *testing.T) {
	ctx := context.Background()
	vg := vgData()
	sys := sysHolders(t, map[string][]string{"dm-0": nil, "dm-1": {"dm-0"}})
	r := newDestroyRig(t, sys, vg)
	r.reports = []*inuse.Report{reportOf(map[string][]inuse.Cause{"sdb": {
		{Kind: inuse.KindHolder, Disk: "sdb", Via: "dm-0", Device: "dm-0", Text: "used by dm-0"},
		{Kind: inuse.KindPV, Disk: "sdb", Via: "data", Device: "sdb", Text: "is an LVM physical volume of volume group data"},
	}})}
	if bs, err := r.s.DestroyBlockers(ctx, vg); err != nil || len(bs) != 0 {
		t.Errorf("vg blockers %v %v", bs, err)
	}

	orphan := orphanSdb1()
	pv := reportOf(map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindPV, Disk: "sdb", Device: "sdb1", Text: "is an LVM physical volume (no volume group)"}}})
	r = newDestroyRig(t, t.TempDir(), orphan)
	r.reports = []*inuse.Report{pv}
	if bs, err := r.s.DestroyBlockers(ctx, orphan); err != nil || len(bs) != 0 {
		t.Errorf("orphan blockers %v %v", bs, err)
	}
	// Another orphan PV on sdb2 is not the selected one.
	two := reportOf(map[string][]inuse.Cause{"sdb": {pv.Causes["sdb"][0],
		{Kind: inuse.KindPV, Disk: "sdb", Device: "sdb2", Text: "is an LVM physical volume (no volume group)"}}})
	r = newDestroyRig(t, t.TempDir(), orphan)
	r.reports = []*inuse.Report{two}
	if bs, _ := r.s.DestroyBlockers(ctx, orphan); len(bs) != 1 {
		t.Errorf("second orphan pv: %v", bs)
	}
	// The same orphan PV cause on a VG entry is not owned: it may be any PV.
	r = newDestroyRig(t, sysHolders(t, nil), vg)
	r.reports = []*inuse.Report{pv}
	if bs, _ := r.s.DestroyBlockers(ctx, vg); len(bs) != 1 {
		t.Errorf("orphan cause on a vg entry: %v", bs)
	}
}

// A VG named like a node, or nodes named like a VG, own nothing of the other
// namespace.
func TestLayout_DestroyOwnedNamespaces(t *testing.T) {
	ctx := context.Background()
	vg := vgData()
	vg.ID, vg.Name = "md127", "md127"
	vg.Extra = map[string]string{ExtraOwnedVG: "md127", ExtraOwnedNodes: "dm-0"}
	r := newDestroyRig(t, sysHolders(t, nil), vg)
	r.reports = []*inuse.Report{reportOf(map[string][]inuse.Cause{"sdb": {
		{Kind: inuse.KindHolder, Disk: "sdb", Via: "md127", Device: "md127", Text: "member of md127 (md RAID)"}}})}
	if bs, _ := r.s.DestroyBlockers(ctx, vg); len(bs) != 1 {
		t.Errorf("vg named md127 owns the md: %v", bs)
	}
	vg = vgData()
	vg.Extra = map[string]string{ExtraOwnedVG: "data", ExtraOwnedNodes: "dm-0,other"}
	r = newDestroyRig(t, sysHolders(t, nil), vg)
	r.reports = []*inuse.Report{reportOf(map[string][]inuse.Cause{"sdb": {
		{Kind: inuse.KindPV, Disk: "sdb", Via: "other", Device: "sdb", Text: "is an LVM physical volume of volume group other"}}})}
	if bs, _ := r.s.DestroyBlockers(ctx, vg); len(bs) != 1 {
		t.Errorf("node named other owns the vg other: %v", bs)
	}
}

func TestLayout_DestroyMDByUUID(t *testing.T) {
	ctx := context.Background()
	md := func(via, device string) map[string][]inuse.Cause {
		m := heldBy()
		m["sdc"] = append(m["sdc"], inuse.Cause{Kind: inuse.KindMD, Disk: "sdc", Via: via, Device: device, Text: "has an md RAID superblock"})
		return m
	}
	for name, tc := range map[string]struct {
		causes map[string][]inuse.Cause
		ok     bool
	}{
		"same uuid, other spelling":       {md(udevUUID, "sdc"), true},
		"other uuid":                      {md("A1B2C3D4-0000-0000-1111-111122222223", "sdc"), false},
		"no uuid":                         {md("", "sdc"), false},
		"foreign md on another partition": {md("ffff0000-0000-0000-0000-000000000000", "sdc2"), false},
	} {
		r := newDestroyRig(t, sysHolders(t, map[string][]string{"md127": nil}), md127())
		r.reports = []*inuse.Report{reportOf(tc.causes)}
		bs, err := r.s.DestroyBlockers(ctx, md127())
		if err != nil || (len(bs) == 0) != tc.ok {
			t.Errorf("%s: %v %v", name, bs, err)
		}
	}
	// An md superblock on a VG entry's disk is never the VG's.
	vg := vgData()
	r := newDestroyRig(t, sysHolders(t, nil), vg)
	r.reports = []*inuse.Report{reportOf(map[string][]inuse.Cause{"sdb": {
		{Kind: inuse.KindMD, Disk: "sdb", Via: "data", Device: "sdb2", Text: "has an md RAID superblock"}}})}
	if bs, _ := r.s.DestroyBlockers(ctx, vg); len(bs) != 1 {
		t.Errorf("md on a vg entry: %v", bs)
	}
}

// TestLayout_DestroyRefusals runs every refusal through both DestroyBlockers
// and Destroy: they must agree, and the provider is never called.
func TestLayout_DestroyRefusals(t *testing.T) {
	plus := func(extra map[string][]inuse.Cause) map[string][]inuse.Cause {
		m := heldBy()
		for d, cs := range extra {
			m[d] = append(m[d], cs...)
		}
		return m
	}
	changed := func(f func(*Existing)) Existing {
		e := md127()
		f(&e)
		return e
	}
	cases := []struct {
		name    string
		shown   *Existing // what the user saw; default md127()
		listed  []Existing
		holders map[string][]string
		causes  map[string][]inuse.Cause
		all     string
		scanErr error
		listErr error
		isErr   bool   // a failure to check, not a blocker
		want    string // in the blockers or the error
	}{
		{name: "mount on a member partition", causes: plus(map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindMount, Disk: "sdb", Via: "/data", Device: "sdb2", Text: "mounted at /data (sdb2)"}}}),
			want: "disk sdb: mounted at /data (sdb2)"},
		{name: "mount on the selected md itself", causes: plus(map[string][]inuse.Cause{"sdc": {{Kind: inuse.KindMount, Disk: "sdc", Via: "/mnt", Device: "md127", Text: "mounted at /mnt (md127)"}}}),
			want: "disk sdc: mounted at /mnt (md127)"},
		{name: "swap", causes: plus(map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindSwap, Disk: "sdb", Via: "/dev/md127", Device: "md127", Text: "active swap (/dev/md127)"}}}),
			want: "active swap"},
		{name: "bbbench path", causes: plus(map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindPath, Disk: "sdb", Via: "/out", Device: "md127", Text: "holds the output directory (/out)"}}}),
			want: "holds the output directory"},
		{name: "foreign md on another partition", causes: plus(map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindHolder, Disk: "sdb", Via: "md126", Device: "md126", Text: "member of md126 (md RAID)"}}}),
			want: "disk sdb: member of md126"},
		{name: "foreign signature", causes: plus(map[string][]inuse.Cause{"sdc": {{Kind: inuse.KindSignature, Disk: "sdc", Device: "sdc", Text: "is LUKS encrypted"}}}),
			want: "is LUKS encrypted"},
		{name: "signature on a partition of the md", causes: plus(map[string][]inuse.Cause{"md127": {{Kind: inuse.KindSignature, Disk: "md127", Device: "md127p1", Text: "is LUKS encrypted"}}}),
			holders: map[string][]string{"md127": nil, "md127/md127p1": nil}, want: "disk md127: is LUKS encrypted"},
		{name: "pv of another vg", causes: plus(map[string][]inuse.Cause{"sdc": {{Kind: inuse.KindPV, Disk: "sdc", Via: "other", Text: "is an LVM physical volume of volume group other"}}}),
			want: "volume group other"},
		{name: "orphan pv cause on an md entry", causes: plus(map[string][]inuse.Cause{"sdc": {{Kind: inuse.KindPV, Disk: "sdc", Text: "is an LVM physical volume"}}}),
			want: "is an LVM physical volume"},
		{name: "stacked foreign holder without mount", holders: map[string][]string{"md127": {"dm-9"}},
			want: "stacked: dm-9 sits on md127"},
		{name: "holder on a partition of the md", holders: map[string][]string{"md127": nil, "md127/md127p1": {"dm-7"}},
			want: "stacked: dm-7 sits on md127p1"},
		{name: "holder above an owned holder", shown: ptr(changed(func(e *Existing) { e.Extra[ExtraOwnedNodes] = "md127,dm-0" })),
			listed:  []Existing{changed(func(e *Existing) { e.Extra[ExtraOwnedNodes] = "md127,dm-0" })},
			holders: map[string][]string{"md127": {"dm-0"}, "dm-0": {"dm-5"}}, want: "stacked: dm-5 sits on dm-0"},
		{name: "holders unreadable", holders: map[string][]string{"md127/md127p1": nil}, isErr: true, want: "cannot read what sits on md127"},
		{name: "all set", all: "the root disk cannot be determined", want: "the root disk cannot be determined"},
		{name: "scan error", scanErr: errors.New("no /proc"), isErr: true, want: "no /proc"},
		{name: "list error", listErr: errors.New("mdadm crashed"), isErr: true, want: "mdadm crashed"},
		{name: "id gone", listed: []Existing{changed(func(e *Existing) { e.ID = "uuid-2" })}, want: "no longer"},
		{name: "serial changed", listed: []Existing{changed(func(e *Existing) { e.Disks[1].Serial = "S9" })}, want: "changed"},
		{name: "model changed", listed: []Existing{changed(func(e *Existing) { e.Disks[0].Model = "N" })}, want: "changed"},
		{name: "disk added", listed: []Existing{changed(func(e *Existing) {
			e.Disks = append(e.Disks, Target{Name: "sdd", Path: "/dev/sdd", Serial: "S3", Model: "M"})
		})}, want: "changed"},
		{name: "owned nodes changed", listed: []Existing{changed(func(e *Existing) { e.Extra[ExtraOwnedNodes] = "md127,dm-4" })}, want: "changed"},
		{name: "caller widened the owned nodes", shown: ptr(changed(func(e *Existing) { e.Extra[ExtraOwnedNodes] = "md127,md126" })),
			want: "changed"},
		{name: "owned vg changed", listed: []Existing{changed(func(e *Existing) { e.Extra[ExtraOwnedVG] = "data" })}, want: "changed"},
		{name: "kind changed", listed: []Existing{changed(func(e *Existing) { delete(e.Extra, ExtraKind) })}, want: "changed"},
		{name: "stack changed", listed: []Existing{changed(func(e *Existing) { e.Stack = append([]string{"lv data/new"}, e.Stack...) })}, want: "changed"},
		{name: "listed twice", listed: []Existing{md127(), md127()}, want: "listed twice"},
		{name: "no serial", shown: ptr(changed(func(e *Existing) { e.Disks[0].Serial = "" })),
			listed: []Existing{changed(func(e *Existing) { e.Disks[0].Serial = "" })}, want: "no serial"},
		{name: "no disks", shown: ptr(changed(func(e *Existing) { e.Disks = nil })), want: "lists no disks"},
		{name: "bad owned node", shown: ptr(changed(func(e *Existing) { e.Extra[ExtraOwnedNodes] = "../md127" })), want: "bad owned name"},
		{name: "unknown backend", shown: ptr(changed(func(e *Existing) { e.Backend = "zz" })), want: "unknown provider"},
		{name: "not a destroyer", shown: ptr(changed(func(e *Existing) { e.Backend = "bb" })), want: "cannot destroy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shown := md127()
			if tc.shown != nil {
				shown = *tc.shown
			}
			listed := tc.listed
			if listed == nil {
				listed = []Existing{md127()}
			}
			causes := tc.causes
			if causes == nil {
				causes = heldBy()
			}
			holders := tc.holders
			if holders == nil {
				holders = map[string][]string{"md127": nil}
			}
			rep := reportOf(causes)
			rep.All = tc.all
			run := func() *destroyRig {
				r := newDestroyRig(t, sysHolders(t, holders), listed...)
				r.d.existingErr = tc.listErr
				r.scanErr = tc.scanErr
				r.reports = []*inuse.Report{rep}
				return r
			}

			r := run()
			bs, err := r.s.DestroyBlockers(context.Background(), shown)
			if (err != nil) != tc.isErr {
				t.Errorf("DestroyBlockers error %v, blockers %v; want error: %v", err, bs, tc.isErr)
			}
			got := strings.Join(bs, "\n")
			if err != nil {
				got += "\n" + err.Error()
			}
			if len(bs) == 0 && err == nil {
				t.Fatal("DestroyBlockers allowed it")
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("DestroyBlockers %q, want %q", got, tc.want)
			}

			r = run()
			err = r.s.Destroy(context.Background(), shown)
			if err == nil {
				t.Fatal("Destroy allowed it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Destroy %q, want %q", err, tc.want)
			}
			if len(r.d.destroyed) != 0 {
				t.Error("the provider was called")
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestLayout_DestroyProviderError(t *testing.T) {
	r := newDestroyRig(t, sysHolders(t, map[string][]string{"md127": nil}), md127())
	r.reports = []*inuse.Report{reportOf(heldBy())}
	r.d.destroyErr = errors.New("mdadm --stop: device busy")
	err := r.s.Destroy(context.Background(), md127())
	if err == nil || !strings.Contains(err.Error(), "device busy") ||
		!strings.Contains(err.Error(), "to finish by hand: tear down "+mdUUID+" by hand") {
		t.Errorf("error %v", err)
	}
}

func TestLayout_DestroyContext(t *testing.T) {
	// Cancelled before: the provider is not called.
	r := newDestroyRig(t, sysHolders(t, map[string][]string{"md127": nil}), md127())
	r.reports = []*inuse.Report{reportOf(heldBy())}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.s.Destroy(ctx, md127()); err == nil || len(r.d.destroyed) != 0 {
		t.Errorf("cancelled: %v, destroyed %d", err, len(r.d.destroyed))
	}

	// Cancelled while the provider runs: settle and re-scan still run, with
	// a live context, and report what is left.
	sys := sysHolders(t, map[string][]string{"md127": nil})
	r = newDestroyRig(t, sys, md127())
	r.reports = []*inuse.Report{reportOf(heldBy())}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	r.d.onDestroy = cancel
	var settleErr error = errors.New("not run")
	l := r.s.(*layout)
	inner := l.env.Run
	l.env.Run = func(c context.Context, name string, args ...string) ([]byte, error) {
		settleErr = c.Err()
		return inner(c, name, args...)
	}
	err := r.s.Destroy(ctx, md127())
	if settleErr != nil {
		t.Errorf("settle context: %v", settleErr)
	}
	if err == nil || !strings.Contains(err.Error(), "member of md127") || !strings.Contains(err.Error(), "to finish by hand") {
		t.Errorf("left over after cancel: %v", err)
	}
	if n := strings.Count(strings.Join(r.log, "|"), "inuse"); n != 2 {
		t.Errorf("scans %d: %v", n, r.log)
	}
}

func TestLayout_DestroyPostCheck(t *testing.T) {
	ctx := context.Background()
	gone := func(sys string) func() { return func() { os.RemoveAll(filepath.Join(sys, "block", "md127")) } }
	type post struct {
		name    string
		e       Existing
		before  map[string][]inuse.Cause
		after   map[string][]inuse.Cause
		keepSys bool   // the owned node is still in sysfs
		want    string // "" = success
	}
	vgBefore := map[string][]inuse.Cause{"sdb": {
		{Kind: inuse.KindHolder, Disk: "sdb", Via: "dm-0", Device: "dm-0", Text: "used by dm-0"},
		{Kind: inuse.KindPV, Disk: "sdb", Via: "data", Device: "sdb", Text: "is an LVM physical volume of volume group data"}}}
	orphanBefore := map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindPV, Disk: "sdb", Device: "sdb1", Text: "is an LVM physical volume (no volume group)"}}}
	for _, tc := range []post{
		{name: "gone", e: md127(), before: heldBy()},
		{name: "still held", e: md127(), before: heldBy(), after: map[string][]inuse.Cause{"sdc": heldBy()["sdc"]}, want: "sdc: member of md127"},
		{name: "superblock left", e: md127(), before: heldBy(), after: map[string][]inuse.Cause{"sdb": {heldBy()["sdb"][1]}}, want: "sdb: has an md RAID superblock"},
		{name: "superblock without uuid left", e: md127(), before: heldBy(),
			after: map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindMD, Disk: "sdb", Device: "sdb", Text: "has an md RAID superblock"}}}, want: "has an md RAID superblock"},
		{name: "node still there", e: md127(), before: heldBy(), keepSys: true, want: "md127 still exists"},
		{name: "vg label left", e: vgData(), before: vgBefore,
			after: map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindPV, Disk: "sdb", Device: "sdb", Text: "is an LVM physical volume"}}}, want: "sdb: is an LVM physical volume"},
		{name: "vg still there", e: vgData(), before: vgBefore, after: vgBefore, want: "volume group data"},
		{name: "vg gone", e: vgData(), before: vgBefore},
		{name: "orphan pv left", e: orphanSdb1(), before: orphanBefore, after: orphanBefore, want: "no volume group"},
		{name: "orphan pv gone", e: orphanSdb1(), before: orphanBefore},
		// Something never owned appearing afterwards is not the teardown's failure.
		{name: "unowned leftover", e: md127(), before: heldBy(),
			after: map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindSignature, Disk: "sdb", Device: "sdb", Text: "has a ZFS pool label"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sys := sysHolders(t, map[string][]string{"md127": nil})
			r := newDestroyRig(t, sys, tc.e)
			r.reports = []*inuse.Report{reportOf(tc.before), reportOf(tc.after)}
			if !tc.keepSys {
				r.d.onDestroy = gone(sys)
			}
			err := r.s.Destroy(ctx, tc.e)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "to finish by hand: tear down "+tc.e.ID+" by hand") {
				t.Errorf("error %v, want %q", err, tc.want)
			}
		})
	}

	// The re-scan fails: not verified.
	r := newDestroyRig(t, t.TempDir(), md127())
	r.reports = []*inuse.Report{reportOf(heldBy())}
	scans := 0
	inner := r.s.(*layout).env.InUse
	r.s.(*layout).env.InUse = func() (*inuse.Report, error) {
		if scans++; scans > 1 {
			return nil, errors.New("scan broke")
		}
		return inner()
	}
	if err := r.s.Destroy(ctx, md127()); err == nil || !strings.Contains(err.Error(), "scan broke") ||
		!strings.Contains(err.Error(), "to finish by hand") {
		t.Errorf("scan error: %v", err)
	}
	// The re-scan cannot judge any disk: not verified.
	r = newDestroyRig(t, t.TempDir(), md127())
	all := reportOf(nil)
	all.All = "root unknown"
	r.reports = []*inuse.Report{reportOf(heldBy()), all}
	if err := r.s.Destroy(ctx, md127()); err == nil || !strings.Contains(err.Error(), "root unknown") {
		t.Errorf("all: %v", err)
	}
}
