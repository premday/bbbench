package flushtrace

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestLogRoundTripRecomputesSummary(t *testing.T) {
	var all []Sample
	for i := int64(1); i <= 100; i++ {
		all = append(all, Sample{TimeNs: i * 10_000_000, LatencyNs: i * 1000})
	}
	want := Summarize("nvme", true, 2, all)

	var buf bytes.Buffer
	if err := WriteLog(&buf, want, all); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "# bbbench-flush-log v1 source=nvme exact=true lost=2\n") {
		t.Errorf("header: %q", strings.SplitN(buf.String(), "\n", 2)[0])
	}
	got, samples, err := ReadLog(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("summary after round trip = %+v, want %+v", got, want)
	}
	if len(samples) != 100 || samples[0] != all[0] || samples[99] != all[99] {
		t.Errorf("samples not preserved: %d, %+v ... %+v", len(samples), samples[0], samples[99])
	}
}

func TestLogBlockSourceKeepsInexactSummary(t *testing.T) {
	all := []Sample{{TimeNs: 1e6, LatencyNs: 200_000}, {TimeNs: 2e6, LatencyNs: 500_000}}
	var buf bytes.Buffer
	_ = WriteLog(&buf, Summarize("block", false, 0, all), all)
	got, _, err := ReadLog(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "block" || got.Exact || got.P99Ns != 0 || math.Abs(got.MeanNs-350_000) > 1e-6 {
		t.Errorf("%+v", got)
	}
}

func TestReadLogRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "hello\n", "# bbbench-flush-log v1 source=nvme exact=true lost=0\nnot,numbers\n"} {
		if _, _, err := ReadLog(strings.NewReader(in)); err == nil {
			t.Errorf("ReadLog(%q) should fail", in)
		}
	}
}

func TestReadLogEmptyRun(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteLog(&buf, Summarize("nvme", true, 0, nil), nil)
	got, samples, err := ReadLog(&buf)
	if err != nil || got.Count != 0 || len(samples) != 0 {
		t.Errorf("%+v %v %v", got, samples, err)
	}
}

func TestBins(t *testing.T) {
	all := []Sample{
		{TimeNs: 100e6, LatencyNs: 100}, {TimeNs: 900e6, LatencyNs: 300}, // window 0: mean 200
		{TimeNs: 2_500e6, LatencyNs: 1000}, // window 2; window 1 empty
	}
	b := Bins(all, 1_000_000_000)
	if len(b) != 2 || b[0].TSec != 0 || b[0].MeanNs != 200 || b[0].Count != 2 || b[1].TSec != 2 || b[1].MeanNs != 1000 {
		t.Errorf("%+v", b)
	}
}

func TestLog_WriteCacheRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	sum := Summarize("nvme", true, 0, nil)
	sum.WriteCache = "write through"
	if err := WriteLog(&buf, sum, nil); err != nil {
		t.Fatal(err)
	}
	got, samples, err := ReadLog(&buf)
	if err != nil || len(samples) != 0 {
		t.Fatalf("ReadLog: %v, %d samples", err, len(samples))
	}
	if got.WriteCache != "write through" || got.Count != 0 {
		t.Errorf("summary = %+v", got)
	}
}
