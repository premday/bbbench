package diskstat

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeMD(t *testing.T, root, array string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, array, "md", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadMDParses(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "md0", map[string]string{
		"sync_action": "check\n", "sync_completed": "1000 / 4000\n", "sync_speed": "5000\n",
		"degraded": "1\n", "array_state": "active\n", "raid_disks": "4\n",
		"dev-sda/state": "in_sync\n", "dev-sdb/state": "faulty,write_mostly\n",
	})
	s, ok := ReadMD(root, "md0", []string{"sda", "sdb"})
	if !ok {
		t.Fatal("want ok")
	}
	want := MDState{Action: "check", Completed: 1000, Total: 4000, SpeedKiBps: 5000, Degraded: 1,
		ArrayState: "active", RaidDisks: 4, Members: map[string]string{"sda": "in_sync", "sdb": "faulty,write_mostly"}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got %+v", s)
	}
	writeMD(t, root, "md0", map[string]string{"sync_completed": "none\n"})
	s, _ = ReadMD(root, "md0", nil)
	if s.Completed != 0 || s.Total != 0 {
		t.Fatalf("none: %+v", s)
	}
}

func TestReadMDNoDirIsNotOK(t *testing.T) {
	if _, ok := ReadMD(t.TempDir(), "md0", nil); ok {
		t.Fatal("want not ok")
	}
}

func TestReadMDUnreadableFieldIsEmpty(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "md0", map[string]string{"array_state": "clean\n", "sync_completed": "garbage\n", "degraded": "x\n"})
	if err := os.MkdirAll(filepath.Join(root, "md0", "md", "dev-sda"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, ok := ReadMD(root, "md0", []string{"sda", "sdb"})
	if !ok || s.ArrayState != "clean" || s.Action != "" || s.Total != 0 || s.Degraded != 0 || s.Members["sda"] != "" {
		t.Fatalf("%v %+v", ok, s)
	}
	// no dev-sdb directory: the member was removed from the array
	if s.Members["sdb"] != "removed" {
		t.Fatalf("removed member: %+v", s.Members)
	}
}

func TestSamplerRecordsMDWindows(t *testing.T) {
	root := t.TempDir()
	writeStat(t, root, "sda", 0, 0, 0, 0, 0, 0)
	writeMD(t, root, "md0", map[string]string{"sync_action": "idle\n", "dev-sda/state": "in_sync\n"})
	s := NewSampler([]string{"sda"}, root).WithArray("md0", []string{"sda"})
	t0 := time.Unix(1000, 0)
	s.Sample(t0)
	writeMD(t, root, "md0", map[string]string{"sync_action": "check\n"})
	s.Sample(t0.Add(time.Second))
	s.Sample(t0.Add(3 * time.Second))
	w := s.MDWindows()
	if len(w) != 2 || w[0].EndMs != 1000 || w[0].SpanMs != 1000 || w[0].S.Action != "check" ||
		w[1].EndMs != 3000 || w[1].SpanMs != 2000 || w[1].S.Members["sda"] != "in_sync" {
		t.Fatalf("%+v", w)
	}
	// no array / no md dir: nothing recorded
	s2 := NewSampler([]string{"sda"}, root)
	s2.Sample(t0)
	s2.Sample(t0.Add(time.Second))
	s3 := NewSampler([]string{"sda"}, root).WithArray("md9", nil)
	s3.Sample(t0)
	s3.Sample(t0.Add(time.Second))
	if len(s2.MDWindows()) != 0 || len(s3.MDWindows()) != 0 {
		t.Fatal("unexpected md windows")
	}
}

func TestLogRoundTripWithMD(t *testing.T) {
	m := Meta{AvgMsec: 1000, Array: "md0", Members: []MemberMeta{{"sda", "data"}}}
	ws := []Window{{EndMs: 1000, SpanMs: 1000, Device: "sda", D: Counters{1, 2, 3, 4, 5, 6}}}
	md := []MDWindow{
		{EndMs: 1000, SpanMs: 1000, S: MDState{Action: "resync", Completed: 10, Total: 100, SpeedKiBps: 7, Degraded: 1,
			ArrayState: "active", RaidDisks: 2, Members: map[string]string{"sda": "in_sync,write_mostly", "sdb": "faulty"}}},
		{EndMs: 2000, SpanMs: 1000, S: MDState{Action: "idle", ArrayState: "clean", RaidDisks: 2}},
	}
	var buf bytes.Buffer
	if err := WriteLog(&buf, m, ws, md); err != nil {
		t.Fatal(err)
	}
	gm, gw, gmd, err := ReadLog(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gm, m) || !reflect.DeepEqual(gw, ws) || !reflect.DeepEqual(gmd, md) {
		t.Fatalf("got %+v %+v %+v", gm, gw, gmd)
	}
}

func TestReadLogOldFormatHasNoMD(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteLog(&buf, Meta{AvgMsec: 1000}, []Window{{EndMs: 1, SpanMs: 1, Device: "sda"}}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "md") {
		t.Fatalf("md lines without md windows: %q", buf.String())
	}
	_, _, md, err := ReadLog(&buf)
	if err != nil || len(md) != 0 {
		t.Fatal(err, md)
	}
}

func TestReadLogRejectsGarbageMDLine(t *testing.T) {
	for _, s := range []string{
		"md, 1, 1, check, 1, 2\n",
		"md, x, 1, check, 1, 2, 3, 0, clean, 2\n",
		"md, 1, 1, check, 1, 2, 3, 0, clean, z\n",
		"mdmember, 1, sda\n",
		"mdmember, 1, sda, in_sync\n", // no md window at 1
		"md, 1, 1, idle, 0, 0, 0, 0, clean, 2\nmdmember, x, sda, in_sync\n",
	} {
		if _, _, _, err := ReadLog(strings.NewReader(s)); err == nil {
			t.Errorf("%q: want error", s)
		}
	}
}
