package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// groupConfig is a run group ready to execute: the setting, its values, how
// they are spread over the drives, and how to run the test plan once.
type groupConfig struct {
	Setting  settings.Setting
	Values   []settings.Value
	Strategy rungroup.Strategy
	Drives   []DriveInfo
	Targets  map[string]settings.Target // keyed by device name
	OutDir   string
	// RecoveryPath is where the original values are kept while the group
	// runs (removed after a verified restore).
	RecoveryPath string
	Host         string
	// Settle is how long a read-back may take to show an applied value.
	Settle time.Duration
	// Resume is a group file to continue: finished runs (done, invalid, not
	// run) are skipped, every other run is executed again from its first
	// phase.
	Resume *rungroup.GroupFile
	// Precheck, when non-nil, checks what a run needs (fio, the fio files,
	// the engines) before any drive is read or changed.
	Precheck func() error
	// Execute runs the test plan once (executeRun in production).
	Execute func(ctx context.Context, spec runSpec) (*BenchmarkResult, error)
	// OnSave, when non-nil, is called after every save of the group file.
	OnSave func(*rungroup.GroupFile)
	// Variants are the layouts of a layout group (a LifetimeRun setting),
	// one run each, in order; nil for a group of values.
	Variants []settings.Variant
	// MakeDrive turns a composite target built by a layout into the drive
	// a run benchmarks; nil refuses composite targets.
	MakeDrive func(bt settings.BenchTarget) (DriveInfo, error)
}

// driveRefs describes the drives for planning; Firmware is the drive's own
// firmware from firmware (device -> revision), empty when unknown.
func driveRefs(drives []DriveInfo, firmware map[string]string) []rungroup.DriveRef {
	refs := make([]rungroup.DriveRef, 0, len(drives))
	for _, d := range drives {
		ref := rungroup.DriveRef{
			Name:          d.Device.Name,
			Serial:        d.Device.Serial,
			Model:         d.Device.Model,
			Vendor:        d.Device.Vendor,
			CapacityBytes: d.Device.CapacityBytes,
			Firmware:      firmware[d.Device.Name],
		}
		refs = append(refs, ref)
	}
	return refs
}

