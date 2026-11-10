package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"bbbench/internal/inuse"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// fakeLayoutEnv finds only tools on the PATH; every command it is asked to
// run (and every in-use scan) is counted and fails.
func fakeLayoutEnv(cmds *int, tools ...string) settings.Env {
	return settings.Env{
		LookPath: func(p string) (string, error) {
			if slices.Contains(tools, p) {
				return "/usr/sbin/" + p, nil
			}
			return "", exec.ErrNotFound
		},
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			*cmds++
			return nil, errors.New("the layout screens must run no command")
		},
		InUse: func() (*inuse.Report, error) {
			*cmds++
			return freeReport(), nil
		},
	}
}

// bothBackends is the production layout setting (both backends) over a
// fake PATH holding tools.
func bothBackends(tools ...string) (settings.LayoutSetting, *int) {
	n := new(int)
	env := fakeLayoutEnv(n, tools...)
	return settings.NewLayout(env, settings.NewMdadm(env), settings.NewLVM(env)), n
}

func freeReport() *inuse.Report { return &inuse.Report{Busy: map[string][]string{}} }

func layoutModelFor(s settings.LayoutSetting, names ...string) layoutModel {
	drives, targets := testDrives(names...)
	return newLayoutModel(s, drives, targets, freeReport(), nil)
}

func lpress(m tea.Model, keys ...string) layoutModel { return press(m, keys...).(layoutModel) }

// toDisk moves the editor cursor onto disk name.
func toDisk(t *testing.T, m layoutModel, name string) layoutModel {
	t.Helper()
	m = lpress(m, slices.Repeat([]string{"up"}, 30)...)
	for i := 0; i < 30 && m.cursorDisk() != name; i++ {
		m = lpress(m, "down")
	}
	if m.cursorDisk() != name {
		t.Fatalf("no editor row for disk %s:\n%s", name, m.View())
	}
	return m
}

// toLevel cycles the level row until the level is want.
func toLevel(t *testing.T, m layoutModel, want string) layoutModel {
	t.Helper()
	m = lpress(m, slices.Repeat([]string{"up"}, 30)...)
	for i := 0; i < 20 && m.edit.level != want; i++ {
		m = lpress(m, "right")
	}
	if m.edit.level != want {
		t.Fatalf("level %q not reached (at %q)", want, m.edit.level)
	}
	return m
}

// setRole presses Space on disk name until it has role ("" = unused).
func setRole(t *testing.T, m layoutModel, name string, role settings.Role) layoutModel {
	t.Helper()
	m = toDisk(t, m, name)
	for i := 0; i < 6 && m.edit.roles[name] != role; i++ {
		m = lpress(m, " ")
	}
	if m.edit.roles[name] != role {
		t.Fatalf("%s: role %q not reached (at %q)", name, role, m.edit.roles[name])
	}
	return m
}

// addLayout adds one layout of the backend at index bi of the picker
// (-1: no picker) and level on the data disks, and saves it.
func addLayout(t *testing.T, m layoutModel, bi int, level string, data ...string) layoutModel {
	t.Helper()
	m = lpress(m, "a")
	if bi >= 0 {
		m = lpress(m, slices.Repeat([]string{"up"}, 5)...)
		m = lpress(m, slices.Repeat([]string{"down"}, bi)...)
		m = lpress(m, "enter")
	}
	if m.step != layoutStepEditor {
		t.Fatalf("not in the editor:\n%s", m.View())
	}
	m = toLevel(t, m, level)
	for _, d := range data {
		m = setRole(t, m, d, settings.RoleData)
	}
	n := len(m.layouts)
	m = lpress(m, "enter")
	if len(m.layouts) != n+1 || m.step != layoutStepList {
		t.Fatalf("layout not saved (%s):\n%s", m.msg, m.View())
	}
	return m
}

func TestMode_ListsVariantSettings(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	drives, targets := testDrives("sdb", "sdc")
	mm := press(newModeModel([]settings.Setting{perDrive(newFakeSetting(nil)), s}, drives, targets), "down", "enter")
	if v := mm.View(); !strings.Contains(v, "software layout: applies to 2 of 2 drives") {
		t.Fatalf("view lacks the software layout with its count:\n%s", v)
	}
	m := press(mm, "down", "enter").(modeModel)
	if !m.done || m.choice.Setting == nil || m.choice.Setting.Name() != "software layout" {
		t.Fatalf("choice = %+v, want the software layout", m.choice)
	}
}

