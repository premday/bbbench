package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// fakeCall is one call the fake setting received.
type fakeCall struct {
	Op, Dev, Value string
	CtxErr         error // ctx.Err() when the call was made
}

// fakeSetting keeps a current value per device. Apply takes effect unless
// onApply says otherwise; Restore fails for the devices in restoreErr.
type fakeSetting struct {
	mu         sync.Mutex
	cur        map[string]string
	calls      []fakeCall
	onApply    func(dev string, v settings.Value) bool // nil: always effective
	restoreErr map[string]bool
	applyN     int
}

func newFakeSetting(cur map[string]string) *fakeSetting {
	return &fakeSetting{cur: cur, restoreErr: map[string]bool{}}
}

func (f *fakeSetting) Name() string             { return "fake cache" }
func (f *fakeSetting) Kind() settings.Kind      { return settings.Applied }
func (f *fakeSetting) Scope() settings.Scope    { return settings.ScopeDrive }
func (f *fakeSetting) Input() settings.Input    { return settings.InputValues }
func (f *fakeSetting) Values() []settings.Value { return fakeValues }
func (f *fakeSetting) Applicable(settings.Target) (bool, string) {
	return true, ""
}

var fakeValues = []settings.Value{{ID: "enabled", Label: "on"}, {ID: "disabled", Label: "off"}}

func (f *fakeSetting) Read(ctx context.Context, t settings.Target) (settings.Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{"read", t.Name, f.cur[t.Name], ctx.Err()})
	return settings.Reading{Value: f.cur[t.Name], Kernel: "write back", At: time.Now()}, nil
}

func (f *fakeSetting) Apply(ctx context.Context, t settings.Target, v settings.Value) error {
	f.mu.Lock()
	hook := f.onApply
	f.applyN++
	f.calls = append(f.calls, fakeCall{"apply", t.Name, v.ID, ctx.Err()})
	f.mu.Unlock()
	effective := hook == nil || hook(t.Name, v)
	if effective {
		f.set(t.Name, v.ID)
	}
	return nil
}

func (f *fakeSetting) Restore(ctx context.Context, t settings.Target, original settings.Reading) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{"restore", t.Name, original.Value, ctx.Err()})
	if original.Value == "" {
		return fmt.Errorf("no original for %s", t.Name)
	}
	if f.restoreErr[t.Name] {
		return fmt.Errorf("restore %s failed", t.Name)
	}
	f.cur[t.Name] = original.Value
	return nil
}

func (f *fakeSetting) set(dev, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cur[dev] = v
}

func (f *fakeSetting) get(dev string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur[dev]
}

func (f *fakeSetting) callsOf(op string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// fakeExec records each spec it is given and returns a result for it.
type fakeExec struct {
	mu    sync.Mutex
	specs []runSpec
	hook  func(ctx context.Context, n int, spec runSpec) error // nil: succeed
}

func (e *fakeExec) Execute(ctx context.Context, spec runSpec) (*BenchmarkResult, error) {
	e.mu.Lock()
	e.specs = append(e.specs, spec)
	n := len(e.specs)
	hook := e.hook
	e.mu.Unlock()
	var err error
	if hook != nil {
		err = hook(ctx, n, spec)
	}
	return &BenchmarkResult{ID: fmt.Sprintf("run_fake%d", n), GroupID: spec.GroupID, Labels: spec.Labels}, err
}

func quietLogger(t *testing.T) {
	old := logger
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() { logger = old })
}

func testDrives(names ...string) ([]DriveInfo, map[string]settings.Target) {
	var drives []DriveInfo
	targets := map[string]settings.Target{}
	for _, n := range names {
		d := blockdev.Device{Name: n, Path: "/dev/" + n, Serial: "SN-" + n, Model: "M1", Vendor: "V", CapacityBytes: 1 << 40}
		drives = append(drives, DriveInfo{Device: d})
		targets[n] = settings.Target{Name: n, Path: d.Path, Serial: d.Serial, Model: d.Model, Vendor: d.Vendor, Interface: "SATA"}
	}
	return drives, targets
}

// testGroup builds a config over drives whose current value is "enabled".
func testGroup(t *testing.T, strategy rungroup.Strategy, names ...string) (groupConfig, *fakeSetting, *fakeExec) {
	t.Helper()
	quietLogger(t)
	ResetStatus()
	t.Cleanup(ResetStatus)
	drives, targets := testDrives(names...)
	cur := map[string]string{}
	for _, n := range names {
		cur[n] = "enabled"
	}
	fs := newFakeSetting(cur)
	ex := &fakeExec{}
	dir := t.TempDir()
	cfg := groupConfig{
		Setting:      fs,
		Values:       fakeValues,
		Strategy:     strategy,
		Drives:       drives,
		Targets:      targets,
		OutDir:       dir,
		RecoveryPath: filepath.Join(dir, "recovery", "group-recovery.json"),
		Host:         "host1",
		Settle:       0,
		Execute:      ex.Execute,
	}
	return cfg, fs, ex
}

