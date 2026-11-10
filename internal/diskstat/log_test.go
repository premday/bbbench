package diskstat

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestLogRoundTrip(t *testing.T) {
	m := Meta{AvgMsec: 1000, OffsetMs: 10000, Array: "md0",
		Members: []MemberMeta{{"sda", "data"}, {"sdb", "spare"}}}
	ws := []Window{
		{EndMs: 1000, SpanMs: 1000, Device: "sda", D: Counters{1, 2, 3, 4, 5, 6}},
		{EndMs: 1000, SpanMs: 1000, Device: "sdb", D: Counters{7, 8, 9, 10, 11, 12}},
		{EndMs: 2000, SpanMs: 1000, Device: "sda"},
	}
	var buf bytes.Buffer
	if err := WriteLog(&buf, m, ws, nil); err != nil {
		t.Fatal(err)
	}
	gm, gw, _, err := ReadLog(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gm, m) || !reflect.DeepEqual(gw, ws) {
		t.Fatalf("got %+v %+v", gm, gw)
	}
}

func TestReadLogRejectsGarbageLine(t *testing.T) {
	for _, s := range []string{"garbage\n", "1, 2, sda, x, 1, 1, 1, 1, 1\n", "1, 2, sda, 1\n", "# avg_msec nope\n", "# device sda\n"} {
		if _, _, _, err := ReadLog(strings.NewReader(s)); err == nil {
			t.Errorf("%q: want error", s)
		}
	}
}

func TestLogNameSuffix(t *testing.T) {
	if got := LogName("run_x"); got != "run_x_members.1.log" {
		t.Fatal(got)
	}
}
