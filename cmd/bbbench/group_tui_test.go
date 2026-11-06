package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bbbench/internal/diskinfo"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// ifaceSetting is a fake setting that does not apply to USB drives.
type ifaceSetting struct{ *fakeSetting }

func (s ifaceSetting) Applicable(t settings.Target) (bool, string) {
	if t.Interface == "USB" {
		return false, "no controllable write cache on USB"
	}
	return true, ""
}

// variantSetting is a fake layout-style setting.
type variantSetting struct{ *fakeSetting }

func (variantSetting) Name() string          { return "raid layout" }
func (variantSetting) Input() settings.Input { return settings.InputVariants }

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func press(m tea.Model, keys ...string) tea.Model {
	for _, k := range keys {
		m, _ = m.Update(keyMsg(k))
	}
	return m
}

func TestMode_SingleRunReturnsNilSetting(t *testing.T) {
	drives, targets := testDrives("sda", "sdb")
	m := press(newModeModel([]settings.Setting{newFakeSetting(nil)}, drives, targets), "enter").(modeModel)
	if !m.done || m.cancelled || m.choice.Setting != nil {
		t.Fatalf("done=%v cancelled=%v setting=%v, want a finished single run", m.done, m.cancelled, m.choice.Setting)
	}
}

func TestMode_ListsSettingsWithDriveCounts(t *testing.T) {
	drives, targets := testDrives("sda", "sdb", "usb1")
	targets["usb1"] = settings.Target{Name: "usb1", Serial: "SN-usb1", Model: "M1", Interface: "USB"}
	fs := ifaceSetting{newFakeSetting(nil)}
	avail := []settings.Setting{fs, variantSetting{newFakeSetting(nil)}}
	mm := press(newModeModel(avail, drives, targets), "down", "enter")
	v := mm.View()
	if !strings.Contains(v, "fake cache") || !strings.Contains(v, "2 of 3 drives") {
		t.Fatalf("view lacks the write cache entry with its count:\n%s", v)
	}
	if !strings.Contains(v, "no controllable write cache on USB") {
		t.Fatalf("view lacks the reason for the drive it does not apply to:\n%s", v)
	}
	if strings.Contains(v, "raid layout") {
		t.Fatalf("a variants setting must not be listed:\n%s", v)
	}
	m := press(mm, "enter").(modeModel)
	if !m.done || m.choice.Setting == nil || m.choice.Setting.Name() != "fake cache" {
		t.Fatalf("choice = %+v, want the fake setting", m.choice)
	}
}

func TestMode_SettingAppliesToNoDriveCannotBePicked(t *testing.T) {
	drives, targets := testDrives("usb1")
	targets["usb1"] = settings.Target{Name: "usb1", Interface: "USB"}
	m := press(newModeModel([]settings.Setting{ifaceSetting{newFakeSetting(nil)}}, drives, targets), "down", "enter", "enter").(modeModel)
	if m.done {
		t.Fatal("a setting that applies to no drive must not be selectable")
	}
}

// setupFor builds a setup model over named identical SATA drives.
func setupFor(names ...string) (setupModel, *fakeSetting) {
	drives, targets := testDrives(names...)
	fs := newFakeSetting(nil)
	return newSetupModel(ifaceSetting{fs}, drives, targets, nil), fs
}

func TestSetup_ValuesBothTickedByDefault(t *testing.T) {
	m, _ := setupFor("sda", "sdb")
	if len(m.ticked) != 2 || !m.ticked[0] || !m.ticked[1] {
		t.Fatalf("ticked = %v, want both", m.ticked)
	}
	if v := m.View(); strings.Count(v, "[x]") != 2 {
		t.Fatalf("view should show both values ticked:\n%s", v)
	}
}

func TestSetup_NeedsTwoValues(t *testing.T) {
	m, _ := setupFor("sda", "sdb")
	got := press(m, " ", "enter").(setupModel)
	if got.step != stepValues {
		t.Fatalf("step = %v, want to stay on the values step", got.step)
	}
	if !strings.Contains(got.View(), "at least two") {
		t.Fatalf("view lacks the message:\n%s", got.View())
	}
	// Ticking again lets it through.
	got = press(got, " ", "enter").(setupModel)
	if got.step != stepStrategy {
		t.Fatalf("step = %v, want the strategy step", got.step)
	}
}

