package iotrace

import "testing"

const ms = int64(1_000_000)

func find(ws []Window, end int64, dev string, k Kind, d Dir) (Window, bool) {
	for _, w := range ws {
		if w.EndMs == end && w.Dev == dev && w.Kind == k && w.Dir == d {
			return w, true
		}
	}
	return Window{}, false
}

func TestAggregatorWindows(t *testing.T) {
	a := newAggregator(1000, 0)
	a.add("sda", KindDevice, DirRead, 100*ms, 1000)
	a.add("sda", KindDevice, DirRead, 900*ms, 3000)
	a.add("sda", KindDevice, DirRead, 1500*ms, 5000)
	ws, _ := a.finish()
	if len(ws) != 2 || ws[0].EndMs != 1000 || ws[1].EndMs != 2000 {
		t.Fatalf("windows %+v", ws)
	}
	if ws[0].Count != 2 || ws[0].MeanNs != 2000 || ws[1].Count != 1 || ws[1].MeanNs != 5000 {
		t.Errorf("windows %+v", ws)
	}
}

func TestAggregatorLateEventGoesToOpenWindow(t *testing.T) {
	a := newAggregator(1000, 0)
	a.add("sda", KindDevice, DirWrite, 1500*ms, 10)
	a.add("sda", KindDevice, DirWrite, 200*ms, 30)
	ws, _ := a.finish()
	if len(ws) != 1 || ws[0].EndMs != 2000 || ws[0].Count != 2 {
		t.Errorf("windows %+v", ws)
	}
}

func TestAggregatorSummarySkipsRamp(t *testing.T) {
	a := newAggregator(1000, 1000)
	a.add("sda", KindBlockLayer, DirRead, 500*ms, 100)
	a.add("sda", KindBlockLayer, DirRead, 1000*ms, 200)
	a.add("sda", KindBlockLayer, DirRead, 1200*ms, 400)
	ws, sum := a.finish()
	var n int64
	for _, w := range ws {
		n += w.Count
	}
	if n != 3 {
		t.Errorf("windows hold %d, want 3", n)
	}
	s := sum["sda"][KindBlockLayer][DirRead]
	if s.Count != 2 || s.MeanNs != 300 {
		t.Errorf("summary %+v", s)
	}
}

func TestAggregatorOrder(t *testing.T) {
	a := newAggregator(0, 0)
	a.add("sdb", KindDevice, DirWrite, 10, 1)
	a.add("sda", KindDevice, DirWrite, 10, 1)
	a.add("sda", KindBlockLayer, DirRead, 10, 1)
	a.add("sda", KindDevice, DirRead, 10, 1)
	a.add("sda", KindDevice, DirRead, 1500*ms, 1)
	ws, _ := a.finish()
	want := [][3]string{{"sda", "blocklayer", "r"}, {"sda", "device", "r"}, {"sda", "device", "w"}, {"sdb", "device", "w"}}
	for i, w := range want {
		if ws[i].Dev != w[0] || string(ws[i].Kind) != w[1] || string(ws[i].Dir) != w[2] || ws[i].EndMs != 1000 {
			t.Fatalf("order %+v", ws)
		}
	}
	if _, ok := find(ws, 2000, "sda", KindDevice, DirRead); !ok {
		t.Errorf("missing second window")
	}
}
