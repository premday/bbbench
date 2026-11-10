package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

const (
	// groupRecoveryPath keeps the original values while a group runs.
	groupRecoveryPath = "/var/lib/bbbench/group-recovery.json"
	// groupSettle is how long a read-back may take to show an applied or
	// restored value.
	groupSettle = 10 * time.Second
)

// askYes prints prompt, reads one line from in and reports whether it is
// "y" or "yes" (any case).
func askYes(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func settingByName(available []settings.Setting, name string) settings.Setting {
	for _, s := range available {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// offerRecovery checks for a recovery file left by a group that did not
// restore its drives, and offers to restore them (answer read from in). The
// file is removed only after every drive was restored. An error means the
// run must stop: the file could not be read, or a drive was not restored.
func offerRecovery(ctx context.Context, in io.Reader, out io.Writer, path string, available []settings.Setting, drives []DriveInfo, targets map[string]settings.Target) error {
	rec, err := rungroup.LoadRecovery(path)
	if err != nil {
		return fmt.Errorf("read the recovery file %s: %w (check it by hand: it lists drive settings bbbench changed)", path, err)
	}
	if rec == nil {
		return nil
	}
	s := settingByName(available, rec.Setting)
	if s == nil {
		return fmt.Errorf("the recovery file %s is for the unknown setting %q; restore the drives by hand and remove it", path, rec.Setting)
	}
	fmt.Fprintf(out, "\nA previous setting test changed the %s of %d drive(s) and did not restore them:\n", s.Name(), len(rec.Drives))
	for _, e := range rec.Drives {
		fmt.Fprintf(out, "  %s (%s, S/N %s): original value %q\n", e.Name, e.Model, e.Serial, e.Original.Value)
	}
	if !askYes(in, out, "Restore the original values now? [y/n] ") {
		fmt.Fprintf(out, "Not restored. A setting test will refuse to start while %s exists; a single run is not affected.\n\n", path)
		return nil
	}
	failed := restoreFromRecovery(ctx, s, rec, drives, targets)
	if len(failed) > 0 {
		fmt.Fprintf(out, "\nNot restored: %s. The recovery file %s is kept. Set them back by hand (the command is printed above when known); a power cycle also resets a volatile setting.\n", strings.Join(failed, ", "), path)
		return fmt.Errorf("could not restore %s on %s", s.Name(), strings.Join(failed, ", "))
	}
	if err := rec.Remove(path); err != nil {
		return fmt.Errorf("restored, but could not remove the recovery file %s: %w", path, err)
	}
	fmt.Fprintf(out, "Restored the original values.\n\n")
	return nil
}

// findResumableGroup returns the newest group file in dir with a run that is
// not finished (RunStatus.Finished: done, invalid and not run are), or nil.
func findResumableGroup(dir string) *rungroup.GroupFile {
	groups, err := rungroup.LoadGroups(dir)
	if err != nil {
		return nil
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Created.After(groups[j].Created) })
	for _, g := range groups {
		for _, r := range g.Runs {
			if !r.Status.Finished() {
				return g
			}
		}
	}
	return nil
}

// resumeChoice rebuilds what the user chose for group g: the setting, the
// values in their order, and the drives (matched by serial and model).
func resumeChoice(g *rungroup.GroupFile, available []settings.Setting, drives []DriveInfo) (*groupChoice, error) {
	s := settingByName(available, g.Setting)
	if s == nil {
		return nil, fmt.Errorf("group %s is for the unknown setting %q", g.ID, g.Setting)
	}
	known := map[string]settings.Value{}
	for _, v := range s.Values() {
		known[v.ID] = v
	}
	c := &groupChoice{Setting: s, Strategy: g.Strategy, Excluded: map[string]string{}}
	for _, id := range g.Values {
		v, ok := known[id]
		if !ok {
			return nil, fmt.Errorf("group %s has the value %q, which %s does not offer", g.ID, id, s.Name())
		}
		c.Values = append(c.Values, v)
	}
	seen := map[string]bool{}
	for _, r := range g.Runs {
		for _, gd := range r.Drives {
			id := gd.Serial + "\x00" + gd.Model
			if seen[id] {
				continue
			}
			seen[id] = true
			var found *DriveInfo
			for i := range drives {
				if drives[i].Device.Serial == gd.Serial && drives[i].Device.Model == gd.Model {
					found = &drives[i]
					break
				}
			}
			if found == nil {
				return nil, fmt.Errorf("cannot resume group %s: drive %s (%s, S/N %s) is not present", g.ID, gd.Device, gd.Model, gd.Serial)
			}
			c.Drives = append(c.Drives, *found)
		}
	}
	if len(c.Drives) == 0 {
		return nil, fmt.Errorf("cannot resume group %s: it recorded no drives (it stopped before its first run started); start a new setting test", g.ID)
	}
	return c, nil
}

// newGroupConfig builds the group to run from the confirmed choice.
func newGroupConfig(c *groupChoice, targets map[string]settings.Target, opts runOptions, host string, resume *rungroup.GroupFile) groupConfig {
	return groupConfig{
		Setting:      c.Setting,
		Values:       c.Values,
		Strategy:     c.Strategy,
		Drives:       c.Drives,
		Targets:      targets,
		OutDir:       opts.OutDir,
		RecoveryPath: groupRecoveryPath,
		Host:         host,
		Settle:       groupSettle,
		Resume:       resume,
		// fio, the fio files and the engines, checked before the first apply.
		Precheck: func() error { return precheckRun(opts, c.Drives) },
		// A running group shows up on the web pages live.
		OnSave: globalGroupStore.AddGroup,
		Execute: func(ctx context.Context, spec runSpec) (*BenchmarkResult, error) {
			// The group loop already removed the stale per-run state file; a
			// run of a group never resumes mid-run.
			o := opts
			o.Resume = nil
			return executeRun(ctx, o, spec)
		},
	}
}

// precheckRun checks, without running anything, what executeRun checks
// before the first phase: fio is installed, every drive's fio file exists and
// parses, and fio has the I/O engines the workloads use.
func precheckRun(opts runOptions, drives []DriveInfo) error {
	ec, err := newExecutionCoordinator(drives, opts.Mode, opts.OutDir)
	if err != nil {
		return err
	}
	defer ec.cancel()
	return checkFioEngines(fioEngineList(), ec.workloads)
}

// resumeIgnoredNote warns that --resume does not apply when a new setting
// test was chosen (there was no group to resume): the single-run state file
// is not used. Empty otherwise.
func resumeIgnoredNote(resume bool, s settings.Setting) string {
	if !resume || s == nil {
		return ""
	}
	return "Note: --resume is ignored: a new setting test starts from its first run; the single-run state file is not used."
}

// printGroupSummary prints one line per run and drive.
func printGroupSummary(w io.Writer, g *rungroup.GroupFile) {
	fmt.Fprintf(w, "\nSetting test %s: %s (%s)\n", g.ID, g.Setting, g.Strategy)
	for i, r := range g.Runs {
		fmt.Fprintf(w, "  run %d (round %d): %s\n", i+1, r.Round+1, r.Status)
		for _, d := range r.Drives {
			state := "verified"
			switch {
			case d.Label.Note != "":
				state = d.Label.Note
			case !d.Label.Valid:
				state = "not verified"
			}
			fmt.Fprintf(w, "    %-10s %-12s %s\n", d.Device, d.Value, state)
		}
	}
	switch g.Restore.Status {
	case "done":
		fmt.Fprintln(w, "  original values restored")
	case "failed":
		fmt.Fprintf(w, "  RESTORE FAILED on: %s\n", strings.Join(g.Restore.Failed, ", "))
	}
}

// groupFileName is the file name of group g in the output directory.
func groupFileName(g *rungroup.GroupFile) string { return rungroup.FileName(g.ID) }

// resumesGroup decides what --resume continues when group g is unfinished:
// a state file tagged with a group (or none at all) belongs to the group; an
// untagged one is a single run's. state is nil when there is no state file.
func resumesGroup(g *rungroup.GroupFile, state *ExecutionState) bool {
	if g == nil {
		return false
	}
	return state == nil || state.Group != ""
}

// stateIfAny loads the state file; nil when there is none or it cannot be
// read (the single-run resume then reports the problem itself).
func stateIfAny(path string) *ExecutionState {
	st, err := loadState(path)
	if err != nil {
		return nil
	}
	return st
}

// prepareGroupResume rebuilds the choice for the unfinished group g and asks
// the user to confirm it: resuming applies settings. An error stops the
// run; nothing has been applied then.
func prepareGroupResume(in io.Reader, out io.Writer, g *rungroup.GroupFile, available []settings.Setting, drives []DriveInfo) (*groupChoice, error) {
	c, err := resumeChoice(g, available, drives)
	if err != nil {
		return nil, fmt.Errorf("%w\nThe unfinished group is %s in the output directory; removing or renaming it unblocks --resume", err, groupFileName(g))
	}
	todo := 0
	for _, r := range g.Runs {
		if !r.Status.Finished() {
			todo++
		}
	}
	fmt.Fprintf(out, "\nResume setting test %s (%s, %s)\n", g.ID, g.Setting, g.Strategy)
	var labels []string
	for _, v := range c.Values {
		labels = append(labels, v.Label)
	}
	fmt.Fprintf(out, "  Values: %s\n  Will be changed on:\n", strings.Join(labels, ", "))
	for _, d := range c.Drives {
		fmt.Fprintf(out, "    %s  %s %s (S/N %s)\n", d.Device.Name, d.Device.Vendor, d.Device.Model, d.Device.Serial)
	}
	fmt.Fprintf(out, "  Runs still to do: %d of %d\n", todo, len(g.Runs))
	if !askYes(in, out, "  The original values are read first and restored when the group ends.\nContinue and change these drives? [y/n] ") {
		return nil, errors.New("setting test not resumed; nothing was changed")
	}
	return c, nil
}
