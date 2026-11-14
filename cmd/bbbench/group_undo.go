package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// restoreTimeout bounds undoing one record (its commands and the read-back).
const restoreTimeout = 2 * time.Minute

// undoLedger is what is still to undo, mirrored in the recovery file at path
// after every change, so a crash leaves exactly the records not undone.
type undoLedger struct {
	path, setting string
	items         []settings.Undo
}

func newUndoLedger(path, setting string, items []settings.Undo) *undoLedger {
	return &undoLedger{path: path, setting: setting, items: slices.Clone(items)}
}

// add appends us and saves the recovery file, creating its directory. When
// the file cannot be written, us are dropped again: what is not recorded
// must not be changed, so there is nothing to undo.
func (l *undoLedger) add(us ...settings.Undo) error {
	n := len(l.items)
	l.items = append(l.items, us...)
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		l.items = l.items[:n]
		return fmt.Errorf("create the recovery directory: %w", err)
	}
	if err := l.save(); err != nil {
		l.items = l.items[:n]
		return fmt.Errorf("write the recovery file: %w", err)
	}
	return nil
}

func (l *undoLedger) save() error {
	return rungroup.RecoveryOf(l.setting, l.items).Save(l.path)
}

// undo runs undoOne on the picked records in order and drops those that
// succeeded; it then saves what is left, or removes the file when nothing
// is left. failed names the records not undone; err joins the file errors.
func (l *undoLedger) undo(ctx context.Context, s settings.Setting, pick func(settings.Undo) bool) (failed []string, err error) {
	var kept []settings.Undo
	for _, u := range l.items {
		if !pick(u) {
			kept = append(kept, u)
			continue
		}
		if uerr := undoOne(ctx, s, u, l.path); uerr != nil {
			failed = append(failed, u.Name())
			kept = append(kept, u)
		}
	}
	l.items = kept
	if len(kept) == 0 {
		return failed, (&rungroup.Recovery{}).Remove(l.path)
	}
	return failed, l.save()
}

// pending lists the records not undone yet.
func (l *undoLedger) pending() []settings.Undo { return slices.Clone(l.items) }

// undoOne undoes u within restoreTimeout of parent and reports a failure
// (recoveryPath is the recovery file that keeps u). Undo and report both run
// under a recover, so a panic in either is returned as an error and the
// other records are still undone.
func undoOne(parent context.Context, s settings.Setting, u settings.Undo, recoveryPath string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = errors.Join(err, fmt.Errorf("panic while restoring or reporting %s: %v", u.Name(), p))
		}
	}()
	if err = undoWithin(parent, s, u); err != nil {
		reportUndoFailure(s, u, err, recoveryPath)
	}
	return err
}

func undoWithin(parent context.Context, s settings.Setting, u settings.Undo) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic while restoring: %v", p)
		}
	}()
	ctx, cancel := context.WithTimeout(parent, restoreTimeout)
	defer cancel()
	return s.Undo(ctx, u)
}

// restoreReportOut is where failed restores are reported (tests replace it).
var restoreReportOut io.Writer = os.Stderr

// layoutDesc names a built layout in every message: "<backend> <name>", plus
// " (UUID <uuid>)" when the backend has one. The backend is data here.
func layoutDesc(a settings.ArrayUndo) string {
	d := a.Backend + " " + a.Name
	if a.UUID != "" {
		d += " (UUID " + a.UUID + ")"
	}
	return d
}

// membersDesc lists disks with their roles: "sdb (data), sdc (spare)".
func membersDesc(ms []rungroup.Member) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, fmt.Sprintf("%s (%s)", m.Name, m.Role))
	}
	return strings.Join(parts, ", ")
}