func loadSavedGroup(t *testing.T, dir string, id string) *rungroup.GroupFile {
	t.Helper()
	g, err := rungroup.LoadGroup(filepath.Join(dir, "group_"+id+"_group.json"))
	if err != nil {
		t.Fatalf("load group file: %v", err)
	}
	return g
}

func recoveryExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// assertRestoredLast checks that every device ends with a Restore to its
// original and that no Apply follows the first Restore.
func assertRestoredLast(t *testing.T, fs *fakeSetting, originals map[string]string) {
	t.Helper()
	fs.mu.Lock()
	calls := append([]fakeCall(nil), fs.calls...)
	fs.mu.Unlock()
	first := -1
	for i, c := range calls {
		if c.Op == "restore" && first < 0 {
			first = i
		}
		if c.Op == "apply" && first >= 0 {
			t.Errorf("apply %s=%s after restore started", c.Dev, c.Value)
		}
	}
	if first < 0 {
		t.Fatal("no restore call")
	}
	for dev, want := range originals {
		if got := fs.get(dev); got != want {
			t.Errorf("%s = %q after restore, want %q", dev, got, want)
		}
	}
}

func TestRunGroup_Sequential(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runGroup: %v", err)
	}
	if len(ex.specs) != 2 {
		t.Fatalf("Execute called %d times, want 2", len(ex.specs))
	}
	for i, want := range []string{"enabled", "disabled"} {
		spec := ex.specs[i]
		if spec.GroupID != g.ID {
			t.Errorf("run %d GroupID = %q, want %q", i, spec.GroupID, g.ID)
		}
		if len(spec.Drives) != 2 {
			t.Errorf("run %d has %d drives", i, len(spec.Drives))
		}
		for _, dev := range []string{"sdb", "sdc"} {
			l, ok := spec.Labels[dev]
			if !ok {
				t.Fatalf("run %d: no label for %s", i, dev)
			}
			if l.Value != want || l.Setting != fs.Name() {
				t.Errorf("run %d %s label = %+v, want value %q", i, dev, l, want)
			}
			if l.Before == nil || l.Before.Value != want {
				t.Errorf("run %d %s Before = %+v, want reading %q", i, dev, l.Before, want)
			}
		}
	}
	saved := loadSavedGroup(t, cfg.OutDir, g.ID)
	if len(saved.Runs) != 2 {
		t.Fatalf("saved %d runs, want 2", len(saved.Runs))
	}
	for i, r := range saved.Runs {
		if r.Status != rungroup.RunDone {
			t.Errorf("run %d status %q, want done", i, r.Status)
		}
		if r.RunID != fmt.Sprintf("run_fake%d", i+1) {
			t.Errorf("run %d RunID %q", i, r.RunID)
		}
		for _, d := range r.Drives {
			if !d.Label.Valid || d.Label.After == nil {
				t.Errorf("run %d %s label %+v, want valid with After", i, d.Device, d.Label)
			}
		}
	}
	if saved.Originals["sdb"].Value != "enabled" || saved.Originals["sdc"].Value != "enabled" {
		t.Errorf("originals = %+v", saved.Originals)
	}
	if saved.Restore.Status != "done" || len(saved.Restore.Failed) != 0 {
		t.Errorf("restore = %+v, want done", saved.Restore)
	}
	assertRestoredLast(t, fs, map[string]string{"sdb": "enabled", "sdc": "enabled"})
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file not removed after a successful restore")
	}
}

func TestRunGroup_Parallel(t *testing.T) {
	cfg, _, ex := testGroup(t, rungroup.Parallel, "sda", "sdb", "sdc", "sdd")
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runGroup: %v", err)
	}
	if len(ex.specs) != 1 {
		t.Fatalf("Execute called %d times, want 1", len(ex.specs))
	}
	spec := ex.specs[0]
	if len(spec.Drives) != 4 {
		t.Fatalf("run has %d drives, want 4", len(spec.Drives))
	}
	byValue := map[string][]string{}
	for dev, l := range spec.Labels {
		byValue[l.Value] = append(byValue[l.Value], dev)
	}
	for _, v := range []string{"enabled", "disabled"} {
		sort.Strings(byValue[v])
		if len(byValue[v]) != 2 {
			t.Errorf("value %s on %v, want two drives", v, byValue[v])
		}
	}
	if !reflect.DeepEqual(byValue["enabled"], []string{"sda", "sdb"}) {
		t.Errorf("enabled on %v", byValue["enabled"])
	}
	if g.Runs[0].Status != rungroup.RunDone {
		t.Errorf("status %q", g.Runs[0].Status)
	}
}

