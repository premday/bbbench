package flushtrace

import "testing"

func TestParseLine_Cases(t *testing.T) {
	good := []struct {
		line    string
		ns      int64
		name    string
		payload string
	}{
		{"           <idle>-0       [002] d.h1.  1234.567890: block_rq_complete: 259,0 F () 0 + 0 [0]", 1234_567_890_000, "block_rq_complete", "259,0 F () 0 + 0 [0]"},
		{"  kworker/u8:3-h-77 [001] ..... 5.000000001: nvme_complete_rq: nvme0: qid=1,", 5_000_000_001, "nvme_complete_rq", "nvme0: qid=1,"},
		{"  my task name-12 [001] ..... 7.5: block_rq_issue: 8,0 R", 7_500_000_000, "block_rq_issue", "8,0 R"},
		{"fio-1 [001] ..... 10.123456: block_rq_insert: x", 10_123_456_000, "block_rq_insert", "x"},
		{"fio-1 [001] ..... 10.123456789012: block_rq_insert: x", 10_123_456_789, "block_rq_insert", "x"},
		{"fio-1 [001] d.h1. 10.000001: block_rq_insert:x", 10_000_001_000, "block_rq_insert", "x"},
		{"fio-1 [001] dNh1. 10.000001: block_rq_insert: a b\r\n", 10_000_001_000, "block_rq_insert", "a b"},
		{"bbbench-1 [000] ..... 1.000000: tracing_mark_write: bbbench-start", 1_000_000_000, "tracing_mark_write", "bbbench-start"},
	}
	for _, g := range good {
		ev, ok := ParseLine(g.line)
		if !ok || ev.TimeNs != g.ns || ev.Name != g.name || ev.Payload != g.payload {
			t.Errorf("%q: got %+v ok=%v", g.line, ev, ok)
		}
	}
	for _, bad := range []string{"", "# tracer: nop", "# entries-in-buffer/entries-written: 3/3   #P:8",
		"CPU:2 [LOST 120 EVENTS]", "fio-1 [001] ..... abc.def: x: y", "fio-x [001] ..... 1.0: x: y"} {
		if _, ok := ParseLine(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func BenchmarkParseLine(b *testing.B) {
	line := "          fio-1234    [003] d.h1.  1234.567890: block_rq_complete: 259,0 W () 123456 + 8 [0]\n"
	for i := 0; i < b.N; i++ {
		if _, ok := ParseLine(line); !ok {
			b.Fatal("no parse")
		}
	}
}
