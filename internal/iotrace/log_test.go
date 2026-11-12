package iotrace

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func sampleResult() Result {
	st := func(n int64, mean float64, mx int64) Stats {
		return Stats{Count: n, MeanNs: mean, MinNs: 1000, MaxNs: mx, P50Ns: 2000, P99Ns: mx / 2}
	}
	r := Result{
		Summary: Summary{AvgMsec: 1000, OffsetMs: 10000, Overrun: 3, Devices: []DeviceSummary{
			{Name: "nvme0n1", Source: "nvme", Exact: true, BlockLayer: true, Pair: "ctrl,qid,cmdid",
				Events: []string{"nvme_setup_cmd -> nvme_complete_rq", "block_rq_insert -> block_rq_complete"},
				Stats: map[Kind]map[Dir]Stats{
					KindDevice:     {DirRead: st(312, 85000.5, 410000), DirWrite: st(10, 90000, 500000)},
					KindBlockLayer: {DirRead: st(312, 95000.25, 450000)}}},
			{Name: "sdb", Source: "block", Exact: true, Pair: "dev,sector", Unpaired: 2,
				Events: []string{"block_rq_issue -> block_rq_complete"},
				Stats:  map[Kind]map[Dir]Stats{KindDevice: {DirWrite: st(50, 8e9, 4_000_000_000)}}},
			{Name: "md0", Note: "bio based, no requests"},
		}},
		Windows: []Window{
			{EndMs: 11000, Dev: "nvme0n1", Kind: KindDevice, Dir: DirRead, Stats: st(100, 85000.5, 410000)},
			{EndMs: 11000, Dev: "sdb", Kind: KindDevice, Dir: DirWrite, Stats: st(5, 3e9, 4_000_000_000)},
		},
	}
	r.Summary.Fill()
	return r
}

func TestLogRoundTrip(t *testing.T) {
	want := sampleResult()
	var b bytes.Buffer
	if err := WriteLog(&b, want); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "events=nvme_setup_cmd->nvme_complete_rq,block_rq_insert->block_rq_complete") {
		t.Fatalf("events not compact:\n%s", b.String())
	}
	got, err := ReadLog(&b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	if got.Summary.Lost != 5 || got.Summary.Text == "" {
		t.Fatalf("Lost/Text not filled: %+v", got.Summary)
	}
}

func TestReadLogRejectsGarbageLine(t *testing.T) {
	in := "# bbbench-iolat-log v1 avg_msec=1000\n1000, device, r, x\n"
	if _, err := ReadLog(strings.NewReader(in)); err == nil {
		t.Fatal("want error")
	}
	if _, err := ReadLog(strings.NewReader("")); err == nil {
		t.Fatal("want error for empty")
	}
}

func TestReadLogNotIOLat(t *testing.T) {
	if _, err := ReadLog(strings.NewReader("# bbbench-flush-log v2 source=nvme\n")); err == nil {
		t.Fatal("want error")
	}
}

func TestLogNameSuffix(t *testing.T) {
	if got := LogName("/x/p_log"); got != "/x/p_log_iolat.1.log" {
		t.Fatal(got)
	}
}
