package flushtrace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"bbbench/internal/tracefake"
)

// rawInstance builds a fake tracefs with format files and the instance
// "test" whose per-CPU pipes hold the events (encoded as pages), plus a
// trace_pipe holding the same events as text.
func rawInstance(t *testing.T, events []string, cpus int, in []tracefake.Event) (root, inst string) {
	t.Helper()
	var lines []string
	for _, e := range in {
		lines = append(lines, e.Line())
	}
	root, inst = fakeTracefs(t, events, strings.Join(lines, "\n")+"\n")
	tr, err := tracefake.WriteTree(root, 4096, events...)
	if err != nil {
		t.Fatal(err)
	}
	pages, err := tr.Pages(in, tracefake.PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tracefake.WritePipes(inst, cpus, pages); err != nil {
		t.Fatal(err)
	}
	return root, inst
}

func TestRawInstance_FeedsInOrderAcrossCPUs(t *testing.T) {
	d := tracefake.Dev(8, 16)
	in := []tracefake.Event{
		{CPU: 2, TimeNs: 1_000_000_000, Name: "print", Text: "bbbench-start"},
		{CPU: 1, TimeNs: 100_000_000_000, Name: "block_rq_issue", Dev: d, Nr: 8, RWBS: "R", Comm: "fio"},
		{CPU: 0, TimeNs: 100_000_300_000, Name: "block_rq_complete", Dev: d, Nr: 8, RWBS: "R"},
		{CPU: 3, Lost: 7},
		{CPU: 3, TimeNs: 100_000_400_000, Name: "block_rq_issue", Dev: d, Sector: 8, Nr: 8, RWBS: "W", Comm: "fio"},
		{CPU: 2, TimeNs: 101_000_000_000, Name: "print", Text: "bbbench-stop"},
	}
	ev := []string{"block/block_rq_issue", "block/block_rq_complete"}
	root, _ := rawInstance(t, ev, 4, in)
	feed, got := collectFeed()
	i, err := OpenInstance(root, "test", 0, enableOf(ev), nil, feed)
	if err != nil {
		t.Fatal(err)
	}
	_ = i.Stop()
	if i.Reader() != ReaderRaw || i.ReaderNote() != "" {
		t.Fatalf("reader %s note %q", i.Reader(), i.ReaderNote())
	}
	evs := got()
	if len(evs) != 3 || evs[0].Name != "block_rq_issue" || evs[1].Name != "block_rq_complete" || evs[2].F.Sector != 8 {
		t.Fatalf("events = %+v", evs)
	}
	if !evs[0].F.Decoded || evs[0].F.Major() != 8 || evs[0].F.Minor() != 16 || evs[0].F.RWBS != "R" {
		t.Errorf("decoded fields %+v", evs[0].F)
	}
	if i.StartNs() != 1_000_000_000 || i.Overrun() != 7 || i.Late() != 0 {
		t.Errorf("StartNs %d overrun %d late %d", i.StartNs(), i.Overrun(), i.Late())
	}
}

// Without per-CPU pipes, or with a format that does not decode, the text
// pipe is read instead and the note says why.
func TestRawInstance_FallsBackToText(t *testing.T) {
	d := tracefake.Dev(8, 16)
	in := []tracefake.Event{
		{TimeNs: 1_000_000_000, Name: "print", Text: "bbbench-start"},
		{TimeNs: 2_000_000_000, Name: "block_rq_issue", Dev: d, Nr: 8, RWBS: "R", Comm: "fio"},
	}
	ev := []string{"block/block_rq_issue"}
	for name, breakIt := range map[string]func(root, inst string){
		"no per_cpu": func(_, inst string) { _ = os.RemoveAll(filepath.Join(inst, "per_cpu")) },
		"bad format": func(root, _ string) {
			_ = os.WriteFile(filepath.Join(root, "events/block/block_rq_issue/format"), []byte("name: block_rq_issue\nID: 1001\n"), 0o644)
		},
		"no header_page": func(root, _ string) { _ = os.Remove(filepath.Join(root, "events/header_page")) },
		"counter clock": func(_, inst string) {
			_ = os.WriteFile(filepath.Join(inst, "trace_clock"), []byte("[counter] x86-tsc\n"), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, inst := rawInstance(t, ev, 2, in)
			breakIt(root, inst)
			feed, got := collectFeed()
			i, err := OpenInstance(root, "test", 0, enableOf(ev), nil, feed)
			if err != nil {
				t.Fatal(err)
			}
			_ = i.Stop()
			if i.Reader() != ReaderText || i.ReaderNote() == "" {
				t.Errorf("reader %s note %q", i.Reader(), i.ReaderNote())
			}
			if evs := got(); len(evs) != 1 || evs[0].F.Decoded || i.StartNs() != 1_000_000_000 {
				t.Errorf("text path events %+v", evs)
			}
		})
	}
}

// The private instance gets a global ns clock when offered, and the raw
// pipes' poll reports any data (buffer_percent 0).
func TestRawInstance_ClockAndBufferPercent(t *testing.T) {
	ev := []string{"block/block_rq_issue"}
	root, inst := rawInstance(t, ev, 1, nil)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(inst, "trace_clock"), []byte("[local] global counter uptime perf mono mono_raw boot tai x86-tsc\n"), 0o644))
	must(os.WriteFile(filepath.Join(inst, "buffer_percent"), []byte("50\n"), 0o644))
	feed, _ := collectFeed()
	i, err := OpenInstance(root, "test", 0, enableOf(ev), nil, feed)
	must(err)
	_ = i.Stop()
	if i.Clock() != "mono" || read(t, filepath.Join(inst, "trace_clock")) != "mono" {
		t.Errorf("clock %q, file %q", i.Clock(), read(t, filepath.Join(inst, "trace_clock")))
	}
	if v := read(t, filepath.Join(inst, "buffer_percent")); v != "0" {
		t.Errorf("buffer_percent %q", v)
	}
	// only global offered
	root, inst = rawInstance(t, ev, 1, nil)
	must(os.WriteFile(filepath.Join(inst, "trace_clock"), []byte("[local] global counter\n"), 0o644))
	i, err = OpenInstance(root, "test", 0, enableOf(ev), nil, feed)
	must(err)
	_ = i.Stop()
	if i.Clock() != "global" || i.Reader() != ReaderRaw {
		t.Errorf("clock %q reader %s", i.Clock(), i.Reader())
	}
}

