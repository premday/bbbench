package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// layoutCall is one call the fake layout (or the fake Execute) received.
type layoutCall struct {
	Op      string // prepare, apply, wait, read, execute, undo
	Variant string // v.Name for prepare/apply, the record name for undo
	Value   string // v.Value.ID, the target name for read
}

// fakeLayout is a LifetimeRun setting that builds composites named md127,
// md126, ... (the baseline "none ..." builds nothing). Results are scripted
// per value; a value's first word is its backend.
type fakeLayout struct {
	mu         sync.Mutex
	calls      []layoutCall
	next       int // composites built so far
	prepareErr map[string]error
	applyErr   map[string]error
	undoErr    map[string]error // keyed by record name
	// read, when non-nil, gives the reading of the nth read (1-based) of a
	// target of value; "" means the target's Want.
	read func(value string, n int) string
	// wait, when non-nil, replaces WaitSteady.
	wait func(ctx context.Context, progress func(float64)) error
	// onApply, when non-nil, runs at the start of Apply.
	onApply func(v settings.Variant)
	reads   map[string]int
	undone  []settings.ArrayUndo
}

func newFakeLayout() *fakeLayout {
	return &fakeLayout{prepareErr: map[string]error{}, applyErr: map[string]error{}, undoErr: map[string]error{}, reads: map[string]int{}}
}

func (f *fakeLayout) add(c layoutCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeLayout) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Op)
	}
	return out
}

func (f *fakeLayout) callsOf(op string) []layoutCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []layoutCall
	for _, c := range f.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

func isNone(value string) bool { return strings.HasPrefix(value, "none") }

func backendOf(value string) string { b, _, _ := strings.Cut(value, " "); return b }

func (f *fakeLayout) Name() string                                      { return "fake layout" }
func (f *fakeLayout) Kind() settings.Kind                               { return settings.Applied }
func (f *fakeLayout) Scope() settings.Scope                             { return settings.ScopeLayout }
func (f *fakeLayout) Input() settings.Input                             { return settings.InputVariants }
func (f *fakeLayout) Lifetime() settings.Lifetime                       { return settings.LifetimeRun }
func (f *fakeLayout) Values() []settings.Value                          { return nil }
func (f *fakeLayout) Applicable(settings.Target) (bool, string)         { return true, "" }
func (f *fakeLayout) ApplicableVariant(settings.Variant) (bool, string) { return true, "" }

func (f *fakeLayout) Prepare(ctx context.Context, v settings.Variant) ([]settings.Undo, error) {
	f.add(layoutCall{"prepare", v.Name, v.Value.ID})
	if err := f.prepareErr[v.Value.ID]; err != nil {
		return nil, err
	}
	if isNone(v.Value.ID) {
		return nil, nil
	}
	rec := &settings.ArrayUndo{Backend: backendOf(v.Value.ID), Name: "bbbench_" + v.Name, Members: slices.Clone(v.Disks)}
	if rec.Backend == "mdadm" {
		rec.UUID = "0a:11:22:" + v.Name
	}
	return []settings.Undo{{Array: rec}}, nil
}

func (f *fakeLayout) Apply(ctx context.Context, v settings.Variant, prepared []settings.Undo) ([]settings.BenchTarget, error) {
	f.add(layoutCall{"apply", v.Name, v.Value.ID})
	if f.onApply != nil {
		f.onApply(v)
	}
	if err := f.applyErr[v.Value.ID]; err != nil {
		return nil, err
	}
	if isNone(v.Value.ID) {
		var ts []settings.BenchTarget
		for _, d := range v.Disks {
			ts = append(ts, settings.BenchTarget{Kind: settings.TargetDrive, Target: d.Target, Want: "none"})
		}
		return ts, nil
	}
	f.mu.Lock()
	md := "md" + strconv.Itoa(127-f.next)
	f.next++
	f.mu.Unlock()
	rec := prepared[0].Array
	return []settings.BenchTarget{{
		Kind: settings.TargetArray, Backend: rec.Backend, UUID: rec.UUID, Members: slices.Clone(v.Disks), Want: v.Value.ID,
		Target: settings.Target{Name: md, Path: "/dev/" + md, Serial: rec.UUID, Model: v.Value.ID},
	}}, nil
}

func (f *fakeLayout) WaitSteady(ctx context.Context, ts []settings.BenchTarget, progress func(float64)) error {
	f.add(layoutCall{Op: "wait"})
	if f.wait != nil {
		return f.wait(ctx, progress)
	}
	return nil
}

func (f *fakeLayout) Read(ctx context.Context, t settings.BenchTarget) (settings.Reading, error) {
	f.add(layoutCall{Op: "read", Value: t.Target.Name})
	f.mu.Lock()
	f.reads[t.Target.Name+"\x00"+t.Want]++
	n := f.reads[t.Target.Name+"\x00"+t.Want]
	f.mu.Unlock()
	v := ""
	if f.read != nil {
		v = f.read(t.Target.Model, n)
	}
	if v == "" {
		v = t.Want
	}
	return settings.Reading{Value: v, At: time.Now()}, nil
}

