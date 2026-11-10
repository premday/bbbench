package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/diskstat"
	"bbbench/internal/rungroup"
)

func writeTestMembersLog(t *testing.T, prefix string) {
	t.Helper()
	f, err := os.Create(diskstat.LogName(prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	meta := diskstat.Meta{AvgMsec: 1000, OffsetMs: 2000, Array: "md0",
		Members: []diskstat.MemberMeta{{Name: "sda", Role: "data"}, {Name: "sdb", Role: "spare"}}}
	var ws []diskstat.Window
	for _, end := range []int64{1000, 2000, 3000, 4000} {
		ws = append(ws,
			diskstat.Window{EndMs: end, SpanMs: 1000, Device: "sda", D: diskstat.Counters{ReadIOs: 100, ReadSectors: 2048, ReadTicksMs: 50}},
			diskstat.Window{EndMs: end, SpanMs: 1000, Device: "sdb"})
	}
	if err := diskstat.WriteLog(f, meta, ws, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMembersLogDropsRamp(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "md0_phase1_1_log")
	writeTestMembersLog(t, prefix)
	got, ok := loadMembersLog(prefix)
	if !ok {
		t.Fatal("not ok")
	}
	a := got["sda"]
	if len(a.IOPS) != 2 || a.IOPS[0].T != 2 || a.IOPS[1].T != 3 {
		t.Fatalf("points %+v", a.IOPS)
	}
	if a.IOPS[0].R != 100 || a.BW[0].R != 1 || a.Lat[0].R != 500 || a.Lat[0].W != 0 {
		t.Errorf("values iops %+v bw %+v lat %+v", a.IOPS[0], a.BW[0], a.Lat[0])
	}
	if a.Role != "" || got["sdb"].Role != "spare" {
		t.Errorf("roles %q %q", a.Role, got["sdb"].Role)
	}
	if l := got["sdb"].Lat[0]; l.R != 0 || l.W != 0 {
		t.Errorf("idle member latency %+v", l)
	}
}

func TestLoadMembersLogMissing(t *testing.T) {
	if _, ok := loadMembersLog(filepath.Join(t.TempDir(), "x_log")); ok {
		t.Error("ok for a missing log")
	}
}

func TestMemberSeriesInGraphData(t *testing.T) {
	ms := map[string]MemberSeries{"sda": {IOPS: []TimePoint{{T: 1, R: 5}}}}
	res := &BenchmarkResult{
		ID: "r",
		Drives: []DriveInfo{
			{Device: blockdev.Device{Name: "md0"}, Members: []rungroup.Member{{Name: "sda"}}},
			{Device: blockdev.Device{Name: "nvme0n1"}},
		},
		Phases: []PhaseResult{{
			PhaseNumber: 1, PhaseName: "p",
			Jobs: []JobResult{
				{Device: "md0", WriteStats: &IOStats{IOPS: 1}},
				{Device: "nvme0n1", WriteStats: &IOStats{IOPS: 1}},
			},
			MemberSeries: map[string]map[string]MemberSeries{"md0": ms},
		}},
	}
	for _, d := range generateDiskPhaseData(res) {
		b, _ := json.Marshal(d)
		has := strings.Contains(string(b), `"ts_members"`)
		if d.Disk == "md0" && !has {
			t.Errorf("array lacks ts_members: %s", b)
		}
		if d.Disk == "nvme0n1" && has {
			t.Errorf("drive has ts_members: %s", b)
		}
	}
}

func TestStartMemberSamplerNoopForDrive(t *testing.T) {
	stop := startMemberSampler(DriveInfo{Device: blockdev.Device{Name: "sda"}}, filepath.Join(t.TempDir(), "p_log"), 1000, 0)
	if stop == nil {
		t.Fatal("nil stop")
	}
	stop()
	stop()
}

func TestStartMemberSamplerWritesLog(t *testing.T) {
	sys := t.TempDir()
	for _, n := range []string{"sda", "sdb"} {
		if err := os.MkdirAll(filepath.Join(sys, n), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sys, n, "stat"), []byte("1 0 8 1 1 0 8 1 0 0 0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	drive := DriveInfo{Device: blockdev.Device{Name: "md0"},
		Members: []rungroup.Member{{Name: "sda"}, {Name: "sdb", Role: "spare"}}}
	prefix := filepath.Join(t.TempDir(), "md0_phase1_1_log")
	stop := startMemberSamplerAt(drive, prefix, 20, 0, sys)
	time.Sleep(100 * time.Millisecond)
	stop()
	stop()
	f, err := os.Open(diskstat.LogName(prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, ws, _, err := diskstat.ReadLog(f)
	if err != nil {
		t.Fatal(err)
	}
	if m.Array != "md0" || len(m.Members) != 2 || m.Members[1].Role != "spare" || len(ws) == 0 {
		t.Errorf("meta %+v windows %d", m, len(ws))
	}
}

func mdw(end int64, s diskstat.MDState) diskstat.MDWindow {
	return diskstat.MDWindow{EndMs: end, SpanMs: 1000, S: s}
}

func TestMDEventsFromWindows(t *testing.T) {
	idle := diskstat.MDState{Action: "idle", ArrayState: "clean", RaidDisks: 4, Members: map[string]string{"sda": "in_sync", "sdb": "in_sync"}}
	chg := idle
	chg.Action, chg.ArrayState, chg.Degraded, chg.RaidDisks = "check", "active", 1, 5
	chg.Members = map[string]string{"sda": "in_sync", "sdb": "faulty"}
	ev := mdEventsFrom(nil, []diskstat.MDWindow{mdw(1000, idle), mdw(2000, idle), mdw(3000, chg), mdw(4000, chg)})
	want := []string{"sync_action idle -> check", "array_state clean -> active", "degraded 0 -> 1", "raid_disks 4 -> 5", "sdb: in_sync -> faulty"}
	if len(ev) != len(want) {
		t.Fatalf("events %+v", ev)
	}
	for i, w := range want {
		if ev[i].Text != w || ev[i].T != 2 {
			t.Errorf("event %d = %+v, want %q at 2", i, ev[i], w)
		}
	}
	start := chg
	ev = mdEventsFrom(nil, []diskstat.MDWindow{mdw(1000, start)})
	if len(ev) != 2 || ev[0].Text != "sync_action check (at phase start)" || ev[1].Text != "degraded 1 (at phase start)" {
		t.Errorf("first window events %+v", ev)
	}
	if ev := mdEventsFrom(nil, []diskstat.MDWindow{mdw(1000, idle)}); len(ev) != 0 {
		t.Errorf("idle start: %+v", ev)
	}
	// a change inside the ramp-up is the baseline of the first visible window
	info, ok := loadMDLog([]diskstat.MDWindow{mdw(1000, idle), mdw(2000, idle), mdw(3000, chg)}, 2000)
	if !ok || len(info.Points) != 1 || len(info.Events) != 5 || info.Events[0].T != 2 {
		t.Errorf("ramp baseline: %+v", info)
	}
}

func TestLoadMembersLogMD(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "md0_p_log")
	f, _ := os.Create(diskstat.LogName(prefix))
	meta := diskstat.Meta{AvgMsec: 1000, OffsetMs: 1000, Array: "md0", Members: []diskstat.MemberMeta{{Name: "sda", Role: "data"}}}
	md := []diskstat.MDWindow{
		mdw(1000, diskstat.MDState{Action: "idle"}),
		mdw(2000, diskstat.MDState{Action: "check", Completed: 25, Total: 100, SpeedKiBps: 500, Degraded: 1, ArrayState: "active"}),
		mdw(3000, diskstat.MDState{Action: "check", Completed: 5, Total: 0}),
	}
	if err := diskstat.WriteLog(f, meta, nil, md); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, info, ok := loadMembersAndMD(prefix)
	if !ok || len(info.Points) != 2 {
		t.Fatalf("%+v", info)
	}
	p := info.Points[0]
	if p.T != 1 || p.Action != "check" || p.Pct != 25 || p.SpeedKiBps != 500 || p.Degraded != 1 || p.State != "active" || info.Points[1].Pct != 0 {
		t.Errorf("points %+v", info.Points)
	}
	var ph PhaseResult
	applyMembersLog(&ph, "md0", prefix)
	if len(ph.MDSeries["md0"].Points) != 2 {
		t.Errorf("not applied: %+v", ph.MDSeries)
	}
}

func TestMDJSONOmittedWithoutMD(t *testing.T) {
	b, _ := json.Marshal(DiskPhaseData{})
	if strings.Contains(string(b), "ts_md") || strings.Contains(string(b), "md_events") {
		t.Errorf("%s", b)
	}
	b, _ = json.Marshal(PhaseResult{})
	if strings.Contains(string(b), "md_series") {
		t.Errorf("%s", b)
	}
}

func TestStartMemberSamplerRecordsMD(t *testing.T) {
	sys := t.TempDir()
	for _, n := range []string{"sda", "md0"} {
		os.MkdirAll(filepath.Join(sys, n), 0o755)
	}
	os.WriteFile(filepath.Join(sys, "sda", "stat"), []byte("1 0 8 1 1 0 8 1 0 0 0\n"), 0o644)
	md := filepath.Join(sys, "md0", "md")
	os.MkdirAll(filepath.Join(md, "dev-sda"), 0o755)
	os.WriteFile(filepath.Join(md, "sync_action"), []byte("check\n"), 0o644)
	os.WriteFile(filepath.Join(md, "dev-sda", "state"), []byte("in_sync\n"), 0o644)
	drive := DriveInfo{Device: blockdev.Device{Name: "md0"}, Members: []rungroup.Member{{Name: "sda"}}}
	prefix := filepath.Join(t.TempDir(), "md0_p_log")
	stop := startMemberSamplerAt(drive, prefix, 20, 0, sys)
	time.Sleep(100 * time.Millisecond)
	stop()
	_, info, ok := loadMembersAndMD(prefix)
	if !ok || len(info.Points) == 0 || info.Points[0].Action != "check" || len(info.Events) == 0 {
		t.Errorf("%+v", info)
	}
}

func TestMDStartEventsWithRamp(t *testing.T) {
	idle := diskstat.MDState{Action: "idle", ArrayState: "clean"}
	chk := diskstat.MDState{Action: "check", ArrayState: "clean", Degraded: 1}
	texts := func(i MDInfo) []string {
		var o []string
		for _, e := range i.Events {
			o = append(o, e.Text)
		}
		return o
	}
	eq := func(got, want []string) bool { return strings.Join(got, "|") == strings.Join(want, "|") }
	// starts non-idle, ramp drops windows, unchanged: reported once at the first visible window
	info, _ := loadMDLog([]diskstat.MDWindow{mdw(1000, chk), mdw(2000, chk), mdw(3000, chk)}, 2000)
	if w := []string{"sync_action check (at phase start)", "degraded 1 (at phase start)"}; !eq(texts(info), w) || info.Events[0].T != 2 {
		t.Errorf("non-idle+ramp: %+v", info.Events)
	}
	// starts idle, change in the ramp: a change event only
	info, _ = loadMDLog([]diskstat.MDWindow{mdw(1000, idle), mdw(2000, chk), mdw(3000, chk)}, 2000)
	if w := []string{"sync_action idle -> check", "degraded 0 -> 1"}; !eq(texts(info), w) || info.Events[0].T != 2 {
		t.Errorf("idle+change in ramp: %+v", info.Events)
	}
	// starts non-idle, no ramp: unchanged behaviour
	info, _ = loadMDLog([]diskstat.MDWindow{mdw(1000, chk)}, 0)
	if w := []string{"sync_action check (at phase start)", "degraded 1 (at phase start)"}; !eq(texts(info), w) || info.Events[0].T != 0 {
		t.Errorf("no ramp: %+v", info.Events)
	}
}

func TestMDEventsIgnoreUnreadable(t *testing.T) {
	mk := func(act string, rd int, mem string) diskstat.MDState {
		return diskstat.MDState{Action: act, ArrayState: "clean", RaidDisks: rd, Members: map[string]string{"sdb": mem}}
	}
	texts := func(ws ...diskstat.MDState) []string {
		var wins []diskstat.MDWindow
		for i, w := range ws {
			wins = append(wins, mdw(int64(i+1)*1000, w))
		}
		var o []string
		for _, e := range mdEventsFrom(&ws[0], wins[1:]) {
			o = append(o, e.Text)
		}
		return o
	}
	check := func(name string, got []string, want ...string) {
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	check("empty member", texts(mk("idle", 4, "in_sync"), mk("idle", 4, ""), mk("idle", 4, "in_sync")))
	check("empty action", texts(mk("idle", 4, "in_sync"), mk("", 4, "in_sync"), mk("idle", 4, "in_sync")))
	check("raid_disks 0", texts(mk("idle", 4, "in_sync"), mk("idle", 0, "in_sync"), mk("idle", 4, "in_sync")))
	check("removed", texts(mk("idle", 4, "in_sync"), mk("idle", 4, "removed"), mk("idle", 4, "in_sync")),
		"sdb: in_sync -> removed", "sdb: removed -> in_sync")
	check("unreadable then change", texts(mk("idle", 4, "in_sync"), mk("", 4, "in_sync"), mk("check", 4, "in_sync")),
		"sync_action idle -> check")
}

// The sampler stamps EndMs from the wall clock, so the window closing the
// ramp-up ends a few ms after the boundary; it must still be dropped.
func TestRampBoundaryJitter(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "md0_j_log")
	f, _ := os.Create(diskstat.LogName(prefix))
	meta := diskstat.Meta{AvgMsec: 1000, OffsetMs: 10000, Array: "md0", Members: []diskstat.MemberMeta{{Name: "sda", Role: "data"}}}
	var ws []diskstat.Window
	var md []diskstat.MDWindow
	for _, end := range []int64{10001, 11001, 12002} {
		ws = append(ws, diskstat.Window{EndMs: end, SpanMs: 1000, Device: "sda", D: diskstat.Counters{ReadIOs: 1}})
		md = append(md, diskstat.MDWindow{EndMs: end, SpanMs: 1000, S: diskstat.MDState{Action: "idle"}})
	}
	if err := diskstat.WriteLog(f, meta, ws, md); err != nil {
		t.Fatal(err)
	}
	f.Close()
	series, info, ok := loadMembersAndMD(prefix)
	if !ok {
		t.Fatal("not ok")
	}
	if got := series["sda"].IOPS; len(got) != 2 || got[0].T < 10.0 || got[0].T > 10.01 {
		t.Errorf("members kept %+v", got)
	}
	if len(info.Points) != 2 || info.Points[0].T < 10.0 || info.Points[0].T > 10.01 {
		t.Errorf("md points %+v", info.Points)
	}
}