func openFDs(t *testing.T) int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(ents)
}

// writePage writes one page into a FIFO opened read-write by the test.
func fifo(t *testing.T, path string) *os.File {
	t.Helper()
	_ = os.Remove(path)
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// With pipes that can be polled (FIFOs stand in for trace_pipe_raw), the
// readers park in the netpoller while there is nothing to read, wake up on
// data, and Stop drains and returns promptly; no goroutine or fd is left.
func TestRawInstance_PollablePipes(t *testing.T) {
	d := tracefake.Dev(8, 16)
	ev := []string{"block/block_rq_issue", "block/block_rq_complete"}
	root, inst := rawInstance(t, ev, 2, nil)
	tr, err := tracefake.WriteTree(root, 4096, ev...)
	if err != nil {
		t.Fatal(err)
	}
	g0, fd0 := runtime.NumGoroutine(), openFDs(t)
	w0 := fifo(t, filepath.Join(inst, "per_cpu/cpu0/trace_pipe_raw"))
	w1 := fifo(t, filepath.Join(inst, "per_cpu/cpu1/trace_pipe_raw"))
	feed, got := collectFeed()
	i, err := OpenInstance(root, "test", 0, enableOf(ev), nil, feed)
	if err != nil {
		t.Fatal(err)
	}
	if i.Reader() != ReaderRaw {
		t.Fatalf("reader %s: %s", i.Reader(), i.ReaderNote())
	}
	page := func(w *os.File, e ...tracefake.Event) {
		p, err := tr.Pages(e, tracefake.PageOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, pg := range p[e[0].CPU] {
			if _, err := w.Write(pg); err != nil {
				t.Fatal(err)
			}
		}
	}
	time.Sleep(20 * time.Millisecond) // both readers park on empty pipes
	page(w1, tracefake.Event{CPU: 1, TimeNs: 1000, Name: "block_rq_issue", Dev: d, Nr: 8, RWBS: "R"})
	page(w0, tracefake.Event{CPU: 0, TimeNs: 2000, Name: "block_rq_complete", Dev: d, Nr: 8, RWBS: "R"})
	// both CPUs go idle again: the events leave the merger after the hold
	deadline := time.Now().Add(2 * time.Second)
	for len(got()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if evs := got(); len(evs) != 2 || evs[0].Name != "block_rq_issue" {
		t.Fatalf("live events %+v", evs)
	}
	page(w0, tracefake.Event{CPU: 0, TimeNs: 3000, Name: "block_rq_issue", Dev: d, Sector: 8, Nr: 8, RWBS: "W"})
	start := time.Now()
	_ = i.Stop()
	if el := time.Since(start); el > time.Second {
		t.Errorf("Stop took %v", el)
	}
	if evs := got(); len(evs) != 3 || evs[2].F.Sector != 8 {
		t.Errorf("after the drain %+v", evs)
	}
	_ = w0.Close()
	_ = w1.Close()
	for k := 0; k < 100 && (runtime.NumGoroutine() > g0 || openFDs(t) > fd0); k++ {
		time.Sleep(10 * time.Millisecond)
	}
	if g, fd := runtime.NumGoroutine(), openFDs(t); g > g0 || fd > fd0 {
		t.Errorf("goroutines %d -> %d, fds %d -> %d", g0, g, fd0, fd)
	}
}

// Close (a force quit) wakes readers that would otherwise wait, and leaves
// nothing behind either.
func TestRawInstance_CloseLeavesNothing(t *testing.T) {
	ev := []string{"block/block_rq_issue"}
	root, inst := rawInstance(t, ev, 1, nil)
	g0, fd0 := runtime.NumGoroutine(), openFDs(t)
	w := fifo(t, filepath.Join(inst, "per_cpu/cpu0/trace_pipe_raw"))
	feed, _ := collectFeed()
	i, err := OpenInstance(root, "test", 0, enableOf(ev), nil, feed)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	_ = i.Close()
	_ = i.Stop() // idempotent
	_ = w.Close()
	for k := 0; k < 100 && (runtime.NumGoroutine() > g0 || openFDs(t) > fd0); k++ {
		time.Sleep(10 * time.Millisecond)
	}
	if g, fd := runtime.NumGoroutine(), openFDs(t); g > g0 || fd > fd0 {
		t.Errorf("goroutines %d -> %d, fds %d -> %d", g0, g, fd0, fd)
	}
}
