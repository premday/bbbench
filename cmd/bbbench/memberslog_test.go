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
	if err := diskstat.WriteLog(f, meta, ws); err != nil {
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
	m, ws, err := diskstat.ReadLog(f)
	if err != nil {
		t.Fatal(err)
	}
	if m.Array != "md0" || len(m.Members) != 2 || m.Members[1].Role != "spare" || len(ws) == 0 {
		t.Errorf("meta %+v windows %d", m, len(ws))
	}
}