func TestLayout_BackendPickerFirst(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	m := lpress(layoutModelFor(s, "sdb", "sdc", "sdd"), "a")
	if m.step != layoutStepBackend {
		t.Fatalf("step = %v, want the backend picker", m.step)
	}
	if v := m.View(); !strings.Contains(v, "mdadm") || !strings.Contains(v, "lvm") {
		t.Fatalf("picker lacks a backend:\n%s", v)
	}
	m = lpress(m, "down", "enter")
	if m.step != layoutStepEditor || !strings.Contains(m.View(), "Layout 1 (lvm)") {
		t.Fatalf("want the lvm editor:\n%s", m.View())
	}
	if m.edit.level != "linear" {
		t.Fatalf("level = %q, want the first lvm level", m.edit.level)
	}
	m = toLevel(t, m, "mirror") // an lvm-only level
	if !strings.Contains(m.View(), "level: mirror") {
		t.Fatalf("view lacks the level:\n%s", m.View())
	}
	// Esc goes back to the list from the picker.
	m = lpress(layoutModelFor(s, "sdb"), "a", "esc")
	if m.step != layoutStepList {
		t.Fatalf("step = %v, want the list after Esc", m.step)
	}
}

func TestLayout_SingleBackendSkipsPicker(t *testing.T) {
	n := new(int)
	env := fakeLayoutEnv(n, "mdadm")
	s := settings.NewLayout(env, settings.NewMdadm(env))
	m := lpress(layoutModelFor(s, "sdb", "sdc"), "a")
	if m.step != layoutStepEditor || !strings.Contains(m.View(), "Layout 1 (mdadm)") {
		t.Fatalf("want the mdadm editor at once:\n%s", m.View())
	}
}

func TestLayout_UnavailableBackendGrayed(t *testing.T) {
	s, _ := bothBackends("mdadm")
	m := lpress(layoutModelFor(s, "sdb", "sdc"), "a")
	if m.step != layoutStepBackend {
		t.Fatalf("step = %v, want the picker", m.step)
	}
	if v := m.View(); !strings.Contains(v, "the lvm2 tools are not installed") {
		t.Fatalf("picker lacks the reason:\n%s", v)
	}
	m = lpress(m, "down", "enter")
	if m.step != layoutStepBackend {
		t.Fatalf("an unavailable backend must not be chosen, step = %v", m.step)
	}
	m = lpress(m, "up", "enter")
	if m.step != layoutStepEditor || !strings.Contains(m.View(), "Layout 1 (mdadm)") {
		t.Fatalf("want the mdadm editor:\n%s", m.View())
	}
}

func TestLayout_NoBackend(t *testing.T) {
	s, _ := bothBackends()
	m := lpress(layoutModelFor(s, "sdb", "sdc"), "a")
	if m.step != layoutStepList {
		t.Fatalf("step = %v, want to stay on the list", m.step)
	}
	v := m.View()
	for _, want := range []string{"no layout backend is available: ", "mdadm is not installed", "the lvm2 tools are not installed"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view lacks %q:\n%s", want, v)
		}
	}
}

func TestLayout_BusyDiskGrayedWithReason(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	drives, targets := testDrives("sdb", "sdc")
	busy := &inuse.Report{Busy: map[string][]string{"sdb": {"mounted at /data"}}}
	m := lpress(newLayoutModel(s, drives, targets, busy, nil), "a", "enter")
	if v := m.View(); !strings.Contains(v, "sdb  V M1  mounted at /data") {
		t.Fatalf("view lacks the grayed disk with its reason:\n%s", v)
	}
	m = lpress(toDisk(t, m, "sdb"), " ")
	if m.edit.roles["sdb"] != "" {
		t.Fatalf("a busy disk got the role %q", m.edit.roles["sdb"])
	}
	if !strings.Contains(m.View(), "sdb is in use: mounted at /data") {
		t.Fatalf("view lacks the message:\n%s", m.View())
	}
	// The baseline takes only the free disks.
	m = lpress(m, "esc", "b")
	if len(m.layouts) != 1 || m.layouts[0].value.ID != "none · 1 disk" {
		t.Fatalf("baseline = %+v, want sdc only", m.layouts)
	}
	// A disk the setting does not apply to is grayed with its reason.
	drives, targets = testDrives("sdb")
	drives[0].Device.Serial = ""
	m = lpress(newLayoutModel(s, drives, targets, freeReport(), nil), "a", "enter")
	m = lpress(toDisk(t, m, "sdb"), " ")
	if m.edit.roles["sdb"] != "" || !strings.Contains(m.View(), "sdb: no serial") {
		t.Fatalf("a disk with no serial must be grayed:\n%s", m.View())
	}
}