func (f *fakeLayout) Undo(ctx context.Context, u settings.Undo) error {
	if u.Array == nil {
		return errors.New("not a layout record")
	}
	f.add(layoutCall{Op: "undo", Variant: u.Array.Name})
	f.mu.Lock()
	f.undone = append(f.undone, *u.Array)
	f.mu.Unlock()
	if ctx.Err() != nil {
		return fmt.Errorf("undo on a cancelled context: %w", ctx.Err())
	}
	return f.undoErr[u.Array.Name]
}

func (f *fakeLayout) UndoCommand(u settings.Undo) string {
	if u.Array == nil {
		return ""
	}
	return "fake-teardown " + u.Array.Name
}

// layoutVariant is value on the named test disks (from targets), data role.
func layoutVariant(targets map[string]settings.Target, value string, names ...string) settings.Variant {
	v := settings.Variant{Value: settings.Value{ID: value, Label: value}}
	for _, n := range names {
		v.Disks = append(v.Disks, settings.Disk{Target: targets[n], Role: settings.RoleData})
	}
	return v
}

// testLayoutGroup is a layout group over sdb..sde; variants come from mk.
func testLayoutGroup(t *testing.T, mk func(targets map[string]settings.Target) []settings.Variant) (groupConfig, *fakeLayout, *fakeExec) {
	t.Helper()
	quietLogger(t)
	captureReport(t)
	ResetStatus()
	t.Cleanup(ResetStatus)
	drives, targets := testDrives("sdb", "sdc", "sdd", "sde")
	fl := newFakeLayout()
	ex := &fakeExec{}
	ex.hook = func(ctx context.Context, n int, spec runSpec) error {
		fl.add(layoutCall{Op: "execute"})
		return nil
	}
	dir := t.TempDir()
	cfg := groupConfig{
		Setting:      fl,
		Variants:     mk(targets),
		Strategy:     rungroup.Sequential,
		Drives:       drives,
		Targets:      targets,
		OutDir:       dir,
		RecoveryPath: filepath.Join(dir, "recovery", "group-recovery.json"),
		Host:         "host1",
		Execute:      ex.Execute,
		MakeDrive: func(bt settings.BenchTarget) (DriveInfo, error) {
			return DriveInfo{Device: blockdev.Device{Name: bt.Target.Name, Path: bt.Target.Path, Serial: bt.UUID, Model: bt.Target.Model}, Members: rungroup.MembersOf(bt.Members), UUID: bt.UUID}, nil
		},
		Identify: identifyAs(drives),
	}
	return cfg, fl, ex
}

// identifyAs re-identifies each disk as the drive of the same name.
func identifyAs(drives []DriveInfo) func(string) (blockdev.Device, error) {
	return func(name string) (blockdev.Device, error) {
		for _, d := range drives {
			if d.Device.Name == name {
				return d.Device, nil
			}
		}
		return blockdev.Device{}, fmt.Errorf("read %s: no such device", name)
	}
}

func twoRaids(targets map[string]settings.Target) []settings.Variant {
	return []settings.Variant{
		layoutVariant(targets, "mdadm raid1 · 2 disks", "sdb", "sdc"),
		layoutVariant(targets, "mdadm raid0 · 2 disks", "sdb", "sdc"),
	}
}

func TestRunGroup_LayoutBuildsRunsTearsDown(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, twoRaids)
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runGroup: %v", err)
	}
	perRun := []string{"prepare", "apply", "wait", "read", "execute", "read", "undo"}
	if got, want := fl.ops(), append(slices.Clone(perRun), perRun...); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v\nwant  %v", got, want)
	}
	if len(ex.specs) != 2 {
		t.Fatalf("Execute called %d times", len(ex.specs))
	}
	for i, spec := range ex.specs {
		md := []string{"md127", "md126"}[i]
		if len(spec.Drives) != 1 || spec.Drives[0].Device.Name != md || len(spec.Drives[0].Members) != 2 {
			t.Fatalf("run %d drives %+v", i+1, spec.Drives)
		}
		l, ok := spec.Labels[md]
		if !ok || len(spec.Labels) != 1 || l.Value != cfg.Variants[i].Value.ID || !l.Valid {
			t.Errorf("run %d labels %+v", i+1, spec.Labels)
		}
	}
	saved := loadSavedGroup(t, cfg.OutDir, g.ID)
	if len(saved.Variants) != 2 || len(saved.Variants[0].Disks) != 2 {
		t.Errorf("variants %+v", saved.Variants)
	}
	for i, r := range saved.Runs {
		if r.Status != rungroup.RunDone {
			t.Errorf("run %d status %s", i+1, r.Status)
		}
		if len(r.Drives) != 1 || r.Drives[0].Kind != settings.TargetArray || len(r.Drives[0].Members) != 2 || r.Drives[0].UUID == "" {
			t.Errorf("run %d drives %+v", i+1, r.Drives)
		}
		if r.Teardown == nil || r.Teardown.Status != rungroup.RestoreDone {
			t.Errorf("run %d teardown %+v", i+1, r.Teardown)
		}
	}
	if saved.Restore.Status != rungroup.RestoreDone {
		t.Errorf("restore %+v", saved.Restore)
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file kept after every layout was torn down")
	}
}

