package iotrace

import (
	"fmt"
	"testing"

	"bbbench/internal/flushtrace"
)

func TestParseBlockRq(t *testing.T) {
	good := []struct {
		payload   string
		withBytes bool
		want      blockRq
	}{
		// block_rq_insert / block_rq_issue: dev rwbs bytes (cmd) sector + nr [comm]
		{"8,16 R 4096 () 1000 + 8 [fio]", true, blockRq{8, 16, "R", 1000, 8}},
		// with the ioprio field of newer kernels
		{"259,0 WS 131072 () 123456789 + 256 none,0,0 [fio]", true, blockRq{259, 0, "WS", 123456789, 256}},
		// block_rq_complete: no bytes field
		{"259,0 F () 0 + 0 [0]", false, blockRq{259, 0, "F", 0, 0}},
		{"8,32 W () 5000 + 16 none,0,0 [0]", false, blockRq{8, 32, "W", 5000, 16}},
		// a passthrough command with spaces in (cmd)
		{"8,0 R 512 (28 00 00 00 00 08 00 00 01 00) 8 + 1 be,0,4 [sg_dd]", true, blockRq{8, 0, "R", 8, 1}},
		{"8,0 R (28 00 00 00 00 08 00 00 01 00) 8 + 1 [0]", false, blockRq{8, 0, "R", 8, 1}},
	}
	for _, g := range good {
		got, ok := parseBlockRq(g.payload, g.withBytes)
		if !ok || got != g.want {
			t.Errorf("%q: got %+v ok=%v, want %+v", g.payload, got, ok, g.want)
		}
	}
	bad := []struct {
		payload   string
		withBytes bool
	}{
		{"", true}, {"garbage", false}, {"8,16 R", true}, {"8 R 4096 () 0 + 8 [fio]", true},
		{"8,16 R () 0 + 8 [0]", true},            // bytes field missing
		{"8,16 R 4096 () x + 8 [fio]", true},     // sector not a number
		{"8,16 R 4096 () 0 - 8 [fio]", true},     // no " + "
		{"8,16 R 4096 () 0 + [fio]", true},       // nr missing
		{"a,16 R 4096 () 0 + 8 [fio]", true},     // major not a number
		{"8,16 R 4096 (no close 0 + 8", true},    // cmd not closed
		{"8,16 R 4096 0 + 8 [fio]", true},        // no (cmd)
		{"8,16 R 4096 () 0 + 99999999999", true}, // nr overflows uint32
	}
	for _, b := range bad {
		if got, ok := parseBlockRq(b.payload, b.withBytes); ok {
			t.Errorf("%q must not parse: %+v", b.payload, got)
		}
	}
}