// runGroup runs the test plan once per run of the group: it applies each
// run's values, verifies them, executes, verifies again and saves the group
// file after every run. The original values are written to the recovery file
// before anything is applied, and restored (on a background context, then
// verified) when runGroup returns for any reason: normal end, error, a
// cancelled ctx, SIGINT/SIGTERM/SIGHUP, or a panic, which is re-raised afterwards.
// The recovery file is removed only when every drive was restored; otherwise
// it is kept with the drives that failed, and the error says so.
//
// A layout group (a LifetimeRun setting, cfg.Variants) runs one layout per
// run (runLayoutOne): each is prepared right before it is built, recorded in
// the recovery file before the build and torn down after its run. Whatever
// is still built when runGroup returns is torn down like a restore.
func runGroup(ctx context.Context, cfg groupConfig) (g *rungroup.GroupFile, err error) {
	if len(cfg.Drives) == 0 {
		return nil, errors.New("run group: no drives")
	}
	if cfg.Setting == nil || cfg.Execute == nil {
		return nil, errors.New("run group: no setting or no way to execute a run")
	}
	s := cfg.Setting
	if s.Kind() != settings.Applied {
		return nil, fmt.Errorf("run group: %s cannot be applied by bbbench", s.Name())
	}
	layoutGroup := false
	switch s.Lifetime() {
	case settings.LifetimeGroup:
		if cfg.Variants != nil {
			return nil, fmt.Errorf("run group: %s takes values, not layouts", s.Name())
		}
	case settings.LifetimeRun:
		if len(cfg.Variants) == 0 {
			return nil, fmt.Errorf("run group: %s needs at least one layout", s.Name())
		}
		layoutGroup = true
	default:
		return nil, fmt.Errorf("run group: %s has the unknown lifetime %q", s.Name(), s.Lifetime())
	}
	if cfg.RecoveryPath == "" {
		return nil, errors.New("run group: no recovery file path")
	}
	if layoutGroup {
		if err := checkLayoutDisks(cfg); err != nil {
			return nil, err
		}
	} else {
		identities := map[string]string{}
		for _, d := range cfg.Drives {
			name := d.Device.Name
			if _, ok := cfg.Targets[name]; !ok {
				return nil, fmt.Errorf("run group: no target for %s", name)
			}
			// The recovery file finds drives by serial and model: two drives
			// it cannot tell apart could be restored to each other's value.
			id := d.Device.Serial + "\x00" + d.Device.Model
			if other, dup := identities[id]; dup {
				return nil, fmt.Errorf("run group: %s and %s have the same serial and model; they cannot be told apart for restoring", other, name)
			}
			identities[id] = name
		}
	}
	if cfg.Precheck != nil {
		if err := cfg.Precheck(); err != nil {
			return nil, fmt.Errorf("run group: %w (nothing was changed)", err)
		}
	}
	values := map[string]settings.Value{}
	var ids []string
	var plans []rungroup.RunPlan
	if layoutGroup {
		rvs := make([]rungroup.Variant, 0, len(cfg.Variants))
		for _, v := range cfg.Variants {
			names := make([]string, 0, len(v.Disks))
			for _, d := range v.Disks {
				names = append(names, d.Name)
			}
			rvs = append(rvs, rungroup.Variant{Value: v.Value.ID, Drives: names})
			ids = append(ids, v.Value.ID)
		}
		plans = rungroup.PlanVariants(rvs)
	} else {
		ids = make([]string, 0, len(cfg.Values))
		for _, v := range cfg.Values {
			values[v.ID] = v
			ids = append(ids, v.ID)
		}
		plans, err = rungroup.PlanRuns(driveRefs(cfg.Drives, nil), ids, cfg.Strategy)
		if err != nil {
			return nil, fmt.Errorf("run group: %w", err)
		}
	}
	g, err = newGroupFile(cfg, plans, ids)
	if err != nil {
		return nil, err
	}

	// A signal anywhere in the group (also between runs, when no
	// coordinator listens) cancels ctx, so the restore below runs. Stopped
	// only after the restore, so a signal during the restore is not fatal.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	ctx, stopWatch := watchGroupSignals(ctx, sigChan, os.Stdout)
	defer func() {
		stopWatch()
		signal.Stop(sigChan)
	}()

	// A recovery file from an earlier group must be restored first.
	if existing, lerr := rungroup.LoadRecovery(cfg.RecoveryPath); lerr != nil || existing != nil {
		if lerr != nil {
			return nil, fmt.Errorf("run group: check the recovery file %s: %w", cfg.RecoveryPath, lerr)
		}
		return nil, fmt.Errorf("run group: a recovery file from an earlier group exists (%s); restore it first", cfg.RecoveryPath)
	}

	ledger := newUndoLedger(cfg.RecoveryPath, s.Name(), nil)
	if layoutGroup {
		// Nothing is prepared here: each layout is prepared (with its in-use
		// re-check) and recorded right before it is built.
		if err := saveGroup(cfg, g); err != nil {
			return nil, fmt.Errorf("run group: write the group file: %w", err)
		}
	} else {
		// The originals, read before anything changes.
		all := settings.Variant{Name: g.ID, Disks: groupDisks(cfg, cfg.Drives)}
		undos, err := s.Prepare(ctx, all)
		if err != nil {
			return nil, fmt.Errorf("run group: %w", err)
		}
		g.Originals = map[string]settings.Reading{}
		for _, u := range undos {
			if u.Original == nil {
				continue
			}
			name, r := u.Name(), *u.Original
			if cfg.Resume != nil {
				if was, ok := cfg.Resume.Originals[name]; ok && was.Value != r.Value {
					logger.Warn("resumed group: drive value differs from the recorded original", "device", name, "now", r.Value, "recorded", was.Value)
				}
			}
			g.Originals[name] = r
		}

		// The recovery file, then the group file, both before the first apply.
		if err := ledger.add(undos...); err != nil {
			return nil, fmt.Errorf("run group: %w", err)
		}
		if err := saveGroup(cfg, g); err != nil {
			if rerr := (&rungroup.Recovery{}).Remove(cfg.RecoveryPath); rerr != nil {
				logger.Warn("remove recovery file", "path", cfg.RecoveryPath, "err", rerr)
			}
			return nil, fmt.Errorf("run group: write the group file: %w", err)
		}
	}

	// From here on the restore always runs. held are the layouts whose
	// teardown already failed: kept for the recovery file, not retried.
	held := map[*settings.ArrayUndo]bool{}
	var current *rungroup.GroupRun
	lastRun := 0 // 1-based number of the run reached, for the status page
	defer func() {
		p := recover()
		if current != nil && current.Status == rungroup.RunRunning {
			current.Status = rungroup.RunInterrupted
			for i := range current.Drives {
				current.Drives[i].Label = interruptedLabel(current.Drives[i].Label)
			}
		}
		err = errors.Join(err, finishGroup(cfg, g, ledger, lastRun, len(plans), held))
		if p != nil {
			panic(p)
		}
	}()

	for i, plan := range plans {
		run := &g.Runs[i]
		if run.Status.Finished() {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return g, fmt.Errorf("run group interrupted before run %d of %d: %w", i+1, len(plans), cerr)
		}
		current, lastRun = run, i+1
		var rerr error
		if layoutGroup {
			rerr = runLayoutOne(ctx, cfg, g, ledger, held, i, len(plans), plan)
		} else {
			rerr = runOne(ctx, cfg, g, i, len(plans), plan, values)
		}
		if rerr != nil {
			return g, rerr
		}
		current = nil
	}
	return g, nil
}

// watchGroupSignals returns ctx cancelled by the first signal from sigs;
// every later signal (or any signal once ctx ended otherwise) is answered on
// out with groupSignalMessage and otherwise ignored. stop ends the watching
// (the caller then stops the delivery to sigs), so nothing is printed after
// the group, during the final wait for Ctrl+C.
func watchGroupSignals(parent context.Context, sigs <-chan os.Signal, out io.Writer) (ctx context.Context, stop func()) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-sigs:
				if ctx.Err() == nil {
					cancel()
					continue
				}
				logger.Warn("signal ignored during a run group: restoring the original settings")
				fmt.Fprintln(out, groupSignalMessage)
			case <-done:
				return
			}
		}
	}()
	return ctx, func() {
		close(done)
		wg.Wait()
		cancel()
	}
}

