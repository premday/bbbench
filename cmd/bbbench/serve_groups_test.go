package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

func groupTestResult(id, device, value string, valid bool, iops float64) *BenchmarkResult {
	return &BenchmarkResult{
		ID:      id,
		GroupID: "g1",
		Drives:  []DriveInfo{{Device: blockdev.Device{Name: device, Model: "M", Serial: "S" + device}}},
		Phases: []PhaseResult{{PhaseNumber: 1, PhaseName: "randwrite",
			Jobs: []JobResult{{Device: device, WriteStats: &IOStats{IOPS: iops, Bandwidth: 1024, AvgLatNS: 1000}}}}},
		Labels: map[string]rungroup.Label{device: {
			Setting: "write_cache", Value: value, Valid: valid,
			After: &settings.Reading{Value: value, Kernel: "write " + value},
		}},
	}
}

func serveTestGroup() *rungroup.GroupFile {
	return &rungroup.GroupFile{
		ID: "g1", Created: time.Unix(1000, 0), Host: "h", Plan: []string{"randwrite"},
		Setting: "write_cache", Strategy: rungroup.Sequential, Values: []string{"enabled", "disabled"},
		Runs: []rungroup.GroupRun{
			{RunID: "r1", Status: rungroup.RunDone, Drives: []rungroup.GroupDrive{{Device: "sda", Value: "enabled",
				Label: rungroup.Label{Value: "enabled", Valid: true, After: &settings.Reading{Value: "enabled", Kernel: "write back"}}}}},
			{RunID: "r2", Round: 0, Status: rungroup.RunInvalid, Drives: []rungroup.GroupDrive{{Device: "sda", Value: "disabled",
				Label: rungroup.Label{Value: "disabled", Valid: false}}}},
			{RunID: "r3", Status: rungroup.RunNotRun},
		},
		Restore: rungroup.GroupRestore{Status: "done"},
	}
}

func seedGroup(t *testing.T) {
	t.Helper()
	globalGroupStore.Clear()
	globalResultsStore.Clear()
	t.Cleanup(func() { globalGroupStore.Clear(); globalResultsStore.Clear() })
	globalGroupStore.AddGroup(serveTestGroup())
	globalResultsStore.AddResult(groupTestResult("r1", "sda", "enabled", true, 100))
	r2 := groupTestResult("r2", "sda", "disabled", false, 50)
	r2.Phases[0].Warnings = map[string]string{"sda": "write through: no Flush"}
	globalResultsStore.AddResult(r2)
}

func get(h http.HandlerFunc, url string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", url, nil))
	return w
}

func TestGroupStore(t *testing.T) {
	s := &GroupStore{groups: map[string]*rungroup.GroupFile{}}
	s.AddGroup(&rungroup.GroupFile{ID: "a", Created: time.Unix(1, 0)})
	s.AddGroup(&rungroup.GroupFile{ID: "b", Created: time.Unix(3, 0)})
	s.AddGroup(&rungroup.GroupFile{ID: "c", Created: time.Unix(2, 0)})
	if g, ok := s.GetGroup("b"); !ok || g.ID != "b" {
		t.Fatalf("GetGroup b = %v %v", g, ok)
	}
	if _, ok := s.GetGroup("zz"); ok {
		t.Error("unknown id found")
	}
	var ids []string
	for _, g := range s.ListGroups() {
		ids = append(ids, g.ID)
	}
	if strings.Join(ids, ",") != "b,c,a" {
		t.Errorf("order = %v, want b,c,a", ids)
	}
	s.AddGroup(&rungroup.GroupFile{ID: "a", Created: time.Unix(9, 0)})
	if len(s.ListGroups()) != 3 || s.ListGroups()[0].ID != "a" {
		t.Error("re-adding must replace")
	}
	s.Clear()
	if len(s.ListGroups()) != 0 {
		t.Error("Clear")
	}
}

func TestHandleGroupsList(t *testing.T) {
	seedGroup(t)
	w := get(handleGroupsList, "/api/groups")
	var out []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 1 {
		t.Fatalf("body %s err %v", w.Body.String(), err)
	}
	g := out[0]
	if g["id"] != "g1" || g["setting"] != "write_cache" || g["strategy"] != "sequential" || g["runs"].(float64) != 3 {
		t.Errorf("fields = %v", g)
	}
	counts := g["status_counts"].(map[string]any)
	if counts["done"].(float64) != 1 || counts["invalid"].(float64) != 1 || counts["not_run"].(float64) != 1 {
		t.Errorf("counts = %v", counts)
	}
	if len(g["values"].([]any)) != 2 || g["created"] == nil {
		t.Errorf("values/created = %v", g)
	}
}

