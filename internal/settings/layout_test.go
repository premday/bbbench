package settings

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"bbbench/internal/inuse"
)

// fakeBackend is a scripted Backend recording the calls it receives.
type fakeBackend struct {
	name     string
	levels   []string
	roles    map[string][]Role
	options  map[string][]Option
	avail    bool
	why      string
	validate error  // returned by Validate
	badLabel string // when set, Label returns it

	prepared   *ArrayUndo
	prepareErr error
	targets    []BenchTarget
	applyErr   error
	reading    string
	undoErr    error
	steadyErr  error

	calls   []string
	names   []string      // names passed to Prepare
	steady  []BenchTarget // targets passed to WaitSteady
	applied *ArrayUndo    // record passed to Apply
}

func (f *fakeBackend) Name() string                  { return f.name }
func (f *fakeBackend) Available() (bool, string)     { return f.avail, f.why }
func (f *fakeBackend) Levels() []string              { return f.levels }
func (f *fakeBackend) Roles(level string) []Role     { return f.roles[level] }
func (f *fakeBackend) Validate(Layout, []Disk) error { return f.validate }

func (f *fakeBackend) Options(level string, _ int) []Option { return f.options[level] }

// Label is "<name> <level> · N disks[ · key=value...]", options sorted.
func (f *fakeBackend) Label(l Layout, disks []Disk) string {
	if f.badLabel != "" {
		return f.badLabel
	}
	s := fmt.Sprintf("%s %s · %d disks", f.name, l.Level, len(disks))
	var keys []string
	for k := range l.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s += " · " + k + "=" + l.Options[k]
	}
	return s
}

func (f *fakeBackend) Parse(value string) (Layout, error) {
	parts := strings.Split(value, " · ")
	w := strings.Fields(parts[0])
	if len(w) != 2 || w[0] != f.name {
		return Layout{}, errors.New("bad value " + value)
	}
	l := Layout{Backend: f.name, Level: w[1], Options: map[string]string{}}
	for _, p := range parts[1:] {
		if k, v, ok := strings.Cut(p, "="); ok {
			l.Options[k] = v
		}
	}
	return l, nil
}

func (f *fakeBackend) Prepare(_ context.Context, _ Variant, _ Layout, name string) (*ArrayUndo, error) {
	f.calls = append(f.calls, "prepare")
	f.names = append(f.names, name)
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	if f.prepared != nil {
		return f.prepared, nil
	}
	return &ArrayUndo{Name: name}, nil
}

func (f *fakeBackend) Apply(_ context.Context, _ Variant, _ Layout, rec *ArrayUndo) ([]BenchTarget, error) {
	f.calls = append(f.calls, "apply")
	f.applied = rec
	return append([]BenchTarget(nil), f.targets...), f.applyErr
}

func (f *fakeBackend) WaitSteady(_ context.Context, ts []BenchTarget, _ func(float64)) error {
	f.calls = append(f.calls, "steady")
	f.steady = append(f.steady, ts...)
	return f.steadyErr
}

func (f *fakeBackend) Read(context.Context, BenchTarget) (Reading, error) {
	f.calls = append(f.calls, "read")
	return Reading{Value: f.reading, At: time.Now()}, nil
}

func (f *fakeBackend) Undo(_ context.Context, u ArrayUndo) error {
	f.calls = append(f.calls, "undo "+u.Name)
	return f.undoErr
}

func (f *fakeBackend) UndoCommand(u ArrayUndo) string { return f.name + " undo " + u.Name }

var chunkOpt = Option{Key: "chunk", Label: "Chunk", Choices: []string{"64K", "512K"}, Default: "512K"}

// aaBackend: levels x (data and spare disks, a chunk option) and y (data only).
func aaBackend() *fakeBackend {
	return &fakeBackend{name: "aa", levels: []string{"x", "y"}, avail: true,
		roles:   map[string][]Role{"x": {RoleData, RoleSpare}, "y": {RoleData}},
		options: map[string][]Option{"x": {chunkOpt}}}
}