// newGroupFile starts the group file, or continues cfg.Resume when it
// matches the plan.
func newGroupFile(cfg groupConfig, plans []rungroup.RunPlan, ids []string) (*rungroup.GroupFile, error) {
	if r := cfg.Resume; r != nil {
		layout := cfg.Variants != nil
		if r.Setting != cfg.Setting.Name() || r.Strategy != cfg.Strategy || (!layout && !slices.Equal(r.Values, ids)) {
			return nil, fmt.Errorf("run group: group %s does not match the setting, values or drives to resume", r.ID)
		}
		if layout {
			if err := sameVariants(r.Variants, cfg.Variants); err != nil {
				return nil, fmt.Errorf("run group: cannot resume group %s: %w", r.ID, err)
			}
		}
		if len(r.Runs) != len(plans) {
			return nil, fmt.Errorf("run group: group %s does not match the setting, values or drives to resume", r.ID)
		}
		serialOf := map[string]string{}
		for _, d := range cfg.Drives {
			serialOf[d.Device.Name] = d.Device.Serial
		}
		g := *r
		g.Runs = make([]rungroup.GroupRun, len(r.Runs))
		for i, run := range r.Runs {
			if run.Round != plans[i].Round {
				return nil, fmt.Errorf("run group: run %d of group %s does not match the plan", i+1, r.ID)
			}
			if !layout {
				if err := sameDrivesPerValue(run, plans[i], serialOf); err != nil {
					return nil, fmt.Errorf("run group: cannot resume group %s: run %d %w (were the drives renamed?)", r.ID, i+1, err)
				}
			}
			run.Drives = slices.Clone(run.Drives)
			g.Runs[i] = run
		}
		g.Restore = rungroup.GroupRestore{Status: rungroup.RestorePending}
		return &g, nil
	}
	now := time.Now()
	g := &rungroup.GroupFile{
		ID:       strconv.FormatInt(now.Unix(), 10),
		Created:  now,
		Host:     cfg.Host,
		Setting:  cfg.Setting.Name(),
		Strategy: cfg.Strategy,
		Values:   ids,
		Runs:     make([]rungroup.GroupRun, len(plans)),
		Restore:  rungroup.GroupRestore{Status: rungroup.RestorePending},
	}
	for i, p := range plans {
		g.Runs[i] = rungroup.GroupRun{Round: p.Round, Status: rungroup.RunPlanned}
	}
	for _, v := range cfg.Variants {
		g.Variants = append(g.Variants, rungroup.GroupVariant{Value: v.Value.ID, Disks: rungroup.MembersOf(v.Disks)})
	}
	return g, nil
}

// sameVariants checks that a resumed layout group has the layouts given now:
// the same number, and per index the same value and the same disks (serial,
// model and role, in order). Device names are not compared: they can change
// across reboots.
func sameVariants(recorded []rungroup.GroupVariant, now []settings.Variant) error {
	for i := range max(len(recorded), len(now)) {
		if i >= len(recorded) {
			return fmt.Errorf("layout %d differs (the group has %d layouts, %d are given)", i+1, len(recorded), len(now))
		}
		if i >= len(now) {
			return fmt.Errorf("layout %d differs (the group has %d layouts, %d are given)", i+1, len(recorded), len(now))
		}
		r, v := recorded[i], now[i]
		if r.Value != v.Value.ID {
			return fmt.Errorf("layout %d differs (%q, now %q)", i+1, r.Value, v.Value.ID)
		}
		if len(r.Disks) != len(v.Disks) {
			return fmt.Errorf("layout %d differs (%d disks, now %d)", i+1, len(r.Disks), len(v.Disks))
		}
		for k, d := range v.Disks {
			m := r.Disks[k]
			if m.Serial != d.Serial || m.Model != d.Model || m.Role != d.Role {
				return fmt.Errorf("layout %d differs (disk %d was %s S/N %s as %s, now %s S/N %s as %s)", i+1, k+1, m.Model, m.Serial, m.Role, d.Model, d.Serial, d.Role)
			}
		}
	}
	return nil
}

// checkLayoutDisks checks the disks of every layout: each is one of the
// group's drives, and the disks (over all layouts) can be told apart by
// serial and model, which the recovery file uses to find them again.
func checkLayoutDisks(cfg groupConfig) error {
	driveOf := map[string]DriveInfo{}
	for _, d := range cfg.Drives {
		driveOf[d.Device.Name] = d
	}
	identities := map[string]string{} // serial+model -> name
	idOf := map[string]string{}       // name -> serial+model
	for i, v := range cfg.Variants {
		if len(v.Disks) == 0 {
			return fmt.Errorf("run group: layout %d (%s) has no disks", i+1, v.Value.ID)
		}
		for _, d := range v.Disks {
			if _, ok := driveOf[d.Name]; !ok {
				return fmt.Errorf("run group: %s of layout %d is not among the drives of the group", d.Name, i+1)
			}
			if d.Serial == "" {
				return fmt.Errorf("run group: %s has no serial; it could not be found again for a teardown", d.Name)
			}
			id := d.Serial + "\x00" + d.Model
			if was, ok := idOf[d.Name]; ok && was != id {
				return fmt.Errorf("run group: %s is two different disks in the layouts", d.Name)
			}
			if other, dup := identities[id]; dup && other != d.Name {
				return fmt.Errorf("run group: %s and %s have the same serial and model; they cannot be told apart for a teardown", other, d.Name)
			}
			identities[id], idOf[d.Name] = d.Name, id
		}
	}
	return nil
}