func TestRunGroup_LayoutMixedBackends(t *testing.T) {
	cfg, fl, _ := testLayoutGroup(t, func(ts map[string]settings.Target) []settings.Variant {
		return []settings.Variant{
			layoutVariant(ts, "mdadm raid1 · 2 disks", "sdb", "sdc"),
			layoutVariant(ts, "lvm raid1 · 2 disks", "sdb", "sdc"),
			layoutVariant(ts, "none · 2 disks", "sdb", "sdc"),
		}
	})
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, c := range fl.callsOf("prepare") {
		values = append(values, c.Value)
	}
	if !reflect.DeepEqual(values, []string{"mdadm raid1 · 2 disks", "lvm raid1 · 2 disks", "none · 2 disks"}) {
		t.Errorf("prepared %v", values)
	}
	var backends []string
	for _, a := range fl.undone {
		backends = append(backends, a.Backend)
	}
	if !reflect.DeepEqual(backends, []string{"mdadm", "lvm"}) {
		t.Errorf("undone backends %v", backends)
	}
	saved := loadSavedGroup(t, cfg.OutDir, g.ID)
	var labels []string
	for _, r := range saved.Runs {
		if len(r.Drives) == 0 {
			t.Fatalf("run without drives: %+v", r)
		}
		labels = append(labels, r.Drives[0].Label.Value)
	}
	if !reflect.DeepEqual(labels, values) {
		t.Errorf("labels %v", labels)
	}
}

func TestRunGroup_LayoutVariantNames(t *testing.T) {
	cfg, fl, _ := testLayoutGroup(t, twoRaids)
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ps := fl.callsOf("prepare")
	if len(ps) != 2 || ps[0].Variant != g.ID+"_1" || ps[1].Variant != g.ID+"_2" {
		t.Errorf("prepare calls %+v", ps)
	}
	as := fl.callsOf("apply")
	if len(as) != 2 || as[0].Variant != g.ID+"_1" || as[1].Variant != g.ID+"_2" {
		t.Errorf("apply calls %+v", as)
	}
}

func TestRunGroup_LayoutRecoveryWrittenBeforeBuild(t *testing.T) {
	cfg, fl, _ := testLayoutGroup(t, twoRaids)
	checked := 0
	fl.onApply = func(v settings.Variant) {
		checked++
		rec, err := rungroup.LoadRecovery(cfg.RecoveryPath)
		if err != nil || rec == nil || len(rec.Arrays) != 1 {
			t.Fatalf("at apply of %s: recovery %+v, %v", v.Name, rec, err)
		}
		a := rec.Arrays[0]
		if a.Backend != "mdadm" || a.Name != "bbbench_"+v.Name || a.UUID != "0a:11:22:"+v.Name || len(a.Members) != 2 || a.Members[0].Serial != "SN-sdb" || a.Members[1].Serial != "SN-sdc" {
			t.Errorf("recovery array %+v", a)
		}
	}
	if _, err := runGroup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if checked != 2 {
		t.Errorf("apply checked %d times", checked)
	}
}

func TestRunGroup_LayoutPrepareFailsNotRun(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, twoRaids)
	fl.prepareErr["mdadm raid1 · 2 disks"] = errors.New("sdc is in use: mounted at /x")
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	as := fl.callsOf("apply")
	if len(as) != 1 || as[0].Value != "mdadm raid0 · 2 disks" {
		t.Errorf("applies %+v: nothing may be built for the busy run", as)
	}
	r := g.Runs[0]
	if r.Status != rungroup.RunNotRun || len(r.Drives) != 2 {
		t.Fatalf("run 1 %+v", r)
	}
	for _, d := range r.Drives {
		if d.Label.Note != "not built: sdc is in use: mounted at /x" || d.Label.Valid {
			t.Errorf("run 1 %s label %+v", d.Device, d.Label)
		}
	}
	if g.Runs[1].Status != rungroup.RunDone || len(ex.specs) != 1 {
		t.Errorf("run 2 %+v, %d executions", g.Runs[1], len(ex.specs))
	}
}