func TestRunGroup_BeforeFails(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	// In run 1 the value is "enabled", already the current value, so make
	// the drives start disabled and sdb ignore the first apply.
	fs.set("sdb", "disabled")
	fs.set("sdc", "disabled")
	first := true
	fs.onApply = func(dev string, v settings.Value) bool {
		if dev == "sdb" && first {
			first = false
			return false
		}
		return true
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runGroup: %v", err)
	}
	if g.Runs[0].Status != rungroup.RunNotRun {
		t.Errorf("run 1 status %q, want not_run", g.Runs[0].Status)
	}
	for _, d := range g.Runs[0].Drives {
		if d.Label.Before == nil {
			t.Errorf("run 1 %s: no Before reading stored", d.Device)
		}
		if d.Device == "sdb" && (d.Label.Valid || d.Label.Note == "") {
			t.Errorf("run 1 sdb label %+v, want invalid with a note", d.Label)
		}
	}
	if len(ex.specs) != 1 {
		t.Fatalf("Execute called %d times, want 1 (run 2 only)", len(ex.specs))
	}
	if ex.specs[0].Labels["sdb"].Value != "disabled" {
		t.Errorf("Execute ran with %+v, want run 2 (disabled)", ex.specs[0].Labels)
	}
	if g.Runs[1].Status != rungroup.RunDone {
		t.Errorf("run 2 status %q, want done", g.Runs[1].Status)
	}
	assertRestoredLast(t, fs, map[string]string{"sdb": "disabled", "sdc": "disabled"})
}

func TestRunGroup_AfterFails(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	cfg.Values = fakeValues[1:] // one run: disabled
	ex.hook = func(ctx context.Context, n int, spec runSpec) error {
		fs.set("sdb", "enabled") // the drive went back to its old value
		return nil
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runGroup: %v", err)
	}
	r := g.Runs[0]
	if r.Status != rungroup.RunInvalid {
		t.Fatalf("status %q, want invalid", r.Status)
	}
	for _, d := range r.Drives {
		wantValid := d.Device != "sdb"
		if d.Label.Valid != wantValid {
			t.Errorf("%s Valid = %v, want %v", d.Device, d.Label.Valid, wantValid)
		}
		if d.Label.After == nil {
			t.Errorf("%s: no After reading", d.Device)
		}
	}
	// The map given to Execute is shared with the live result: never changed.
	if l := ex.specs[0].Labels["sdb"]; !l.Valid || l.After != nil {
		t.Errorf("Execute's labels changed after the run: %+v", l)
	}
	// The run summary carries the after readings too.
	sum, err := os.ReadFile(filepath.Join(cfg.OutDir, "run_fake1_summary.json"))
	if err != nil {
		t.Fatalf("run summary not rewritten: %v", err)
	}
	if !strings.Contains(string(sum), `"after"`) {
		t.Errorf("summary has no after reading: %s", sum)
	}
}

func TestRunGroup_InterruptRestores(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex.hook = func(ctx2 context.Context, n int, spec runSpec) error {
		cancel()
		<-ctx2.Done()
		return errRunInterrupted
	}
	g, err := runGroup(ctx, cfg)
	if err == nil {
		t.Fatal("runGroup returned no error after an interrupt")
	}
	if len(ex.specs) != 1 {
		t.Errorf("Execute called %d times, want 1", len(ex.specs))
	}
	if g == nil {
		t.Fatal("no group file returned")
	}
	if g.Runs[0].Status != rungroup.RunInterrupted {
		t.Errorf("run 1 status %q, want interrupted", g.Runs[0].Status)
	}
	if g.Runs[1].Status != rungroup.RunPlanned {
		t.Errorf("run 2 status %q, want planned", g.Runs[1].Status)
	}
	assertRestoredLast(t, fs, map[string]string{"sdb": "enabled", "sdc": "enabled"})
	for _, c := range fs.callsOf("restore") {
		if c.CtxErr != nil {
			t.Errorf("restore of %s ran on a cancelled context", c.Dev)
		}
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file kept after a successful restore")
	}
	saved := loadSavedGroup(t, cfg.OutDir, g.ID)
	if saved.Runs[0].Status != rungroup.RunInterrupted || saved.Restore.Status != "done" {
		t.Errorf("saved run %q restore %+v", saved.Runs[0].Status, saved.Restore)
	}
}

func TestRunGroup_PanicRestores(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	ex.hook = func(ctx context.Context, n int, spec runSpec) error { panic("boom") }
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, want the panic to propagate", r)
			}
		}()
		runGroup(context.Background(), cfg)
	}()
	assertRestoredLast(t, fs, map[string]string{"sdb": "enabled", "sdc": "enabled"})
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file kept after a successful restore")
	}
	gs, _ := rungroup.LoadGroups(cfg.OutDir)
	if len(gs) != 1 || gs[0].Runs[0].Status != rungroup.RunInterrupted || gs[0].Restore.Status != "done" {
		t.Errorf("saved group after panic: %+v", gs)
	}
}

