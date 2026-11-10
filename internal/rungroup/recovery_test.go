package rungroup

import (
	"path/filepath"
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
