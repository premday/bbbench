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

func TestGroupFile_ArrayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	members := []Member{{Name: "sdb", Serial: "S1", Model: "M", Role: settings.RoleData}, {Name: "sdc", Serial: "S2", Model: "M", Role: settings.RoleData}}
	g := &GroupFile{
		ID: "g2", Setting: "layout", Strategy: Sequential,
		Variants: []GroupVariant{{Value: "mdadm raid1 · 2 disks", Disks: members}},
		Runs: []GroupRun{{
			RunID: "r1", Status: RunDone,
			Drives: []GroupDrive{{
				Device: "md127", Serial: "0a:0b:0c:0d", Model: "raid1", Kind: settings.TargetArray,
				UUID: "0a:0b:0c:0d", Members: members,
			}},
			Teardown: &GroupRestore{Status: RestoreDone},
		}},
	}
	if err := g.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadGroup(filepath.Join(dir, FileName("g2")))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, got) {
		t.Errorf("got %+v want %+v", got, g)
	}
}

func TestGroupFile_DriveFileHasNoTargetKeys(t *testing.T) {
	dir := t.TempDir()
	g := &GroupFile{ID: "g3", Runs: []GroupRun{{RunID: "r", Drives: []GroupDrive{{Device: "sda"}}}}}
	if err := g.Save(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, FileName("g3")))
	for _, k := range []string{`"variants"`, `"teardown"`, `"kind"`, `"uuid"`, `"members"`} {
		if strings.Contains(string(data), k) {
			t.Errorf("drive group file contains %s:\n%s", k, data)
		}
	}
}
