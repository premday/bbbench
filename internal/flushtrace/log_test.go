package flushtrace

import (
	"bytes"
	"strings"
	"testing"
)

func TestLogRoundTripKeepsSummaryAndWindows(t *testing.T) {
	var all []Sample
	for i := int64(1); i <= 100; i++ {
		all = append(all, Sample{TimeNs: i * 10_000_000, LatencyNs: i * 1000})
	}
	want := Finish(Summary{Source: "nvme", Exact: true, Lost: 2, WriteCache: "write back", AvgMsec: 500, OffsetMs: 100}, all)

	var buf bytes.Buffer
	if err := WriteLog(&buf, want, all); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "# bbbench-flush-log v2 source=nvme exact=true lost=2 write_cache=write_back avg_msec=500 offset_ms=100\n") {
		t.Errorf("header: %q", strings.SplitN(buf.String(), "\n", 2)[0])
	}
	got, win, err := ReadLog(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("summary after round trip = %+v, want %+v", got, want)
	}
	// 100 samples every 10 ms (10..1000 ms) in 500 ms windows: [0,500) has 49,
	// [500,1000) 50, and the sample at exactly 1000 ms opens a third window.
	if len(win) != 3 || win[0].EndMs != 500 || win[1].EndMs != 1000 || win[2].EndMs != 1500 ||
		win[0].Count != 49 || win[1].Count != 50 || win[2].Count != 1 {
		t.Errorf("windows = %+v", win)
	}
}

// The log has one line per window, not one per Flush: it stays small however
// many Flushes a phase has.
func TestLogSizeFollowsWindowsNotFlushes(t *testing.T) {
	var all []Sample
	for i := int64(0); i < 200_000; i++ { // 200k Flushes over 100 s
		all = append(all, Sample{TimeNs: i * 500_000, LatencyNs: 150_000 + i%1000})
	}
	var buf bytes.Buffer
	if err := WriteLog(&buf, Finish(Summary{Source: "nvme", Exact: true, AvgMsec: 1000}, all), all); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(buf.String(), "\n"); lines > 110 {
		t.Errorf("%d lines for 100 windows", lines)
	}
	if buf.Len() > 10_000 {
		t.Errorf("%d bytes", buf.Len())
	}
}

// The first five columns are fio's, so fio log tooling reads the file.
func TestLogLinesLookLikeFioLines(t *testing.T) {
	all := []Sample{{TimeNs: 1_500_000_000, LatencyNs: 200_000}}
	var buf bytes.Buffer
	_ = WriteLog(&buf, Finish(Summary{Source: "nvme", Exact: true, AvgMsec: 1000}, all), all)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if got := lines[len(lines)-1]; got != "2000, 200000, 3, 0, 0, 1, 200000, 200000" {
		t.Errorf("last line = %q", got)
	}
}

func TestLogBlockSourceKeepsInexactSummary(t *testing.T) {
	all := []Sample{{TimeNs: 1e6, LatencyNs: 200_000}, {TimeNs: 2e6, LatencyNs: 500_000}}
	var buf bytes.Buffer
	_ = WriteLog(&buf, Finish(Summary{Source: "block"}, all), all)
	got, _, err := ReadLog(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "block" || got.Exact || got.P99Ns != 0 || got.MeanNs != 350_000 {
		t.Errorf("%+v", got)
	}
}

func TestReadLogRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"", "hello\n",
		"# bbbench-flush-log v2 source=nvme exact=true lost=0\nnot,numbers\n",
		"# bbbench-flush-log v1 source=nvme exact=true lost=0\n# time_ms, latency_ns\n1.0, 5\n", // the old per-Flush format
	} {
		if _, _, err := ReadLog(strings.NewReader(in)); err == nil {
			t.Errorf("ReadLog(%q) should fail", in)
		}
	}
}

func TestReadLogEmptyRun(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteLog(&buf, Finish(Summary{Source: "nvme", Exact: true}, nil), nil)
	got, win, err := ReadLog(&buf)
	if err != nil || got.Count != 0 || len(win) != 0 {
		t.Errorf("%+v %v %v", got, win, err)
	}
}

func TestWindows(t *testing.T) {
	all := []Sample{
		{TimeNs: 100e6, LatencyNs: 100}, {TimeNs: 900e6, LatencyNs: 300}, // window 0: mean 200
		{TimeNs: 2_500e6, LatencyNs: 1000}, // window 2; window 1 empty
	}
	w := Windows(all, 1000)
	if len(w) != 2 || w[0].EndMs != 1000 || w[0].MeanNs != 200 || w[0].Count != 2 || w[0].MinNs != 100 || w[0].MaxNs != 300 ||
		w[1].EndMs != 3000 || w[1].MeanNs != 1000 {
		t.Errorf("%+v", w)
	}
}

// A Flush that completed before the last start marker (a session opened
// before a start gate) has a negative time: it is in no window and not in the
// summary, with or without a ramp.
func TestNegativeTimesLeftOut(t *testing.T) {
	all := []Sample{{TimeNs: -200e6, LatencyNs: 9000}, {TimeNs: 100e6, LatencyNs: 100}}
	w := Windows(all, 1000)
	if len(w) != 1 || w[0].Count != 1 || w[0].MeanNs != 100 {
		t.Errorf("windows = %+v", w)
	}
	for _, off := range []int{0, 50} {
		sum := Finish(Summary{Source: "nvme", Exact: true, OffsetMs: off}, all)
		if sum.Count != 1 || sum.MaxNs != 100 {
			t.Errorf("offset %d: summary = %+v", off, sum)
		}
	}
}

func TestLog_WriteCacheRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	sum := Summarize("nvme", true, 0, nil)
	sum.WriteCache = "write through"
	if err := WriteLog(&buf, sum, nil); err != nil {
		t.Fatal(err)
	}
	got, win, err := ReadLog(&buf)
	if err != nil || len(win) != 0 {
		t.Fatalf("ReadLog: %v, %d windows", err, len(win))
	}
	if got.WriteCache != "write through" || got.Count != 0 {
		t.Errorf("summary = %+v", got)
	}
}