// TestRunGroup_LayoutReidentifiesBeforeBuild: sdb is another disk by run 2
// (re-enumerated, hot-plugged): run 2 builds nothing and is not run.
func TestRunGroup_LayoutReidentifiesBeforeBuild(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, twoRaids)
	base := cfg.Identify
	runs := 0
	fl.onApply = func(settings.Variant) { runs++ }
	cfg.Identify = func(name string) (blockdev.Device, error) {
		d, err := base(name)
		if name == "sdb" && runs >= 1 {
			d.Serial = "SN-other"
		}
		return d, err
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ps, as := fl.callsOf("prepare"), fl.callsOf("apply"); len(ps) != 1 || len(as) != 1 {
		t.Fatalf("prepares %+v, applies %+v: run 2 may not be prepared or built", ps, as)
	}
	r := g.Runs[1]
	if r.Status != rungroup.RunNotRun || len(r.Drives) != 2 {
		t.Fatalf("run 2 %+v", r)
	}
	for _, d := range r.Drives {
		if d.Label.Note != "not built: sdb is no longer SN-sdb (found SN-other)" || d.Label.Valid {
			t.Errorf("run 2 %s label %+v", d.Device, d.Label)
		}
	}
	if g.Runs[0].Status != rungroup.RunDone || len(ex.specs) != 1 {
		t.Errorf("run 1 %+v, %d executions", g.Runs[0], len(ex.specs))
	}
}

func TestRunGroup_LayoutReidentifyModelOrCapacity(t *testing.T) {
	for name, change := range map[string]func(*blockdev.Device){
		"model":    func(d *blockdev.Device) { d.Model = "M2" },
		"capacity": func(d *blockdev.Device) { d.CapacityBytes /= 2 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, fl, _ := testLayoutGroup(t, twoRaids)
			base := cfg.Identify
			cfg.Identify = func(n string) (blockdev.Device, error) {
				d, err := base(n)
				if n == "sdc" {
					change(&d)
				}
				return d, err
			}
			g, err := runGroup(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if ps := fl.callsOf("prepare"); len(ps) != 0 {
				t.Fatalf("prepared %+v", ps)
			}
			for i, r := range g.Runs {
				if r.Status != rungroup.RunNotRun || !strings.HasPrefix(r.Drives[0].Label.Note, "not built: sdc is no longer ") {
					t.Errorf("run %d %+v", i+1, r)
				}
			}
		})
	}
}

func TestRunGroup_LayoutReidentifyUnreadableRefuses(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, twoRaids)
	cfg.Identify = func(name string) (blockdev.Device, error) {
		return blockdev.Device{}, fmt.Errorf("read %s: no such device", name)
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ps := fl.callsOf("prepare"); len(ps) != 0 || len(ex.specs) != 0 {
		t.Fatalf("prepared %+v, %d executions", ps, len(ex.specs))
	}
	for i, r := range g.Runs {
		if r.Status != rungroup.RunNotRun || r.Drives[0].Label.Note != "not built: cannot re-identify sdb: read sdb: no such device" {
			t.Errorf("run %d %+v", i+1, r)
		}
	}
	// No serial read back is unreadable too, and so is no way to read.
	cfg, fl, _ = testLayoutGroup(t, twoRaids)
	cfg.Identify = func(name string) (blockdev.Device, error) { return blockdev.Device{Name: name}, nil }
	if g, err = runGroup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if ps := fl.callsOf("prepare"); len(ps) != 0 || g.Runs[0].Drives[0].Label.Note != "not built: cannot re-identify sdb: no serial" {
		t.Errorf("prepared %+v, run 1 %+v", ps, g.Runs[0])
	}
	cfg, fl, _ = testLayoutGroup(t, twoRaids)
	cfg.Identify = nil
	if g, err = runGroup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if ps := fl.callsOf("prepare"); len(ps) != 0 || g.Runs[0].Status != rungroup.RunNotRun {
		t.Errorf("prepared %+v without a way to re-identify, run 1 %+v", ps, g.Runs[0])
	}
}

// TestCheckLayoutDisks_IdentityOfTheDrive: a layout disk must be the group's
// drive of that name, by serial, model and path.
func TestCheckLayoutDisks_IdentityOfTheDrive(t *testing.T) {
	for name, change := range map[string]func(*settings.Disk){
		"serial": func(d *settings.Disk) { d.Serial = "SN-other" },
		"model":  func(d *settings.Disk) { d.Model = "M2" },
		"path":   func(d *settings.Disk) { d.Path = "/dev/sdz" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _, _ := testLayoutGroup(t, twoRaids)
			if err := checkLayoutDisks(cfg); err != nil {
				t.Fatalf("unchanged: %v", err)
			}
			change(&cfg.Variants[1].Disks[0])
			err := checkLayoutDisks(cfg)
			if err == nil || !strings.Contains(err.Error(), "sdb of layout 2 is not the drive sdb") {
				t.Errorf("err %v", err)
			}
		})
	}
}

