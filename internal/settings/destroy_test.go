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
	log         *[]string
}

func (f *fakeDestroyer) Existing(context.Context) ([]Existing, error) {
	*f.log = append(*f.log, f.name+" existing")
	return f.existing, f.existingErr
}

func (f *fakeDestroyer) Destroy(_ context.Context, e Existing) error {
	*f.log = append(*f.log, f.name+" destroy "+e.ID)
	f.destroyed = append(f.destroyed, e)
	return f.destroyErr
}

func (f *fakeDestroyer) DestroyCommand(e Existing) string {
	return "tear down " + e.ID + " by hand"
}

// md127 on sdb and sdc, owned node md127.
func md127() Existing {
	return Existing{Backend: "aa", ID: "uuid-1", Name: "md127", Summary: "raid1",
		Disks: []Target{
			{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M"},
			{Name: "sdc", Path: "/dev/sdc", Serial: "S2", Model: "M"},
		},
		Stack: []string{"md127 (raid1, clean)"},
		Extra: map[string]string{ExtraOwned: "md127"}}
}

// heldBy is the in-use report of an assembled md127 on sdb, sdc.
func heldBy() map[string][]inuse.Cause {
	return map[string][]inuse.Cause{
		"sdb": {{Kind: inuse.KindHolder, Disk: "sdb", Via: "md127", Device: "md127", Text: "member of md127 (md RAID)"},
			{Kind: inuse.KindMD, Disk: "sdb", Text: "has an md RAID superblock"}},
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

// sysHolders builds a sysfs tree with /block/<node>/holders/<h> entries.
func sysHolders(t *testing.T, holders map[string][]string) string {
	t.Helper()
	root := t.TempDir()
	for node, hs := range holders {
		dir := filepath.Join(root, "block", node, "holders")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
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
	// bbBackend is not a Destroyer: it is not asked.
	s := NewLayout(Env{InUse: freeDisks}, bad, bbBackend(), good)
	es, errs := s.Existing(context.Background())
	if len(es) != 1 || es[0].ID != "uuid-1" {
		t.Errorf("existing %+v", es)
	}
	if len(errs) != 1 || errs[0].Error() != "cc: tool crashed" {
		t.Errorf("errors %v", errs)
	}
	// An entry that names another backend is not passed on as this one's.
	good.existing = []Existing{{Backend: "zz", ID: "x", Disks: md127().Disks}}
	if es, errs := s.Existing(context.Background()); len(es) != 0 || len(errs) != 2 {
		t.Errorf("foreign backend entry: %+v %v", es, errs)
	}
}

func TestLayout_DestroySuccessOrder(t *testing.T) {
	sys := sysHolders(t, map[string][]string{"md127": nil})
	r := newDestroyRig(t, sys, md127())
	r.reports = []*inuse.Report{reportOf(heldBy()), reportOf(nil)}
	if bs, err := r.s.(*layout).DestroyBlockers(context.Background(), md127()); err != nil || len(bs) != 0 {
		t.Fatalf("blockers %v %v", bs, err)
	}
	r.log = nil
	r.reports = []*inuse.Report{reportOf(heldBy()), reportOf(nil)}
	if err := r.s.Destroy(context.Background(), md127()); err != nil {
		t.Fatal(err)
	}
	want := []string{"aa existing", "inuse", "aa destroy uuid-1", "udevadm settle --timeout=1", "inuse"}
	if !slices.Equal(r.log, want) {
		t.Errorf("order\n got %v\nwant %v", r.log, want)
	}
}

func TestLayout_DestroyOwnedVGAndOrphanPV(t *testing.T) {
	vg := Existing{Backend: "aa", ID: "data", Name: "data",
		Disks: []Target{{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M"}},
		Extra: map[string]string{ExtraOwned: "data,dm-0,dm-1"}}
	sys := sysHolders(t, map[string][]string{"dm-0": nil, "dm-1": {"dm-0"}})
	r := newDestroyRig(t, sys, vg)
	r.reports = []*inuse.Report{reportOf(map[string][]inuse.Cause{"sdb": {
		{Kind: inuse.KindHolder, Disk: "sdb", Via: "dm-0", Device: "dm-0", Text: "used by dm-0"},
		{Kind: inuse.KindPV, Disk: "sdb", Via: "data", Text: "is an LVM physical volume of volume group data"},
	}})}
	if bs, err := r.s.(*layout).DestroyBlockers(context.Background(), vg); err != nil || len(bs) != 0 {
		t.Errorf("vg blockers %v %v", bs, err)
	}

	orphan := Existing{Backend: "aa", ID: "pv-sdb", Name: "/dev/sdb",
		Disks: []Target{{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M"}},
		Extra: map[string]string{ExtraOrphanPV: "yes"}}
	r = newDestroyRig(t, t.TempDir(), orphan)
	pv := reportOf(map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindPV, Disk: "sdb", Text: "is an LVM physical volume (no volume group)"}}})
	r.reports = []*inuse.Report{pv}
	if bs, err := r.s.(*layout).DestroyBlockers(context.Background(), orphan); err != nil || len(bs) != 0 {
		t.Errorf("orphan blockers %v %v", bs, err)
	}
	// The same orphan PV cause on a VG entry is not owned: it may be any PV.
	r = newDestroyRig(t, t.TempDir(), vg)
	r.reports = []*inuse.Report{pv}
	if bs, _ := r.s.(*layout).DestroyBlockers(context.Background(), vg); len(bs) != 1 {
		t.Errorf("orphan cause on a vg entry: %v", bs)
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
		listed  []Existing
		holders map[string][]string
		causes  map[string][]inuse.Cause
		all     string
		scanErr error
		listErr error
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
		{name: "foreign signature", causes: plus(map[string][]inuse.Cause{"sdc": {{Kind: inuse.KindSignature, Disk: "sdc", Text: "is LUKS encrypted"}}}),
			want: "is LUKS encrypted"},
		{name: "pv of another vg", causes: plus(map[string][]inuse.Cause{"sdc": {{Kind: inuse.KindPV, Disk: "sdc", Via: "other", Text: "is an LVM physical volume of volume group other"}}}),
			want: "volume group other"},
		{name: "orphan pv cause on an md entry", causes: plus(map[string][]inuse.Cause{"sdc": {{Kind: inuse.KindPV, Disk: "sdc", Text: "is an LVM physical volume"}}}),
			want: "is an LVM physical volume"},
		{name: "stacked foreign holder without mount", holders: map[string][]string{"md127": {"dm-9"}},
			want: "stacked: dm-9 sits on md127"},
		{name: "all set", all: "the root disk cannot be determined", want: "the root disk cannot be determined"},
		{name: "scan error", scanErr: errors.New("no /proc"), want: "no /proc"},
		{name: "list error", listErr: errors.New("mdadm crashed"), want: "mdadm crashed"},
		{name: "id gone", listed: []Existing{changed(func(e *Existing) { e.ID = "uuid-2" })}, want: "no longer"},
		{name: "serial changed", listed: []Existing{changed(func(e *Existing) { e.Disks[1].Serial = "S9" })}, want: "changed"},
		{name: "model changed", listed: []Existing{changed(func(e *Existing) { e.Disks[0].Model = "N" })}, want: "changed"},
		{name: "disk added", listed: []Existing{changed(func(e *Existing) {
			e.Disks = append(e.Disks, Target{Name: "sdd", Path: "/dev/sdd", Serial: "S3", Model: "M"})
		})}, want: "changed"},
		{name: "owned set changed", listed: []Existing{changed(func(e *Existing) { e.Extra = map[string]string{ExtraOwned: "md127,dm-4"} })}, want: "changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			bs, err := r.s.(*layout).DestroyBlockers(context.Background(), md127())
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
			err = r.s.Destroy(context.Background(), md127())
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

func TestLayout_DestroyRefusesBadRequests(t *testing.T) {
	ctx := context.Background()
	r := newDestroyRig(t, t.TempDir(), md127())
	r.reports = []*inuse.Report{reportOf(heldBy())}
	for name, e := range map[string]Existing{
		"unknown backend": {Backend: "zz", ID: "uuid-1", Disks: md127().Disks},
		"not a destroyer": {Backend: "bb", ID: "uuid-1", Disks: md127().Disks},
		"no disks":        {Backend: "aa", ID: "uuid-1"},
		"no serial": func() Existing {
			e := md127()
			e.Disks[0].Serial = ""
			return e
		}(),
		"bad owned node": func() Existing {
			e := md127()
			e.Extra = map[string]string{ExtraOwned: "../md127"}
			return e
		}(),
	} {
		if err := r.s.Destroy(ctx, e); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
	if len(r.d.destroyed) != 0 {
		t.Error("the provider was called")
	}
}

func TestLayout_DestroyProviderError(t *testing.T) {
	r := newDestroyRig(t, t.TempDir(), md127())
	r.reports = []*inuse.Report{reportOf(heldBy())}
	r.d.destroyErr = errors.New("mdadm --stop: device busy")
	err := r.s.Destroy(context.Background(), md127())
	if err == nil || !strings.Contains(err.Error(), "device busy") ||
		!strings.Contains(err.Error(), "to finish by hand: tear down uuid-1 by hand") {
		t.Errorf("error %v", err)
	}
}

func TestLayout_DestroyPostCheck(t *testing.T) {
	ctx := context.Background()
	// Still held after the provider says it is done.
	r := newDestroyRig(t, t.TempDir(), md127())
	r.reports = []*inuse.Report{reportOf(heldBy()), reportOf(map[string][]inuse.Cause{"sdc": heldBy()["sdc"]})}
	err := r.s.Destroy(ctx, md127())
	if err == nil || !strings.Contains(err.Error(), "sdc: member of md127") ||
		!strings.Contains(err.Error(), "to finish by hand: tear down uuid-1 by hand") {
		t.Errorf("still held: %v", err)
	}
	// The re-scan fails: not verified.
	r = newDestroyRig(t, t.TempDir(), md127())
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
	// Something not owned appearing afterwards is not the teardown's failure.
	r = newDestroyRig(t, t.TempDir(), md127())
	r.reports = []*inuse.Report{reportOf(heldBy()), reportOf(map[string][]inuse.Cause{"sdb": {{Kind: inuse.KindSignature, Disk: "sdb", Text: "has a ZFS pool label"}}})}
	if err := r.s.Destroy(ctx, md127()); err != nil {
		t.Errorf("unowned leftover: %v", err)
	}
}