// TestLayout_CarriesDataSelectableAndMarked: a disk with an unmounted
// filesystem is offered, marked in the editor, on each confirmation line and
// in the erased list.
func TestLayout_CarriesDataSelectableAndMarked(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	drives, targets := testDrives("sdb", "sdc")
	rep := freeReport()
	rep.Carries = map[string]string{"sdb": "gpt partition table, ext4 on sdb1"}
	m := lpress(newLayoutModel(s, drives, targets, rep, nil), "a", "enter")
	if v := m.View(); !strings.Contains(v, "sdb  V M1  1099 GB  carries data: gpt partition table, ext4 on sdb1") {
		t.Fatalf("editor lacks the mark:\n%s", v)
	}
	m = setRole(t, m, "sdb", settings.RoleData)
	m = setRole(t, m, "sdc", settings.RoleData)
	m = lpress(m, "enter", "b", "c")
	if m.step != layoutStepConfirm {
		t.Fatalf("not on the confirmation (%s):\n%s", m.msg, m.View())
	}
	v := m.View()
	for _, want := range []string{
		"sdb  V M1 (S/N SN-sdb)  data  carries data: gpt partition table, ext4 on sdb1",
		"Disks erased: sdb (carries data: gpt partition table, ext4 on sdb1), sdc",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, v)
		}
	}
}

func TestLayout_InUseCheckFailedGraysAll(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	drives, targets := testDrives("sdb", "sdc")
	m := newLayoutModel(s, drives, targets, nil, errors.New("read mountinfo: boom"))
	if v := m.View(); !strings.Contains(v, "No disk can be used: in-use check failed: read mountinfo: boom") {
		t.Fatalf("list lacks the reason:\n%s", v)
	}
	m = lpress(m, "b")
	if len(m.layouts) != 0 {
		t.Fatalf("a baseline was added on busy disks: %+v", m.layouts)
	}
	m = lpress(m, "a", "enter")
	for _, d := range []string{"sdb", "sdc"} {
		m = lpress(toDisk(t, m, d), " ")
		if m.edit.roles[d] != "" {
			t.Fatalf("%s selectable after a failed in-use check", d)
		}
	}
	if !strings.Contains(m.View(), "sdc is in use: in-use check failed: read mountinfo: boom") {
		t.Fatalf("view lacks the message:\n%s", m.View())
	}
}

// Report.All (the output directory on tmpfs, say) makes every disk
// unavailable, and the list says why at the top, not as an empty list.
func TestLayout_AllReasonShownProminently(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	drives, targets := testDrives("sdb", "sdc")
	why := "the output directory (/run/out) could not be attributed to a disk; no disk can be offered"
	m := newLayoutModel(s, drives, targets, &inuse.Report{Busy: map[string][]string{}, All: why}, nil)
	if v := m.View(); !strings.Contains(v, "No disk can be used: "+why) {
		t.Fatalf("list lacks the All reason:\n%s", v)
	}
	m = lpress(m, "a", "enter")
	m = lpress(toDisk(t, m, "sdb"), " ")
	if m.edit.roles["sdb"] != "" {
		t.Fatal("a disk is selectable while no disk can be used")
	}
}