func TestRunGroup_LayoutBeforeFailsTearsDownContinues(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, twoRaids)
	fl.read = func(value string, n int) string {
		if value == "mdadm raid1 · 2 disks" {
			return "degraded"
		}
		return ""
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if g.Runs[0].Status != rungroup.RunNotRun || g.Runs[1].Status != rungroup.RunDone {
		t.Fatalf("statuses %s, %s", g.Runs[0].Status, g.Runs[1].Status)
	}
	if len(ex.specs) != 1 || ex.specs[0].Drives[0].Device.Name != "md126" {
		t.Errorf("executions %+v", ex.specs)
	}
	ops := fl.ops()
	firstUndo, secondPrepare := slices.Index(ops, "undo"), -1
	for i, op := range ops {
		if op == "prepare" && i > 0 {
			secondPrepare = i
			break
		}
	}
	if firstUndo < 0 || secondPrepare < 0 || firstUndo > secondPrepare {
		t.Errorf("run 1 not torn down before run 2's prepare: %v", ops)
	}
	if !strings.Contains(g.Runs[0].Drives[0].Label.Note, "not effective") || g.Runs[0].Teardown == nil || g.Runs[0].Teardown.Status != rungroup.RestoreDone {
		t.Errorf("run 1 %+v", g.Runs[0])
	}
}

func TestRunGroup_LayoutApplyErrorNotRun(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, twoRaids)
	fl.applyErr["mdadm raid1 · 2 disks"] = errors.New("mdadm --create failed")
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	us := fl.callsOf("undo")
	if len(us) != 2 || us[0].Variant != "bbbench_"+g.ID+"_1" {
		t.Errorf("undos %+v: the partial build of run 1 must be torn down", us)
	}
	r := g.Runs[0]
	if r.Status != rungroup.RunNotRun || len(r.Drives) != 2 || r.Drives[0].Label.Note != "build failed: mdadm --create failed" {
		t.Errorf("run 1 %+v", r)
	}
	if g.Runs[1].Status != rungroup.RunDone || len(ex.specs) != 1 {
		t.Errorf("run 2 %+v", g.Runs[1])
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file kept")
	}
}

func TestRunGroup_LayoutAfterFailsInvalid(t *testing.T) {
	cfg, fl, _ := testLayoutGroup(t, twoRaids)
	fl.read = func(value string, n int) string {
		if value == "mdadm raid1 · 2 disks" && n == 2 {
			return "degraded"
		}
		return ""
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := g.Runs[0]
	if r.Status != rungroup.RunInvalid || r.Drives[0].Label.Valid || !strings.Contains(r.Drives[0].Label.Note, "changed during the run") {
		t.Errorf("run 1 %+v", r)
	}
	if r.Teardown == nil || r.Teardown.Status != rungroup.RestoreDone || g.Runs[1].Status != rungroup.RunDone {
		t.Errorf("runs %+v", g.Runs)
	}
}

func TestRunGroup_LayoutTeardownFailureStopsGroup(t *testing.T) {
	cfg, fl, _ := testLayoutGroup(t, twoRaids)
	out := captureReport(t)
	var id string
	fl.onApply = func(v settings.Variant) {
		id, _, _ = strings.Cut(v.Name, "_")
		fl.undoErr["bbbench_"+v.Name] = errors.New("mdadm --stop: device busy")
	}
	g, err := runGroup(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "the group stops") {
		t.Fatalf("err = %v", err)
	}
	if ps := fl.callsOf("prepare"); len(ps) != 1 {
		t.Errorf("prepares %+v: run 2 must not be prepared", ps)
	}
	if us := fl.callsOf("undo"); len(us) != 1 {
		t.Errorf("undos %+v: a failed teardown is not retried by the restore", us)
	}
	name := "bbbench_" + id + "_1"
	rec, rerr := rungroup.LoadRecovery(cfg.RecoveryPath)
	if rerr != nil || rec == nil || len(rec.Arrays) != 1 || rec.Arrays[0].Name != name {
		t.Fatalf("recovery %+v, %v", rec, rerr)
	}
	if g.Restore.Status != rungroup.RestoreFailed || !reflect.DeepEqual(g.Restore.Failed, []string{name}) {
		t.Errorf("restore %+v", g.Restore)
	}
	if td := g.Runs[0].Teardown; td == nil || td.Status != rungroup.RestoreFailed || !reflect.DeepEqual(td.Failed, []string{name}) {
		t.Errorf("teardown %+v", td)
	}
	rep := out.String()
	for _, want := range []string{"TEARDOWN FAILED", "mdadm " + name + " (UUID", "fake-teardown " + name, "is kept"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
}

func TestRunGroup_LayoutInterruptDuringResync(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, twoRaids)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fl.wait = func(ctx context.Context, progress func(float64)) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	g, err := runGroup(ctx, cfg)
	if err == nil {
		t.Fatal("no error for an interrupted group")
	}
	if g.Runs[0].Status != rungroup.RunInterrupted || len(ex.specs) != 0 {
		t.Errorf("run 1 %+v, %d executions", g.Runs[0], len(ex.specs))
	}
	if us := fl.callsOf("undo"); len(us) != 1 {
		t.Errorf("undos %+v", us)
	}
	if len(fl.callsOf("prepare")) != 1 {
		t.Error("run 2 prepared after the interrupt")
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file kept after the teardown")
	}
	if g.Restore.Status != rungroup.RestoreDone {
		t.Errorf("restore %+v", g.Restore)
	}
}

func TestRunGroup_LayoutResyncStatus(t *testing.T) {
	cfg, fl, _ := testLayoutGroup(t, twoRaids)
	var seen []BenchmarkStatusSnapshot
	fl.wait = func(ctx context.Context, progress func(float64)) error {
		progress(42)
		seen = append(seen, GetStatus())
		return nil
	}
	if _, err := runGroup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("%d snapshots", len(seen))
	}
	gs := seen[0].Group
	if gs == nil || gs.Step != "resyncing" || gs.Progress != 42 || gs.Variant != "mdadm raid1 · 2 disks" || gs.Run != 1 {
		t.Errorf("group status %+v", gs)
	}
	if len(seen[0].Drives) != 2 || seen[0].Drives[0].Name != "sdb" {
		t.Errorf("status drives %+v: want the variant's disks while building", seen[0].Drives)
	}
}

