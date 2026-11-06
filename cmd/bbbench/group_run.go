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

// restoreTimeout bounds restoring one drive (its commands and the read-back).
const restoreTimeout = 2 * time.Minute

// recoverySettle is how long a restore from the recovery file waits for the
// read-back to show the original value.
const recoverySettle = 10 * time.Second

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
	// Resume is a group file to continue: runs already done or invalid are
	// skipped, every other run is executed again from its first phase.
	Resume *rungroup.GroupFile
	// Precheck, when non-nil, checks what a run needs (fio, the fio files,
	// the engines) before any drive is read or changed.
	Precheck func() error
	// Execute runs the test plan once (executeRun in production).
	Execute func(ctx context.Context, spec runSpec) (*BenchmarkResult, error)
	// OnSave, when non-nil, is called after every save of the group file.
	OnSave func(*rungroup.GroupFile)
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
	if cfg.RecoveryPath == "" {
		return nil, errors.New("run group: no recovery file path")
	}
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
	if cfg.Precheck != nil {
		if err := cfg.Precheck(); err != nil {
			return nil, fmt.Errorf("run group: %w (nothing was changed)", err)
		}
	}
	values := map[string]settings.Value{}
	ids := make([]string, 0, len(cfg.Values))
	for _, v := range cfg.Values {
		values[v.ID] = v
		ids = append(ids, v.ID)
	}
	plans, err := rungroup.PlanRuns(driveRefs(cfg.Drives, nil), ids, cfg.Strategy)
	if err != nil {
		return nil, fmt.Errorf("run group: %w", err)
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

	// The originals, read before anything changes.
	rec := &rungroup.Recovery{Setting: s.Name()}
	g.Originals = map[string]settings.Reading{}
	for _, d := range cfg.Drives {
		name := d.Device.Name
		r, rerr := s.Read(ctx, cfg.Targets[name])
		if rerr != nil {
			return nil, fmt.Errorf("run group: read the original %s of %s: %w", s.Name(), name, rerr)
		}
		if r.Value == "" {
			return nil, fmt.Errorf("run group: the %s of %s read back empty", s.Name(), name)
		}
		if cfg.Resume != nil {
			if was, ok := cfg.Resume.Originals[name]; ok && was.Value != r.Value {
				logger.Warn("resumed group: drive value differs from the recorded original", "device", name, "now", r.Value, "recorded", was.Value)
			}
		}
		g.Originals[name] = r
		rec.Drives = append(rec.Drives, rungroup.RecoveryDrive{Serial: d.Device.Serial, Model: d.Device.Model, Name: name, Original: r})
	}

	// The recovery file, then the group file, both before the first apply.
	if existing, lerr := rungroup.LoadRecovery(cfg.RecoveryPath); lerr != nil || existing != nil {
		if lerr != nil {
			return nil, fmt.Errorf("run group: check the recovery file %s: %w", cfg.RecoveryPath, lerr)
		}
		return nil, fmt.Errorf("run group: a recovery file from an earlier group exists (%s); restore it first", cfg.RecoveryPath)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.RecoveryPath), 0o755); err != nil {
		return nil, fmt.Errorf("run group: create the recovery directory: %w", err)
	}
	if err := rec.Save(cfg.RecoveryPath); err != nil {
		return nil, fmt.Errorf("run group: write the recovery file: %w", err)
	}
	if err := saveGroup(cfg, g); err != nil {
		if rerr := rec.Remove(cfg.RecoveryPath); rerr != nil {
			logger.Warn("remove recovery file", "path", cfg.RecoveryPath, "err", rerr)
		}
		return nil, fmt.Errorf("run group: write the group file: %w", err)
	}

	// From here on the restore always runs.
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
		err = errors.Join(err, finishGroup(cfg, g, rec, lastRun, len(plans)))
		if p != nil {
			panic(p)
		}
	}()

	for i, plan := range plans {
		run := &g.Runs[i]
		if run.Status == rungroup.RunDone || run.Status == rungroup.RunInvalid {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return g, fmt.Errorf("run group interrupted before run %d of %d: %w", i+1, len(plans), cerr)
		}
		current, lastRun = run, i+1
		if rerr := runOne(ctx, cfg, g, i, len(plans), plan, values); rerr != nil {
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
		if r.Setting != cfg.Setting.Name() || r.Strategy != cfg.Strategy || !slices.Equal(r.Values, ids) || len(r.Runs) != len(plans) {
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
			if err := sameDrivesPerValue(run, plans[i], serialOf); err != nil {
				return nil, fmt.Errorf("run group: cannot resume group %s: run %d %w (were the drives renamed?)", r.ID, i+1, err)
			}
			run.Drives = slices.Clone(run.Drives)
			g.Runs[i] = run
		}
		g.Restore = rungroup.GroupRestore{Status: "pending"}
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
		Restore:  rungroup.GroupRestore{Status: "pending"},
	}
	for i, p := range plans {
		g.Runs[i] = rungroup.GroupRun{Round: p.Round, Status: rungroup.RunPlanned}
	}
	return g, nil
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

	applyErr := map[string]error{}
	for _, d := range drives {
		name := d.Device.Name
		if err := s.Apply(ctx, cfg.Targets[name], values[valueOf[name]]); err != nil {
			applyErr[name] = err
		}
	}

	step(stepVerifyingBefore)
	before := map[string]bool{}
	for _, d := range drives {
		name := d.Device.Name
		l := labels[name]
		var r settings.Reading
		var ok bool
		if aerr := applyErr[name]; aerr != nil {
			if rr, rerr := s.Read(ctx, cfg.Targets[name]); rerr == nil {
				r = rr
			}
			l.Note = fmt.Sprintf("apply failed: %v", aerr)
		} else if r, ok = settings.Verify(ctx, s, cfg.Targets[name], values[l.Value], cfg.Settle); !ok {
			l.Note = fmt.Sprintf("setting not effective: reads %q, want %q", r.Value, l.Value)
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
		r, ok := settings.Verify(ctx, s, cfg.Targets[name], values[l.Value], cfg.Settle)
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

// finishGroup restores every drive to its original value, verifies it,
// records the result in the group file and removes the recovery file, or
// keeps it with only the drives that failed.
func finishGroup(cfg groupConfig, g *rungroup.GroupFile, rec *rungroup.Recovery, run, total int) error {
	setGroupStatus(g, run, total, stepRestoring)
	fmt.Println("\nRun group: restoring the original values")
	var failed []string
	var kept []rungroup.RecoveryDrive
	for _, e := range rec.Drives {
		if err := restoreOne(context.Background(), cfg.Setting, cfg.Targets, e.Name, e, cfg.Settle); err != nil {
			failed = append(failed, e.Name)
			kept = append(kept, e)
		}
	}
	var errs []error
	if len(failed) == 0 {
		g.Restore = rungroup.GroupRestore{Status: "done"}
		if err := rec.Remove(cfg.RecoveryPath); err != nil {
			errs = append(errs, fmt.Errorf("remove the recovery file: %w", err))
		}
		fmt.Println("Original values restored and verified.")
	} else {
		g.Restore = rungroup.GroupRestore{Status: "failed", Failed: failed}
		left := &rungroup.Recovery{Setting: rec.Setting, Drives: kept}
		if err := left.Save(cfg.RecoveryPath); err != nil {
			errs = append(errs, fmt.Errorf("update the recovery file: %w", err))
		}
		errs = append(errs, fmt.Errorf("restore failed on %s; the recovery file %s is kept", strings.Join(failed, ", "), cfg.RecoveryPath))
	}
	if err := saveGroup(cfg, g); err != nil {
		errs = append(errs, fmt.Errorf("save the group file: %w", err))
	}
	setGroupStatus(g, run, total, stepDone)
	UpdateStatus(func(s *BenchmarkStatus) { s.Running = false })
	return errors.Join(errs...)
}

// restoreDrive restores t to original and reads it back, within
// restoreTimeout of parent.
// A panic in the setting is returned as an error, so one drive cannot keep
// the others from being restored.
func restoreDrive(parent context.Context, s settings.Setting, t settings.Target, original settings.Reading, settle time.Duration) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic while restoring: %v", p)
		}
	}()
	ctx, cancel := context.WithTimeout(parent, restoreTimeout)
	defer cancel()
	if err := s.Restore(ctx, t, original); err != nil {
		return err
	}
	if r, ok := settings.Verify(ctx, s, t, settings.Value{ID: original.Value}, settle); !ok {
		return fmt.Errorf("reads %q after restoring, want %q", r.Value, original.Value)
	}
	return nil
}

