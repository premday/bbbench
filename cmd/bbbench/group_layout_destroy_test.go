package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"bbbench/internal/inuse"
	"bbbench/internal/settings"
)

// destroyFake is a layout setting whose listing, dry gate and Destroy are
// fakes; the rest is the production setting over a fake PATH.
type destroyFake struct {
	settings.LayoutSetting
	entries    []settings.Existing
	errs       []error
	blockers   map[string][]string // by ID
	blockErr   map[string]error
	destroyErr error
	destroyed  []settings.Existing
	onDestroy  func()
}

func (f *destroyFake) Existing(context.Context) ([]settings.Existing, []error) {
	return f.entries, f.errs
}

func (f *destroyFake) DestroyBlockers(_ context.Context, e settings.Existing) ([]string, error) {
	return f.blockers[e.ID], f.blockErr[e.ID]
}

func (f *destroyFake) Destroy(_ context.Context, e settings.Existing) error {
	f.destroyed = append(f.destroyed, e)
	if f.onDestroy != nil {
		f.onDestroy()
	}
	return f.destroyErr
}

func disk(name string) settings.Target {
	return settings.Target{Name: name, Path: "/dev/" + name, Serial: "SN-" + name, Model: "M1"}
}

// destroyFixture: sdb..sde discovered; sdb and sdc carry the array "data"
// (and are busy for that), sdd carries a blocked group "vg0", sde is free.
func destroyFixture(t *testing.T) (layoutModel, *destroyFake, **inuse.Report) {
	t.Helper()
	quietLogger(t)
	s, _ := bothBackends("mdadm", "lvm")
	f := &destroyFake{
		LayoutSetting: s,
		entries: []settings.Existing{
			{Backend: "provA", ID: "uuid-1", Name: "data", Summary: "raid1 · 2 disks", Disks: []settings.Target{disk("sdb"), disk("sdc")},
				Stack: []string{"filesystem ext4 on data", "array data (raid1)"}},
			{Backend: "provB", ID: "vg0", Name: "vg0", Summary: "volume group · 1 disk", Disks: []settings.Target{disk("sdd")},
				Blocked: "it holds the root filesystem"},
			{Backend: "provB", ID: "vg1", Name: "vg1", Summary: "volume group · 1 disk", Disks: []settings.Target{disk("sde")}},
		},
		errs:     []error{errors.New("provC: not available: the tool is not installed")},
		blockers: map[string][]string{"vg1": {"vg1 is open", "a logical volume is mounted"}},
	}
	rep := &inuse.Report{Busy: map[string][]string{"sdb": {"member of data"}, "sdc": {"member of data"}, "sdd": {"member of vg0"}, "sde": {"member of vg1"}}}
	cur := &rep
	drives, targets := testDrives("sdb", "sdc", "sdd", "sde")
	m := newLayoutModel(f, drives, targets, rep, nil)
	m.rescan = func() (*inuse.Report, error) { return *cur, nil }
	return m, f, cur
}

// lkey presses one key and runs the commands it returns to completion,
// feeding every message back into the model.
func lkey(t *testing.T, m layoutModel, key string) layoutModel {
	t.Helper()
	nm, cmd := m.Update(keyMsg(key))
	return drain(t, nm.(layoutModel), cmd)
}

func drain(t *testing.T, m layoutModel, cmd tea.Cmd) layoutModel {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for i := 0; len(queue) > 0; i++ {
		if i > 100 {
			t.Fatal("commands do not settle")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil, tea.QuitMsg:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			nm, next := m.Update(msg)
			m = nm.(layoutModel)
			queue = append(queue, next)
		}
	}
	return m
}

func TestDestroy_HelpLineAndNilRescan(t *testing.T) {
	m, _, _ := destroyFixture(t)
	if v := m.View(); !strings.Contains(v, "x destroy an existing RAID configuration") {
		t.Fatalf("help line lacks x:\n%s", v)
	}
	m.rescan = nil
	if v := m.View(); strings.Contains(v, "x destroy") {
		t.Fatalf("help line offers x without a rescan:\n%s", v)
	}
	if m = lkey(t, m, "x"); m.step != layoutStepList {
		t.Fatalf("x with no rescan: step %v", m.step)
	}
}