func TestRunGroup_LayoutBaselineNone(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, func(ts map[string]settings.Target) []settings.Variant {
		return []settings.Variant{layoutVariant(ts, "none · 4 disks", "sdb", "sdc", "sdd", "sde")}
	})
	fl.onApply = func(settings.Variant) {
		if recoveryExists(t, cfg.RecoveryPath) {
			t.Error("recovery file written for the baseline")
		}
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.specs) != 1 || len(ex.specs[0].Drives) != 4 {
		t.Fatalf("executions %+v", ex.specs)
	}
	for _, n := range []string{"sdb", "sdc", "sdd", "sde"} {
		if l := ex.specs[0].Labels[n]; l.Value != "none · 4 disks" || !l.Valid {
			t.Errorf("%s label %+v", n, l)
		}
	}
	if len(fl.callsOf("undo")) != 0 {
		t.Error("undo called for the baseline")
	}
	if g.Runs[0].Status != rungroup.RunDone || g.Runs[0].Drives[0].Kind != "" {
		t.Errorf("run %+v", g.Runs[0])
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file left")
	}
}

func TestRunGroup_LayoutMakeDriveError(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, twoRaids)
	cfg.MakeDrive = func(bt settings.BenchTarget) (DriveInfo, error) {
		return DriveInfo{}, errors.New("no fio file")
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.specs) != 0 {
		t.Errorf("%d executions", len(ex.specs))
	}
	r := g.Runs[0]
	if r.Status != rungroup.RunNotRun || len(r.Drives) == 0 || r.Drives[0].Label.Note != "cannot benchmark md127: no fio file" {
		t.Errorf("run 1 %+v", r)
	}
	if us := fl.callsOf("undo"); len(us) != 2 {
		t.Errorf("undos %+v", us)
	}
	if r.Teardown == nil || r.Teardown.Status != rungroup.RestoreDone {
		t.Errorf("teardown %+v", r.Teardown)
	}
}

func layoutResume(cfg groupConfig, runs ...rungroup.GroupRun) *rungroup.GroupFile {
	g := &rungroup.GroupFile{ID: "1700000000", Host: "host1", Setting: cfg.Setting.Name(), Strategy: rungroup.Sequential, Runs: runs, Restore: rungroup.GroupRestore{Status: rungroup.RestoreDone}}
	for _, v := range cfg.Variants {
		g.Values = append(g.Values, v.Value.ID)
		g.Variants = append(g.Variants, rungroup.GroupVariant{Value: v.Value.ID, Disks: rungroup.MembersOf(v.Disks)})
	}
	return g
}

func TestRunGroup_LayoutResumeRebuilds(t *testing.T) {
	cfg, fl, ex := testLayoutGroup(t, twoRaids)
	done := rungroup.GroupRun{RunID: "run_old", Round: 0, Status: rungroup.RunDone, Teardown: &rungroup.GroupRestore{Status: rungroup.RestoreDone},
		Drives: []rungroup.GroupDrive{{Device: "md127", Value: "mdadm raid1 · 2 disks", Kind: settings.TargetArray}}}
	cfg.Resume = layoutResume(cfg, done, rungroup.GroupRun{Round: 1, Status: rungroup.RunInterrupted})
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ps := fl.callsOf("prepare")
	if len(ps) != 1 || ps[0].Value != "mdadm raid0 · 2 disks" || ps[0].Variant != "1700000000_2" {
		t.Errorf("prepares %+v", ps)
	}
	if len(ex.specs) != 1 || g.Runs[1].Status != rungroup.RunDone || !reflect.DeepEqual(g.Runs[0], done) {
		t.Errorf("runs %+v", g.Runs)
	}
}