// restoreOne restores drive name (entry e) and reports a failure. Restore
// and report both run under one recover, so a panic in either is returned as
// an error and the other drives are still restored.
func restoreOne(parent context.Context, s settings.Setting, targets map[string]settings.Target, name string, e rungroup.RecoveryDrive, settle time.Duration) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = errors.Join(err, fmt.Errorf("panic while restoring or reporting %s: %v", name, p))
		}
	}()
	t, ok := targets[name]
	if !ok {
		err = errors.New("no target for the drive")
	} else {
		err = restoreDrive(parent, s, t, e.Original, settle)
	}
	if err != nil {
		e.Name = name
		reportRestoreFailure(s, t, e, err)
	}
	return err
}

// restoreReportOut is where failed restores are reported (tests replace it).
var restoreReportOut io.Writer = os.Stderr

// reportRestoreFailure makes a failed restore impossible to miss, with the
// command to run by hand when the setting knows it.
func reportRestoreFailure(s settings.Setting, t settings.Target, e rungroup.RecoveryDrive, err error) {
	logger.Error("restore failed", "setting", s.Name(), "device", e.Name, "serial", e.Serial, "original", e.Original.Value, "err", err)
	fmt.Fprintf(restoreReportOut, "\n*** RESTORE FAILED: %s on %s (%s, S/N %s): %v\n", s.Name(), e.Name, e.Model, e.Serial, err)
	fmt.Fprintf(restoreReportOut, "*** Set it back to %q by hand; it is volatile, so a power cycle also resets it.\n", e.Original.Value)
	if h, ok := s.(settings.CommandHinter); ok {
		if cmd := h.RestoreCommand(t, e.Original); cmd != "" {
			fmt.Fprintf(restoreReportOut, "*** Command to set it back by hand: %s\n", cmd)
		}
	}
}

