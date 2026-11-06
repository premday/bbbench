package disktemp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func put(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func noRun(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no smartctl") }

func TestFind_NVMeHwmonWithLimits(t *testing.T) {
	sys := t.TempDir()
	put(t, sys, map[string]string{
		"class/nvme/nvme0/hwmon4/name":        "nvme",
		"class/nvme/nvme0/hwmon4/temp1_input": "41850",
		"class/nvme/nvme0/hwmon4/temp1_label": "Composite",
		"class/nvme/nvme0/hwmon4/temp1_max":   "82850",
		"class/nvme/nvme0/hwmon4/temp1_crit":  "84850",
	})
	s, err := Find("nvme0n1", Options{Sys: sys, Run: noRun})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Read()
	if s.Source != "hwmon" || s.Name != "Composite" || c != 41.85 || s.WarnC != 82.85 || s.CritC != 84.85 {
		t.Errorf("%+v %v", s, c)
	}
	// The reading follows the file.
	put(t, sys, map[string]string{"class/nvme/nvme0/hwmon4/temp1_input": "50000"})
	if c, _ := s.Read(); c != 50 {
		t.Errorf("re-read = %v", c)
	}
}

// Drives that report an unset threshold as thousands of degrees get no limit.
func TestFind_ImplausibleLimitsAreDropped(t *testing.T) {
	sys := t.TempDir()
	put(t, sys, map[string]string{
		"class/nvme/nvme1/hwmon2/temp1_input": "30000",
		"class/nvme/nvme1/hwmon2/temp1_max":   "65261850",
		"class/nvme/nvme1/hwmon2/temp1_crit":  "0",
	})
	s, err := Find("nvme1n1", Options{Sys: sys, Run: noRun})
	if err != nil || s.WarnC != 0 || s.CritC != 0 || s.Name != "" {
		t.Errorf("%+v %v", s, err)
	}
}

func TestFind_DrivetempForSATA(t *testing.T) {
	sys := t.TempDir()
	put(t, sys, map[string]string{
		"block/sda/device/hwmon/hwmon3/name":        "drivetemp",
		"block/sda/device/hwmon/hwmon3/temp1_input": "36000",
		"block/sda/device/hwmon/hwmon3/temp1_max":   "60000",
	})
	s, err := Find("sda", Options{Sys: sys, Run: noRun})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Read(); s.Name != "drivetemp" || c != 36 || s.WarnC != 60 {
		t.Errorf("%+v %v", s, c)
	}
}

func TestFind_SmartctlFallbackAndNoSensor(t *testing.T) {
	sys := t.TempDir()
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "smartctl" && strings.Join(args, " ") == "-j -A /dev/sdb" {
			return []byte(`{"temperature":{"current":44}}`), errors.New("exit status 4")
		}
		return nil, errors.New("x")
	}
	s, err := Find("sdb", Options{Sys: sys, Run: run})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Read(); s.Source != "smartctl" || c != 44 {
		t.Errorf("%+v %v", s, c)
	}
	if _, err := Find("sdc", Options{Sys: sys, Run: noRun}); !errors.Is(err, ErrNoSensor) || !strings.Contains(err.Error(), "drivetemp") {
		t.Errorf("no sensor must say how to get one: %v", err)
	}
}

func TestSampler(t *testing.T) {
	var n atomic.Int64
	s := &Sensor{Source: "hwmon", Name: "Composite", read: func() (float64, error) {
		return 40 + float64(n.Add(1)), nil
	}}
	var seen atomic.Int64
	sm := StartSampler(s, 10*time.Millisecond, func(Sample) { seen.Add(1) })
	time.Sleep(55 * time.Millisecond)
	got := sm.Stop()
	if len(got) < 4 || got[0].MilliC != 41000 || got[0].TimeMs > 5 {
		t.Fatalf("samples = %+v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].TimeMs < got[i-1].TimeMs || got[i].MilliC <= got[i-1].MilliC {
			t.Errorf("not in order: %+v", got)
		}
	}
	if int(seen.Load()) != len(got) {
		t.Errorf("callback saw %d of %d", seen.Load(), len(got))
	}
	if again := sm.Stop(); len(again) != len(got) {
		t.Errorf("a second Stop changed the samples: %d vs %d", len(again), len(got))
	}
}

// A failing reading is a gap in the series, not an end of it.
func TestSampler_SkipsFailedReadings(t *testing.T) {
	var n atomic.Int64
	s := &Sensor{read: func() (float64, error) {
		if n.Add(1)%2 == 0 {
			return 0, errors.New("busy")
		}
		return 40, nil
	}}
	sm := StartSampler(s, 5*time.Millisecond, nil)
	time.Sleep(30 * time.Millisecond)
	if got := sm.Stop(); len(got) == 0 || len(got) >= int(n.Load()) {
		t.Errorf("%d samples from %d reads", len(got), n.Load())
	}
}

func TestLogRoundTrip(t *testing.T) {
	s := &Sensor{Source: "hwmon", Name: "Composite", WarnC: 82.85, CritC: 84.85}
	all := []Sample{{0, 41850}, {5000, 43000}, {10000, 47150}}
	want := Summarize(s, 5000, all)
	var buf bytes.Buffer
	if err := WriteLog(&buf, want, all); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "# bbbench-temp-log v1 source=hwmon sensor=Composite interval_ms=5000 warn_mc=82850 crit_mc=84850\n") ||
		!strings.Contains(buf.String(), "\n5000, 43000, 0, 0, 0\n") {
		t.Errorf("log:\n%s", buf.String())
	}
	got, samples, err := ReadLog(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || len(samples) != 3 || samples[2] != all[2] {
		t.Errorf("summary %+v want %+v, samples %+v", got, want, samples)
	}
	if got.MinC != 41.85 || got.MaxC != 47.15 || got.Count != 3 {
		t.Errorf("%+v", got)
	}
}

func TestReadLogRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "hello\n", "# bbbench-temp-log v1 source=hwmon\nx,y\n", "# bbbench-flush-log v2 source=nvme\n"} {
		if _, _, err := ReadLog(strings.NewReader(in)); err == nil {
			t.Errorf("ReadLog(%q) should fail", in)
		}
	}
}

func TestSummarize_Empty(t *testing.T) {
	if got := Summarize(&Sensor{Source: "hwmon"}, 5000, nil); got.Count != 0 || got.MaxC != 0 {
		t.Errorf("%+v", got)
	}
}