func TestHandleGroupDetail(t *testing.T) {
	seedGroup(t)
	if c := get(handleGroupDetail, "/api/groups/detail").Code; c != 400 {
		t.Errorf("no id: %d", c)
	}
	if c := get(handleGroupDetail, "/api/groups/detail?id=nope").Code; c != 404 {
		t.Errorf("unknown: %d", c)
	}
	w := get(handleGroupDetail, "/api/groups/detail?id=g1")
	var out struct {
		ID   string `json:"id"`
		Runs []struct {
			RunID     string            `json:"run_id"`
			Kernel    map[string]string `json:"kernel"`
			FlushNote bool              `json:"flush_note"`
			Summary   *struct {
				ID     string `json:"id"`
				Phases int    `json:"phases"`
			} `json:"summary"`
		} `json:"runs"`
		Restore struct{ Status string } `json:"restore"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if out.ID != "g1" || len(out.Runs) != 3 || out.Restore.Status != "done" {
		t.Fatalf("out = %+v", out)
	}
	r1, r2, r3 := out.Runs[0], out.Runs[1], out.Runs[2]
	if r1.Kernel["sda"] != "write back" || r1.FlushNote || r1.Summary == nil || r1.Summary.ID != "r1" {
		t.Errorf("r1 = %+v", r1)
	}
	if !r2.FlushNote {
		t.Errorf("r2 with a warning must have flush_note: %+v", r2)
	}
	if r3.FlushNote || r3.Summary != nil {
		t.Errorf("r3 = %+v", r3)
	}
}

func TestHandleGroupGraphs_UnionAndInvalid(t *testing.T) {
	seedGroup(t)
	globalResultsStore.AddResult(groupTestResult("r3", "sda", "enabled", true, 70)) // not_run run: ignored
	if c := get(handleGroupGraphs, "/api/groups/graphs").Code; c != 400 {
		t.Errorf("no id: %d", c)
	}
	if c := get(handleGroupGraphs, "/api/groups/graphs?id=nope").Code; c != 404 {
		t.Errorf("unknown: %d", c)
	}
	w := get(handleGroupGraphs, "/api/groups/graphs?id=g1")
	var out []DiskPhaseData
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d entries: %+v", len(out), out)
	}
	by := map[string]DiskPhaseData{}
	for _, d := range out {
		by[d.RunID] = d
	}
	if by["r1"].SettingValue != "enabled" || by["r1"].Invalid {
		t.Errorf("r1 = %+v", by["r1"])
	}
	if by["r2"].SettingValue != "disabled" || !by["r2"].Invalid {
		t.Errorf("r2 = %+v", by["r2"])
	}
	if _, ok := by["r3"]; ok {
		t.Error("not_run run must contribute nothing")
	}
}

func TestGroupMatrix(t *testing.T) {
	mk := func(phase, value string, iops float64, invalid bool) DiskPhaseData {
		return DiskPhaseData{PhaseNum: 1, PhaseName: phase, SettingValue: value, WriteIOPS: iops, HasWrite: true, Invalid: invalid}
	}
	rows := groupMatrix([]DiskPhaseData{
		mk("w", "on", 100, false), mk("w", "on", 300, false), mk("w", "on", 9999, true),
		mk("w", "off", 5, true),
	}, []string{"on", "off"})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	on, off := rows[0], rows[1]
	if on.Value != "on" || on.N != 2 || on.IOPS.Mean != 200 || on.IOPS.Min != 100 || on.IOPS.Max != 300 || on.Incomplete {
		t.Errorf("on = %+v", on)
	}
	if off.Value != "off" || off.N != 0 || !off.Incomplete {
		t.Errorf("off = %+v", off)
	}
}

func TestHandleGroupPage(t *testing.T) {
	seedGroup(t)
	if c := get(handleGroupPage, "/group").Code; c != 400 {
		t.Errorf("no id: %d", c)
	}
	if c := get(handleGroupPage, "/group?id=nope").Code; c != 404 {
		t.Errorf("unknown: %d", c)
	}
	w := get(handleGroupPage, "/group?id=g1")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "topnav") || !strings.Contains(body, "/api/groups/detail?id=g1") {
		t.Errorf("page: %d %.200s", w.Code, body)
	}
	if strings.Contains(body, "%!") {
		t.Error("format error in page")
	}
	// An id with markup must come back escaped.
	globalGroupStore.AddGroup(&rungroup.GroupFile{ID: `<b>"x`})
	body = get(handleGroupPage, "/group?id="+`%3Cb%3E%22x`).Body.String()
	if strings.Contains(body, `<b>"x`) {
		t.Error("id not escaped")
	}
}

func TestLoadGroups_RegistersInStore(t *testing.T) {
	globalGroupStore.Clear()
	t.Cleanup(globalGroupStore.Clear)
	dir := t.TempDir()
	if err := loadGroups(filepath.Join(dir, "missing")); err != nil {
		t.Fatal(err)
	}
	g := serveTestGroup()
	if err := g.Save(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "group_bad_group.json"), []byte("{"), 0o644)
	if err := loadGroups(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := globalGroupStore.GetGroup("g1"); !ok {
		t.Error("group not loaded")
	}
}

func TestNewGroupConfig_OnSaveRegistersGroup(t *testing.T) {
	globalGroupStore.Clear()
	t.Cleanup(globalGroupStore.Clear)
	opts := runOptions{Mode: "sequential", OutDir: t.TempDir()}
	c := &groupChoice{Setting: perDrive(newFakeSetting(nil)), Values: fakeValues, Strategy: rungroup.Sequential}
	cfg := newGroupConfig(c, nil, opts, "h", nil)
	if cfg.OnSave == nil {
		t.Fatal("OnSave not wired")
	}
	cfg.OnSave(&rungroup.GroupFile{ID: "live"})
	if _, ok := globalGroupStore.GetGroup("live"); !ok {
		t.Error("hook did not register the group")
	}
}

// An interrupted run is shown but never counted: its entries are invalid and
// its value gets n=0, even when its stored labels still say valid (a crash
// before the labels were rewritten).
func TestGroupEntries_InterruptedRunIsInvalid(t *testing.T) {
	globalResultsStore.Clear()
	t.Cleanup(globalResultsStore.Clear)
	globalResultsStore.AddResult(groupTestResult("r1", "sda", "enabled", true, 100))
	globalResultsStore.AddResult(groupTestResult("r2", "sda", "disabled", true, 50))
	g := &rungroup.GroupFile{ID: "g1", Values: []string{"enabled", "disabled"}, Runs: []rungroup.GroupRun{
		{RunID: "r1", Status: rungroup.RunDone},
		{RunID: "r2", Status: rungroup.RunInterrupted},
	}}
	entries := groupEntries(g)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	for _, e := range entries {
		if want := e.RunID == "r2"; e.Invalid != want {
			t.Errorf("%s Invalid = %v, want %v", e.RunID, e.Invalid, want)
		}
	}
	for _, row := range groupMatrix(entries, g.Values) {
		if row.Value == "disabled" && (row.N != 0 || !row.Incomplete) {
			t.Errorf("interrupted value row = %+v, want n=0 incomplete", row)
		}
	}
}

func TestGroupPage_InterruptedRunNotValid(t *testing.T) {
	if !strings.Contains(groupPageHTML, "function runValid(run, d) { return run.status !== 'interrupted' && d.label && d.label.valid; }") {
		t.Error("the group page must treat an interrupted run as invalid")
	}
}

// A value whose runs were all not run still has a row per phase, marked
// incomplete, in the order of the group's values.
func TestGroupMatrix_SeedsValuesWithoutResults(t *testing.T) {
	mk := func(num int, phase, value string, iops float64) DiskPhaseData {
		return DiskPhaseData{PhaseNum: num, PhaseName: phase, SettingValue: value, WriteIOPS: iops, HasWrite: true}
	}
	rows := groupMatrix([]DiskPhaseData{mk(1, "w", "off", 10), mk(2, "r", "off", 20)}, []string{"on", "off"})
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%d %s %s n=%d inc=%v", r.PhaseNum, r.PhaseName, r.Value, r.N, r.Incomplete))
	}
	want := []string{"1 w on n=0 inc=true", "1 w off n=1 inc=false", "2 r on n=0 inc=true", "2 r off n=1 inc=false"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rows =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestHandleGroupDetail_NotRunValueHasRow(t *testing.T) {
	globalGroupStore.Clear()
	globalResultsStore.Clear()
	t.Cleanup(func() { globalGroupStore.Clear(); globalResultsStore.Clear() })
	globalResultsStore.AddResult(groupTestResult("r1", "sda", "enabled", true, 100))
	globalGroupStore.AddGroup(&rungroup.GroupFile{ID: "g2", Values: []string{"enabled", "disabled"}, Runs: []rungroup.GroupRun{
		{RunID: "r1", Status: rungroup.RunDone},
		{Status: rungroup.RunNotRun},
	}})
	var out struct {
		Matrix []MatrixRow `json:"matrix"`
	}
	if err := json.Unmarshal(get(handleGroupDetail, "/api/groups/detail?id=g2").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Matrix) != 2 || out.Matrix[1].Value != "disabled" || !out.Matrix[1].Incomplete || out.Matrix[1].N != 0 {
		t.Errorf("matrix = %+v, want a disabled row marked incomplete", out.Matrix)
	}
}

func TestNewGroupConfig_HasPrecheck(t *testing.T) {
	c := &groupChoice{Setting: perDrive(newFakeSetting(nil)), Values: fakeValues, Strategy: rungroup.Sequential}
	if cfg := newGroupConfig(c, nil, runOptions{Mode: "sequential", OutDir: t.TempDir()}, "h", nil); cfg.Precheck == nil {
		t.Error("no precheck: fio and the fio files would only be checked after the first apply")
	}
}
