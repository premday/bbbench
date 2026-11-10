package rungroup

import (
	"path/filepath"
	"reflect"
	"testing"

	"bbbench/internal/settings"
)

func TestRecovery_RoundTripAndMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	if r, err := LoadRecovery(path); r != nil || err != nil {
		t.Fatalf("missing: %v %v", r, err)
	}
	r := &Recovery{Setting: "write_cache", Drives: []RecoveryDrive{
		{Serial: "S1", Model: "M", Name: "sdb", Original: settings.Reading{Value: "enabled"}},
		{Serial: "S2", Model: "M", Name: "sdd", Original: settings.Reading{Value: "disabled"}},
	}}
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecovery(path)
	if err != nil || got == nil || len(got.Drives) != 2 {
		t.Fatalf("load: %+v %v", got, err)
	}
	found, missing := got.Match([]DriveRef{{Name: "sdc", Serial: "S1", Model: "M"}, {Name: "sdb", Serial: "S9", Model: "M"}})
	if e, ok := found["sdc"]; !ok || e.Original.Value != "enabled" || len(found) != 1 {
		t.Errorf("found = %+v", found)
	}
	if len(missing) != 1 || missing[0].Serial != "S2" {
		t.Errorf("missing = %+v", missing)
	}
	if err := got.Remove(path); err != nil {
		t.Fatal(err)
	}
	if r, _ := LoadRecovery(path); r != nil {
		t.Error("still there")
	}
}

func TestRecoveryOf_DriveEntries(t *testing.T) {
	b := settings.Target{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M"}
	d := settings.Target{Name: "sdd", Path: "/dev/sdd", Serial: "S2", Model: "M"}
	r := RecoveryOf("write_cache", []settings.Undo{
		{Drive: &b, Original: &settings.Reading{Value: "enabled"}},
		{Drive: &d, Original: &settings.Reading{Value: "disabled"}},
	})
	want := []RecoveryDrive{
		{Serial: "S1", Model: "M", Name: "sdb", Original: settings.Reading{Value: "enabled"}},
		{Serial: "S2", Model: "M", Name: "sdd", Original: settings.Reading{Value: "disabled"}},
	}
	if r.Setting != "write_cache" || !reflect.DeepEqual(r.Drives, want) {
		t.Errorf("%+v", r)
	}
}

func TestRecovery_Undos(t *testing.T) {
	r := &Recovery{Setting: "write_cache", Drives: []RecoveryDrive{
		{Serial: "S1", Model: "M", Name: "sdb", Original: settings.Reading{Value: "enabled"}},
		{Serial: "S2", Model: "M", Name: "sdd", Original: settings.Reading{Value: "disabled"}},
	}}
	now := settings.Target{Name: "sdc", Path: "/dev/sdc", Serial: "S1", Model: "M", Interface: "SATA"}
	other := settings.Target{Name: "sdb", Path: "/dev/sdb", Serial: "S9", Model: "M", Interface: "SATA"}
	found, missing := r.Undos([]settings.Target{other, now})
	if len(found) != 1 || found[0].Drive == nil || !reflect.DeepEqual(*found[0].Drive, now) || found[0].Name() != "sdc" || found[0].Original.Value != "enabled" {
		t.Errorf("found = %+v", found)
	}
	if len(missing) != 1 || missing[0].Name() != "sdd" || missing[0].Drive.Serial != "S2" || missing[0].Drive.Model != "M" || missing[0].Original.Value != "disabled" {
		t.Errorf("missing = %+v", missing)
	}
}