// bbBackend: level z, data disks, no options; unavailable.
func bbBackend() *fakeBackend {
	return &fakeBackend{name: "bb", levels: []string{"z"}, why: "tool b missing",
		roles: map[string][]Role{"z": {RoleData}}}
}

func freeDisks() (*inuse.Report, error) { return &inuse.Report{}, nil }

func hdd(name string) Disk {
	return Disk{Target: Target{Name: name, Path: "/dev/" + name, Rotational: true}, Role: RoleData}
}

func ssd(name string) Disk {
	return Disk{Target: Target{Name: name, Path: "/dev/" + name}, Role: RoleData}
}

func withRole(d Disk, r Role) Disk { d.Role = r; return d }

func TestLayout_Identity(t *testing.T) {
	s := NewLayout(Env{}, aaBackend())
	if s.Name() != "storage layout" || s.Kind() != Applied || s.Scope() != ScopeLayout ||
		s.Input() != InputVariants || s.Lifetime() != LifetimeRun {
		t.Errorf("%s %s %s %s %s", s.Name(), s.Kind(), s.Scope(), s.Input(), s.Lifetime())
	}
	if len(s.Backends()) != 1 || s.Backends()[0].Name() != "aa" {
		t.Errorf("backends %v", s.Backends())
	}
}

func TestLayout_ValuesListBackends(t *testing.T) {
	s := NewLayout(Env{}, aaBackend(), bbBackend())
	var ids []string
	for _, v := range s.Values() {
		ids = append(ids, v.ID)
	}
	if got := strings.Join(ids, "|"); got != "none|aa x|aa y|bb z" {
		t.Errorf("%s", got)
	}
}

func TestLayout_Applicable(t *testing.T) {
	a := aaBackend()
	a.avail, a.why = false, "tool a missing"
	s := NewLayout(Env{}, a, bbBackend())
	ok, why := s.Applicable(Target{Name: "sdb"})
	if ok || why != "aa: tool a missing; bb: tool b missing" {
		t.Errorf("%v %q", ok, why)
	}
	s = NewLayout(Env{}, aaBackend(), bbBackend())
	if ok, why := s.Applicable(Target{Name: "sdb"}); !ok || why != "" {
		t.Errorf("one available: %v %q", ok, why)
	}
	s = NewLayout(Env{})
	if ok, _ := s.Applicable(Target{Name: "sdb"}); ok {
		t.Error("no backend: applicable")
	}
}

