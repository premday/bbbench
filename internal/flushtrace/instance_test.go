package flushtrace

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func collectFeed() (func(Event), func() []Event) {
	var mu sync.Mutex
	var evs []Event
	return func(e Event) { mu.Lock(); evs = append(evs, e); mu.Unlock() },
		func() []Event { mu.Lock(); defer mu.Unlock(); return append([]Event(nil), evs...) }
}

func TestInstance_FeedsEventsNotMarkers(t *testing.T) {
	pipe := "bbbench-1 [000] ..... 1.000000: tracing_mark_write: bbbench-start\n" +
		"fio-1 [001] ..... 100.000000: block_rq_issue: 8,16 R 4096 () 0 + 8 [fio]\n" +
		"<idle>-0 [001] d.h1. 100.000300: block_rq_complete: 8,16 R () 0 + 8 [0]\n" +
		"bbbench-1 [000] ..... 101.000000: tracing_mark_write: bbbench-stop\n"
	root, inst := fakeTracefs(t, []string{"block/block_rq_issue"}, pipe)
	feed, got := collectFeed()
	i, err := OpenInstance(root, "test", 4096, []string{"events/block/block_rq_issue"},
		map[string]string{"events/block/block_rq_issue": "nr_sector > 0"}, feed)
	if err != nil {
		t.Fatal(err)
	}
	if v := read(t, filepath.Join(inst, "buffer_size_kb")); v != "4096" {
		t.Errorf("buffer_size_kb = %q", v)
	}
	if v := read(t, filepath.Join(inst, "events/block/block_rq_issue/filter")); v != "nr_sector > 0" {
		t.Errorf("filter = %q", v)
	}
	_ = i.Stop() // the fake instance directory is not empty: a cleanup error is expected
	evs := got()
	if len(evs) != 2 || evs[0].Name != "block_rq_issue" || evs[1].Name != "block_rq_complete" {
		t.Fatalf("events = %+v", evs)
	}
	if i.StartNs() != 1_000_000_000 {
		t.Errorf("StartNs = %d", i.StartNs())
	}
	if i.Overrun() != 0 {
		t.Errorf("Overrun = %d", i.Overrun())
	}
}

func TestInstance_CountsLostEvents(t *testing.T) {
	pipe := "CPU:2 [LOST 120 EVENTS]\n" +
		"fio-1 [001] ..... 100.000000: block_rq_issue: 8,16 R 4096 () 0 + 8 [fio]\n" +
		"CPU:0 [LOST 3 EVENTS]\n"
	root, _ := fakeTracefs(t, []string{"block/block_rq_issue"}, pipe)
	feed, got := collectFeed()
	i, err := OpenInstance(root, "test", 0, []string{"events/block/block_rq_issue"}, nil, feed)
	if err != nil {
		t.Fatal(err)
	}
	_ = i.Stop()
	if i.Overrun() != 123 {
		t.Errorf("Overrun = %d, want 123", i.Overrun())
	}
	if n := len(got()); n != 1 {
		t.Errorf("%d events, want 1", n)
	}
}

func TestInstance_CleansUpOnEnableFailure(t *testing.T) {
	root := t.TempDir()
	inst := filepath.Join(root, "instances", "bad")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// The first event enables fine; the second cannot: its enable path is a directory.
	must(os.MkdirAll(filepath.Join(inst, "events", "x"), 0o755))
	must(os.MkdirAll(filepath.Join(inst, "events", "y", "enable"), 0o755))
	must(os.WriteFile(filepath.Join(inst, "trace_pipe"), nil, 0o644))
	feed, _ := collectFeed()
	_, err := OpenInstance(root, "bad", 0, []string{"events/x", "events/y"}, nil, feed)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "enable events/y") {
		t.Errorf("error does not name the failure: %v", err)
	}
	if got := read(t, filepath.Join(inst, "events/x/enable")); got != "0" {
		t.Errorf("first event left enabled: %q", got)
	}
	// The fake instance directory is not empty, so the removal fails; the
	// error must say so instead of dropping it.
	if !strings.Contains(err.Error(), "remove trace instance") {
		t.Errorf("cleanup error not reported: %v", err)
	}
}

func TestInstance_BlockOnlyTracefs(t *testing.T) {
	pipe := "fio-1 [001] ..... 100.000000: block_rq_issue: 8,0 F 0 () 0 + 0 none,0,0 [fio]\n" +
		"<idle>-0 [001] d.h1. 100.000900: block_rq_complete: 8,0 F () 0 + 0 [0]\n" +
		"fio-1 [001] ..... 101.000000: block_rq_issue: 8,16 F 0 () 0 + 0 none,0,0 [fio]\n" +
		"<idle>-0 [001] d.h1. 101.000300: block_rq_complete: 8,16 F () 0 + 0 [0]\n"
	root, _ := fakeTracefs(t, []string{"block/block_rq_issue", "block/block_rq_complete"}, pipe)
	if !HasEvents(root, "block/block_rq_issue", "block/block_rq_complete") {
		t.Fatal("HasEvents false")
	}
	if HasEvents(root, "block/block_rq_issue", "nvme/nvme_setup_cmd") || HasEvents(root, "block/block_rq_insert") {
		t.Fatal("HasEvents true for missing events")
	}
	sys := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(sys, "sdb"), 0o755))
	must(os.WriteFile(filepath.Join(sys, "sdb", "dev"), []byte("8:16\n"), 0o644))
	maj, min, err := DevNumbers(sys, "sdb")
	if err != nil || maj != 8 || min != 16 {
		t.Fatalf("DevNumbers = %d,%d,%v", maj, min, err)
	}
	if _, _, err := DevNumbers(sys, "nope"); err == nil {
		t.Error("DevNumbers of a missing device must fail")
	}
	// No nvme directory exists anywhere: Start takes the block path.
	s, err := Start("sdb", Options{TracefsRoot: root, SysBlock: sys, InstanceName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := s.Stop()
	if sum.Source != "block" || sum.Count != 1 || sum.MeanNs != 300_000 {
		t.Errorf("only 8,16 must count: %+v", sum)
	}
}

func TestInstance_CleanupErrorIsReportedOnStop(t *testing.T) {
	root, _ := fakeTracefs(t, []string{"block/block_rq_issue"}, "")
	feed, _ := collectFeed()
	i, err := OpenInstance(root, "test", 0, []string{"events/block/block_rq_issue"}, nil, feed)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.Stop(); err == nil || !strings.Contains(err.Error(), "remove trace instance") {
		t.Errorf("Stop error = %v", err)
	}
}

func TestInstanceName(t *testing.T) {
	a, b := InstanceName("iolat"), InstanceName("iolat")
	if a == b || !strings.HasPrefix(a, "bbbench-iolat-") {
		t.Errorf("names %q %q", a, b)
	}
}
