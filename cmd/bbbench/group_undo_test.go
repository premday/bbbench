package main

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// driveUndos are the undos of the named test drives, back to "enabled".
func driveUndos(names ...string) []settings.Undo {
	_, targets := testDrives(names...)
	var us []settings.Undo
	for _, n := range names {
		t := targets[n]
		us = append(us, settings.Undo{Drive: &t, Original: &settings.Reading{Value: "enabled"}})
	}
	return us
}

func recordedNames(t *testing.T, path string) []string {
	t.Helper()
	rec, err := rungroup.LoadRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		return nil
	}
	var names []string
	for _, d := range rec.Drives {
		names = append(names, d.Name)
	}
	return names
}

func all(settings.Undo) bool { return true }

func TestUndoLedger_MirrorsRecoveryFile(t *testing.T) {
	quietLogger(t)
	captureReport(t)
	path := filepath.Join(t.TempDir(), "sub", "rec.json")
	fs := newFakeSetting(map[string]string{"sdb": "disabled", "sdc": "disabled"})
	s := perDrive(fs)
	l := newUndoLedger(path, s.Name(), nil)
	if err := l.add(driveUndos("sdb", "sdc")...); err != nil {
		t.Fatal(err)
	}
	if got := recordedNames(t, path); !reflect.DeepEqual(got, []string{"sdb", "sdc"}) {
		t.Fatalf("file lists %v", got)
	}
	fs.restoreErr["sdb"] = true
	failed, err := l.undo(context.Background(), s, all)
	if err != nil || !reflect.DeepEqual(failed, []string{"sdb"}) {
		t.Fatalf("failed %v, err %v", failed, err)
	}
	if got := recordedNames(t, path); !reflect.DeepEqual(got, []string{"sdb"}) {
		t.Errorf("file lists %v, want [sdb]", got)
	}
	if p := l.pending(); len(p) != 1 || p[0].Name() != "sdb" {
		t.Errorf("pending %+v", p)
	}
	fs.restoreErr["sdb"] = false
	if failed, err := l.undo(context.Background(), s, all); err != nil || len(failed) != 0 {
		t.Fatalf("failed %v, err %v", failed, err)
	}
	if recoveryExists(t, path) {
		t.Error("recovery file kept after everything was undone")
	}
	if fs.get("sdb") != "enabled" || fs.get("sdc") != "enabled" {
		t.Errorf("sdb %q sdc %q", fs.get("sdb"), fs.get("sdc"))
	}
}

func TestUndoLedger_PanicInUndoIsAnError(t *testing.T) {
	quietLogger(t)
	captureReport(t)
	path := filepath.Join(t.TempDir(), "rec.json")
	fs := newFakeSetting(map[string]string{"sdb": "disabled", "sdc": "disabled"})
	s := perDrive(panicSetting{fs, "sdb"})
	l := newUndoLedger(path, s.Name(), nil)
	if err := l.add(driveUndos("sdb", "sdc")...); err != nil {
		t.Fatal(err)
	}
	failed, err := l.undo(context.Background(), s, all)
	if err != nil || !reflect.DeepEqual(failed, []string{"sdb"}) {
		t.Fatalf("failed %v, err %v", failed, err)
	}
	if fs.get("sdc") != "enabled" {
		t.Errorf("sdc = %q, want undone", fs.get("sdc"))
	}
	if got := recordedNames(t, path); !reflect.DeepEqual(got, []string{"sdb"}) {
		t.Errorf("file lists %v", got)
	}
}
