package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	ctxErr     error // ctx.Err() as Destroy saw it
	onDestroy  func()
}

func (f *destroyFake) Existing(context.Context) ([]settings.Existing, []error) {
	return f.entries, f.errs
}

func (f *destroyFake) DestroyBlockers(_ context.Context, e settings.Existing) ([]string, error) {
	return f.blockers[e.ID], f.blockErr[e.ID]
}

func (f *destroyFake) Destroy(ctx context.Context, e settings.Existing) error {
	f.destroyed = append(f.destroyed, e)
	select {
	case <-ctx.Done():
		f.ctxErr = ctx.Err()
	default:
	}
	if f.onDestroy != nil {
		f.onDestroy()
	}
	return f.destroyErr
}

func disk(name string) settings.Target {
	return settings.Target{Name: name, Path: "/dev/" + name, Serial: "SN-" + name, Model: "M1"}
}

// destroyFixture: sdb..sdf discovered; sdb and sdc carry the array "data"
// (and are busy for that), sdd carries a blocked group "vg0", sde the group
// vg1 whose gate check finds blockers, sdf is free.
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
	drives, targets := testDrives("sdb", "sdc", "sdd", "sde", "sdf")
	m := newLayoutModel(f, drives, targets, rep, nil)
	m.rescan = func() (*inuse.Report, error) { return *cur, nil }
	m.tick = func() tea.Cmd { return nil }
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

func sized(m layoutModel, w, h int) layoutModel {
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return nm.(layoutModel)
}