// sameDrivesPerValue checks that a recorded run gave each value to the drives
// (by serial) the plan gives it now: the plan works on device names, which
// can change across reboots. A run that recorded no drives is not checked.
func sameDrivesPerValue(run rungroup.GroupRun, plan rungroup.RunPlan, serialOf map[string]string) error {
	if len(run.Drives) == 0 {
		return nil
	}
	recorded := map[string][]string{}
	for _, d := range run.Drives {
		recorded[d.Value] = append(recorded[d.Value], d.Serial)
	}
	planned := map[string][]string{}
	for _, v := range plan.Variants {
		for _, name := range v.Drives {
			planned[v.Value] = append(planned[v.Value], serialOf[name])
		}
	}
	values := map[string]bool{}
	for v := range recorded {
		values[v] = true
	}
	for v := range planned {
		values[v] = true
	}
	ids := make([]string, 0, len(values))
	for v := range values {
		ids = append(ids, v)
	}
	slices.Sort(ids)
	for _, v := range ids {
		was, now := slices.Sorted(slices.Values(recorded[v])), slices.Sorted(slices.Values(planned[v]))
		if !slices.Equal(was, now) {
			return fmt.Errorf("ran %s on S/N %s, the plan now gives it to S/N %s", v, strings.Join(was, ", "), strings.Join(now, ", "))
		}
	}
	return nil
}

// runOne applies, verifies, executes and verifies run idx. It returns an
// error when the group must stop (interrupt, failed execution, group file
// not saved); a run that is not started or is invalid is not an error.
func runOne(ctx context.Context, cfg groupConfig, g *rungroup.GroupFile, idx, total int, plan rungroup.RunPlan, values map[string]settings.Value) error {
	s := cfg.Setting
	run := &g.Runs[idx]
	valueOf := map[string]string{}
	for _, v := range plan.Variants {
		for _, name := range v.Drives {
			valueOf[name] = v.Value
		}
	}
	var drives []DriveInfo
	for _, d := range cfg.Drives {
		if _, ok := valueOf[d.Device.Name]; ok {
			drives = append(drives, d)
		}
	}
	labels := map[string]rungroup.Label{}
	for _, d := range drives {
		name := d.Device.Name
		labels[name] = rungroup.Label{Setting: s.Name(), Value: valueOf[name]}
	}
	record := func() {
		run.Drives = make([]rungroup.GroupDrive, 0, len(drives))
		for _, d := range drives {
			name := d.Device.Name
			run.Drives = append(run.Drives, rungroup.GroupDrive{Device: name, Serial: d.Device.Serial, Model: d.Device.Model, Value: valueOf[name], Label: labels[name]})
		}
	}
	step := func(st string) { setGroupStatus(g, idx+1, total, st) }
	// interrupted marks the run interrupted: kept, but never valid (the
	// setting was not verified after it), also in its run summary.
	interrupted := func(res *BenchmarkResult) {
		run.Status = rungroup.RunInterrupted
		for name, l := range labels {
			l = interruptedLabel(l)
			labels[name] = l
			setDriveSetting(name, l, true)
		}
		record()
		if res != nil {
			saveRunLabels(res, labels, cfg.OutDir)
		}
	}

	run.Status = rungroup.RunRunning
	record()
	step(stepApplying)
	UpdateStatus(func(st *BenchmarkStatus) {
		st.Running = true
		st.Drives = newDriveStatuses(drives, nil)
	})
	for name, l := range labels {
		setDriveSetting(name, l, false)
	}
	if err := saveGroup(cfg, g); err != nil {
		return fmt.Errorf("run %d of %d: save the group file: %w", idx+1, total, err)
	}
	fmt.Printf("\nRun group: run %d of %d (%s)\n", idx+1, total, describeRun(plan))

	// One variant per value of the plan, its disks in drive order.
	bench := map[string]settings.BenchTarget{} // keyed by target name
	for _, v := range runVariants(cfg, g.ID, idx, plan, drives, values) {
		ts, err := s.Apply(ctx, v, nil)
		if err != nil {
			// The whole variant failed: each of its disks carries the error.
			ts = ts[:0]
			for _, d := range v.Disks {
				ts = append(ts, settings.BenchTarget{Kind: settings.TargetDrive, Target: d.Target, Want: v.Value.ID, Err: err})
			}
		}
		for _, bt := range ts {
			bench[bt.Target.Name] = bt
		}
	}

	rb := &runBench{cfg: cfg, g: g, idx: idx, total: total, run: run, drives: drives, bench: bench, labels: labels, record: record, interrupted: interrupted, step: step}
	return rb.verifyAndExecute(ctx)
}

// runBench is one run once its setting is applied: the drives to execute
// (in order), their bench targets and labels (keyed by device name), and how
// the run records itself. verifyAndExecute is the same for values and
// layouts.
type runBench struct {
	cfg         groupConfig
	g           *rungroup.GroupFile
	idx, total  int
	run         *rungroup.GroupRun
	drives      []DriveInfo
	bench       map[string]settings.BenchTarget
	labels      map[string]rungroup.Label
	record      func()
	interrupted func(res *BenchmarkResult)
	step        func(string)
}