func TestRunGroup_ExecuteErrorStopsAndRestores(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	cfg.Execute = func(ctx context.Context, spec runSpec) (*BenchmarkResult, error) {
		ex.Execute(ctx, spec)
		return nil, errors.New("fio file missing")
	}
	g, err := runGroup(context.Background(), cfg)
	if err == nil {
		t.Fatal("no error")
	}
	if len(ex.specs) != 1 || g.Runs[0].Status != rungroup.RunInterrupted || g.Runs[1].Status != rungroup.RunPlanned {
		t.Errorf("execs %d, statuses %q %q", len(ex.specs), g.Runs[0].Status, g.Runs[1].Status)
	}
	assertRestoredLast(t, fs, map[string]string{"sdb": "enabled", "sdc": "enabled"})
}

func TestRunGroup_RestoreFailureOnOneDrive(t *testing.T) {
	cfg, fs, _ := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	fs.restoreErr["sdb"] = true
	g, err := runGroup(context.Background(), cfg)
	if err == nil {
		t.Error("a failed restore returned no error")
	}
	if fs.get("sdc") != "enabled" {
		t.Errorf("sdc = %q, want restored to enabled", fs.get("sdc"))
	}
	if g.Restore.Status != "failed" || !reflect.DeepEqual(g.Restore.Failed, []string{"sdb"}) {
		t.Errorf("restore = %+v, want failed [sdb]", g.Restore)
	}
	rec, lerr := rungroup.LoadRecovery(cfg.RecoveryPath)
	if lerr != nil || rec == nil {
		t.Fatalf("recovery file not kept: %v", lerr)
	}
	if len(rec.Drives) != 1 || rec.Drives[0].Name != "sdb" || rec.Drives[0].Original.Value != "enabled" {
		t.Errorf("recovery drives = %+v, want only sdb", rec.Drives)
	}
	saved := loadSavedGroup(t, cfg.OutDir, g.ID)
	if saved.Restore.Status != "failed" {
		t.Errorf("saved restore %+v", saved.Restore)
	}
}

func TestRunGroup_WritesRecoveryBeforeApply(t *testing.T) {
	cfg, fs, _ := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	checked := false
	fs.onApply = func(dev string, v settings.Value) bool {
		if checked {
			return true
		}
		checked = true
		rec, err := rungroup.LoadRecovery(cfg.RecoveryPath)
		if err != nil || rec == nil {
			t.Errorf("first apply: recovery file missing (%v)", err)
			return true
		}
		got := map[string]string{}
		for _, d := range rec.Drives {
			got[d.Name] = d.Original.Value
			if d.Serial != "SN-"+d.Name || d.Model != "M1" {
				t.Errorf("recovery entry %+v lacks serial/model", d)
			}
		}
		if !reflect.DeepEqual(got, map[string]string{"sdb": "enabled", "sdc": "enabled"}) {
			t.Errorf("recovery originals %v", got)
		}
		return true
	}
	if _, err := runGroup(context.Background(), cfg); err != nil {
		t.Fatalf("runGroup: %v", err)
	}
	if !checked {
		t.Fatal("no apply call")
	}
}