func usbSetup(names ...string) setupModel {
	drives, targets := testDrives(names...)
	for _, n := range names {
		if strings.HasPrefix(n, "usb") {
			tg := targets[n]
			tg.Interface = "USB"
			targets[n] = tg
		}
	}
	return newSetupModel(ifaceSetting{newFakeSetting(nil)}, drives, targets, nil)
}

func TestSetup_ExcludedDrivesListedWithReason(t *testing.T) {
	m := usbSetup("sda", "sdb", "usb1")
	cm := press(m, "enter", "enter") // values, strategy
	if v := cm.View(); !strings.Contains(v, "usb1") || !strings.Contains(v, "no controllable write cache on USB") {
		t.Fatalf("confirmation lacks the excluded drive and its reason:\n%s", v)
	}
	fin := press(cm, "enter").(setupModel)
	if !fin.done || fin.result == nil {
		t.Fatal("confirm did not finish")
	}
	if len(fin.result.Drives) != 2 {
		t.Fatalf("Drives = %d, want 2", len(fin.result.Drives))
	}
	if r := fin.result.Excluded["usb1"]; !strings.Contains(r, "USB") {
		t.Fatalf("Excluded = %v", fin.result.Excluded)
	}

	none := usbSetup("usb1", "usb2")
	if !none.stopped {
		t.Fatal("no applicable drive must stop the flow")
	}
	if v := none.View(); !strings.Contains(v, "no controllable write cache on USB") {
		t.Fatalf("view lacks the explanation:\n%s", v)
	}
	if _, err := none.outcome(); err == nil {
		t.Fatal("a stopped flow must return an error")
	}
}

func TestSetup_StrategyOptions(t *testing.T) {
	m, _ := setupFor("sda", "sdb")
	m = press(m, "enter").(setupModel)
	want := []rungroup.Strategy{rungroup.Sequential, rungroup.Parallel, rungroup.ParallelSwap}
	if len(m.options) != 3 || m.options[0] != want[0] || m.options[1] != want[1] || m.options[2] != want[2] {
		t.Fatalf("options = %v, want %v", m.options, want)
	}
	if m.options[m.cursor] != rungroup.Sequential {
		t.Fatal("sequential must be the default")
	}

	// Three drives cannot be split in two.
	m3, _ := setupFor("sda", "sdb", "sdc")
	m3 = press(m3, "enter").(setupModel)
	if len(m3.options) != 1 || m3.options[0] != rungroup.Sequential {
		t.Fatalf("options = %v, want sequential only", m3.options)
	}
	if v := m3.View(); !strings.Contains(v, "parallel is not offered") {
		t.Fatalf("view lacks the reason:\n%s", v)
	}
}