func TestLayout_RolesCycle(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	cycle := func(m layoutModel, disk string) []settings.Role {
		m = toDisk(t, m, disk)
		var got []settings.Role
		for range 4 {
			m = lpress(m, " ")
			got = append(got, m.edit.roles[disk])
			if m.edit.roles[disk] == "" {
				break
			}
		}
		return got
	}
	m := lpress(layoutModelFor(s, "sdb", "sdc"), "a", "enter")
	if got, want := cycle(toLevel(t, m, "raid5"), "sdb"), []settings.Role{"data", "spare", "journal", ""}; !slices.Equal(got, want) {
		t.Errorf("mdadm raid5: %v, want %v", got, want)
	}
	if got, want := cycle(toLevel(t, m, "raid0"), "sdb"), []settings.Role{"data", ""}; !slices.Equal(got, want) {
		t.Errorf("mdadm raid0: %v, want %v", got, want)
	}
	m = lpress(layoutModelFor(s, "sdb", "sdc"), "a", "down", "enter")
	if got, want := cycle(toLevel(t, m, "raid5"), "sdb"), []settings.Role{"data", ""}; !slices.Equal(got, want) {
		t.Errorf("lvm raid5: %v, want %v", got, want)
	}
	// A level change drops roles the new level does not take.
	m = lpress(layoutModelFor(s, "sdb", "sdc"), "a", "enter")
	m = setRole(t, toLevel(t, m, "raid5"), "sdb", settings.RoleSpare)
	m = toLevel(t, m, "raid0")
	if m.edit.roles["sdb"] != "" {
		t.Errorf("raid0 kept a %q disk", m.edit.roles["sdb"])
	}
}

func TestLayout_RejectsTooFewDisks(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	m := lpress(layoutModelFor(s, "sdb", "sdc", "sdd"), "a", "enter")
	m = toLevel(t, m, "raid5")
	m = setRole(t, m, "sdb", settings.RoleData)
	m = setRole(t, m, "sdc", settings.RoleData)
	m = lpress(m, "enter")
	if m.step != layoutStepEditor || len(m.layouts) != 0 {
		t.Fatalf("an invalid layout was saved: step %v, %d layouts", m.step, len(m.layouts))
	}
	if !strings.Contains(m.View(), "raid5 needs at least 3 data disks, has 2") {
		t.Fatalf("view lacks the validation error:\n%s", m.View())
	}
	m = setRole(t, m, "sdd", settings.RoleData)
	if v := m.View(); !strings.Contains(v, "mdadm raid5 · 3 disks · 512K") {
		t.Fatalf("view lacks the label of the valid layout:\n%s", v)
	}
	if m = lpress(m, "enter"); len(m.layouts) != 1 {
		t.Fatalf("the valid layout was not saved:\n%s", m.View())
	}
	// Esc cancels an edit.
	m = lpress(m, "a", "enter", "esc")
	if m.step != layoutStepList || len(m.layouts) != 1 {
		t.Fatalf("Esc: step %v, %d layouts", m.step, len(m.layouts))
	}
}

func optionLabels(m layoutModel) []string {
	var out []string
	for _, r := range m.rows() {
		if r.kind == rowOption {
			out = append(out, r.opt.Label)
		}
	}
	return out
}

func TestLayout_OptionRowsPerBackend(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	md := lpress(layoutModelFor(s, "sdb", "sdc", "sdd", "sde"), "a", "enter")
	if got := optionLabels(toLevel(t, md, "raid1")); slices.Contains(got, "chunk") || !slices.Contains(got, "write-intent bitmap") {
		t.Errorf("mdadm raid1 options = %v", got)
	}
	if got := optionLabels(toLevel(t, md, "raid0")); slices.Contains(got, "write-intent bitmap") || !slices.Contains(got, "chunk") {
		t.Errorf("mdadm raid0 options = %v", got)
	}
	lv := lpress(layoutModelFor(s, "sdb", "sdc", "sdd", "sde"), "a", "down", "enter")
	if got := optionLabels(toLevel(t, lv, "striped")); !slices.Equal(got, []string{"stripes", "stripe size"}) {
		t.Errorf("lvm striped options = %v", got)
	}
	if got := optionLabels(toLevel(t, lv, "raid1")); len(got) != 0 {
		t.Errorf("lvm raid1 options = %v, want none", got)
	}
	// A no/yes option toggles with Space.
	md = toLevel(t, md, "raid1")
	md = lpress(md, "down", " ") // the bitmap row
	if v := md.View(); !strings.Contains(v, "write-intent bitmap: yes") {
		t.Fatalf("Space did not toggle the bitmap:\n%s", v)
	}
}