// restoreFromRecovery restores the drives of a recovery file left by an
// earlier group, matching them by serial and model (names change across
// reboots). It returns the drives not restored: the recorded name of an
// entry with no matching drive, the current name of one whose restore or
// read-back failed.
func restoreFromRecovery(ctx context.Context, s settings.Setting, rec *rungroup.Recovery, devices []DriveInfo, targets map[string]settings.Target) (failed []string) {
	found, _ := rec.Match(driveRefs(devices, nil))
	current := map[string]string{} // serial+model -> current device name
	for name, e := range found {
		current[e.Serial+"\x00"+e.Model] = name
	}
	for _, e := range rec.Drives {
		name, ok := current[e.Serial+"\x00"+e.Model]
		if !ok {
			logger.Warn("recovery: drive not present", "name", e.Name, "serial", e.Serial, "model", e.Model)
			fmt.Fprintf(os.Stderr, "Recovery: %s (%s, S/N %s) is not present; not restored.\n", e.Name, e.Model, e.Serial)
			failed = append(failed, e.Name)
			continue
		}
		if err := restoreOne(ctx, s, targets, name, e, recoverySettle); err != nil {
			failed = append(failed, name)
		}
	}
	return failed
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
	gs := &GroupStatus{ID: g.ID, Setting: g.Setting, Strategy: g.Strategy, Run: run, Runs: runs, Step: step}
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
