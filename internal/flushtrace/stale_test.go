package flushtrace

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeProc makes procRoot a directory where only self and the given pids run.
func fakeProc(t *testing.T, pids ...int) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range append([]string{"self"}, itoas(pids)...) {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = old })
}

func itoas(ns []int) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return out
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Instances of dead bbbench processes go; those of a running process, of
// this process and of anything else stay.
func TestRemoveStale(t *testing.T) {
	fakeProc(t, 4242) // 4343 is not running
	root := t.TempDir()
	in := func(n string) string { return filepath.Join(root, "instances", n) }
	for _, n := range []string{
		"bbbench-iolat-4343-1", "bbbench-flush-4343-7", // dead: removed
		"bbbench-iolat-4242-1",                                 // running
		"bbbench-flush-" + strconv.Itoa(os.Getpid()) + "-1",    // this process
		"bbbench-other-4343-1", "bbbench-iolat-x-1", "someone", // not ours
	} {
		mkdir(t, in(n))
	}
	// A real tracefs instance has its control files; the fake keeps them,
	// so this one cannot be removed, but its events are switched off.
	busy := in("bbbench-iolat-4343-2")
	mkdir(t, filepath.Join(busy, "events"))
	for _, f := range []string{"tracing_on", "events/enable"} {
		if err := os.WriteFile(filepath.Join(busy, f), []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removed := RemoveStale(root)
	if len(removed) != 2 {
		t.Errorf("removed = %v", removed)
	}
	for _, n := range []string{"bbbench-iolat-4343-1", "bbbench-flush-4343-7"} {
		if exists(in(n)) {
			t.Errorf("%s not removed", n)
		}
	}
	for _, n := range []string{"bbbench-iolat-4242-1", "bbbench-flush-" + strconv.Itoa(os.Getpid()) + "-1",
		"bbbench-other-4343-1", "bbbench-iolat-x-1", "someone"} {
		if !exists(in(n)) {
			t.Errorf("%s removed", n)
		}
	}
	for _, f := range []string{"tracing_on", "events/enable"} {
		if v := read(t, filepath.Join(busy, f)); v != "0" {
			t.Errorf("%s = %q, want 0", f, v)
		}
	}
}

// Without a readable /proc nothing can be proven dead: nothing is touched.
func TestRemoveStale_NoProc(t *testing.T) {
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { procRoot = old })
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "instances", "bbbench-iolat-4343-1"))
	if removed := RemoveStale(root); len(removed) != 0 || !exists(filepath.Join(root, "instances", "bbbench-iolat-4343-1")) {
		t.Errorf("removed = %v", removed)
	}
}

// Opening an instance clears the stale ones first.
func TestOpenInstance_RemovesStale(t *testing.T) {
	fakeProc(t)
	root, _ := fakeTracefs(t, []string{"block/block_rq_issue"}, "")
	stale := filepath.Join(root, "instances", "bbbench-flush-4343-1")
	mkdir(t, stale)
	feed, _ := collectFeed()
	i, err := OpenInstance(root, "test", 0, []string{"events/block/block_rq_issue"}, nil, feed)
	if err != nil {
		t.Fatal(err)
	}
	_ = i.Stop()
	if exists(stale) {
		t.Error("stale instance kept")
	}
}

// The last start marker is time zero (MarkStart after a start gate).
func TestInstance_MarkStartMovesTimeZero(t *testing.T) {
	pipe := "bbbench-1 [000] ..... 1.000000: tracing_mark_write: bbbench-start\n" +
		"bbbench-1 [000] ..... 1.250000: tracing_mark_write: bbbench-start\n"
	root, inst := fakeTracefs(t, []string{"block/block_rq_issue"}, pipe)
	feed, _ := collectFeed()
	i, err := OpenInstance(root, "test", 0, []string{"events/block/block_rq_issue"}, nil, feed)
	if err != nil {
		t.Fatal(err)
	}
	i.MarkStart()
	if v := read(t, filepath.Join(inst, "trace_marker")); v != "bbbench-start" {
		t.Errorf("trace_marker = %q", v)
	}
	_ = i.Stop()
	if i.StartNs() != 1_250_000_000 {
		t.Errorf("StartNs = %d, want the last marker", i.StartNs())
	}
}

// Close (force quit) and Stop end the instance once; the second call returns
// the first one's result.
func TestInstance_CloseThenStop(t *testing.T) {
	root, inst := fakeTracefs(t, []string{"block/block_rq_issue"}, "")
	feed, _ := collectFeed()
	i, err := OpenInstance(root, "test", 0, []string{"events/block/block_rq_issue"}, nil, feed)
	if err != nil {
		t.Fatal(err)
	}
	e1 := i.Close()
	if v := read(t, filepath.Join(inst, "tracing_on")); v != "0" {
		t.Errorf("tracing_on = %q after Close", v)
	}
	if v := read(t, filepath.Join(inst, "events/block/block_rq_issue/enable")); v != "0" {
		t.Errorf("enable = %q after Close", v)
	}
	if e2 := i.Stop(); e2 != e1 {
		t.Errorf("Stop after Close = %v, want %v", e2, e1)
	}
}

// FindStale lists the same instances RemoveStale would remove, and changes
// nothing.
func TestFindStale(t *testing.T) {
	fakeProc(t, 4242)
	root := t.TempDir()
	in := func(n string) string { return filepath.Join(root, "instances", n) }
	for _, n := range []string{"bbbench-iolat-4343-1", "bbbench-iolat-4242-1", "someone"} {
		mkdir(t, in(n))
	}
	if got := FindStale(root); len(got) != 1 || got[0] != "bbbench-iolat-4343-1" {
		t.Errorf("FindStale = %v", got)
	}
	if !exists(in("bbbench-iolat-4343-1")) {
		t.Error("FindStale removed an instance")
	}
}