func TestLayout_StripesFollowDisks(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	m := lpress(layoutModelFor(s, "sdb", "sdc", "sdd", "sde"), "a", "down", "enter")
	m = toLevel(t, m, "striped")
	for _, d := range []string{"sdb", "sdc", "sdd", "sde"} {
		m = setRole(t, m, d, settings.RoleData)
	}
	if m.edit.options["stripes"] != "4" {
		t.Fatalf("stripes = %q, want 4 (every disk)", m.edit.options["stripes"])
	}
	if v := m.View(); !strings.Contains(v, "lvm striped · 4 disks · 64K stripe") || strings.Contains(v, "stripes ·") {
		t.Fatalf("label of a full-width stripe:\n%s", v)
	}
	m = lpress(m, slices.Repeat([]string{"up"}, 30)...)
	m = lpress(m, "down", "left", "left") // stripes 4 -> 3 -> 2
	if v := m.View(); !strings.Contains(v, "· 2 stripes ·") {
		t.Fatalf("label lacks the stripe count:\n%s", v)
	}
	m = setRole(t, m, "sdd", "")
	m = setRole(t, m, "sde", "")
	if m.edit.options["stripes"] != "2" {
		t.Fatalf("stripes = %q after removing two disks, want 2", m.edit.options["stripes"])
	}
	// A count no longer possible goes back to the default.
	m = lpress(layoutModelFor(s, "sdb", "sdc", "sdd", "sde"), "a", "down", "enter")
	m = toLevel(t, m, "striped")
	for _, d := range []string{"sdb", "sdc", "sdd"} {
		m = setRole(t, m, d, settings.RoleData)
	}
	m = lpress(m, slices.Repeat([]string{"up"}, 30)...)
	m = lpress(m, "down", "left") // 3 -> 2
	m = setRole(t, m, "sde", settings.RoleData)
	if m.edit.options["stripes"] != "2" {
		t.Fatalf("a chosen stripe count must be kept, got %q", m.edit.options["stripes"])
	}
}

func TestLayout_BaselineVariant(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	m := lpress(layoutModelFor(s, "sdb", "sdc", "sdd", "sde"), "b")
	if len(m.layouts) != 1 || m.layouts[0].value.ID != "none · 4 disks" {
		t.Fatalf("layouts = %+v, want the baseline over 4 disks", m.layouts)
	}
	if !strings.Contains(m.View(), "none · 4 disks") {
		t.Fatalf("list lacks the baseline:\n%s", m.View())
	}
	// Edit it: a disk taken out of the baseline.
	m = lpress(m, "enter")
	if m.step != layoutStepEditor {
		t.Fatalf("Enter does not edit the baseline:\n%s", m.View())
	}
	m = setRole(t, m, "sde", "")
	m = lpress(m, "enter")
	if m.layouts[0].value.ID != "none · 3 disks" {
		t.Fatalf("baseline after edit = %q", m.layouts[0].value.ID)
	}
	// d deletes the selected layout.
	if m = lpress(m, "d"); len(m.layouts) != 0 {
		t.Fatalf("d did not delete: %+v", m.layouts)
	}
}

func TestLayout_DiskInSeveralVariants(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	m := layoutModelFor(s, "sdb", "sdc")
	m = addLayout(t, m, 0, "raid0", "sdb", "sdc")
	m = addLayout(t, m, 1, "striped", "sdb", "sdc")
	m = lpress(m, "c", "y")
	c, err := m.outcome()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Variants) != 2 || len(c.Drives) != 2 {
		t.Fatalf("variants %d, drives %d; want 2 and 2", len(c.Variants), len(c.Drives))
	}
	for _, v := range c.Variants {
		if len(v.Disks) != 2 || v.Disks[0].Name != "sdb" || v.Disks[1].Name != "sdc" {
			t.Fatalf("variant %s disks = %+v", v.Value.ID, v.Disks)
		}
	}
	// The same layout twice is refused.
	m = layoutModelFor(s, "sdb", "sdc")
	m = addLayout(t, m, 0, "raid0", "sdb", "sdc")
	m = lpress(m, "a", "enter")
	m = toLevel(t, m, "raid0")
	m = setRole(t, setRole(t, m, "sdb", settings.RoleData), "sdc", settings.RoleData)
	if m = lpress(m, "enter"); len(m.layouts) != 1 || !strings.Contains(m.View(), "already") {
		t.Fatalf("a duplicate layout was saved:\n%s", m.View())
	}
}