func writeFio(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const fioTwoMinutes = "[global]\ntime_based\nruntime=60s\n\n[a]\nstonewall\nrw=read\n\n[b]\nstonewall\nrw=write\n"

func TestSetup_ConfirmShowsRunCountAndEstimate(t *testing.T) {
	dir := t.TempDir()
	drives, targets := testDrives("sda", "sdb")
	for i := range drives {
		drives[i].FioFilePath = writeFio(t, dir, drives[i].Device.Name+".fio", fioTwoMinutes)
	}
	m := newSetupModel(ifaceSetting{newFakeSetting(nil)}, drives, targets, nil)

	seq := press(m, "enter", "enter")
	if v := seq.View(); !strings.Contains(v, "2 runs") || !strings.Contains(v, "4m0s") {
		t.Fatalf("sequential confirm lacks 2 runs / 4m0s:\n%s", v)
	}
	par := press(m, "enter", "down", "enter")
	if v := par.View(); !strings.Contains(v, "1 run ") || !strings.Contains(v, "2m0s") {
		t.Fatalf("parallel confirm lacks 1 run / 2m0s:\n%s", v)
	}
	swap := press(m, "enter", "down", "down", "enter")
	if v := swap.View(); !strings.Contains(v, "2 runs") {
		t.Fatalf("swap confirm lacks 2 runs:\n%s", v)
	}

	// A phase without a time limit is noted, not guessed.
	for i := range drives {
		drives[i].FioFilePath = writeFio(t, dir, drives[i].Device.Name+"-open.fio", fioTwoMinutes+"\n[c]\nstonewall\nrw=trim\nruntime=0\n")
	}
	m = newSetupModel(ifaceSetting{newFakeSetting(nil)}, drives, targets, nil)
	if v := press(m, "enter", "enter").View(); !strings.Contains(v, "unknown length") {
		t.Fatalf("confirm lacks the note about unknown phases:\n%s", v)
	}
}

func TestSetup_FirmwareMismatchWarns(t *testing.T) {
	drives, targets := testDrives("sda", "sdb")
	infos := map[string]diskinfo.Info{
		"sda": {Controller: &diskinfo.Controller{Firmware: "FW1"}},
		"sdb": {Controller: &diskinfo.Controller{Firmware: "FW2"}},
	}
	m := newSetupModel(ifaceSetting{newFakeSetting(nil)}, drives, targets, infos)
	strat := press(m, "enter").(setupModel)
	if v := strat.View(); !strings.Contains(v, "different firmware") {
		t.Fatalf("strategy view lacks the firmware warning:\n%s", v)
	}
	if len(strat.options) != 3 {
		t.Fatalf("parallel must stay offered, options = %v", strat.options)
	}
	if v := press(strat, "enter").View(); !strings.Contains(v, "different firmware") {
		t.Fatalf("confirm view lacks the firmware warning:\n%s", v)
	}
}

func TestSetup_NothingAppliedBeforeConfirm(t *testing.T) {
	m, fs := setupFor("sda", "sdb")
	var cur tea.Model = m
	cur = press(cur, " ", " ", "enter", "down", "enter") // values, strategy
	if len(fs.calls) != 0 {
		t.Fatalf("setting called before the confirm: %v", fs.calls)
	}
	if sm := cur.(setupModel); sm.done {
		t.Fatal("finished before the final confirm key")
	}
	cur = press(cur, "enter")
	if !cur.(setupModel).done {
		t.Fatal("not finished after the confirm key")
	}
	if len(fs.calls) != 0 {
		t.Fatalf("the model must never call the setting, got %v", fs.calls)
	}
}

func TestSetup_QuitCancels(t *testing.T) {
	m, _ := setupFor("sda", "sdb")
	got := press(m, "q").(setupModel)
	if !got.cancelled || got.result != nil {
		t.Fatalf("cancelled=%v result=%v", got.cancelled, got.result)
	}
}

func recoveryFor(t *testing.T, names ...string) (string, *fakeSetting, []DriveInfo, map[string]settings.Target) {
	t.Helper()
	quietLogger(t)
	drives, targets := testDrives(names...)
	rec := &rungroup.Recovery{Setting: "fake cache"}
	cur := map[string]string{}
	for _, d := range drives {
		cur[d.Device.Name] = "disabled"
		rec.Drives = append(rec.Drives, rungroup.RecoveryDrive{Serial: d.Device.Serial, Model: d.Device.Model, Name: d.Device.Name, Original: settings.Reading{Value: "enabled"}})
	}
	path := filepath.Join(t.TempDir(), "rec.json")
	if err := rec.Save(path); err != nil {
		t.Fatal(err)
	}
	return path, newFakeSetting(cur), drives, targets
}

func TestRecoveryOffer_YesRestoresAndRemovesFile(t *testing.T) {
	path, fs, drives, targets := recoveryFor(t, "sda", "sdb")
	var out strings.Builder
	if err := offerRecovery(context.Background(), strings.NewReader("y\n"), &out, path, []settings.Setting{fs}, drives, targets); err != nil {
		t.Fatal(err)
	}
	if fs.get("sda") != "enabled" || fs.get("sdb") != "enabled" {
		t.Fatalf("not restored: %q %q", fs.get("sda"), fs.get("sdb"))
	}
	if rec, _ := rungroup.LoadRecovery(path); rec != nil {
		t.Fatal("recovery file must be removed after a verified restore")
	}
}

func TestRecoveryOffer_NoLeavesEverything(t *testing.T) {
	path, fs, drives, targets := recoveryFor(t, "sda")
	var out strings.Builder
	if err := offerRecovery(context.Background(), strings.NewReader("n\n"), &out, path, []settings.Setting{fs}, drives, targets); err != nil {
		t.Fatal(err)
	}
	if len(fs.callsOf("restore")) != 0 || fs.get("sda") != "disabled" {
		t.Fatal("answering no must not restore")
	}
	if rec, _ := rungroup.LoadRecovery(path); rec == nil {
		t.Fatal("recovery file must stay")
	}
}

func TestRecoveryOffer_FailureKeepsFileAndStops(t *testing.T) {
	path, fs, drives, targets := recoveryFor(t, "sda", "sdb")
	fs.restoreErr["sdb"] = true
	old := restoreReportOut
	restoreReportOut = io.Discard
	t.Cleanup(func() { restoreReportOut = old })
	var out strings.Builder
	err := offerRecovery(context.Background(), strings.NewReader("yes\n"), &out, path, []settings.Setting{fs}, drives, targets)
	if err == nil || !strings.Contains(err.Error(), "sdb") {
		t.Fatalf("err = %v, want a failure naming sdb", err)
	}
	if rec, _ := rungroup.LoadRecovery(path); rec == nil {
		t.Fatal("recovery file must be kept after a failed restore")
	}
	if !strings.Contains(out.String(), "sdb") {
		t.Fatalf("output must name the failed drive:\n%s", out.String())
	}
}

func TestRecoveryOffer_NoFileDoesNothing(t *testing.T) {
	if err := offerRecovery(context.Background(), strings.NewReader(""), io.Discard, filepath.Join(t.TempDir(), "none.json"), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestResume_FindsUnfinishedGroupAndRebuildsChoice(t *testing.T) {
	drives, _ := testDrives("sda", "sdb")
	dir := t.TempDir()
	done := &rungroup.GroupFile{ID: "1", Setting: "fake cache", Runs: []rungroup.GroupRun{{Status: rungroup.RunDone}}}
	g := &rungroup.GroupFile{
		ID: "2", Created: time.Now(), Setting: "fake cache", Strategy: rungroup.Sequential, Values: []string{"enabled", "disabled"},
		Runs: []rungroup.GroupRun{
			{Status: rungroup.RunDone, Drives: []rungroup.GroupDrive{{Device: "old1", Serial: "SN-sda", Model: "M1"}, {Device: "old2", Serial: "SN-sdb", Model: "M1"}}},
			{Status: rungroup.RunInterrupted},
		},
	}
	for _, f := range []*rungroup.GroupFile{done, g} {
		if err := f.Save(dir); err != nil {
			t.Fatal(err)
		}
	}
	got := findResumableGroup(dir)
	if got == nil || got.ID != "2" {
		t.Fatalf("found %v, want group 2", got)
	}
	c, err := resumeChoice(got, []settings.Setting{newFakeSetting(nil)}, drives)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Drives) != 2 || c.Drives[0].Device.Name != "sda" || len(c.Values) != 2 || c.Strategy != rungroup.Sequential {
		t.Fatalf("choice = %+v", c)
	}
	// A drive that is gone stops the resume.
	if _, err := resumeChoice(got, []settings.Setting{newFakeSetting(nil)}, drives[:1]); err == nil {
		t.Fatal("a missing drive must be an error")
	}
	// All runs finished: nothing to resume.
	g.Runs[1].Status = rungroup.RunInvalid
	if err := g.Save(dir); err != nil {
		t.Fatal(err)
	}
	if findResumableGroup(dir) != nil {
		t.Fatal("a finished group must not be offered")
	}
}

func TestNewGroupConfig_Defaults(t *testing.T) {
	opts := runOptions{Mode: "sequential", OutDir: t.TempDir(), Resume: &ExecutionState{}}
	c := &groupChoice{Setting: newFakeSetting(nil), Values: fakeValues, Strategy: rungroup.Sequential}
	cfg := newGroupConfig(c, nil, opts, "h", nil)
	if cfg.Execute == nil || cfg.OnSave != nil || cfg.RecoveryPath != groupRecoveryPath || cfg.Settle != groupSettle {
		t.Fatalf("cfg = %+v", cfg)
	}
}
