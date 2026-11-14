package iotrace

import (
	"fmt"
	"reflect"
	"testing"

	"bbbench/internal/flushtrace"
)

func fanDevices() []*device {
	return []*device{
		{name: "sdb", source: SourceBlock, maj: 8, min: 16},
		{name: "sdc", source: SourceBlock, maj: 8, min: 32},
		{name: "nvme0n1", source: SourceNVMe, ctrls: []string{"nvme0"}},
		{name: "nvme0n2", source: SourceNVMe, ctrls: []string{"nvme0"}},
		{name: "nvme1n1", source: SourceNVMe, ctrls: []string{"nvme1"}},
		{name: "md0", source: SourceBio, maj: 9, min: 0},
	}
}

func TestFanoutShards(t *testing.T) {
	f := newFanout(fanDevices(), 1000, 0)
	defer f.stop()
	// sdb, sdc, {nvme0n1, nvme0n2}, nvme1n1, md0
	if len(f.shards) != 5 {
		t.Fatalf("%d shards, want 5", len(f.shards))
	}
	if f.byDisk["nvme0n1"] != f.byDisk["nvme0n2"] || f.byDisk["nvme0n1"] == f.byDisk["nvme1n1"] {
		t.Error("namespaces of one controller must share a shard, others not")
	}
	if f.byNum[devNum{8, 16}] == f.byNum[devNum{8, 32}] {
		t.Error("block devices share a shard")
	}
	u := newFanout([]*device{
		{name: "nvme0n1", source: SourceNVMe}, {name: "nvme1n1", source: SourceNVMe},
	}, 1000, 0)
	defer u.stop()
	if len(u.shards) != 1 {
		t.Errorf("namespaces of unknown controllers: %d shards, want 1", len(u.shards))
	}
}

// feedAll runs the same events through a fanout and through one collector.
func TestFanoutMatchesCollector(t *testing.T) {
	var evs []flushtrace.Event
	var starts []int64
	for i := 0; i < 5000; i++ {
		tm := int64(10_000_000_000) + int64(i)*10_000
		start := int64(1_000_000_000)
		if i >= 2500 {
			start = 10_025_000_000 // a second start marker: counting restarts
		}
		for _, min := range []int{16, 32} {
			sector := uint64(i%64) * 8
			for _, l := range []string{
				fmt.Sprintf("fio-1 [003] ..... %s: block_rq_issue: 8,%d W 4096 () %d + 8 none,0,0 [fio]", secs(tm), min, sector),
				fmt.Sprintf("<idle>-0 [003] d.h1. %s: block_rq_complete: 8,%d W () %d + 8 none,0,0 [0]", secs(tm+5_000), min, sector),
			} {
				ev, ok := flushtrace.ParseLine(l)
				if !ok {
					t.Fatal("no parse")
				}
				evs, starts = append(evs, ev), append(starts, start)
			}
		}
	}
	mk := func() []*device {
		return []*device{
			{name: "sdb", source: SourceBlock, maj: 8, min: 16},
			{name: "sdc", source: SourceBlock, maj: 8, min: 32},
			{name: "sdd", source: SourceBlock, maj: 8, min: 48}, // never sees an event
		}
	}
	one := newCollector(1000, 0)
	for _, d := range mk() {
		one.addDevice(d)
	}
	for i, ev := range evs {
		one.feed(ev, starts[i])
	}
	wantW, wantS := one.finish()

	f := newFanout(mk(), 1000, 0)
	for i, ev := range evs {
		f.feed(ev, starts[i])
	}
	f.stop()
	f.feed(evs[0], starts[0]) // after stop: ignored, no panic
	f.stop()
	gotW, gotS := f.finish()
	if !reflect.DeepEqual(gotW, wantW) || !reflect.DeepEqual(gotS, wantS) {
		t.Errorf("fanout differs from one collector: %d windows vs %d, stats %v vs %v", len(gotW), len(wantW), gotS, wantS)
	}
	if len(gotW) == 0 {
		t.Error("no windows")
	}
}