func TestValidateLayout_Generic(t *testing.T) {
	a := aaBackend()
	s := NewLayout(Env{}, a, bbBackend())
	none := Layout{Level: LevelNone}
	x := func(chunk string) Layout {
		return Layout{Backend: "aa", Level: "x", Options: map[string]string{"chunk": chunk}}
	}
	rows := []struct {
		name  string
		l     Layout
		disks []Disk
		want  string
	}{
		{"unknown backend", Layout{Backend: "zz", Level: "x"}, []Disk{ssd("sdb")}, `unknown backend "zz"`},
		{"unavailable before level", Layout{Backend: "bb", Level: "q"}, []Disk{ssd("sdb")}, "bb is not available: tool b missing"},
		{"unknown level", Layout{Backend: "aa", Level: "q"}, []Disk{ssd("sdb")}, `unknown aa level "q"`},
		{"level without backend", Layout{Level: "x"}, []Disk{ssd("sdb")}, "a layout needs a backend"},
		{"used twice", x("64K"), []Disk{ssd("sdb"), ssd("sdc"), ssd("sdb")}, "sdb is used twice"},
		{"none spare", none, []Disk{ssd("sdb"), withRole(ssd("sdc"), RoleSpare)}, "none takes data disks only"},
		{"none empty", none, nil, "none needs at least 1 disk"},
		{"role not in level", Layout{Backend: "aa", Level: "y"}, []Disk{ssd("sdb"), withRole(ssd("sdc"), RoleSpare)}, "y cannot have a spare disk"},
		{"option not in level", Layout{Backend: "aa", Level: "y", Options: map[string]string{"chunk": "64K"}}, []Disk{ssd("sdb")}, "y has no chunk option"},
		{"none has no option", Layout{Level: LevelNone, Options: map[string]string{"chunk": "64K"}}, []Disk{ssd("sdb")}, "none has no chunk option"},
		{"option not set", Layout{Backend: "aa", Level: "x"}, []Disk{ssd("sdb")}, "Chunk is not set"},
		{"option not a choice", x("7K"), []Disk{ssd("sdb")}, "Chunk must be one of 64K, 512K"},
		{"backend rule", x("64K"), []Disk{ssd("sdb")}, "backend says no"},
		{"mixed media", x("64K"), []Disk{ssd("sdc"), hdd("sdb"), ssd("sdd")}, "mixed media: data disks sdb (HDD) and sdc (SSD)"},
		{"mixed spare ignored", x("64K"), []Disk{ssd("sdc"), withRole(hdd("sdb"), RoleSpare)}, ""},
		{"valid baseline mixed", none, []Disk{hdd("sdb"), ssd("sdc")}, ""},
		{"valid layout", x("512K"), []Disk{ssd("sdb"), withRole(ssd("sdc"), RoleSpare)}, ""},
	}
	for _, r := range rows {
		a.validate = nil
		if r.name == "backend rule" {
			a.validate = errors.New("backend says no")
		}
		err := s.ValidateLayout(r.l, r.disks)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != r.want {
			t.Errorf("%s: got %q, want %q", r.name, got, r.want)
		}
	}
}

func TestLayoutValue_Baseline(t *testing.T) {
	s := NewLayout(Env{}, aaBackend())
	v, err := s.LayoutValue(Layout{Level: LevelNone}, []Disk{ssd("sdb"), ssd("sdc"), ssd("sdd"), ssd("sde")})
	if err != nil || v.ID != "none · 4 disks" || v.Label != v.ID {
		t.Errorf("%+v %v", v, err)
	}
	v, err = s.LayoutValue(Layout{Level: LevelNone}, []Disk{ssd("sdb")})
	if err != nil || v.ID != "none · 1 disk" {
		t.Errorf("%+v %v", v, err)
	}
	if _, err := s.LayoutValue(Layout{Level: LevelNone}, nil); err == nil {
		t.Error("no disks: no error")
	}
}

func TestLayoutValue_RequiresBackendPrefix(t *testing.T) {
	a := aaBackend()
	s := NewLayout(Env{}, a)
	l := Layout{Backend: "aa", Level: "y"}
	v, err := s.LayoutValue(l, []Disk{ssd("sdb")})
	if err != nil || v.ID != "aa y · 1 disks" || v.Label != v.ID {
		t.Fatalf("%+v %v", v, err)
	}
	a.badLabel = "aay · 1 disks"
	if _, err := s.LayoutValue(l, []Disk{ssd("sdb")}); err == nil || err.Error() != `backend aa labels "aay · 1 disks" without its name` {
		t.Errorf("%v", err)
	}
}

func TestParseLayout_Dispatch(t *testing.T) {
	s := NewLayout(Env{}, aaBackend())
	l, err := s.ParseLayout("none · 3 disks")
	if err != nil || l.Level != LevelNone || l.Backend != "" {
		t.Errorf("%+v %v", l, err)
	}
	l, err = s.ParseLayout("aa x · 2 disks · chunk=64K")
	if err != nil || l.Backend != "aa" || l.Level != "x" || l.Options["chunk"] != "64K" {
		t.Errorf("%+v %v", l, err)
	}
	if _, err := s.ParseLayout("zz x · 2 disks"); err == nil || err.Error() != `unknown backend "zz"` {
		t.Errorf("%v", err)
	}
}

