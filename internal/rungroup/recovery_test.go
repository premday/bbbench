package rungroup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestRecovery_ArraysRoundTrip(t *testing.T) {
	b := settings.Target{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M"}
	c := settings.Target{Name: "sdc", Path: "/dev/sdc", Serial: "S2", Model: "M"}
	arr := &settings.ArrayUndo{
		Backend: "lvm", Name: "bbbench_1_2",
		Members: []settings.Disk{{Target: b, Role: settings.RoleData}, {Target: c, Role: settings.RoleSpare}},
		Extra:   map[string]string{"vg": "bbbench_1_2_vg"},
	}
	r := RecoveryOf("software layout", []settings.Undo{{Array: arr}})
	want := []RecoveryArray{{Backend: "lvm", Name: "bbbench_1_2", Extra: map[string]string{"vg": "bbbench_1_2_vg"}, Members: []RecoveryMember{
		{Serial: "S1", Model: "M", Name: "sdb", Role: settings.RoleData},
		{Serial: "S2", Model: "M", Name: "sdc", Role: settings.RoleSpare},
	}}}
	if !reflect.DeepEqual(r.Arrays, want) || len(r.Drives) != 0 {
		t.Fatalf("RecoveryOf = %+v", r)
	}
	path := filepath.Join(t.TempDir(), "rec.json")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"uuid"`) {
		t.Errorf("empty uuid written: %s", data)
	}
	got, err := LoadRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	found, missing := got.Undos([]settings.Target{c, b})
	if len(missing) != 0 || len(found) != 1 || found[0].Array == nil || !reflect.DeepEqual(*found[0].Array, *arr) {
		t.Fatalf("Undos found %+v missing %+v, want %+v", found, missing, *arr)
	}

	drives := RecoveryOf("write_cache", []settings.Undo{{Drive: &b, Original: &settings.Reading{Value: "enabled"}}})
	data, err = json.Marshal(drives)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"arrays"`) {
		t.Errorf("drive-only recovery has an arrays key: %s", data)
	}
}

func TestRecovery_ArrayUndosResolveMembers(t *testing.T) {
	r := &Recovery{Setting: "software layout", Arrays: []RecoveryArray{
		{Backend: "mdadm", Name: "a1", UUID: "u1", Members: []RecoveryMember{
			{Serial: "S1", Model: "M", Name: "sdb", Role: settings.RoleData},
			{Serial: "S2", Model: "M", Name: "sdc", Role: settings.RoleData},
		}},
		{Backend: "mdadm", Name: "a2", UUID: "u2", Members: []RecoveryMember{
			{Serial: "S8", Model: "M", Name: "sdx", Role: settings.RoleData},
		}},
	}}
	now := settings.Target{Name: "sdd", Path: "/dev/sdd", Serial: "S1", Model: "M"}
	found, missing := r.Undos([]settings.Target{now})
	if len(found) != 1 || found[0].Name() != "a1" {
		t.Fatalf("found %+v", found)
	}
	ms := found[0].Array.Members
	if ms[0].Name != "sdd" || ms[0].Path != "/dev/sdd" || ms[1].Name != "sdc" || ms[1].Path != "" || ms[1].Serial != "S2" {
		t.Errorf("members %+v: want the current sdd and an unresolved sdc", ms)
	}
	if len(missing) != 1 || missing[0].Name() != "a2" || missing[0].Array.Members[0].Path != "" {
		t.Errorf("missing %+v", missing)
	}
}

func TestRecovery_LVMRecordRoundTrip(t *testing.T) {
	// The shape NewLVM's Prepare records: backend lvm, the VG/LV name, no
	// uuid, the devices-file entries pvcreate will add in Extra.
	arr := &settings.ArrayUndo{Backend: "lvm", Name: "bbbench_1_1",
		Members: []settings.Disk{
			{Target: settings.Target{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M"}, Role: settings.RoleData},
			{Target: settings.Target{Name: "sdc", Path: "/dev/sdc", Serial: "S2", Model: "M"}, Role: settings.RoleData},
		},
		Extra: map[string]string{"devicesfile_added": "S2"},
	}
	path := filepath.Join(t.TempDir(), "rec.json")
	if err := RecoveryOf("software layout", []settings.Undo{{Array: arr}}).Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	// After a reboot the members are sde and sdd.
	now := []settings.Target{
		{Name: "sdd", Path: "/dev/sdd", Serial: "S2", Model: "M"},
		{Name: "sde", Path: "/dev/sde", Serial: "S1", Model: "M"},
	}
	found, missing := got.Undos(now)
	if len(missing) != 0 || len(found) != 1 || found[0].Array == nil {
		t.Fatalf("found %+v missing %+v", found, missing)
	}
	a := found[0].Array
	want := settings.ArrayUndo{Backend: "lvm", Name: "bbbench_1_1",
		Members: []settings.Disk{{Target: now[1], Role: settings.RoleData}, {Target: now[0], Role: settings.RoleData}},
		Extra:   map[string]string{"devicesfile_added": "S2"},
	}
	if !reflect.DeepEqual(*a, want) {
		t.Errorf("\n got %+v\nwant %+v", *a, want)
	}
}