func TestDestroy_PickerGraysBlockedAndShowsNotes(t *testing.T) {
	m, f, _ := destroyFixture(t)
	nm, cmd := m.Update(keyMsg("x"))
	m = nm.(layoutModel)
	if m.step != layoutStepDestroyPick || !strings.Contains(m.View(), "looking for existing configurations...") {
		t.Fatalf("x must open the picker while it lists:\n%s", m.View())
	}
	m = drain(t, m, cmd)
	v := m.View()
	for _, want := range []string{
		"data  raid1 · 2 disks  sdb, sdc",
		"vg0  volume group · 1 disk  sdd  (it holds the root filesystem)",
		"vg1  volume group · 1 disk  sde  (vg1 is open)",
		"provC: not available: the tool is not installed",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("picker lacks %q", want)
		}
	}
	if strings.Contains(v, "a logical volume is mounted") {
		t.Errorf("a not highlighted entry shows more than its first reason")
	}
	// Every reason of the highlighted entry.
	m = lkey(t, lkey(t, m, "down"), "down")
	if v := m.View(); !strings.Contains(v, "a logical volume is mounted") {
		t.Errorf("highlighted entry lacks its reasons:\n%s", v)
	}
	// Enter on a grayed entry does nothing.
	if m = lkey(t, m, "enter"); m.step != layoutStepDestroyPick {
		t.Fatalf("a blocked entry was selected")
	}
	if m = lkey(t, lkey(t, m, "up"), "enter"); m.step != layoutStepDestroyPick {
		t.Fatalf("a provider-blocked entry was selected")
	}
	if t.Failed() {
		t.Log(v)
	}
	if len(f.destroyed) != 0 {
		t.Fatal("Destroy called from the picker")
	}
	// Esc back to the list.
	if m = lkey(t, m, "esc"); m.step != layoutStepList {
		t.Fatalf("Esc: step %v", m.step)
	}
}

func TestDestroy_GateErrorGrays(t *testing.T) {
	m, f, _ := destroyFixture(t)
	f.blockErr = map[string]error{"uuid-1": errors.New("scan failed")}
	m = lkey(t, m, "x")
	if !strings.Contains(m.View(), "check failed: scan failed") {
		t.Fatalf("gate error not shown:\n%s", m.View())
	}
	if m = lkey(t, m, "enter"); m.step != layoutStepDestroyPick {
		t.Fatal("an entry whose check failed was selected")
	}
}