func TestLayout_NeedsTwoVariants(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	m := lpress(layoutModelFor(s, "sdb", "sdc"), "b", "c")
	if m.step != layoutStepList {
		t.Fatalf("step = %v, want the list", m.step)
	}
	if !strings.Contains(m.View(), "Add at least two layouts to compare (the no-array baseline counts as one).") {
		t.Fatalf("view lacks the message:\n%s", m.View())
	}
	m = addLayout(t, m, 0, "raid1", "sdb", "sdc")
	if m = lpress(m, "c"); m.step != layoutStepConfirm {
		t.Fatalf("two layouts must reach the confirmation:\n%s", m.View())
	}
}

func TestLayout_CapacityWarning(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	drives, targets := testDrives("sdb", "sdc")
	drives[0].Device.CapacityBytes = 4000 * 1_000_000_000
	drives[1].Device.CapacityBytes = 2000 * 1_000_000_000
	m := lpress(newLayoutModel(s, drives, targets, freeReport(), nil), "a", "enter")
	m = toLevel(t, m, "raid1")
	m = setRole(t, setRole(t, m, "sdb", settings.RoleData), "sdc", settings.RoleData)
	if v := m.View(); !strings.Contains(v, "members differ in capacity (sdb 4000 GB, sdc 2000 GB): the layout uses the smallest") {
		t.Fatalf("view lacks the capacity warning:\n%s", v)
	}
	// Same capacity: no warning.
	m = lpress(layoutModelFor(s, "sdb", "sdc"), "a", "enter")
	m = setRole(t, setRole(t, toLevel(t, m, "raid1"), "sdb", settings.RoleData), "sdc", settings.RoleData)
	if strings.Contains(m.View(), "differ in capacity") {
		t.Fatalf("warning on equal disks:\n%s", m.View())
	}
}

// confirmFixture is a baseline over sdb..sde and an mdadm raid0 on sdb, sdc
// on the confirmation screen.
func confirmFixture(t *testing.T) (layoutModel, *int) {
	t.Helper()
	s, cmds := bothBackends("mdadm", "lvm")
	m := lpress(layoutModelFor(s, "sdb", "sdc", "sdd", "sde"), "b")
	m = addLayout(t, m, 0, "raid0", "sdb", "sdc")
	m = lpress(m, "c")
	if m.step != layoutStepConfirm {
		t.Fatalf("not on the confirmation:\n%s", m.View())
	}
	return m, cmds
}

