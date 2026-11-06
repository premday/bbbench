package rungroup

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"bbbench/internal/settings"
)

func TestGroupFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC)
	g := &GroupFile{
		ID: "g1", Created: at, Host: "h", Plan: []string{"p"}, Setting: "write_cache",
		Strategy: ParallelSwap, Values: []string{"enabled", "disabled"},
		Originals: map[string]settings.Reading{"sda": {Value: "enabled", At: at}},
		Runs: []GroupRun{{RunID: "r1", Round: 0, Status: RunDone, Drives: []GroupDrive{{
			Device: "sda", Serial: "S", Model: "M", Value: "enabled",
			Label: Label{Setting: "write_cache", Value: "enabled", Before: &settings.Reading{Value: "enabled", At: at}, Valid: true},
		}}}},
		Restore: GroupRestore{Status: "pending", Failed: []string{"sdb"}},
	}
	if err := g.Save(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "group_g1_group.json")
	got, err := LoadGroup(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, got) {
		t.Errorf("got %+v want %+v", got, g)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestLoadGroups_SkipsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := (&GroupFile{ID: "ok"}).Save(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "group_bad_group.json"), []byte("{nope"), 0o644)
	gs, err := LoadGroups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 || gs[0].ID != "ok" {
		t.Errorf("got %+v", gs)
	}
}