func TestDestroy_EmptyList(t *testing.T) {
	m, f, _ := destroyFixture(t)
	f.entries = nil
	m = lkey(t, m, "x")
	v := m.View()
	for _, want := range []string{"no existing RAID configuration found", "provC: not available: the tool is not installed"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if m = lkey(t, m, "enter"); m.step != layoutStepDestroyPick {
		t.Fatal("Enter on an empty list moved on")
	}
}

func toConfirm(t *testing.T, m layoutModel) layoutModel {
	t.Helper()
	m = lkey(t, lkey(t, m, "x"), "enter")
	if m.step != layoutStepDestroyConfirm {
		t.Fatalf("not on the destroy confirmation:\n%s", m.View())
	}
	return m
}

func TestDestroy_ConfirmListsEverything(t *testing.T) {
	m, f, _ := destroyFixture(t)
	m = toConfirm(t, m)
	v := m.View()
	for _, want := range []string{
		"THIS DESTROYS DATA", "data: raid1 · 2 disks",
		"filesystem ext4 on data", "array data (raid1)",
		"sdb  M1 (S/N SN-sdb)", "sdc  M1 (S/N SN-sdc)",
		"The data on these disks is not kept.",
		"mounted, used as swap or holding bbbench or its output are refused",
		"Press y to destroy, any other key to cancel",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("confirmation lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(v)
	}
	if m = lkey(t, m, "enter"); m.step != layoutStepDestroyConfirm {
		t.Fatalf("Enter must do nothing: step %v", m.step)
	}
	for _, k := range []string{"esc", "n", "q", "Y", " "} {
		c := lkey(t, m, k)
		if c.step != layoutStepList || c.cancelled {
			t.Errorf("%q: step %v cancelled %v, want back on the list", k, c.step, c.cancelled)
		}
	}
	if len(f.destroyed) != 0 {
		t.Fatalf("Destroy called without y: %v", f.destroyed)
	}
}

func TestDestroy_YDestroysOnceAndRefreshes(t *testing.T) {
	m, f, cur := destroyFixture(t)
	// A layout added before keeps its entry.
	m = lkey(t, m, "b")
	if len(m.layouts) != 0 {
		t.Fatalf("baseline on busy disks: %+v", m.layouts)
	}
	m = toConfirm(t, m)
	f.onDestroy = func() {
		// sdb is free afterwards, sdc still busy for another reason.
		*cur = &inuse.Report{Busy: map[string][]string{"sdc": {"mounted at /x"}, "sdd": {"member of vg0"}, "sde": {"member of vg1"}}}
	}
	nm, cmd := m.Update(keyMsg("y"))
	m = nm.(layoutModel)
	if m.step != layoutStepDestroyRun || !strings.Contains(m.View(), "destroying data ... do not interrupt") {
		t.Fatalf("not running:\n%s", m.View())
	}
	for _, k := range []string{"ctrl+c", "q", "esc", "y"} {
		var c tea.Cmd
		nm, c = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		if k == "ctrl+c" {
			nm, c = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		}
		if r := nm.(layoutModel); r.step != layoutStepDestroyRun || r.cancelled || c != nil {
			t.Fatalf("%q during the run: step %v cancelled %v", k, r.step, r.cancelled)
		}
	}
	m = drain(t, m, cmd)
	if len(f.destroyed) != 1 || f.destroyed[0].ID != "uuid-1" {
		t.Fatalf("destroyed %+v, want uuid-1 once", f.destroyed)
	}
	if m.step != layoutStepList {
		t.Fatalf("step %v, want the list", m.step)
	}
	if _, gray := m.grayed["sdb"]; gray {
		t.Errorf("sdb still grayed: %q", m.grayed["sdb"])
	}
	if m.grayed["sdc"] != "mounted at /x" {
		t.Errorf("sdc grayed %q", m.grayed["sdc"])
	}
	v := m.View()
	for _, want := range []string{"data destroyed; sdb is free", "sdc is still in use: mounted at /x"} {
		if !strings.Contains(v, want) {
			t.Errorf("list lacks %q:\n%s", want, v)
		}
	}
	// The freed disk is offered now.
	m = lkey(t, m, "b")
	if len(m.layouts) != 1 || m.layouts[0].value.ID != "none · 1 disk" {
		t.Fatalf("baseline = %+v, want sdb", m.layouts)
	}
}

func TestDestroy_FailureShowsErrorAndKeepsEntry(t *testing.T) {
	m, f, _ := destroyFixture(t)
	f.destroyErr = errors.New("the array is still assembled; run: tool --stop data")
	m = lkey(t, toConfirm(t, m), "y")
	if m.step != layoutStepDestroyFailed {
		t.Fatalf("step %v, want the failure screen", m.step)
	}
	if v := m.View(); !strings.Contains(v, "the array is still assembled; run: tool --stop data") {
		t.Fatalf("failure lacks the error:\n%s", v)
	}
	if m = lkey(t, m, "n"); m.step != layoutStepList || m.cancelled {
		t.Fatalf("any key must return to the list: step %v", m.step)
	}
	if m = lkey(t, m, "x"); !strings.Contains(m.View(), "data  raid1 · 2 disks") {
		t.Fatalf("entry gone on the next x:\n%s", m.View())
	}
	if len(f.destroyed) != 1 {
		t.Fatalf("Destroy called %d times", len(f.destroyed))
	}
}