// reportUndoFailure makes a failed undo impossible to miss, with the command
// to run by hand when the setting knows it. recoveryPath is the recovery
// file that keeps a layout's record.
func reportUndoFailure(s settings.Setting, u settings.Undo, err error, recoveryPath string) {
	if a := u.Array; a != nil {
		members := membersDesc(rungroup.MembersOf(a.Members))
		logger.Error("teardown failed", "setting", s.Name(), "layout", layoutDesc(*a), "members", members, "err", err)
		fmt.Fprintf(restoreReportOut, "\n*** TEARDOWN FAILED: %s; members %s: %v\n", layoutDesc(*a), members, err)
		if h, ok := s.(settings.CommandHinter); ok {
			if cmd := h.UndoCommand(u); cmd != "" {
				fmt.Fprintf(restoreReportOut, "*** Tear it down and wipe its members by hand: %s\n", cmd)
			}
		}
		fmt.Fprintf(restoreReportOut, "*** The recovery file %s is kept; bbbench offers the cleanup again at the next start.\n", recoveryPath)
		return
	}
	if u.Drive == nil || u.Original == nil {
		logger.Error("restore failed", "setting", s.Name(), "err", err)
		fmt.Fprintf(restoreReportOut, "\n*** RESTORE FAILED: %s: %v\n", s.Name(), err)
		return
	}
	d, original := u.Drive, u.Original.Value
	logger.Error("restore failed", "setting", s.Name(), "device", d.Name, "serial", d.Serial, "original", original, "err", err)
	fmt.Fprintf(restoreReportOut, "\n*** RESTORE FAILED: %s on %s (%s, S/N %s): %v\n", s.Name(), d.Name, d.Model, d.Serial, err)
	fmt.Fprintf(restoreReportOut, "*** Set it back to %q by hand; it is volatile, so a power cycle also resets it.\n", original)
	if h, ok := s.(settings.CommandHinter); ok {
		if cmd := h.UndoCommand(u); cmd != "" {
			fmt.Fprintf(restoreReportOut, "*** Command to set it back by hand: %s\n", cmd)
		}
	}
}

// restoreFromRecovery undoes the records of a recovery file left by an
// earlier group, matching drives by serial and model (names change across
// reboots). It returns the records not undone: the recorded name of an entry
// with no matching drive, the current name of one whose undo or read-back
// failed. The file at path keeps only what is not undone.
func restoreFromRecovery(ctx context.Context, s settings.Setting, rec *rungroup.Recovery, path string, targets map[string]settings.Target) (failed []string) {
	ts := make([]settings.Target, 0, len(targets))
	for _, t := range targets {
		ts = append(ts, t)
	}
	slices.SortFunc(ts, func(a, b settings.Target) int { return strings.Compare(a.Name, b.Name) })
	found, missing := rec.Undos(ts)
	for _, u := range missing {
		if a := u.Array; a != nil {
			logger.Warn("recovery: no member of a layout is present", "layout", layoutDesc(*a))
			fmt.Fprintf(os.Stderr, "Recovery: no member of %s is present; not torn down. When its disks are back, wipe them by hand and remove %s.\n", layoutDesc(*a), path)
			failed = append(failed, u.Name())
			continue
		}
		logger.Warn("recovery: drive not present", "name", u.Drive.Name, "serial", u.Drive.Serial, "model", u.Drive.Model)
		fmt.Fprintf(os.Stderr, "Recovery: %s (%s, S/N %s) is not present; not restored.\n", u.Drive.Name, u.Drive.Model, u.Drive.Serial)
		failed = append(failed, u.Name())
	}
	// A layout with a member gone is torn down on the members present, but
	// its record is always kept: the superblock of the absent disk is still
	// there, whatever the backend reports.
	isFound := map[settings.Undo]bool{}
	var partial []settings.Undo
	for _, u := range found {
		if a := u.Array; a != nil {
			if gone := goneMembers(*a); len(gone) > 0 {
				for _, d := range gone {
					logger.Warn("recovery: layout member not present", "layout", layoutDesc(*a), "name", d.Name, "serial", d.Serial)
					fmt.Fprintf(os.Stderr, "Recovery: member %s (%s, S/N %s) of %s is not present; wipe it by hand when it is back, then remove %s.\n", d.Name, d.Model, d.Serial, layoutDesc(*a), path)
				}
				partial = append(partial, u)
				continue
			}
		}
		isFound[u] = true
	}
	l := newUndoLedger(path, rec.Setting, append(slices.Clone(found), missing...))
	notUndone, err := l.undo(ctx, s, func(u settings.Undo) bool { return isFound[u] })
	if err != nil {
		logger.Warn("recovery: update the recovery file", "path", path, "err", err)
	}
	for _, u := range partial {
		// The error is reported by undoOne; the record stays either way.
		_ = undoOne(ctx, s, u, path)
		failed = append(failed, u.Name())
	}
	return append(failed, notUndone...)
}

// goneMembers are the recorded members no present disk matched (no Path).
func goneMembers(a settings.ArrayUndo) []settings.Disk {
	var out []settings.Disk
	for _, d := range a.Members {
		if d.Path == "" {
			out = append(out, d)
		}
	}
	return out
}