func TestRunGroup_UnwritableOutDirFailsBeforeApply(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	file := filepath.Join(cfg.OutDir, "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.OutDir = filepath.Join(file, "sub")
	if _, err := runGroup(context.Background(), cfg); err == nil {
		t.Fatal("no error for an unwritable output directory")
	}
	if n := len(fs.callsOf("apply")); n != 0 {
		t.Errorf("%d apply calls, want none", n)
	}
	if len(ex.specs) != 0 {
		t.Error("Execute called")
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file left behind although nothing was applied")
	}
}

func TestRunGroup_UnwritableRecoveryFailsBeforeApply(t *testing.T) {
	cfg, fs, _ := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	file := filepath.Join(cfg.OutDir, "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.RecoveryPath = filepath.Join(file, "sub", "rec.json")
	if _, err := runGroup(context.Background(), cfg); err == nil {
		t.Fatal("no error for an unwritable recovery path")
	}
	if n := len(fs.callsOf("apply")); n != 0 {
		t.Errorf("%d apply calls, want none", n)
	}
}

func TestRunGroup_ExistingRecoveryRefused(t *testing.T) {
	cfg, fs, _ := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	old := &rungroup.Recovery{Setting: fs.Name(), Drives: []rungroup.RecoveryDrive{{Serial: "SN-sdb", Model: "M1", Name: "sdb", Original: settings.Reading{Value: "disabled"}}}}
	if err := os.MkdirAll(filepath.Dir(cfg.RecoveryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := old.Save(cfg.RecoveryPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runGroup(context.Background(), cfg); err == nil {
		t.Fatal("runGroup overwrote an existing recovery file")
	}
	if n := len(fs.callsOf("apply")); n != 0 {
		t.Errorf("%d apply calls, want none", n)
	}
	rec, _ := rungroup.LoadRecovery(cfg.RecoveryPath)
	if rec == nil || rec.Drives[0].Original.Value != "disabled" {
		t.Errorf("existing recovery file changed: %+v", rec)
	}
}

func TestRunGroup_NoDrives(t *testing.T) {
	cfg, fs, _ := testGroup(t, rungroup.Sequential, "sdb")
	cfg.Drives = nil
	if _, err := runGroup(context.Background(), cfg); err == nil {
		t.Fatal("no error for zero drives")
	}
	if len(fs.calls) != 0 {
		t.Errorf("setting touched: %+v", fs.calls)
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file written")
	}
}

func TestRunGroup_ResumeSkipsFinishedRuns(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	stale := getStateFilePath(cfg.OutDir)
	if err := os.WriteFile(stale, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	doneRun := rungroup.GroupRun{RunID: "run_old", Round: 0, Status: rungroup.RunDone, Drives: []rungroup.GroupDrive{
		{Device: "sdb", Serial: "SN-sdb", Model: "M1", Value: "enabled", Label: rungroup.Label{Setting: fs.Name(), Value: "enabled", Valid: true}},
		{Device: "sdc", Serial: "SN-sdc", Model: "M1", Value: "enabled", Label: rungroup.Label{Setting: fs.Name(), Value: "enabled", Valid: true}},
	}}
	cfg.Resume = &rungroup.GroupFile{
		ID: "1700000000", Host: "host1", Setting: fs.Name(), Strategy: rungroup.Sequential,
		Values:    []string{"enabled", "disabled"},
		Originals: map[string]settings.Reading{"sdb": {Value: "enabled"}, "sdc": {Value: "enabled"}},
		Runs: []rungroup.GroupRun{
			doneRun,
			{RunID: "run_broken", Round: 1, Status: rungroup.RunInterrupted},
		},
	}
	executed := false
	ex.hook = func(ctx context.Context, n int, spec runSpec) error {
		executed = true
		if _, err := os.Stat(stale); err == nil {
			t.Error("stale orchestrator state file present when the restarted run executes")
		}
		return nil
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runGroup: %v", err)
	}
	if !executed || len(ex.specs) != 1 {
		t.Fatalf("Execute called %d times, want 1", len(ex.specs))
	}
	if v := ex.specs[0].Labels["sdb"].Value; v != "disabled" {
		t.Errorf("executed run with value %q, want disabled (run 2)", v)
	}
	applies := fs.callsOf("apply")
	if len(applies) != 2 {
		t.Errorf("applies %+v, want one per drive for run 2", applies)
	}
	for _, a := range applies {
		if a.Value != "disabled" {
			t.Errorf("apply %+v, want disabled", a)
		}
	}
	if ex.specs[0].Labels["sdb"].Before == nil {
		t.Error("run 2 not verified before")
	}
	if g.ID != "1700000000" {
		t.Errorf("group ID %q, want the resumed one", g.ID)
	}
	saved := loadSavedGroup(t, cfg.OutDir, g.ID)
	if !reflect.DeepEqual(saved.Runs[0], doneRun) {
		t.Errorf("run 1 changed:\n got %+v\nwant %+v", saved.Runs[0], doneRun)
	}
	if saved.Runs[1].Status != rungroup.RunDone || saved.Runs[1].RunID != "run_fake1" {
		t.Errorf("run 2 = %+v", saved.Runs[1])
	}
	assertRestoredLast(t, fs, map[string]string{"sdb": "enabled", "sdc": "enabled"})
}

func TestRunGroup_OnSaveCalled(t *testing.T) {
	cfg, _, _ := testGroup(t, rungroup.Sequential, "sdb")
	var saves []rungroup.RunStatus
	cfg.OnSave = func(g *rungroup.GroupFile) { saves = append(saves, g.Runs[len(g.Runs)-1].Status) }
	if _, err := runGroup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(saves) < 3 || saves[len(saves)-1] != rungroup.RunDone {
		t.Errorf("OnSave calls %v, want one per save ending with done", saves)
	}
}

func TestDriveRefs(t *testing.T) {
	drives, _ := testDrives("sda", "sdb", "sdc")
	refs := driveRefs(drives, map[string]string{"sda": "FW1"})
	want := []rungroup.DriveRef{
		{Name: "sda", Serial: "SN-sda", Model: "M1", Vendor: "V", Firmware: "FW1", CapacityBytes: 1 << 40},
		{Name: "sdb", Serial: "SN-sdb", Model: "M1", Vendor: "V", CapacityBytes: 1 << 40},
		{Name: "sdc", Serial: "SN-sdc", Model: "M1", Vendor: "V", CapacityBytes: 1 << 40},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("driveRefs =\n%+v\nwant\n%+v", refs, want)
	}
}

func TestRestoreFromRecovery_MatchesBySerial(t *testing.T) {
	quietLogger(t)
	drives, targets := testDrives("sdc")
	drives[0].Device.Serial = "S1"
	targets["sdc"] = settings.Target{Name: "sdc", Path: "/dev/sdc", Serial: "S1", Model: "M1"}
	fs := newFakeSetting(map[string]string{"sdc": "disabled"})
	rec := &rungroup.Recovery{Setting: fs.Name(), Drives: []rungroup.RecoveryDrive{
		{Serial: "S1", Model: "M1", Name: "sdb", Original: settings.Reading{Value: "enabled"}},
		{Serial: "S9", Model: "M1", Name: "sdx", Original: settings.Reading{Value: "disabled"}},
	}}
	failed := restoreFromRecovery(context.Background(), fs, rec, drives, targets)
	restores := fs.callsOf("restore")
	if len(restores) != 1 || restores[0].Dev != "sdc" || restores[0].Value != "enabled" {
		t.Errorf("restores %+v, want sdc to enabled", restores)
	}
	if fs.get("sdc") != "enabled" {
		t.Errorf("sdc = %q", fs.get("sdc"))
	}
	if !reflect.DeepEqual(failed, []string{"sdx"}) {
		t.Errorf("failed = %v, want [sdx]", failed)
	}
}

func TestRestoreFromRecovery_VerifyFailureReported(t *testing.T) {
	quietLogger(t)
	drives, targets := testDrives("sdb")
	fs := newFakeSetting(map[string]string{"sdb": "disabled"})
	fs.restoreErr["sdb"] = true
	rec := &rungroup.Recovery{Setting: fs.Name(), Drives: []rungroup.RecoveryDrive{
		{Serial: "SN-sdb", Model: "M1", Name: "sdb", Original: settings.Reading{Value: "enabled"}},
	}}
	if failed := restoreFromRecovery(context.Background(), fs, rec, drives, targets); !reflect.DeepEqual(failed, []string{"sdb"}) {
		t.Errorf("failed = %v, want [sdb]", failed)
	}
}

func TestStatus_GroupFields(t *testing.T) {
	cfg, _, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	var seen []BenchmarkStatusSnapshot
	ex.hook = func(ctx context.Context, n int, spec runSpec) error {
		seen = append(seen, GetStatus())
		return nil
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("%d snapshots", len(seen))
	}
	for i, st := range seen {
		if st.Group == nil {
			t.Fatalf("run %d: no group in status", i)
		}
		gs := st.Group
		if gs.ID != g.ID || gs.Setting != "fake cache" || gs.Strategy != rungroup.Sequential || gs.Run != i+1 || gs.Runs != 2 || gs.Step != "running" {
			t.Errorf("run %d group status %+v", i, *gs)
		}
		if len(st.Drives) != 2 {
			t.Fatalf("run %d: %d drives in status", i, len(st.Drives))
		}
		for _, d := range st.Drives {
			if d.Setting == nil {
				t.Fatalf("run %d %s: no setting", i, d.Name)
			}
			want := fakeValues[i].ID
			if d.Setting.Value != want || d.Setting.Before == nil || d.Setting.Before.Value != want || d.Setting.After != nil {
				t.Errorf("run %d %s setting %+v", i, d.Name, *d.Setting)
			}
		}
	}
	end := GetStatus()
	if end.Group == nil || end.Group.Step != "done" {
		t.Errorf("final group status %+v", end.Group)
	}
	for _, d := range end.Drives {
		if d.Setting == nil || d.Setting.After == nil || d.Setting.Valid == nil || !*d.Setting.Valid {
			t.Errorf("final %s setting %+v", d.Name, d.Setting)
		}
	}
}

func TestRunGroup_SignalRestores(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	ex.hook = func(ctx context.Context, n int, spec runSpec) error {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Error("SIGINT did not cancel the group context")
		}
		return errRunInterrupted
	}
	g, err := runGroup(context.Background(), cfg)
	if err == nil || g.Runs[0].Status != rungroup.RunInterrupted {
		t.Errorf("err %v, status %q", err, g.Runs[0].Status)
	}
	assertRestoredLast(t, fs, map[string]string{"sdb": "enabled", "sdc": "enabled"})
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file kept")
	}
}

// panicSetting panics when restoring one drive.
type panicSetting struct {
	*fakeSetting
	dev string
}

func (p panicSetting) Restore(ctx context.Context, t settings.Target, original settings.Reading) error {
	if t.Name == p.dev {
		panic("restore bug")
	}
	return p.fakeSetting.Restore(ctx, t, original)
}

func TestRunGroup_RestorePanicOnOneDrive(t *testing.T) {
	cfg, fs, _ := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	cfg.Setting = panicSetting{fs, "sdb"}
	g, err := runGroup(context.Background(), cfg)
	if err == nil {
		t.Error("no error")
	}
	if fs.get("sdc") != "enabled" {
		t.Errorf("sdc = %q, want restored", fs.get("sdc"))
	}
	if g.Restore.Status != "failed" || !reflect.DeepEqual(g.Restore.Failed, []string{"sdb"}) {
		t.Errorf("restore %+v", g.Restore)
	}
	if !recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file removed although sdb was not restored")
	}
}

// hinterSetting adds a manual restore command to the fake setting.
type hinterSetting struct {
	*fakeSetting
	cmd func(t settings.Target, original settings.Reading) string
}

func (h hinterSetting) RestoreCommand(t settings.Target, original settings.Reading) string {
	return h.cmd(t, original)
}

// captureReport sends restore failure reports to a buffer.
func captureReport(t *testing.T) *strings.Builder {
	t.Helper()
	var b strings.Builder
	old := restoreReportOut
	restoreReportOut = &b
	t.Cleanup(func() { restoreReportOut = old })
	return &b
}

func TestRunGroup_RestoreFailureReportsCommand(t *testing.T) {
	cfg, fs, _ := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	out := captureReport(t)
	fs.restoreErr["sdb"] = true
	cfg.Setting = hinterSetting{fs, func(tg settings.Target, o settings.Reading) string {
		return "fixit --value=" + o.Value + " " + tg.Path
	}}
	runGroup(context.Background(), cfg)
	rep := out.String()
	for _, want := range []string{"RESTORE FAILED", "sdb", "SN-sdb", `"enabled"`, "to set it back by hand: fixit --value=enabled /dev/sdb"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	if strings.Contains(rep, "sdc") {
		t.Errorf("sdc reported although restored:\n%s", rep)
	}
}

func TestRunGroup_RestoreFailureReportsWithoutHinter(t *testing.T) {
	cfg, fs, _ := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	out := captureReport(t)
	fs.restoreErr["sdb"] = true
	runGroup(context.Background(), cfg)
	rep := out.String()
	for _, want := range []string{"RESTORE FAILED", "sdb", "SN-sdb", `"enabled"`} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	if strings.Contains(rep, "by hand:") {
		t.Errorf("command line printed without a hinter:\n%s", rep)
	}
}

func TestRunGroup_ReportPanicDoesNotStopRestore(t *testing.T) {
	cfg, fs, _ := testGroup(t, rungroup.Sequential, "sdb", "sdc", "sdd")
	captureReport(t)
	fs.restoreErr["sdb"] = true
	cfg.Setting = hinterSetting{fs, func(tg settings.Target, o settings.Reading) string { panic("report bug") }}
	g, err := runGroup(context.Background(), cfg)
	if err == nil {
		t.Error("no error")
	}
	for _, dev := range []string{"sdc", "sdd"} {
		if fs.get(dev) != "enabled" {
			t.Errorf("%s = %q, want restored", dev, fs.get(dev))
		}
	}
	if g.Restore.Status != "failed" || !reflect.DeepEqual(g.Restore.Failed, []string{"sdb"}) {
		t.Errorf("restore %+v", g.Restore)
	}
	if !recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file removed")
	}
}

// An interrupted run is kept but excluded: its labels say not valid, with a
// note, in the group file and in the rewritten run summary.
func TestRunGroup_InterruptedRunLabelsInvalid(t *testing.T) {
	cfg, _, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex.hook = func(ctx2 context.Context, n int, spec runSpec) error {
		cancel()
		<-ctx2.Done()
		return errRunInterrupted
	}
	g, err := runGroup(ctx, cfg)
	if err == nil {
		t.Fatal("no error after an interrupt")
	}
	saved := loadSavedGroup(t, cfg.OutDir, g.ID)
	r := saved.Runs[0]
	if r.Status != rungroup.RunInterrupted || len(r.Drives) != 2 {
		t.Fatalf("run 1 = %+v", r)
	}
	for _, d := range r.Drives {
		if d.Label.Valid || !strings.Contains(d.Label.Note, "interrupted") {
			t.Errorf("%s label %+v, want invalid with an interrupted note", d.Device, d.Label)
		}
	}
	sum, err := os.ReadFile(filepath.Join(cfg.OutDir, "run_fake1_summary.json"))
	if err != nil {
		t.Fatalf("run summary not rewritten: %v", err)
	}
	if strings.Contains(string(sum), `"valid": true`) || !strings.Contains(string(sum), "interrupted") {
		t.Errorf("summary labels not marked interrupted: %s", sum)
	}
}

// A closed terminal (SIGHUP) must restore the drives like Ctrl+C.
func TestRunGroup_SIGHUPRestores(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	ex.hook = func(ctx context.Context, n int, spec runSpec) error {
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Error("SIGHUP did not cancel the group context")
		}
		return errRunInterrupted
	}
	g, err := runGroup(context.Background(), cfg)
	if err == nil || g.Runs[0].Status != rungroup.RunInterrupted {
		t.Errorf("err %v, status %q", err, g.Runs[0].Status)
	}
	assertRestoredLast(t, fs, map[string]string{"sdb": "enabled", "sdc": "enabled"})
}

// A resume whose drives now carry other names (sda and sdb swapped after a
// reboot) would give the recorded values to the wrong drives: refused.
func TestRunGroup_ResumeRefusesSwappedDrives(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Parallel, "sda", "sdb")
	cfg.Resume = &rungroup.GroupFile{
		ID: "1700000000", Host: "host1", Setting: fs.Name(), Strategy: rungroup.Parallel,
		Values:    []string{"enabled", "disabled"},
		Originals: map[string]settings.Reading{"sda": {Value: "enabled"}, "sdb": {Value: "enabled"}},
		Runs: []rungroup.GroupRun{{RunID: "run_old", Round: 0, Status: rungroup.RunInterrupted, Drives: []rungroup.GroupDrive{
			{Device: "sda", Serial: "SN-sdb", Model: "M1", Value: "enabled"},
			{Device: "sdb", Serial: "SN-sda", Model: "M1", Value: "disabled"},
		}}},
	}
	_, err := runGroup(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "SN-") {
		t.Fatalf("err = %v, want a refusal naming the drives", err)
	}
	if n := len(fs.callsOf("apply")); n != 0 || len(ex.specs) != 0 {
		t.Errorf("%d applies, %d runs; want none", n, len(ex.specs))
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file written")
	}
}

// The same drives under the same names resume.
func TestRunGroup_ResumeAcceptsSameDrives(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Parallel, "sda", "sdb")
	cfg.Resume = &rungroup.GroupFile{
		ID: "1700000000", Host: "host1", Setting: fs.Name(), Strategy: rungroup.Parallel,
		Values: []string{"enabled", "disabled"},
		Runs: []rungroup.GroupRun{{RunID: "run_old", Round: 0, Status: rungroup.RunInterrupted, Drives: []rungroup.GroupDrive{
			{Device: "sda", Serial: "SN-sda", Model: "M1", Value: "enabled"},
			{Device: "sdb", Serial: "SN-sdb", Model: "M1", Value: "disabled"},
		}}},
	}
	if _, err := runGroup(context.Background(), cfg); err != nil {
		t.Fatalf("runGroup: %v", err)
	}
	if len(ex.specs) != 1 {
		t.Errorf("%d runs, want 1", len(ex.specs))
	}
}

