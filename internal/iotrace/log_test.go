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
		Summary: Summary{AvgMsec: 1000, OffsetMs: 10000, Overrun: 3, StartMissing: true,
			Reader: "raw", Clock: "mono", Late: 4, ReaderNote: "2 raw records could not be decoded; x=y", Devices: []DeviceSummary{
				{Name: "nvme0n1", Source: "nvme", Exact: true, BlockLayer: true, Pair: "ctrl,qid,cmdid",
					Events: []string{"nvme_setup_cmd -> nvme_complete_rq", "block_rq_insert -> block_rq_complete"},
					Stats: map[Kind]map[Dir]Stats{
						KindDevice:     {DirRead: st(312, 85000.5, 410000), DirWrite: st(10, 90000, 500000)},
						KindBlockLayer: {DirRead: st(312, 95000.25, 450000)}}},
				{Name: "sdb", Source: "block", Exact: true, Pair: "dev,sector", Unpaired: 2,
					Events: []string{"block_rq_issue -> block_rq_complete"},
					Stats:  map[Kind]map[Dir]Stats{KindDevice: {DirWrite: st(50, 8e9, 4_000_000_000)}}},
				{Name: "md0", Note: "bio_based, a=b 100% +x, no requests"},
				{Name: "sdc", Source: "block", Pair: "dev,sector=1", Events: []string{"a_b -> c=d"}},
				{Name: "md1", Source: "bio", BioBased: true, Pair: PairBio, Unpaired: 1,
					Events: []string{"block_bio_queue -> block_bio_complete"},
					Stats:  map[Kind]map[Dir]Stats{KindBio: {DirRead: st(20, 300000, 900000), DirWrite: st(4, 9.5e6, 12_000_000)}}},
			}},
		Windows: []Window{
			{EndMs: 11000, Dev: "nvme0n1", Kind: KindDevice, Dir: DirRead, Stats: st(100, 85000.5, 410000)},
			{EndMs: 11000, Dev: "nvme0n1", Kind: KindBlockLayer, Dir: DirWrite, Stats: st(7, 99999.5, 450000)},
			{EndMs: 12000, Dev: "sdc", Kind: KindDevice, Dir: DirRead, Stats: st(3, 1000, 5000)},
			{EndMs: 11000, Dev: "sdb", Kind: KindDevice, Dir: DirWrite, Stats: st(5, 3e9, 4_000_000_000)},
			{EndMs: 11000, Dev: "md1", Kind: KindBio, Dir: DirRead, Stats: st(20, 300000, 900000)},
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
	got, err := ReadLog(strings.NewReader(strings.Replace(b.String(), "# device", "# future line\n# device", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	if !strings.Contains(b.String(), "# device md1 source=bio exact=false block_layer=false unpaired=1 bio_based=true pair=dev,sector,nr_sectors events=block_bio_queue->block_bio_complete") ||
		!strings.Contains(b.String(), "11000, bio, r, 20, 300000,") || !strings.Contains(b.String(), "# summary md1 bio w count=4") {
		t.Fatalf("bio lines missing:\n%s", b.String())
	}
	if !strings.Contains(got.Summary.TextDevice, "; md1: block_bio_queue -> block_bio_complete · pairing: first-in first-out (dev,sector,nr_sectors)") {
		t.Fatalf("TextDevice not filled: %+v", got.Summary)
	}
	if got.Summary.Lost != 6 || got.Summary.Text == "" {
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

func TestReadLogRejectsBadWindow(t *testing.T) {
	h := "# bbbench-iolat-log v1 avg_msec=1000\n"
	for _, l := range []string{
		"1000, device, r, 1, 2, 3, 4, 5, 6",
		"1000, device, r, 1, 2, 3, 4, 5, 6, sda, extra",
		"1000, device, r, 1, abc, 3, 4, 5, 6, sda",
		"1000, device, r, 1, 2, 3, 4, 5, x, sda",
	} {
		if _, err := ReadLog(strings.NewReader(h + l + "\n")); err == nil {
			t.Errorf("want error for %q", l)
		}
	}
}

// A log written before the bio kind existed reads as before: no bio-based
// device, no bio text.
func TestReadLogBeforeBio(t *testing.T) {
	in := "# bbbench-iolat-log v1 avg_msec=1000 offset_ms=0 overrun=0 lost=0\n" +
		"# device md0 source=block exact=false block_layer=true unpaired=0 pair=dev,sector events=block_rq_issue->block_rq_complete,block_rq_insert->block_rq_complete\n" +
		"# device sdb source=block exact=false block_layer=true unpaired=0 pair=dev,sector events=block_rq_issue->block_rq_complete,block_rq_insert->block_rq_complete\n" +
		"# summary sdb device r count=1 mean_ns=5 min_ns=5 max_ns=5 p50_ns=5 p99_ns=5\n" +
		"# end_ms, kind, dir, count, mean_ns, min_ns, max_ns, p50_ns, p99_ns, dev\n" +
		"1000, device, r, 1, 5, 5, 5, 5, 5, sdb\n"
	got, err := ReadLog(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	md := got.Summary.Devices[0]
	if md.BioBased || md.Source != SourceBlock || len(got.Windows) != 1 {
		t.Errorf("old log = %+v", got)
	}
	if md.BlockLayerNote != "ftrace saw no block-layer requests on md0" {
		t.Errorf("old md0 note = %q", md.BlockLayerNote)
	}
}