func TestRunGroup_LayoutResumeRefusesOtherDisks(t *testing.T) {
	cfg, fl, _ := testLayoutGroup(t, twoRaids)
	cfg.Resume = layoutResume(cfg, rungroup.GroupRun{Round: 0, Status: rungroup.RunInterrupted}, rungroup.GroupRun{Round: 1, Status: rungroup.RunPlanned})
	cfg.Resume.Variants[1].Disks[1].Serial = "SN-other"
	_, err := runGroup(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot resume group 1700000000: layout 2 differs") {
		t.Fatalf("err = %v", err)
	}
	if len(fl.calls) != 0 {
		t.Errorf("calls %+v", fl.calls)
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file written")
	}
}

func TestRunGroup_LayoutRecoveryUnwritableStops(t *testing.T) {
	cfg, fl, _ := testLayoutGroup(t, twoRaids)
	blocker := filepath.Join(cfg.OutDir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.RecoveryPath = filepath.Join(blocker, "rec.json")
	if _, err := runGroup(context.Background(), cfg); err == nil {
		t.Fatal("no error when the layout cannot be recorded")
	}
	if len(fl.callsOf("apply")) != 0 {
		t.Error("built although the recovery file could not be written")
	}
	if len(fl.callsOf("undo")) != 0 {
		t.Error("an unrecorded layout was torn down: the ledger kept a record it could not write")
	}
}

func arrayRecovery(t *testing.T) (string, *rungroup.Recovery) {
	t.Helper()
	rec := &rungroup.Recovery{Setting: "fake layout", Arrays: []rungroup.RecoveryArray{
		{Backend: "mdadm", Name: "bbbench_1_1", UUID: "0a:11", Members: []rungroup.RecoveryMember{
			{Serial: "SN-x", Model: "M1", Name: "sdb", Role: settings.RoleData},
			{Serial: "SN-y", Model: "M1", Name: "sdc", Role: settings.RoleSpare},
		}},
		{Backend: "lvm", Name: "bbbench_1_2", Members: []rungroup.RecoveryMember{
			{Serial: "SN-x", Model: "M1", Name: "sdb", Role: settings.RoleData},
		}},
	}}
	path := filepath.Join(t.TempDir(), "rec.json")
	if err := rec.Save(path); err != nil {
		t.Fatal(err)
	}
	return path, rec
}

func TestRestoreFromRecovery_ArraysBySerial(t *testing.T) {
	quietLogger(t)
	captureReport(t)
	path, rec := arrayRecovery(t)
	targets := map[string]settings.Target{
		"sdc": {Name: "sdc", Path: "/dev/sdc", Serial: "SN-x", Model: "M1"},
		"sdd": {Name: "sdd", Path: "/dev/sdd", Serial: "SN-y", Model: "M1"},
	}
	fl := newFakeLayout()
	if failed := restoreFromRecovery(context.Background(), fl, rec, path, targets); len(failed) != 0 {
		t.Fatalf("failed %v", failed)
	}
	if len(fl.undone) != 2 {
		t.Fatalf("undone %+v", fl.undone)
	}
	ms := fl.undone[0].Members
	if ms[0].Name != "sdc" || ms[0].Path != "/dev/sdc" || ms[1].Name != "sdd" || ms[1].Path != "/dev/sdd" || ms[1].Role != settings.RoleSpare {
		t.Errorf("members %+v: want the current names and paths", ms)
	}
	if fl.undone[0].UUID != "0a:11" || fl.undone[1].Backend != "lvm" {
		t.Errorf("undone %+v", fl.undone)
	}
	if recoveryExists(t, path) {
		t.Error("recovery file kept")
	}
}

func TestRestoreFromRecovery_ArrayMemberGoneKeepsRecord(t *testing.T) {
	quietLogger(t)
	captureReport(t)
	path, rec := arrayRecovery(t)
	rec.Arrays = rec.Arrays[:1]
	targets := map[string]settings.Target{"sdc": {Name: "sdc", Path: "/dev/sdc", Serial: "SN-x", Model: "M1"}}
	fl := newFakeLayout()
	failed := restoreFromRecovery(context.Background(), fl, rec, path, targets)
	if !reflect.DeepEqual(failed, []string{"bbbench_1_1"}) {
		t.Fatalf("failed %v: a layout with a member gone is never done", failed)
	}
	kept, err := rungroup.LoadRecovery(path)
	if err != nil || kept == nil || len(kept.Arrays) != 1 || kept.Arrays[0].Members[1].Serial != "SN-y" {
		t.Fatalf("recovery %+v, %v", kept, err)
	}
}

func TestOfferRecovery_ArraysDeclineRefusesStart(t *testing.T) {
	quietLogger(t)
	captureReport(t)
	path, _ := arrayRecovery(t)
	targets := map[string]settings.Target{
		"sdb": {Name: "sdb", Path: "/dev/sdb", Serial: "SN-x", Model: "M1"},
		"sdc": {Name: "sdc", Path: "/dev/sdc", Serial: "SN-y", Model: "M1"},
	}
	fl := newFakeLayout()
	var out strings.Builder
	err := offerRecovery(context.Background(), strings.NewReader("n\n"), &out, path, []settings.Setting{fl}, targets)
	if err == nil || !strings.Contains(err.Error(), "does not start") {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"mdadm bbbench_1_1 (UUID 0a:11): members sdb (data), sdc (spare)", "lvm bbbench_1_2: members sdb (data)", "Tear down these layouts and wipe their members now? [y/n] "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if len(fl.undone) != 0 || !recoveryExists(t, path) {
		t.Fatal("declining must not touch anything")
	}
	out.Reset()
	if err := offerRecovery(context.Background(), strings.NewReader("y\n"), &out, path, []settings.Setting{fl}, targets); err != nil {
		t.Fatal(err)
	}
	if len(fl.undone) != 2 || recoveryExists(t, path) {
		t.Errorf("undone %+v, file kept %v", fl.undone, recoveryExists(t, path))
	}
}

func TestPrintGroupSummary_Arrays(t *testing.T) {
	g := &rungroup.GroupFile{ID: "1", Setting: "storage layout", Strategy: rungroup.Sequential, Runs: []rungroup.GroupRun{{
		Status:   rungroup.RunDone,
		Teardown: &rungroup.GroupRestore{Status: rungroup.RestoreFailed, Failed: []string{"bbbench_1_1"}},
		Drives: []rungroup.GroupDrive{{Device: "md127", Value: "mdadm raid1", Kind: settings.TargetArray, Label: rungroup.Label{Valid: true},
			Members: []rungroup.Member{{Name: "sdb", Role: settings.RoleData}, {Name: "sdc", Role: settings.RoleSpare}}}},
	}}, Restore: rungroup.GroupRestore{Status: rungroup.RestoreFailed, Failed: []string{"bbbench_1_1"}}}
	var out strings.Builder
	printGroupSummary(&out, g)
	for _, want := range []string{"    md127      mdadm raid1  verified\n      members: sdb (data), sdc (spare)\n", "  run 1 teardown FAILED on: bbbench_1_1\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, out.String())
		}
	}
}

func TestLayoutDesc(t *testing.T) {
	if got := layoutDesc(settings.ArrayUndo{Backend: "mdadm", Name: "bbbench_1_1", UUID: "0a:b"}); got != "mdadm bbbench_1_1 (UUID 0a:b)" {
		t.Errorf("got %q", got)
	}
	if got := layoutDesc(settings.ArrayUndo{Backend: "lvm", Name: "bbbench_1_2"}); got != "lvm bbbench_1_2" {
		t.Errorf("got %q", got)
	}
}

// A layout group's summary speaks of tearing layouts down, not of restoring
// original values; a write-cache group's keeps its wording.
func TestPrintGroupSummary_LayoutRestoreWording(t *testing.T) {
	layoutGroup := func(st rungroup.GroupRestore) *rungroup.GroupFile {
		return &rungroup.GroupFile{ID: "1", Setting: "storage layout", Strategy: rungroup.Sequential,
			Variants: []rungroup.GroupVariant{{Value: "raid1 · 2 disks"}},
			Runs:     []rungroup.GroupRun{{Status: rungroup.RunDone, Teardown: &rungroup.GroupRestore{Status: rungroup.RestoreDone}}},
			Restore:  st}
	}
	for _, c := range []struct {
		g         *rungroup.GroupFile
		want, not string
	}{
		{layoutGroup(rungroup.GroupRestore{Status: rungroup.RestoreDone}), "  layouts torn down\n", "restored"},
		{layoutGroup(rungroup.GroupRestore{Status: rungroup.RestoreFailed, Failed: []string{"bbbench_1_1"}}), "  TEARDOWN FAILED on: bbbench_1_1\n", "RESTORE"},
		{&rungroup.GroupFile{ID: "2", Setting: "write_cache", Restore: rungroup.GroupRestore{Status: rungroup.RestoreDone}}, "  original values restored\n", "torn down"},
		{&rungroup.GroupFile{ID: "2", Setting: "write_cache", Restore: rungroup.GroupRestore{Status: rungroup.RestoreFailed, Failed: []string{"sda"}}}, "  RESTORE FAILED on: sda\n", "TEARDOWN"},
	} {
		var out strings.Builder
		printGroupSummary(&out, c.g)
		if !strings.Contains(out.String(), c.want) || strings.Contains(out.String(), c.not) {
			t.Errorf("summary wants %q, not %q:\n%s", c.want, c.not, out.String())
		}
	}
}
