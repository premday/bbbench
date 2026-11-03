package flushtrace

import (
	"math"
	"testing"
)

func feed(t *testing.T, c Collector, lines ...string) {
	t.Helper()
	for _, l := range lines {
		ev, ok := ParseLine(l)
		if !ok {
			t.Fatalf("line did not parse: %q", l)
		}
		c.Feed(ev)
	}
}

func TestParseLine(t *testing.T) {
	ev, ok := ParseLine("           <idle>-0       [002] d.h1.  1234.567890: block_rq_complete: 259,0 F () 0 + 0 [0]")
	if !ok || ev.Name != "block_rq_complete" || ev.TimeNs != 1234_567_890_000 || ev.Payload != "259,0 F () 0 + 0 [0]" {
		t.Fatalf("got %+v ok=%v", ev, ok)
	}
	// Task names may contain '-' and spaces.
	ev, ok = ParseLine("  kworker/u8:3-h-77 [001] ..... 5.000000001: nvme_complete_rq: nvme0: qid=1, cmdid=2,")
	if !ok || ev.TimeNs != 5_000_000_001 {
		t.Fatalf("got %+v ok=%v", ev, ok)
	}
	for _, bad := range []string{"", "# tracer: nop", "# entries-in-buffer/entries-written: 3/3   #P:8"} {
		if _, ok := ParseLine(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestNVMe_PairsByCommandID(t *testing.T) {
	c := NewNVMe("nvme0n1")
	feed(t, c,
		// Two Flushes in flight on different queues, completing out of order.
		"fio-10 [001] ..... 100.000000: nvme_setup_cmd: nvme0: disk=nvme0n1, qid=1, cmdid=40, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_flush cdw10=00000000)",
		"fio-10 [002] ..... 100.000010: nvme_setup_cmd: nvme0: disk=nvme0n1, qid=2, cmdid=40, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_flush cdw10=00000000)",
		// A write and another namespace's Flush must be ignored.
		"fio-10 [001] ..... 100.000020: nvme_setup_cmd: nvme0: disk=nvme0n1, qid=1, cmdid=41, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_write slba=0, len=7)",
		"fio-10 [001] ..... 100.000030: nvme_setup_cmd: nvme1: disk=nvme1n1, qid=1, cmdid=42, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_flush cdw10=00000000)",
		"<idle>-0 [002] d.h1. 100.000200: nvme_complete_rq: nvme0: disk=nvme0n1, qid=2, cmdid=40, res=0x0, retries=0, flags=0x0, status=0x0",
		"<idle>-0 [001] d.h1. 100.000300: nvme_complete_rq: nvme0: disk=nvme0n1, qid=1, cmdid=40, res=0x0, retries=0, flags=0x0, status=0x0",
		"<idle>-0 [001] d.h1. 100.000310: nvme_complete_rq: nvme0: disk=nvme0n1, qid=1, cmdid=41, res=0x0, retries=0, flags=0x0, status=0x0",
		"<idle>-0 [001] d.h1. 100.000320: nvme_complete_rq: nvme1: disk=nvme1n1, qid=1, cmdid=42, res=0x0, retries=0, flags=0x0, status=0x0",
	)
	s := c.Summary()
	// qid1: 300000-0 = 300000ns ; qid2: 200000-10000 = 190000ns
	if s.Source != "nvme" || !s.Exact || s.Count != 2 || s.Lost != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if s.MinNs != 190_000 || s.MaxNs != 300_000 || math.Abs(s.MeanNs-245_000) > 1e-6 {
		t.Errorf("latencies wrong: %+v", s)
	}
}

func TestNVMe_UnansweredFlushIsLost(t *testing.T) {
	c := NewNVMe("nvme0n1")
	feed(t, c, "fio-10 [001] ..... 1.000000: nvme_setup_cmd: nvme0: disk=nvme0n1, qid=1, cmdid=7, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_flush cdw10=00000000)")
	if s := c.Summary(); s.Count != 0 || s.Lost != 1 {
		t.Errorf("summary = %+v", s)
	}
}

func TestBlock_FlushOnlyForTheRightDevice(t *testing.T) {
	c := NewBlock(259, 0)
	feed(t, c,
		"fio-1 [000] ..... 10.000000: block_rq_issue: 259,0 F 0 () 0 + 0 none,0,0 [fio]",
		// Data write with FUA, other device flush, and a plain write: all ignored.
		"fio-1 [000] ..... 10.000010: block_rq_issue: 259,0 WF 4096 () 8 + 8 none,0,0 [fio]",
		"fio-1 [000] ..... 10.000020: block_rq_issue: 8,16 F 0 () 0 + 0 none,0,0 [fio]",
		"fio-1 [000] ..... 10.000030: block_rq_issue: 259,0 W 4096 () 16 + 8 none,0,0 [fio]",
		"<idle>-0 [000] d.h1. 10.000195: block_rq_complete: 259,0 WF () 8 + 8 [0]",
		"<idle>-0 [000] d.h1. 10.000200: block_rq_complete: 8,16 F () 0 + 0 [0]",
		"<idle>-0 [000] d.h1. 10.000250: block_rq_complete: 259,0 F () 0 + 0 [0]",
	)
	s := c.Summary()
	if s.Source != "block" || s.Exact || s.Count != 1 || s.Lost != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if s.MeanNs != 250_000 {
		t.Errorf("mean = %v ns, want 250000", s.MeanNs)
	}
	if s.MinNs != 0 || s.P99Ns != 0 {
		t.Errorf("percentiles must not be reported for FIFO pairing: %+v", s)
	}
}

func TestBlock_MeanIndependentOfPairingOrder(t *testing.T) {
	// Two Flushes in flight; they complete in the opposite order to issue.
	// FIFO pairing mismatches the samples but the mean is preserved.
	c := NewBlock(8, 0)
	feed(t, c,
		"a-1 [000] ..... 1.000000: block_rq_issue: 8,0 F 0 () 0 + 0 none,0,0 [a]",
		"a-1 [000] ..... 1.000100: block_rq_issue: 8,0 F 0 () 0 + 0 none,0,0 [a]",
		"<idle>-0 [000] d.h1. 1.000300: block_rq_complete: 8,0 F () 0 + 0 [0]", // really the 2nd: 200us
		"<idle>-0 [000] d.h1. 1.000500: block_rq_complete: 8,0 F () 0 + 0 [0]", // really the 1st: 500us
	)
	// true mean = (200+500)/2 = 350us
	if s := c.Summary(); s.Count != 2 || math.Abs(s.MeanNs-350_000) > 1e-6 {
		t.Errorf("summary = %+v", s)
	}
}

func TestPercentile(t *testing.T) {
	s := samples{}
	for i := int64(1); i <= 100; i++ {
		s.add(i, i)
	}
	sum := s.summary("nvme", true, 0)
	if sum.P50Ns != 50 || sum.P99Ns != 99 || sum.MinNs != 1 || sum.MaxNs != 100 {
		t.Errorf("%+v", sum)
	}
}