func TestLayout_ApplicableVariantMismatch(t *testing.T) {
	s := NewLayout(Env{}, aaBackend())
	three, err := s.LayoutValue(Layout{Backend: "aa", Level: "y"}, []Disk{ssd("sdb"), ssd("sdc"), ssd("sdd")})
	if err != nil {
		t.Fatal(err)
	}
	ok, why := s.ApplicableVariant(Variant{Value: three, Disks: []Disk{ssd("sdb"), ssd("sdc")}})
	if ok || why != `layout "aa y · 3 disks" does not match its disks ("aa y · 2 disks")` {
		t.Errorf("%v %q", ok, why)
	}
	ok, why = s.ApplicableVariant(Variant{Value: three, Disks: []Disk{ssd("sdb"), ssd("sdc"), ssd("sdd")}})
	if !ok || why != "" {
		t.Errorf("matching: %v %q", ok, why)
	}
	// A value that fails validation on its disks is refused with the reason.
	ok, why = s.ApplicableVariant(Variant{Value: three, Disks: []Disk{ssd("sdb"), ssd("sdb"), ssd("sdd")}})
	if ok || why != "sdb is used twice" {
		t.Errorf("invalid: %v %q", ok, why)
	}
}

// variantOf is the variant named name of layout l on ds.
func variantOf(t *testing.T, s LayoutSetting, name string, l Layout, ds ...Disk) Variant {
	t.Helper()
	v, err := s.LayoutValue(l, ds)
	if err != nil {
		t.Fatal(err)
	}
	return Variant{Name: name, Value: v, Disks: ds}
}

func busy(r *inuse.Report) func() (*inuse.Report, error) {
	return func() (*inuse.Report, error) { return r, nil }
}

func TestLayout_PrepareRefusesBusyDisk(t *testing.T) {
	a := aaBackend()
	s := NewLayout(Env{InUse: busy(&inuse.Report{Busy: map[string][]string{"sdc": {"mounted at /x"}}})}, a)
	v := variantOf(t, s, "1762790400_1", Layout{Backend: "aa", Level: "y"}, ssd("sdb"), ssd("sdc"))
	us, err := s.Prepare(context.Background(), v)
	if err == nil || err.Error() != "sdc is in use: mounted at /x" || us != nil {
		t.Errorf("%v %v", us, err)
	}
	if len(a.calls) != 0 {
		t.Errorf("backend called: %v", a.calls)
	}
}

func TestLayout_PrepareBaselineChecksInUse(t *testing.T) {
	a := aaBackend()
	s := NewLayout(Env{InUse: busy(&inuse.Report{Busy: map[string][]string{"sdc": {"mounted at /x"}}})}, a)
	v := variantOf(t, s, "1762790400_1", Layout{Level: LevelNone}, ssd("sdb"), ssd("sdc"))
	if _, err := s.Prepare(context.Background(), v); err == nil || err.Error() != "sdc is in use: mounted at /x" {
		t.Errorf("%v", err)
	}
	s = NewLayout(Env{InUse: freeDisks}, a)
	us, err := s.Prepare(context.Background(), v)
	if err != nil || len(us) != 0 {
		t.Errorf("free baseline: %v %v", us, err)
	}
	if len(a.calls) != 0 {
		t.Errorf("backend called: %v", a.calls)
	}
}

func TestLayout_PrepareNothingUsable(t *testing.T) {
	a := aaBackend()
	s := NewLayout(Env{InUse: busy(&inuse.Report{All: "cannot read /proc/self/mountinfo"})}, a)
	v := variantOf(t, s, "1762790400_1", Layout{Backend: "aa", Level: "y"}, ssd("sdb"))
	if _, err := s.Prepare(context.Background(), v); err == nil || err.Error() != "no disk can be used: cannot read /proc/self/mountinfo" {
		t.Errorf("%v", err)
	}
	s = NewLayout(Env{InUse: func() (*inuse.Report, error) { return nil, errors.New("boom") }}, a)
	if _, err := s.Prepare(context.Background(), v); err == nil || err.Error() != "cannot check which disks are in use: boom" {
		t.Errorf("%v", err)
	}
	if len(a.calls) != 0 {
		t.Errorf("backend called: %v", a.calls)
	}
}