func TestParseNVMe(t *testing.T) {
	type setup struct {
		ctrl, disk string
		qid, cmdid int
		op         string
	}
	good := []struct {
		payload string
		want    setup
	}{
		{"nvme0: disk=nvme0n1, qid=1, cmdid=40, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_read slba=0, len=7, ctrl=0x0, dsmgmt=0, reftag=0)",
			setup{"nvme0", "nvme0n1", 1, 40, "nvme_cmd_read"}},
		{"nvme1: disk=nvme1n2, qid=12, cmdid=4127, nsid=2, flags=0x0, meta=0x0, cmd=(nvme_cmd_write slba=8, len=255, ctrl=0x0, dsmgmt=0, reftag=0)",
			setup{"nvme1", "nvme1n2", 12, 4127, "nvme_cmd_write"}},
		{"nvme0: disk=nvme0n1, qid=6, cmdid=832, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_flush cdw10=00 00 00 00)",
			setup{"nvme0", "nvme0n1", 6, 832, "nvme_cmd_flush"}},
		// admin command: no disk= field
		{"nvme0: qid=0, cmdid=12, nsid=0, flags=0x0, meta=0x0, cmd=(nvme_admin_identify cns=1, ctrlid=0)",
			setup{"nvme0", "", 0, 12, "nvme_admin_identify"}},
		{"nvme0: qid=0, cmdid=13, nsid=0, flags=0x0, meta=0x0, cmd=(nvme_admin_keep_alive)",
			setup{"nvme0", "", 0, 13, "nvme_admin_keep_alive"}},
	}
	for _, g := range good {
		ctrl, disk, qid, cmdid, op, ok := parseNVMeSetup(g.payload)
		if got := (setup{ctrl, disk, qid, cmdid, op}); !ok || got != g.want {
			t.Errorf("%q: got %+v ok=%v, want %+v", g.payload, got, ok, g.want)
		}
	}
	for _, bad := range []string{"", "nvme0", "nvme0: disk=nvme0n1, cmdid=1, cmd=(nvme_cmd_read)",
		"nvme0: qid=x, cmdid=1, cmd=(nvme_cmd_read)", "nvme0: qid=1, cmdid=1, nsid=1"} {
		if _, _, _, _, _, ok := parseNVMeSetup(bad); ok {
			t.Errorf("setup %q must not parse", bad)
		}
	}

	done := []struct {
		payload    string
		ctrl       string
		qid, cmdid int
	}{
		{"nvme0: disk=nvme0n1, qid=2, cmdid=40, res=0x0, retries=0, flags=0x0, status=0x0", "nvme0", 2, 40},
		{"nvme3: qid=1, cmdid=7, res=0x0, retries=0, flags=0x0, status=0x0", "nvme3", 1, 7},
	}
	for _, d := range done {
		ctrl, qid, cmdid, ok := parseNVMeDone(d.payload)
		if !ok || ctrl != d.ctrl || qid != d.qid || cmdid != d.cmdid {
			t.Errorf("%q: got %s %d %d ok=%v", d.payload, ctrl, qid, cmdid, ok)
		}
	}
	for _, bad := range []string{"", "nvme0: disk=nvme0n1, res=0x0", "x: qid=1, cmdid=", "nvme0: qid=1, cmdid=z"} {
		if _, _, _, ok := parseNVMeDone(bad); ok {
			t.Errorf("complete %q must not parse", bad)
		}
	}
}

func TestDirOf(t *testing.T) {
	for _, c := range []struct {
		rwbs string
		nr   uint32
		want Dir
	}{
		{"R", 8, DirRead}, {"RA", 8, DirRead}, {"RS", 8, DirRead}, {"RM", 8, DirRead},
		{"W", 8, DirWrite}, {"WS", 8, DirWrite}, {"WFS", 8, DirWrite}, {"FWS", 8, DirWrite}, {"WF", 8, DirWrite},
	} {
		if d, ok := dirOf(c.rwbs, c.nr); !ok || d != c.want {
			t.Errorf("dirOf(%q, %d) = %q, %v; want %q", c.rwbs, c.nr, d, ok, c.want)
		}
	}
	for _, c := range []struct {
		rwbs string
		nr   uint32
	}{{"F", 0}, {"FF", 0}, {"W", 0}, {"R", 0}, {"D", 8}, {"DS", 8}, {"DE", 8}, {"E", 8}, {"N", 8}, {"", 8}, {"S", 8}} {
		if d, ok := dirOf(c.rwbs, c.nr); ok {
			t.Errorf("dirOf(%q, %d) = %q, must be ignored", c.rwbs, c.nr, d)
		}
	}
}

func BenchmarkFeed(b *testing.B) {
	const n = 100_000
	lines := make([]string, 0, 2*n)
	for i := 0; i < n; i++ {
		t := int64(10_000_000_000) + int64(i)*10_000
		sector := uint64(i%64) * 8
		lines = append(lines,
			fmt.Sprintf("fio-1234    [003] ..... %s: block_rq_issue: 8,16 W 4096 () %d + 8 none,0,0 [fio]", secs(t), sector),
			fmt.Sprintf("<idle>-0    [003] d.h1. %s: block_rq_complete: 8,16 W () %d + 8 none,0,0 [0]", secs(t+5_000), sector))
	}
	b.ResetTimer()
	for it := 0; it < b.N; it++ {
		c := newCollector(1000, 0)
		c.addDevice(&device{name: "sdb", source: SourceBlock, maj: 8, min: 16, blockLayer: true})
		for _, l := range lines {
			ev, ok := flushtrace.ParseLine(l)
			if !ok {
				b.Fatal("no parse")
			}
			c.feed(ev, 1_000_000_000)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(lines)), "ns/line")
}

// secs formats a trace-clock time in ns as the kernel prints it.
func secs(ns int64) string { return fmt.Sprintf("%d.%06d", ns/1e9, ns%1e9/1e3) }