func TestLayout_ConfirmListsEveryDiskAndLayout(t *testing.T) {
	m, _ := confirmFixture(t)
	v := m.View()
	for _, want := range []string{
		"Confirm the layout test: THE DISKS BELOW WILL BE ERASED",
		"Run 1: none · 4 disks", "Run 2: mdadm raid0 · 2 disks · 512K",
		"sdb  V M1 (S/N SN-sdb)  data", "SN-sdc", "SN-sdd", "SN-sde",
		"Disks erased: sdb, sdc, sdd, sde",
		"2 runs of the full test plan; estimated time",
		"plus the initial resync of each layout, which is not included",
		"Each layout is torn down and its members wiped after its run, also after an error or Ctrl+C.",
		"Nothing is built before you press y.",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("confirmation lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(v)
	}
	// Esc goes back to the list, q quits.
	if b := lpress(m, "esc"); b.step != layoutStepList {
		t.Errorf("Esc: step %v", b.step)
	}
	if q := lpress(m, "q"); !q.cancelled || q.result != nil {
		t.Errorf("q: cancelled %v result %v", q.cancelled, q.result)
	}
}

func TestLayout_NothingBuiltBeforeConfirm(t *testing.T) {
	s, cmds := bothBackends("mdadm", "lvm")
	m := layoutModelFor(s, "sdb", "sdc", "sdd", "sde")
	m = lpress(m, "b")
	m = lpress(m, "a")                         // picker
	m = lpress(m, "esc", "a", "down", "enter") // lvm editor
	m = toLevel(t, m, "striped")               // option rows
	m = setRole(t, setRole(t, m, "sdb", "data"), "sdc", "data")
	m = lpress(m, "enter")
	m = addLayout(t, m, 0, "raid5", "sdb", "sdc", "sdd")
	m = lpress(m, "up", "enter", "esc") // edit, cancel
	m = lpress(m, "c")
	if *cmds != 0 {
		t.Fatalf("%d commands or scans before the confirmation", *cmds)
	}
	m = lpress(m, "enter")
	if m.done || m.result != nil || m.step != layoutStepConfirm {
		t.Fatalf("Enter on the confirmation must do nothing (done %v)", m.done)
	}
	m = lpress(m, "y")
	if !m.done || m.result == nil {
		t.Fatal("y must produce the choice")
	}
	if *cmds != 0 {
		t.Fatalf("the model ran %d commands or scans", *cmds)
	}
}

func TestLayout_OutcomeVariants(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	m := layoutModelFor(s, "sdb", "sdc", "sdd", "sde")
	m = lpress(m, "a", "enter")
	m = toLevel(t, m, "raid1")
	m = setRole(t, m, "sdc", settings.RoleData)
	m = setRole(t, m, "sdd", settings.RoleData)
	m = setRole(t, m, "sde", settings.RoleSpare)
	m = lpress(m, "enter")
	m = lpress(m, "b", "c", "y")
	c, err := m.outcome()
	if err != nil {
		t.Fatal(err)
	}
	wantValues := []string{"mdadm raid1 · 2 data + 1 spare", "none · 4 disks"}
	var gotValues []string
	for _, v := range c.Values {
		gotValues = append(gotValues, v.ID)
	}
	if !slices.Equal(gotValues, wantValues) {
		t.Fatalf("values = %v, want %v", gotValues, wantValues)
	}
	if c.Strategy != rungroup.Sequential || c.Setting.Name() != "software layout" {
		t.Fatalf("strategy %v setting %v", c.Strategy, c.Setting.Name())
	}
	if len(c.Variants) != 2 || c.Variants[0].Value.ID != wantValues[0] || c.Variants[1].Value.ID != wantValues[1] {
		t.Fatalf("variants = %+v", c.Variants)
	}
	var roles []string
	for _, d := range c.Variants[0].Disks {
		roles = append(roles, d.Name+":"+string(d.Role))
	}
	if !slices.Equal(roles, []string{"sdc:data", "sdd:data", "sde:spare"}) {
		t.Fatalf("roles = %v", roles)
	}
	var names []string
	for _, d := range c.Drives {
		names = append(names, d.Device.Name)
	}
	if !slices.Equal(names, []string{"sdc", "sdd", "sde", "sdb"}) {
		t.Fatalf("drives = %v, want the union in order of first use", names)
	}
	// Every disk of every layout is a group drive with a serial, at the
	// node of the same discovery.
	byName := drivesByName(c.Drives)
	for _, v := range c.Variants {
		for _, d := range v.Disks {
			di, ok := byName[d.Name]
			if !ok || d.Serial == "" || d.Path != di.Device.Path || d.Serial != di.Device.Serial {
				t.Fatalf("disk %+v does not match its drive %+v", d.Target, di.Device)
			}
		}
	}
	if err := checkLayoutDisks(groupConfig{Variants: c.Variants, Drives: c.Drives}); err != nil {
		t.Fatalf("the group loop refuses the choice: %v", err)
	}
	cfg := newGroupConfig(c, nil, runOptions{OutDir: t.TempDir()}, "h", nil, nil)
	if len(cfg.Variants) != 2 {
		t.Fatalf("newGroupConfig dropped the variants: %+v", cfg.Variants)
	}
}

func TestInUsePaths_OutputDirNotYetCreated(t *testing.T) {
	dir := t.TempDir()
	got := inUsePaths("/usr/local/bin/bbbench", filepath.Join(dir, "out", "run1"))
	if got["/usr/local/bin/bbbench"] != "the bbbench binary" {
		t.Errorf("binary: %v", got)
	}
	if got[dir] != "the output directory" {
		t.Errorf("want the nearest existing ancestor %s as the output directory: %v", dir, got)
	}
	if len(got) != 2 {
		t.Errorf("paths = %v", got)
	}
	// An existing output directory is itself.
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := inUsePaths("", filepath.Join(dir, "out")); got[filepath.Join(dir, "out")] != "the output directory" || len(got) != 1 {
		t.Errorf("paths = %v", got)
	}
}

func layoutGroupFile() *rungroup.GroupFile {
	return &rungroup.GroupFile{
		ID: "5", Setting: "software layout", Strategy: rungroup.Sequential,
		Values: []string{"mdadm raid1 · 2 disks · 512K", "none · 2 disks"},
		Variants: []rungroup.GroupVariant{
			{Value: "mdadm raid1 · 2 disks", Disks: []rungroup.Member{{Name: "old1", Serial: "SN-sdb", Model: "M1", Role: settings.RoleData}, {Name: "old2", Serial: "SN-sdc", Model: "M1", Role: settings.RoleData}}},
			{Value: "none · 2 disks", Disks: []rungroup.Member{{Name: "old1", Serial: "SN-sdb", Model: "M1", Role: settings.RoleData}, {Name: "old2", Serial: "SN-sdc", Model: "M1", Role: settings.RoleData}}},
		},
		Runs: []rungroup.GroupRun{{Status: rungroup.RunDone}, {Status: rungroup.RunPlanned}},
	}
}

func TestResume_LayoutGroupRebuildsVariants(t *testing.T) {
	s, _ := bothBackends("mdadm", "lvm")
	drives, _ := testDrives("sdx", "sdy")
	drives[0].Device.Serial, drives[1].Device.Serial = "SN-sdb", "SN-sdc"
	free := func() (*inuse.Report, error) { return freeReport(), nil }
	c, err := resumeChoice(layoutGroupFile(), []settings.Setting{s}, drives, free)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Variants) != 2 || c.Variants[0].Value.ID != "mdadm raid1 · 2 disks" || c.Variants[1].Value.ID != "none · 2 disks" {
		t.Fatalf("variants = %+v", c.Variants)
	}
	if d := c.Variants[0].Disks[0]; d.Name != "sdx" || d.Path != "/dev/sdx" || d.Serial != "SN-sdb" || d.Role != settings.RoleData {
		t.Fatalf("disk = %+v, want sdx found by its serial", d)
	}
	if len(c.Drives) != 2 || c.Drives[0].Device.Name != "sdx" || c.Drives[1].Device.Name != "sdy" {
		t.Fatalf("drives = %+v", c.Drives)
	}
	if len(c.Values) != 2 || c.Values[0].ID != "mdadm raid1 · 2 disks" {
		t.Fatalf("values = %+v", c.Values)
	}
	if err := sameVariants(layoutGroupFile().Variants, c.Variants); err != nil {
		t.Fatalf("the group loop would refuse the resume: %v", err)
	}
	// A disk now busy stops the resume.
	busy := func() (*inuse.Report, error) {
		return &inuse.Report{Busy: map[string][]string{"sdy": {"mounted at /data"}}}, nil
	}
	_, err = resumeChoice(layoutGroupFile(), []settings.Setting{s}, drives, busy)
	if err == nil || err.Error() != "cannot resume group 5: sdy is in use: mounted at /data" {
		t.Fatalf("err = %v", err)
	}
	// A disk gone stops it too.
	if _, err := resumeChoice(layoutGroupFile(), []settings.Setting{s}, drives[:1], free); err == nil {
		t.Fatal("a missing disk must be an error")
	}
	// prepareGroupResume lists the disks to erase and asks.
	var out strings.Builder
	if _, err := prepareGroupResume(strings.NewReader("n\n"), &out, layoutGroupFile(), []settings.Setting{s}, drives, free); err == nil {
		t.Fatal("answering no must not resume")
	}
	if o := out.String(); !strings.Contains(o, "WILL BE ERASED") || !strings.Contains(o, "sdx") || !strings.Contains(o, "mdadm raid1 · 2 disks") {
		t.Fatalf("resume confirmation incomplete:\n%s", o)
	}
	if c, err := prepareGroupResume(strings.NewReader("y\n"), io.Discard, layoutGroupFile(), []settings.Setting{s}, drives, free); err != nil || len(c.Variants) != 2 {
		t.Fatalf("yes: %v %v", c, err)
	}
}