func TestLayout_PrepareRecordsBackend(t *testing.T) {
	a := aaBackend()
	a.prepared = &ArrayUndo{Name: "bbbench_1762790400_1", Backend: "wrong", Extra: map[string]string{"k": "v"}}
	s := NewLayout(Env{InUse: freeDisks}, a)
	v := variantOf(t, s, "1762790400_1", Layout{Backend: "aa", Level: "y"}, ssd("sdb"), ssd("sdc"))
	us, err := s.Prepare(context.Background(), v)
	if err != nil || len(us) != 1 || us[0].Array == nil {
		t.Fatalf("%v %v", us, err)
	}
	if us[0].Array.Backend != "aa" || us[0].Name() != "bbbench_1762790400_1" || us[0].Array.Extra["k"] != "v" {
		t.Errorf("%+v", us[0].Array)
	}
	if len(a.names) != 1 || a.names[0] != "bbbench_1762790400_1" {
		t.Errorf("names %v", a.names)
	}
	a.prepareErr = errors.New("exists")
	if _, err := s.Prepare(context.Background(), v); err == nil {
		t.Error("backend prepare error not returned")
	}
	// A name too long for an array refuses before the backend is asked.
	a.prepareErr, a.calls = nil, nil
	v.Name = strings.Repeat("1", 30)
	if _, err := s.Prepare(context.Background(), v); err == nil || len(a.calls) != 0 {
		t.Errorf("%v %v", err, a.calls)
	}
}

func TestArrayName(t *testing.T) {
	n, err := ArrayName("1762790400_1")
	if err != nil || n != "bbbench_1762790400_1" {
		t.Errorf("%q %v", n, err)
	}
	if n, err := ArrayName(strings.Repeat("1", 24)); err != nil || len(n) != 32 {
		t.Errorf("32 characters: %q %v", n, err)
	}
	if _, err := ArrayName(strings.Repeat("1", 30)); err == nil {
		t.Error("30-character variant name: no error")
	}
	if _, err := ArrayName(""); err == nil {
		t.Error("empty variant name: no error")
	}
}

func TestLayout_ApplyBaseline(t *testing.T) {
	a := aaBackend()
	s := NewLayout(Env{InUse: freeDisks}, a)
	v := variantOf(t, s, "1762790400_1", Layout{Level: LevelNone}, ssd("sdb"), ssd("sdc"))
	ts, err := s.Apply(context.Background(), v, nil)
	if err != nil || len(ts) != 2 {
		t.Fatalf("%v %v", ts, err)
	}
	for i, tg := range ts {
		if tg.Kind != TargetDrive || tg.Want != "none" || tg.Backend != "" || tg.Target.Name != v.Disks[i].Name {
			t.Errorf("%+v", tg)
		}
	}
	if len(a.calls) != 0 {
		t.Errorf("backend called: %v", a.calls)
	}
}

