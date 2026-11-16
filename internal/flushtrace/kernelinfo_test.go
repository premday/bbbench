package flushtrace

import (
	"os"
	"path/filepath"
	"testing"
)

const cpuStatsText = `entries: 12
overrun: 3400
commit overrun: 1
bytes: 4096
oldest event ts:  5.1
now ts:  9.9
dropped events: 7
read events: 5000
`

func TestParseCPUStats(t *testing.T) {
	c := parseCPUStats(2, cpuStatsText)
	want := CPUStats{CPU: 2, Entries: 12, Overrun: 3400, CommitOverrun: 1, Bytes: 4096, Dropped: 7, Read: 5000}
	if c != want {
		t.Errorf("got %+v, want %+v", c, want)
	}
}

func TestInstance_RecordsKernelView(t *testing.T) {
	root, inst := fakeTracefs(t, []string{"block/block_bio_queue", "block/block_bio_complete"}, "")
	for cpu, text := range map[string]string{"cpu0": cpuStatsText, "cpu1": "overrun: 5\nread events: 6\n"} {
		d := filepath.Join(inst, "per_cpu", cpu)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "stats"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// the kernel rejects the filter of one event: the file cannot be written
	bad := filepath.Join(inst, "events", "block", "block_bio_complete", "filter")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	feed, _ := collectFeed()
	i, err := OpenInstance(root, "test", 0,
		[]string{"events/block/block_bio_queue", "events/block/block_bio_complete"},
		map[string]string{"events/block/block_bio_queue": "dev == 9", "events/block/block_bio_complete": "dev == 9"}, feed)
	if err != nil {
		t.Fatal(err)
	}
	_ = i.Stop()
	k := i.Kernel()
	if k == nil {
		t.Fatal("no kernel view")
	}
	if len(k.CPUs) != 2 || k.CPUs[0].Overrun != 3400 || k.Overrun() != 3405 || k.Read() != 5006 {
		t.Errorf("cpus %+v overrun %d read %d", k.CPUs, k.Overrun(), k.Read())
	}
	if i.Overrun() != 3405 {
		t.Errorf("Overrun = %d, want the kernel's 3405", i.Overrun())
	}
	if len(k.Filters) != 2 {
		t.Fatalf("filters %+v", k.Filters)
	}
	if k.Filters[0].NotApplied() || k.Filters[0].Applied != "dev == 9" {
		t.Errorf("queue filter %+v", k.Filters[0])
	}
	if !k.Filters[1].NotApplied() || k.Filters[1].Err == "" {
		t.Errorf("rejected filter %+v", k.Filters[1])
	}
	if u := k.Unfiltered(); len(u) != 1 || u[0].Event != "events/block/block_bio_complete" {
		t.Errorf("unfiltered %+v", u)
	}
}

func TestFilterState_NotApplied(t *testing.T) {
	for f, want := range map[FilterState]bool{
		{Wanted: "dev == 1", Applied: "dev == 1"}:             false,
		{Wanted: "dev == 1", Applied: "none"}:                 true,
		{Wanted: "dev == 1", Applied: ""}:                     true,
		{Wanted: "dev == 1", Applied: "dev == 1\nError: bad"}: true,
		{Wanted: "", Applied: "none"}:                         false,
	} {
		if got := f.NotApplied(); got != want {
			t.Errorf("%+v: %v, want %v", f, got, want)
		}
	}
}