// A missing fio (or another environment check) stops the group before any
// drive is touched.
func TestRunGroup_PrecheckFailsBeforeApply(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb", "sdc")
	cfg.Precheck = func() error { return errors.New("fio is not installed") }
	if _, err := runGroup(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "fio is not installed") {
		t.Fatalf("err = %v", err)
	}
	if n := len(fs.callsOf("apply")) + len(fs.callsOf("read")); n != 0 || len(ex.specs) != 0 {
		t.Errorf("setting touched (%d calls) or run executed (%d)", n, len(ex.specs))
	}
	if recoveryExists(t, cfg.RecoveryPath) {
		t.Error("recovery file written")
	}
}

// runGroup's own watcher: the first signal cancels, a further one says the
// drives are being restored, and after stop nothing is printed any more.
func TestWatchGroupSignals(t *testing.T) {
	quietLogger(t)
	sigs := make(chan os.Signal, 1)
	var mu sync.Mutex
	var out strings.Builder
	w := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return out.Write(p) })
	ctx, stop := watchGroupSignals(context.Background(), sigs, w)
	sigs <- syscall.SIGHUP
	<-ctx.Done()
	sigs <- os.Interrupt
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := out.String()
		mu.Unlock()
		if strings.Contains(got, "restoring") || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	mu.Lock()
	n := strings.Count(out.String(), "this signal is ignored")
	mu.Unlock()
	if n != 1 {
		t.Fatalf("message printed %d times, want 1", n)
	}
	select {
	case sigs <- os.Interrupt:
	default:
	}
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if c := strings.Count(out.String(), "this signal is ignored"); c != 1 {
		t.Errorf("message printed after stop (%d times)", c)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestResumeIgnoredNote(t *testing.T) {
	if n := resumeIgnoredNote(false, newFakeSetting(nil)); n != "" {
		t.Errorf("note without --resume: %q", n)
	}
	if n := resumeIgnoredNote(true, nil); n != "" {
		t.Errorf("note for a single run: %q", n)
	}
	if n := resumeIgnoredNote(true, newFakeSetting(nil)); !strings.Contains(n, "--resume") || !strings.Contains(n, "state file") {
		t.Errorf("note = %q", n)
	}
}

// A resumed group does not retry a run that was not run.
func TestRunGroup_ResumeSkipsNotRun(t *testing.T) {
	cfg, fs, ex := testGroup(t, rungroup.Sequential, "sdb")
	cfg.Resume = &rungroup.GroupFile{
		ID: "1700000000", Host: "host1", Setting: fs.Name(), Strategy: rungroup.Sequential,
		Values: []string{"enabled", "disabled"},
		Runs: []rungroup.GroupRun{
			{Round: 0, Status: rungroup.RunNotRun, Drives: []rungroup.GroupDrive{{Device: "sdb", Serial: "SN-sdb", Model: "M1", Value: "enabled"}}},
			{Round: 1, Status: rungroup.RunInterrupted},
		},
	}
	g, err := runGroup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.specs) != 1 || ex.specs[0].Labels["sdb"].Value != "disabled" {
		t.Errorf("runs %+v, want only run 2", ex.specs)
	}
	if g.Runs[0].Status != rungroup.RunNotRun {
		t.Errorf("run 1 = %q", g.Runs[0].Status)
	}
}