func TestLayout_ApplySetsBackend(t *testing.T) {
	a := aaBackend()
	a.targets = []BenchTarget{{Kind: TargetArray, Target: Target{Name: "md127"}, Want: "aa y · 2 disks"}}
	s := NewLayout(Env{InUse: freeDisks}, a)
	v := variantOf(t, s, "1762790400_1", Layout{Backend: "aa", Level: "y"}, ssd("sdb"), ssd("sdc"))
	us, err := s.Prepare(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := s.Apply(context.Background(), v, us)
	if err != nil || len(ts) != 1 || ts[0].Backend != "aa" || ts[0].Target.Name != "md127" {
		t.Fatalf("%+v %v", ts, err)
	}
	if a.applied != us[0].Array {
		t.Error("backend did not get the prepared record")
	}
	if _, err := s.Apply(context.Background(), v, nil); err == nil {
		t.Error("apply without a prepared record: no error")
	}
}

// holdersSys builds <root>/block with sdb (free), sdc (held by md5) and sdd
// (its partition sdd1 held by dm-0).
func holdersSys(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, "block", p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(p string) {
		if err := os.WriteFile(filepath.Join(root, "block", p), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("sdb/holders")
	mk("sdb/sdb1/holders")
	touch("sdb/sdb1/partition")
	mk("sdb/queue")
	mk("sdc/holders/md5")
	mk("sdd/holders")
	mk("sdd/sdd1/holders/dm-0")
	touch("sdd/sdd1/partition")
	return root
}

func TestLayout_ReadNoneDisk(t *testing.T) {
	s := NewLayout(Env{Sys: holdersSys(t)}, aaBackend())
	for disk, want := range map[string]string{"sdb": "none", "sdc": "member of md5", "sdd": "member of dm-0"} {
		r, err := s.Read(context.Background(), BenchTarget{Kind: TargetDrive, Target: Target{Name: disk}})
		if err != nil || r.Value != want || r.At.IsZero() {
			t.Errorf("%s: %+v %v, want %q", disk, r, err, want)
		}
	}
	if _, err := s.Read(context.Background(), BenchTarget{Kind: TargetDrive, Target: Target{Name: "sdz"}}); err == nil {
		t.Error("missing disk: no error")
	}
}

func TestLayout_DispatchByBackend(t *testing.T) {
	a, b := aaBackend(), bbBackend()
	b.reading = "bb z · 2 disks"
	s := NewLayout(Env{LookPath: lookNone}, a, b)
	ctx := context.Background()
	bt := BenchTarget{Kind: TargetArray, Target: Target{Name: "md127"}, Backend: "bb"}
	r, err := s.Read(ctx, bt)
	if err != nil || r.Value != "bb z · 2 disks" {
		t.Errorf("read %+v %v", r, err)
	}
	drive := BenchTarget{Kind: TargetDrive, Target: Target{Name: "sdb"}}
	if err := s.WaitSteady(ctx, []BenchTarget{drive, bt}, nil); err != nil {
		t.Errorf("steady %v", err)
	}
	if len(b.steady) != 1 || b.steady[0].Target.Name != "md127" {
		t.Errorf("bb steady got %v", b.steady)
	}
	u := Undo{Array: &ArrayUndo{Backend: "bb", Name: "bbbench_1_1"}}
	if err := s.Undo(ctx, u); err != nil {
		t.Errorf("undo %v", err)
	}
	if got := s.UndoCommand(u); got != "bb undo bbbench_1_1" {
		t.Errorf("undo command %q", got)
	}
	if got := strings.Join(b.calls, "|"); got != "read|steady|undo bbbench_1_1" {
		t.Errorf("bb calls %s", got)
	}
	if len(a.calls) != 0 {
		t.Errorf("aa called: %v", a.calls)
	}

	zz := BenchTarget{Kind: TargetArray, Target: Target{Name: "md1"}, Backend: "zz"}
	if _, err := s.Read(ctx, zz); err == nil || err.Error() != `unknown backend "zz"` {
		t.Errorf("read zz %v", err)
	}
	if err := s.WaitSteady(ctx, []BenchTarget{zz}, nil); err == nil || err.Error() != `unknown backend "zz"` {
		t.Errorf("steady zz %v", err)
	}
	uz := Undo{Array: &ArrayUndo{Backend: "zz", Name: "bbbench_1_1"}}
	if err := s.Undo(ctx, uz); err == nil || err.Error() != `unknown backend "zz"; nothing torn down` {
		t.Errorf("undo zz %v", err)
	}
	if got := s.UndoCommand(uz); got != "" {
		t.Errorf("undo command zz %q", got)
	}
	if err := s.Undo(ctx, Undo{Drive: &Target{Name: "sdb"}}); err == nil {
		t.Error("drive record: no error")
	}
}

func TestLayout_UndoSettlesUdev(t *testing.T) {
	ctx := context.Background()
	u := Undo{Array: &ArrayUndo{Backend: "aa", Name: "bbbench_1_1"}}

	f := &fake{answers: map[string]string{"udevadm settle --timeout=10": ""}}
	a := aaBackend()
	s := NewLayout(Env{Run: f.run, LookPath: lookAll, Settle: 10 * time.Second}, a)
	if err := s.Undo(ctx, u); err != nil {
		t.Fatal(err)
	}
	if len(f.ran) == 0 || f.ran[len(f.ran)-1] != "udevadm settle --timeout=10" {
		t.Errorf("ran %v", f.ran)
	}

	// Under a second rounds up to one.
	f = &fake{answers: map[string]string{"udevadm settle --timeout=1": ""}}
	s = NewLayout(Env{Run: f.run, LookPath: lookAll, Settle: 300 * time.Millisecond}, a)
	if err := s.Undo(ctx, u); err != nil || strings.Join(f.ran, "|") != "udevadm settle --timeout=1" {
		t.Errorf("%v %v", err, f.ran)
	}

	// A failed backend undo runs no settle.
	f = &fake{}
	a.undoErr = errors.New("busy")
	s = NewLayout(Env{Run: f.run, LookPath: lookAll, Settle: 10 * time.Second}, a)
	if err := s.Undo(ctx, u); err == nil || len(f.ran) != 0 {
		t.Errorf("%v %v", err, f.ran)
	}

	// No udevadm: nothing runs, the undo succeeds.
	a.undoErr = nil
	s = NewLayout(Env{Run: f.run, LookPath: lookNone, Settle: 10 * time.Second}, a)
	if err := s.Undo(ctx, u); err != nil || len(f.ran) != 0 {
		t.Errorf("%v %v", err, f.ran)
	}

	// A settle error is logged, not returned.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)
	f = &fake{}
	s = NewLayout(Env{Run: f.run, LookPath: lookAll, Settle: 10 * time.Second}, a)
	if err := s.Undo(ctx, u); err != nil || len(f.ran) != 1 {
		t.Errorf("%v %v", err, f.ran)
	}
}

func TestWithDefaults(t *testing.T) {
	b := &fakeBackend{name: "aa", levels: []string{"x", "y"}, options: map[string][]Option{
		"x": {chunkOpt},
		"y": {{Key: "bitmap", Label: "Bitmap", Choices: []string{"on", "off"}, Default: "off"}, {Key: "chunk", Label: "Chunk", Choices: []string{"64K"}, Default: "64K"}},
	}}
	l := WithDefaults(b, Layout{Backend: "aa", Level: "x"}, 2)
	if len(l.Options) != 1 || l.Options["chunk"] != "512K" {
		t.Errorf("x defaults %v", l.Options)
	}
	l.Options["chunk"] = "512K"
	l.Options["stale"] = "1"
	l.Level = "y"
	in := l.Options
	l = WithDefaults(b, l, 2)
	if len(l.Options) != 2 || l.Options["bitmap"] != "off" || l.Options["chunk"] != "64K" {
		t.Errorf("y %v", l.Options)
	}
	if in["stale"] != "1" || in["chunk"] != "512K" {
		t.Errorf("input map changed: %v", in)
	}
	l.Options["bitmap"] = "on"
	if l = WithDefaults(b, l, 2); l.Options["bitmap"] != "on" {
		t.Errorf("kept choice lost: %v", l.Options)
	}
	if l = WithDefaults(b, Layout{Level: LevelNone, Options: map[string]string{"chunk": "64K"}}, 2); len(l.Options) != 0 {
		t.Errorf("none %v", l.Options)
	}
}