// verifyAndExecute verifies every target before, executes the run when all
// are effective, verifies after and labels the results (Policy). It returns
// an error when the group must stop (interrupt, failed execution, group file
// not saved); a run that is not started or is invalid is not an error.
func (rb *runBench) verifyAndExecute(ctx context.Context) error {
	cfg, g, idx, total, run := rb.cfg, rb.g, rb.idx, rb.total, rb.run
	drives, bench, labels, record, interrupted, step := rb.drives, rb.bench, rb.labels, rb.record, rb.interrupted, rb.step
	s := cfg.Setting
	step(stepVerifyingBefore)
	before := map[string]bool{}
	for _, d := range drives {
		name := d.Device.Name
		l := labels[name]
		var r settings.Reading
		var ok bool
		bt, applied := bench[name]
		if !applied {
			bt = settings.BenchTarget{Kind: settings.TargetDrive, Target: cfg.Targets[name], Want: l.Value, Err: errors.New("no target to benchmark")}
			bench[name] = bt
		}
		if aerr := bt.Err; aerr != nil {
			if rr, rerr := s.Read(ctx, bt); rerr == nil {
				r = rr
			}
			l.Note = fmt.Sprintf("apply failed: %v", aerr)
		} else if r, ok = settings.Verify(ctx, s, bt, cfg.Settle); !ok {
			l.Note = fmt.Sprintf("setting not effective: reads %q, want %q", r.Value, bt.Want)
		}
		if !r.At.IsZero() {
			l.Before = &r
		}
		before[name] = ok
		labels[name] = l
		setDriveSetting(name, l, false)
	}
	if err := ctx.Err(); err != nil {
		interrupted(nil)
		return fmt.Errorf("run %d of %d interrupted: %w", idx+1, total, err)
	}
	if st, _ := rungroup.Policy(before, nil); st == rungroup.RunNotRun {
		run.Status = rungroup.RunNotRun
		record()
		for _, d := range drives {
			if l := labels[d.Device.Name]; l.Note != "" {
				fmt.Printf("  %s: %s\n", d.Device.Name, l.Note)
			}
		}
		fmt.Printf("Run %d of %d not run: setting not effective\n", idx+1, total)
		if err := saveGroup(cfg, g); err != nil {
			return fmt.Errorf("run %d of %d: save the group file: %w", idx+1, total, err)
		}
		return nil
	}

	// Every drive verified: valid until verified otherwise after the run.
	// Execute gets its own map, never changed afterwards (the live result
	// keeps it and the web view reads it).
	execLabels := make(map[string]rungroup.Label, len(labels))
	for name, l := range labels {
		l.Valid = true
		labels[name] = l
		execLabels[name] = l
	}
	record()
	if cfg.Resume != nil {
		// A restarted run starts from its first phase: a stale per-run state
		// file must not make it skip phases.
		if err := os.Remove(getStateFilePath(cfg.OutDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("run %d of %d: remove the stale state file: %w", idx+1, total, err)
		}
	}

	step(stepRunning)
	res, xerr := cfg.Execute(ctx, runSpec{Drives: drives, Labels: execLabels, GroupID: g.ID})
	if res != nil {
		run.RunID = res.ID
	}
	if cerr := ctx.Err(); cerr != nil || errors.Is(xerr, errRunInterrupted) || res == nil {
		interrupted(res)
		cause := errors.Join(xerr, cerr)
		if cause == nil {
			cause = errors.New("no result (dry run?)")
		}
		return fmt.Errorf("run %d of %d stopped: %w", idx+1, total, cause)
	}
	if xerr != nil {
		// The run completed; a later step (display) failed.
		logger.Warn("run group: run finished with an error", "run", idx+1, "err", xerr)
	}

	step(stepVerifyingAfter)
	after := map[string]bool{}
	for _, d := range drives {
		name := d.Device.Name
		l := labels[name]
		r, ok := settings.Verify(ctx, s, bench[name], cfg.Settle)
		if !r.At.IsZero() {
			l.After = &r
		}
		after[name] = ok
		labels[name] = l
	}
	if err := ctx.Err(); err != nil {
		// Unverified: cannot tell valid from invalid.
		interrupted(res)
		return fmt.Errorf("run %d of %d interrupted while verifying: %w", idx+1, total, err)
	}
	status, valid := rungroup.Policy(before, after)
	for name, l := range labels {
		l.Valid = valid == nil || valid[name]
		if !l.Valid {
			got := ""
			if l.After != nil {
				got = l.After.Value
			}
			l.Note = fmt.Sprintf("setting changed during the run: reads %q, want %q", got, l.Value)
			fmt.Printf("  %s: %s; its results are invalid\n", name, l.Note)
		}
		labels[name] = l
		setDriveSetting(name, l, true)
	}
	run.Status = status
	record()
	saveRunLabels(res, labels, cfg.OutDir)
	if err := saveGroup(cfg, g); err != nil {
		return fmt.Errorf("run %d of %d: save the group file: %w", idx+1, total, err)
	}
	fmt.Printf("Run %d of %d: %s\n", idx+1, total, status)
	return nil
}

// runVariants are the variants of run idx of group id: one per value of
// plan, named "<group id>_<run n>", with the plan's drives (in the order of
// drives) as data disks.
func runVariants(cfg groupConfig, id string, idx int, plan rungroup.RunPlan, drives []DriveInfo, values map[string]settings.Value) []settings.Variant {
	out := make([]settings.Variant, 0, len(plan.Variants))
	for _, pv := range plan.Variants {
		var picked []DriveInfo
		for _, d := range drives {
			if slices.Contains(pv.Drives, d.Device.Name) {
				picked = append(picked, d)
			}
		}
		out = append(out, settings.Variant{Name: id + "_" + strconv.Itoa(idx+1), Value: values[pv.Value], Disks: groupDisks(cfg, picked)})
	}
	return out
}

// interruptedNote explains why the results of an interrupted run are excluded.
const interruptedNote = "run interrupted, setting not verified after the run"

// interruptedLabel is l for an interrupted run: not valid, with the note
// (any earlier note, such as a failed apply, is kept after it).
func interruptedLabel(l rungroup.Label) rungroup.Label {
	l.Valid = false
	switch {
	case l.Note == "":
		l.Note = interruptedNote
	case !strings.HasPrefix(l.Note, interruptedNote):
		l.Note = interruptedNote + "; " + l.Note
	}
	return l
}

// describeRun says which value goes on which drives.
func describeRun(p rungroup.RunPlan) string {
	parts := make([]string, 0, len(p.Variants))
	for _, v := range p.Variants {
		parts = append(parts, v.Value+" on "+strings.Join(v.Drives, ", "))
	}
	return strings.Join(parts, "; ")
}

// finishGroup undoes every record (each restored drive is verified),
// records the result in the group file and removes the recovery file, or
// keeps it with only the records that failed. Layouts in held already failed
// their teardown: they are not retried, and count as failed.
func finishGroup(cfg groupConfig, g *rungroup.GroupFile, ledger *undoLedger, run, total int, held map[*settings.ArrayUndo]bool) error {
	layouts := cfg.Setting.Lifetime() == settings.LifetimeRun
	setGroupStatus(g, run, total, stepRestoring)
	if layouts {
		fmt.Println("\nRun group: tearing down any layout still built")
	} else {
		fmt.Println("\nRun group: restoring the original values")
	}
	isHeld := func(u settings.Undo) bool { return u.Array != nil && held[u.Array] }
	failed, ferr := ledger.undo(context.Background(), cfg.Setting, func(u settings.Undo) bool { return !isHeld(u) })
	var heldNames []string
	for _, u := range ledger.pending() {
		if isHeld(u) {
			heldNames = append(heldNames, u.Name())
		}
	}
	failed = append(heldNames, failed...)
	var errs []error
	if len(failed) == 0 {
		g.Restore = rungroup.GroupRestore{Status: rungroup.RestoreDone}
		if ferr != nil {
			errs = append(errs, fmt.Errorf("remove the recovery file: %w", ferr))
		}
		if layouts {
			fmt.Println("No layout is left built.")
		} else {
			fmt.Println("Original values restored and verified.")
		}
	} else {
		g.Restore = rungroup.GroupRestore{Status: rungroup.RestoreFailed, Failed: failed}
		if ferr != nil {
			errs = append(errs, fmt.Errorf("update the recovery file: %w", ferr))
		}
		what := "restore"
		if layouts {
			what = "teardown"
		}
		errs = append(errs, fmt.Errorf("%s failed on %s; the recovery file %s is kept", what, strings.Join(failed, ", "), cfg.RecoveryPath))
	}
	if err := saveGroup(cfg, g); err != nil {
		errs = append(errs, fmt.Errorf("save the group file: %w", err))
	}
	setGroupStatus(g, run, total, stepDone)
	UpdateStatus(func(s *BenchmarkStatus) { s.Running = false })
	return errors.Join(errs...)
}

// groupDisks are the data disks of drives, with their targets.
func groupDisks(cfg groupConfig, drives []DriveInfo) []settings.Disk {
	disks := make([]settings.Disk, 0, len(drives))
	for _, d := range drives {
		disks = append(disks, settings.Disk{Target: cfg.Targets[d.Device.Name], Role: settings.RoleData})
	}
	return disks
}

// saveGroup writes the group file and tells OnSave.
func saveGroup(cfg groupConfig, g *rungroup.GroupFile) error {
	if err := g.Save(cfg.OutDir); err != nil {
		return err
	}
	if cfg.OnSave != nil {
		cfg.OnSave(g)
	}
	return nil
}

// saveRunLabels rewrites the run summary with the labels verified after the
// run. The stored result is replaced by a copy, never changed in place.
func saveRunLabels(res *BenchmarkResult, labels map[string]rungroup.Label, outDir string) {
	cp := *res
	cp.Labels = make(map[string]rungroup.Label, len(labels))
	for k, v := range labels {
		cp.Labels[k] = v
	}
	dir := cp.OutputDir
	if dir == "" {
		dir = outDir
	}
	data, err := json.MarshalIndent(&cp, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, cp.ID+"_summary.json"), data, 0o644)
	}
	if err != nil {
		logger.Warn("run group: save the verified labels in the run summary", "run", cp.ID, "err", err)
	}
	if _, ok := globalResultsStore.GetResult(cp.ID); ok {
		globalResultsStore.AddResult(&cp)
	}
}