// checkBounds fails when the view is taller or wider than w x h.
func checkBounds(t *testing.T, v string, w, h int) []string {
	t.Helper()
	lines := strings.Split(v, "\n")
	if len(lines) > h {
		t.Errorf("%dx%d: %d lines:\n%s", w, h, len(lines), v)
	}
	for _, l := range lines {
		if lipgloss.Width(l) > w {
			t.Errorf("%dx%d: line wider than %d: %q", w, h, w, l)
		}
	}
	return lines
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

// With no backend that can list existing configurations, x is not offered.
func TestDestroy_HiddenWithoutDestroyer(t *testing.T) {
	n := new(int)
	s := settings.NewLayout(fakeLayoutEnv(n))
	drives, targets := testDrives("sdb")
	m := newLayoutModel(s, drives, targets, freeReport(), nil)
	m.rescan = func() (*inuse.Report, error) { return freeReport(), nil }
	if v := m.View(); strings.Contains(v, "x destroy") {
		t.Fatalf("x offered with no provider:\n%s", v)
	}
	if m = lkey(t, m, "x"); m.step != layoutStepList {
		t.Fatalf("x with no provider: step %v", m.step)
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
	// No provider could be asked: not "none found".
	f.errs = []error{errors.New("provA: not available: a"), errors.New("provB: not available: b")}
	m = lkey(t, lkey(t, m, "esc"), "x")
	v = m.View()
	if strings.Contains(v, "no existing RAID configuration found") || !strings.Contains(v, "no provider could list existing configurations") {
		t.Fatalf("every provider failed, view:\n%s", v)
	}
}

// Answers of an earlier opening of the picker change nothing.
func TestDestroy_StaleAnswersDropped(t *testing.T) {
	m, f, _ := destroyFixture(t)
	nm, oldList := m.Update(keyMsg("x"))
	m = nm.(layoutModel)
	oldMsg := oldList()
	m = lkey(t, m, "esc")
	m = lkey(t, m, "x")
	// The old listing had other entries; the new picker is kept.
	old := oldMsg.(existingMsg)
	old.entries = []settings.Existing{{Backend: "provA", ID: "stale", Name: "stale"}}
	before := m.View()
	nm, cmd := m.Update(old)
	if cmd != nil || nm.(layoutModel).View() != before {
		t.Fatalf("a stale listing changed the picker:\n%s", nm.(layoutModel).View())
	}
	nm, _ = m.Update(blockersMsg{gen: old.gen, idx: 0, reasons: []string{"stale reason"}})
	if v := nm.(layoutModel).View(); v != before || strings.Contains(v, "stale reason") {
		t.Fatalf("stale blockers changed the picker:\n%s", v)
	}
	if len(f.destroyed) != 0 {
		t.Fatal("Destroy called")
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
	v := strings.Join(strings.Fields(m.View()), " ")
	for _, want := range []string{
		"THIS DESTROYS DATA", "data: raid1 · 2 disks",
		"filesystem ext4 on data", "array data (raid1)",
		"sdb M1 (S/N SN-sdb)", "sdc M1 (S/N SN-sdc)",
		"The data on these disks is not kept.",
		"Destroy checks again that nothing of it is in use and refuses if something is.",
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
	for _, k := range []string{"up", "down", "pgup", "pgdown"} {
		if c := lkey(t, m, k); c.step != layoutStepDestroyConfirm {
			t.Errorf("%q must only scroll: step %v", k, c.step)
		}
	}
	for _, k := range []string{"esc", "n", "q", "Y", " ", "j"} {
		c := lkey(t, m, k)
		if c.step != layoutStepList || c.cancelled {
			t.Errorf("%q: step %v cancelled %v, want back on the list", k, c.step, c.cancelled)
		}
	}
	if len(f.destroyed) != 0 {
		t.Fatalf("Destroy called without y: %v", f.destroyed)
	}
}

// bigFixture: one entry over 24 disks with a long Stack line.
func bigFixture(t *testing.T) layoutModel {
	t.Helper()
	m, f, _ := destroyFixture(t)
	var disks []settings.Target
	for i := 1; i <= 24; i++ {
		disks = append(disks, disk(fmt.Sprintf("sd%c%c", 'a'+i/26, 'a'+i%26)))
	}
	f.entries = []settings.Existing{{Backend: "provA", ID: "big", Name: "big", Summary: "raid6 · 24 disks", Disks: disks,
		Stack: []string{"filesystem xfs on big mounted nowhere, with a label that is much longer than the forty columns", "array big (raid6)", "partition table on big"}}}
	return m
}

var disksShown = regexp.MustCompile(`disks (\d+)-(\d+) of 24`)

func TestDestroy_ConfirmBounded(t *testing.T) {
	stack := []string{"filesystem xfs on big", "array big (raid6)", "partition table on big"}
	for _, sz := range [][2]int{{80, 24}, {80, 12}, {40, 24}, {40, 12}} {
		w, h := sz[0], sz[1]
		m := sized(toConfirm(t, bigFixture(t)), w, h)
		v := m.View()
		lines := checkBounds(t, v, w, h)
		flat := strings.Join(strings.Fields(v), " ")
		for _, want := range append([]string{"THIS DESTROYS DATA", "big: raid6 · 24 disks"}, stack...) {
			if !strings.Contains(flat, want) {
				t.Errorf("%dx%d lacks %q:\n%s", w, h, want, v)
			}
		}
		if !strings.HasSuffix(lines[len(lines)-1], "cancel") || !strings.Contains(flat, "Press y to destroy, any other key to cancel") {
			t.Errorf("%dx%d: the prompt is not last:\n%s", w, h, v)
		}
		sm := disksShown.FindStringSubmatch(v)
		if sm == nil {
			t.Fatalf("%dx%d: no disk window line:\n%s", w, h, v)
		}
		a, _ := strconv.Atoi(sm[1])
		b, _ := strconv.Atoi(sm[2])
		if got := strings.Count(v, "(S/N SN-"); got != b-a+1 || got < 1 {
			t.Errorf("%dx%d: %d disks shown, window says %d-%d", w, h, got, a, b)
		}
		// Scrolling moves the window and keeps the screen.
		s := lkey(t, m, "pgdown")
		if sm2 := disksShown.FindStringSubmatch(s.View()); s.step != layoutStepDestroyConfirm || sm2 == nil || sm2[1] == sm[1] {
			t.Errorf("%dx%d: pgdown did not scroll:\n%s", w, h, s.View())
		}
		checkBounds(t, s.View(), w, h)
	}
}

func TestDestroy_PickerBounded(t *testing.T) {
	m, f, _ := destroyFixture(t)
	f.entries = nil
	f.blockers = map[string][]string{}
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("e%02d", i)
		f.entries = append(f.entries, settings.Existing{Backend: "provA", ID: id, Name: id, Summary: "raid1 · 2 disks", Disks: []settings.Target{disk("sdb")}})
		f.blockers[id] = []string{"reason one of " + id, "reason two of " + id + " which is long enough to need wrapping at forty columns"}
	}
	m = sized(lkey(t, m, "x"), 40, 12)
	for i := 0; i < 29; i++ {
		m = lkey(t, m, "down")
		v := m.View()
		checkBounds(t, v, 40, 12)
		id := fmt.Sprintf("e%02d", i+1)
		if !strings.Contains(v, "reason two of "+id) {
			t.Fatalf("highlighted %s's reasons not visible:\n%s", id, v)
		}
	}
}

func TestDestroy_YDestroysOnceAndRefreshes(t *testing.T) {
	m, f, cur := destroyFixture(t)
	// A layout on a free disk, added before, keeps its entry.
	m = lkey(t, m, "b")
	if len(m.layouts) != 1 || m.layouts[0].value.ID != "none · 1 disk" {
		t.Fatalf("baseline = %+v, want sdf", m.layouts)
	}
	m = toConfirm(t, m)
	f.onDestroy = func() {
		// sdb is free afterwards, sdc still busy for another reason.
		*cur = &inuse.Report{Busy: map[string][]string{"sdc": {"mounted at /x"}, "sdd": {"member of vg0"}, "sde": {"member of vg1"}}}
	}
	nm, cmd := m.Update(keyMsg("y"))
	m = nm.(layoutModel)
	if m.step != layoutStepDestroyRun || !strings.Contains(m.View(), "destroying data ... 0 s, do not interrupt") {
		t.Fatalf("not running:\n%s", m.View())
	}
	for _, k := range []string{"q", "esc", "y"} {
		nm, c := m.Update(keyMsg(k))
		if r := nm.(layoutModel); r.step != layoutStepDestroyRun || r.cancelled || c != nil {
			t.Fatalf("%q during the run: step %v cancelled %v", k, r.step, r.cancelled)
		}
	}
	m = drain(t, m, cmd)
	if len(f.destroyed) != 1 || f.destroyed[0].ID != "uuid-1" {
		t.Fatalf("destroyed %+v, want uuid-1 once", f.destroyed)
	}
	if f.ctxErr != nil {
		t.Fatalf("ctx cancelled without Ctrl+C: %v", f.ctxErr)
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
	if len(m.layouts) != 1 || m.layouts[0].value.ID != "none · 1 disk" {
		t.Fatalf("the added layout changed: %+v", m.layouts)
	}
	// The freed disk is offered now.
	m = lkey(t, m, "b")
	if len(m.layouts) != 2 || m.layouts[1].value.ID != "none · 2 disks" {
		t.Fatalf("layouts = %+v, want a baseline on sdb, sdf", m.layouts)
	}
}

func TestDestroy_TickShowsElapsed(t *testing.T) {
	m, _, _ := destroyFixture(t)
	nm, _ := toConfirm(t, m).Update(keyMsg("y"))
	m = nm.(layoutModel)
	ticks := 0
	m.tick = func() tea.Cmd { ticks++; return nil }
	m.destroy.started = time.Now().Add(-12 * time.Second)
	nm, _ = m.Update(destroyTickMsg{gen: m.destroy.gen})
	m = nm.(layoutModel)
	if !strings.Contains(m.View(), "destroying data ... 12 s, do not interrupt") || ticks != 1 {
		t.Fatalf("ticks %d, view:\n%s", ticks, m.View())
	}
}

// Messages that only a disk without a serial or not discovered is grayed
// for: not claimed free.
func TestDestroy_FreeOnlyWhenOffered(t *testing.T) {
	m, f, cur := destroyFixture(t)
	f.entries[0].Disks = append(f.entries[0].Disks, disk("sdz"))
	m.drives[0].Device.Serial = "" // sdb
	f.onDestroy = func() { *cur = freeReport() }
	m = lkey(t, toConfirm(t, m), "y")
	v := m.View()
	for _, want := range []string{"data destroyed; sdc is free", "sdb is not offered: no serial", "sdz is not among the discovered disks"} {
		if !strings.Contains(v, want) {
			t.Errorf("message lacks %q:\n%s", want, v)
		}
	}
}

func TestDestroy_CtrlCStopsBetweenSteps(t *testing.T) {
	m, f, _ := destroyFixture(t)
	f.destroyErr = errors.New("stopped after the array was stopped; left: superblocks on sdb, sdc; run: tool --zero-superblock /dev/sdb /dev/sdc")
	nm, cmd := toConfirm(t, m).Update(keyMsg("y"))
	m = nm.(layoutModel)
	calls := 0
	orig := m.destroy.cancel
	m.destroy.cancel = func() { calls++; orig() }
	for range 2 {
		nm, c := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = nm.(layoutModel)
		if c != nil || m.cancelled || m.step != layoutStepDestroyRun {
			t.Fatalf("Ctrl+C must not quit while destroying (step %v)", m.step)
		}
	}
	if calls != 1 {
		t.Fatalf("cancel called %d times, want 1", calls)
	}
	if v := m.View(); !strings.Contains(strings.Join(strings.Fields(v), " "), "stopping after the step in flight finishes; nothing new is started") {
		t.Fatalf("view lacks the stopping line:\n%s", v)
	}
	m = drain(t, m, cmd)
	if !errors.Is(f.ctxErr, context.Canceled) {
		t.Fatalf("Destroy saw ctx err %v, want cancelled", f.ctxErr)
	}
	if m.step != layoutStepDestroyFailed || !strings.Contains(m.View(), "tool --zero-superblock /dev/sdb /dev/sdc") {
		t.Fatalf("the error is not shown:\n%s", m.View())
	}
}

func TestDestroy_FailureShowsErrorAndKeepsEntry(t *testing.T) {
	m, f, cur := destroyFixture(t)
	f.destroyErr = errors.New("the array is still assembled; run: tool --stop data")
	f.onDestroy = func() { *cur = &inuse.Report{Busy: map[string][]string{"sdc": {"member of data"}}} }
	m = lkey(t, toConfirm(t, m), "y")
	if m.step != layoutStepDestroyFailed {
		t.Fatalf("step %v, want the failure screen", m.step)
	}
	if v := m.View(); !strings.Contains(v, "run: tool --stop data") || !strings.Contains(v, "the array is still assembled;") {
		t.Fatalf("failure lacks the error:\n%s", v)
	}
	if _, gray := m.grayed["sdb"]; gray {
		t.Errorf("the rescan after the failure did not refresh sdb: %q", m.grayed["sdb"])
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

func TestDestroy_FailureWrapped(t *testing.T) {
	long := "stopped the array data; wiped the superblock on sdb; could not wipe the superblock on sdc: device or resource busy (exit status 1); what is left: the md superblock on sdc, which still names the array uuid 0123abcd:4567ef01:89abcdef:01234567; run by hand: tool --zero-superblock /dev/sdc"
	for _, w := range []int{40, 80} {
		m, f, _ := destroyFixture(t)
		f.destroyErr = errors.New(long)
		m = sized(lkey(t, toConfirm(t, m), "y"), w, 60)
		v := m.View()
		checkBounds(t, v, w, 60)
		squeeze := func(s string) string { return strings.Join(strings.Fields(s), "") }
		if !strings.Contains(squeeze(v), squeeze(long)) {
			t.Errorf("width %d: the error is not all there:\n%s", w, v)
		}
		// At 80 columns the command is on one line; at 40 it cannot be,
		// and is split only at its spaces.
		if w == 80 && !strings.Contains(v, "run by hand: tool --zero-superblock /dev/sdc") || !strings.Contains(strings.Join(strings.Fields(v), " "), "run by hand: tool --zero-superblock /dev/sdc") {
			t.Errorf("width %d: the command is split:\n%s", w, v)
		}
		// Bounded and scrollable on a short screen.
		s := sized(m, w, 8)
		lines := checkBounds(t, s.View(), w, 8)
		if !strings.Contains(s.View(), "of ") || !strings.Contains(lines[len(lines)-1], "list") {
			t.Errorf("width %d: short failure screen:\n%s", w, s.View())
		}
		if s = lkey(t, s, "pgdown"); s.step != layoutStepDestroyFailed {
			t.Errorf("pgdown must scroll, step %v", s.step)
		}
	}
}

// A layout whose disk became busy after a destroy's rescan is marked and
// cannot be confirmed; also when the rescan failed.
func TestDestroy_AddedLayoutRecheckedAfterRescan(t *testing.T) {
	for _, failed := range []bool{false, true} {
		m, f, cur := destroyFixture(t)
		// sdb and sdf free: a baseline and a linear layout on them.
		m.applyInUse(&inuse.Report{Busy: map[string][]string{"sdc": {"member of data"}}}, nil)
		m = lkey(t, m, "b")
		m = addLayout(t, m, 0, "linear", "sdb", "sdf")
		f.onDestroy = func() {
			*cur = &inuse.Report{Busy: map[string][]string{"sdf": {"mounted at /new"}}}
		}
		if failed {
			m.rescan = func() (*inuse.Report, error) { return nil, errors.New("read mountinfo: boom") }
		}
		m = lkey(t, toConfirm(t, m), "y")
		name, why := "sdf", "mounted at /new"
		if failed { // every disk is busy: the first one of the layout is named
			name, why = "sdb", "in-use check failed: read mountinfo: boom"
		}
		v := m.View()
		if !strings.Contains(v, "disk "+name+" is now in use: "+why) {
			t.Errorf("failed=%v: list lacks the mark:\n%s", failed, v)
		}
		m = lkey(t, m, "c")
		if m.step != layoutStepList || !strings.Contains(m.msg, name) || !strings.Contains(m.msg, why) {
			t.Errorf("failed=%v: c must refuse (step %v, msg %q)", failed, m.step, m.msg)
		}
	}
}
