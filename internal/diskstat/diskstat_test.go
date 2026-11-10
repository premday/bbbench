package diskstat

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeStat(t *testing.T, root, name string, rIOs, rSec, rTicks, wIOs, wSec, wTicks uint64) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// fields: 0 rios,1 rmerge,2 rsect,3 rticks,4 wios,5 wmerge,6 wsect,7 wticks,8 inflight,...
	line := fmtFields(rIOs, 11, rSec, rTicks, wIOs, 12, wSec, wTicks, 0, 99, 99)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fmtFields(v ...uint64) string {
	s := "   "
	for i, x := range v {
		if i > 0 {
			s += "  "
		}
		s += utoa(x)
	}
	return s + "\n"
}

func utoa(x uint64) string {
	if x == 0 {
		return "0"
	}
	var b []byte
	for x > 0 {
		b = append([]byte{byte('0' + x%10)}, b...)
		x /= 10
	}
	return string(b)
}

func TestReadCounters(t *testing.T) {
	root := t.TempDir()
	writeStat(t, root, "sda", 1, 2, 3, 4, 5, 6)
	c, err := ReadCounters(root, "sda")
	if err != nil {
		t.Fatal(err)
	}
	want := Counters{1, 2, 3, 4, 5, 6}
	if c != want {
		t.Fatalf("got %+v want %+v", c, want)
	}
	if _, err := ReadCounters(root, "nope"); err == nil {
		t.Fatal("want error for missing device")
	}
}

func TestSampleDeltas(t *testing.T) {
	root := t.TempDir()
	writeStat(t, root, "sda", 0, 0, 0, 0, 0, 0)
	writeStat(t, root, "sdb", 10, 0, 0, 0, 0, 0)
	s := NewSampler([]string{"sda", "sdb"}, root)
	t0 := time.Unix(1000, 0)
	s.Sample(t0)
	writeStat(t, root, "sda", 100, 200, 50, 10, 20, 5)
	writeStat(t, root, "sdb", 15, 0, 0, 0, 0, 0)
	s.Sample(t0.Add(time.Second))
	writeStat(t, root, "sda", 150, 300, 70, 10, 20, 5)
	s.Sample(t0.Add(3 * time.Second))
	w := s.Windows()
	if len(w) != 4 {
		t.Fatalf("got %d windows", len(w))
	}
	if w[0].Device != "sda" || w[0].EndMs != 1000 || w[0].SpanMs != 1000 || w[0].D != (Counters{100, 200, 50, 10, 20, 5}) {
		t.Fatalf("w0 %+v", w[0])
	}
	if w[1].Device != "sdb" || w[1].D.ReadIOs != 5 {
		t.Fatalf("w1 %+v", w[1])
	}
	if w[2].Device != "sda" || w[2].EndMs != 3000 || w[2].SpanMs != 2000 || w[2].D != (Counters{50, 100, 20, 0, 0, 0}) {
		t.Fatalf("w2 %+v", w[2])
	}
}

func TestCounterBackwardsIsZero(t *testing.T) {
	root := t.TempDir()
	writeStat(t, root, "sda", 100, 100, 100, 100, 100, 100)
	s := NewSampler([]string{"sda"}, root)
	t0 := time.Unix(1000, 0)
	s.Sample(t0)
	writeStat(t, root, "sda", 50, 200, 10, 100, 100, 100)
	s.Sample(t0.Add(time.Second))
	w := s.Windows()
	if len(w) != 1 || w[0].D != (Counters{0, 100, 0, 0, 0, 0}) {
		t.Fatalf("%+v", w)
	}
}

func TestMissingDeviceKeepsWindows(t *testing.T) {
	root := t.TempDir()
	writeStat(t, root, "sda", 0, 0, 0, 0, 0, 0)
	writeStat(t, root, "sdb", 0, 0, 0, 0, 0, 0)
	s := NewSampler([]string{"sda", "sdb"}, root)
	t0 := time.Unix(1000, 0)
	s.Sample(t0)
	writeStat(t, root, "sda", 1, 0, 0, 0, 0, 0)
	writeStat(t, root, "sdb", 1, 0, 0, 0, 0, 0)
	s.Sample(t0.Add(time.Second))
	os.Remove(filepath.Join(root, "sdb", "stat"))
	writeStat(t, root, "sda", 2, 0, 0, 0, 0, 0)
	s.Sample(t0.Add(2 * time.Second))
	s.Sample(t0.Add(3 * time.Second))
	n := map[string]int{}
	for _, w := range s.Windows() {
		n[w.Device]++
	}
	if n["sda"] != 3 || n["sdb"] != 1 {
		t.Fatalf("%v", n)
	}
}

func TestRatesZeroIO(t *testing.T) {
	w := Window{SpanMs: 1000}
	a, b, c, d, e, f := w.Rates()
	for _, v := range []float64{a, b, c, d, e, f} {
		if v != 0 || math.IsNaN(v) {
			t.Fatalf("got %v", v)
		}
	}
	w = Window{}
	a, _, _, _, _, _ = w.Rates()
	if a != 0 || math.IsNaN(a) {
		t.Fatal("zero span")
	}
}

func TestRatesValues(t *testing.T) {
	w := Window{SpanMs: 2000, D: Counters{ReadIOs: 200, ReadSectors: 4096 * 2, ReadTicksMs: 100, WriteIOs: 50, WriteSectors: 2048 * 2, WriteTicksMs: 25}}
	rI, wI, rB, wB, rL, wL := w.Rates()
	if rI != 100 || wI != 25 || rB != 2 || wB != 1 || rL != 500 || wL != 500 {
		t.Fatalf("%v %v %v %v %v %v", rI, wI, rB, wB, rL, wL)
	}
}