// setGroupStatus puts the group's position on the status page.
func setGroupStatus(g *rungroup.GroupFile, run, runs int, step string) {
	setGroupStep(g, run, runs, step, "", 0)
}

// setGroupStep is setGroupStatus with the current layout's label and the
// resync progress (layout groups).
func setGroupStep(g *rungroup.GroupFile, run, runs int, step, variant string, progress float64) {
	gs := &GroupStatus{ID: g.ID, Setting: g.Setting, Strategy: g.Strategy, Run: run, Runs: runs, Step: step, Variant: variant, Progress: progress}
	UpdateStatus(func(s *BenchmarkStatus) { s.Group = gs })
}

// setDriveSetting puts a drive's label on the status page; verified says
// whether Valid is known (after the run).
func setDriveSetting(device string, l rungroup.Label, verified bool) {
	ds := &DriveSetting{Value: l.Value, Before: copyReading(l.Before), After: copyReading(l.After)}
	if verified {
		v := l.Valid
		ds.Valid = &v
	}
	markDrive(device, func(d *DriveStatus) { d.Setting = ds })
}

func copyReading(r *settings.Reading) *settings.Reading {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// runLayoutOne runs layout idx: it prepares it (the in-use re-check runs
// here, after the previous layout was torn down), records it in the recovery
// file, builds it, waits until it is steady, benchmarks its targets like a
// run of values (verifyAndExecute) and tears it down. The order is the
// safety contract: nothing is built that is not recorded first, and nothing
// else is built while a teardown failed.
//
// A run that cannot be built or benchmarked is not run (with the reason as
// its note) and is torn down before the group moves on. An interrupted run
// returns at once; the deferred finishGroup tears down what the ledger holds.
// A failed teardown stops the group; its layout stays in the recovery file
// and in held.
func runLayoutOne(ctx context.Context, cfg groupConfig, g *rungroup.GroupFile, ledger *undoLedger, held map[*settings.ArrayUndo]bool, idx, total int, plan rungroup.RunPlan) error {
	s := cfg.Setting
	run := &g.Runs[idx]
	v := cfg.Variants[idx]
	v.Name = g.ID + "_" + strconv.Itoa(idx+1)
	v.Disks = slices.Clone(v.Disks)
	driveOf := map[string]DriveInfo{}
	for _, d := range cfg.Drives {
		driveOf[d.Device.Name] = d
	}
	diskDrives := make([]DriveInfo, 0, len(v.Disks))
	for _, d := range v.Disks {
		diskDrives = append(diskDrives, driveOf[d.Name])
	}
	stepAt := func(st string, pct float64) { setGroupStep(g, idx+1, total, st, v.Value.Label, pct) }
	step := func(st string) { stepAt(st, 0) }

	// The run records the variant's disks until there are targets, then
	// the targets.
	type benchRow struct {
		di DriveInfo
		bt settings.BenchTarget
	}
	var rows []benchRow
	labels := map[string]rungroup.Label{}
	for _, d := range v.Disks {
		labels[d.Name] = rungroup.Label{Setting: s.Name(), Value: v.Value.ID}
	}
	record := func() {
		if rows == nil {
			run.Drives = make([]rungroup.GroupDrive, 0, len(v.Disks))
			for _, d := range v.Disks {
				run.Drives = append(run.Drives, rungroup.GroupDrive{Device: d.Name, Serial: d.Serial, Model: d.Model, Value: v.Value.ID, Label: labels[d.Name]})
			}
			return
		}
		run.Drives = make([]rungroup.GroupDrive, 0, len(rows))
		for _, r := range rows {
			name := r.di.Device.Name
			gd := rungroup.GroupDrive{Device: name, Serial: r.di.Device.Serial, Model: r.di.Device.Model, Value: v.Value.ID, Label: labels[name]}
			if r.bt.Kind == settings.TargetArray {
				gd.Kind, gd.UUID, gd.Members = settings.TargetArray, r.bt.UUID, rungroup.MembersOf(r.bt.Members)
			}
			run.Drives = append(run.Drives, gd)
		}
	}
	interrupted := func(res *BenchmarkResult) {
		run.Status = rungroup.RunInterrupted
		for name, l := range labels {
			l = interruptedLabel(l)
			labels[name] = l
			setDriveSetting(name, l, true)
		}
		record()
		if res != nil {
			saveRunLabels(res, labels, cfg.OutDir)
		}
	}
	stopped := func(what string, err error) error {
		interrupted(nil)
		return fmt.Errorf("run %d of %d interrupted %s: %w", idx+1, total, what, err)
	}

	run.Status = rungroup.RunRunning
	run.Teardown = nil
	record()
	step(stepBuilding)
	UpdateStatus(func(st *BenchmarkStatus) {
		st.Running = true
		st.Drives = newDriveStatuses(diskDrives, nil)
	})
	for name, l := range labels {
		setDriveSetting(name, l, false)
	}
	if err := saveGroup(cfg, g); err != nil {
		return fmt.Errorf("run %d of %d: save the group file: %w", idx+1, total, err)
	}
	fmt.Printf("\nRun group: run %d of %d (%s)\n", idx+1, total, describeRun(plan))

	// This run's records; teardown undoes exactly these.
	var undos []settings.Undo
	teardown := func() error {
		step(stepTearingDown)
		mine := map[*settings.ArrayUndo]bool{}
		for _, u := range undos {
			if u.Array != nil {
				mine[u.Array] = true
			}
		}
		failed, ferr := ledger.undo(context.Background(), s, func(u settings.Undo) bool { return u.Array != nil && mine[u.Array] })
		if len(failed) > 0 {
			var descs []string
			for _, u := range ledger.pending() {
				if u.Array != nil && mine[u.Array] {
					held[u.Array] = true
					descs = append(descs, layoutDesc(*u.Array))
				}
			}
			run.Teardown = &rungroup.GroupRestore{Status: rungroup.RestoreFailed, Failed: failed}
			var serr error
			if err := saveGroup(cfg, g); err != nil {
				serr = fmt.Errorf("save the group file: %w", err)
			}
			if ferr != nil {
				ferr = fmt.Errorf("update the recovery file: %w", ferr)
			}
			return errors.Join(fmt.Errorf("run %d of %d: teardown of %s failed; the group stops so that no further layout is built on disks still assembled", idx+1, total, strings.Join(descs, ", ")), ferr, serr)
		}
		run.Teardown = &rungroup.GroupRestore{Status: rungroup.RestoreDone}
		if err := saveGroup(cfg, g); err != nil {
			return fmt.Errorf("run %d of %d: save the group file: %w", idx+1, total, err)
		}
		if ferr != nil {
			// The file may still list a layout already torn down: stop
			// before another one is recorded next to it.
			return fmt.Errorf("run %d of %d: update the recovery file after the teardown: %w", idx+1, total, ferr)
		}
		return nil
	}
	notRun := func(note string) error {
		for name, l := range labels {
			l.Note, l.Valid = note, false
			labels[name] = l
			setDriveSetting(name, l, true)
		}
		run.Status = rungroup.RunNotRun
		record()
		fmt.Printf("  %s\nRun %d of %d not run\n", note, idx+1, total)
		if err := saveGroup(cfg, g); err != nil {
			return errors.Join(fmt.Errorf("run %d of %d: save the group file: %w", idx+1, total, err), teardown())
		}
		return teardown()
	}

	// Prepared right before the build: the in-use re-check sees the disks
	// as they are now, after the previous layout was torn down.
	prepared, err := s.Prepare(ctx, v)
	if cerr := ctx.Err(); cerr != nil {
		return stopped("before the build", cerr)
	}
	if err != nil {
		return notRun("not built: " + err.Error())
	}
	// Recorded before the build: a crash from here on leaves the record.
	if len(prepared) > 0 {
		if err := ledger.add(prepared...); err != nil {
			return fmt.Errorf("run %d of %d: record %s before building it: %w (nothing was built)", idx+1, total, v.Value.ID, err)
		}
		undos = prepared
	}

	ts, aerr := s.Apply(ctx, v, prepared)
	if cerr := ctx.Err(); cerr != nil {
		return stopped("while building", cerr)
	}
	if aerr != nil {
		return notRun("build failed: " + aerr.Error())
	}
	if len(ts) == 0 {
		return notRun("build failed: no target to benchmark")
	}
	if st, ok := s.(settings.Settler); ok {
		step(stepResyncing)
		werr := st.WaitSteady(ctx, ts, func(pct float64) { stepAt(stepResyncing, pct) })
		if cerr := ctx.Err(); cerr != nil {
			return stopped("while resyncing", cerr)
		}
		if werr != nil {
			return notRun("resync failed: " + werr.Error())
		}
	}

	// The drives to benchmark: a composite through MakeDrive, a drive as
	// it is in the group.
	built := make([]benchRow, 0, len(ts))
	for _, bt := range ts {
		name := bt.Target.Name
		var di DriveInfo
		var derr error
		switch {
		case bt.Kind == settings.TargetArray && cfg.MakeDrive == nil:
			derr = errors.New("no way to benchmark a composite target")
		case bt.Kind == settings.TargetArray:
			if di, derr = cfg.MakeDrive(bt); derr == nil {
				if di.Device.Name != name {
					derr = fmt.Errorf("its drive is named %q", di.Device.Name)
				}
				if len(di.Members) == 0 {
					di.Members = rungroup.MembersOf(bt.Members)
				}
				if di.UUID == "" {
					di.UUID = bt.UUID
				}
			}
		default:
			d, ok := driveOf[name]
			if !ok {
				derr = errors.New("not among the drives of the group")
			}
			di = d
		}
		if derr != nil {
			return notRun(fmt.Sprintf("cannot benchmark %s: %v", name, derr))
		}
		built = append(built, benchRow{di: di, bt: bt})
	}
	rows = built
	drives := make([]DriveInfo, 0, len(rows))
	bench := map[string]settings.BenchTarget{}
	for k := range labels {
		delete(labels, k)
	}
	for _, r := range rows {
		name := r.di.Device.Name
		labels[name] = rungroup.Label{Setting: s.Name(), Value: v.Value.ID}
		bench[name] = r.bt
		drives = append(drives, r.di)
	}
	record()
	UpdateStatus(func(st *BenchmarkStatus) { st.Drives = newDriveStatuses(drives, nil) })
	for name, l := range labels {
		setDriveSetting(name, l, false)
	}

	rb := &runBench{cfg: cfg, g: g, idx: idx, total: total, run: run, drives: drives, bench: bench, labels: labels, record: record, interrupted: interrupted, step: step}
	if err := rb.verifyAndExecute(ctx); err != nil {
		return err
	}
	return teardown()
}
