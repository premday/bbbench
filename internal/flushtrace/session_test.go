package flushtrace

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeTracefs builds a minimal tracefs tree with a pre-created instance
// whose trace_pipe already holds the given lines.
func fakeTracefs(t *testing.T, events []string, pipe string) (root, inst string) {
	t.Helper()
	root = t.TempDir()
	inst = filepath.Join(root, "instances", "test")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(inst, 0o755))
	for _, e := range events { // e.g. "nvme/nvme_setup_cmd"
		must(os.MkdirAll(filepath.Join(root, "events", e), 0o755))
		must(os.MkdirAll(filepath.Join(inst, "events", e), 0o755))
	}
	must(os.WriteFile(filepath.Join(inst, "trace_pipe"), []byte(pipe), 0o644))
	return root, inst
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestSession_NVMe(t *testing.T) {
	pipe := "bbbench-1 [000] ..... 1.000000: tracing_mark_write: bbbench-start\n" +
		"fio-1 [001] ..... 100.000000: nvme_setup_cmd: nvme0: disk=nvme0n1, qid=1, cmdid=5, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_flush cdw10=00000000)\n" +
		"<idle>-0 [001] d.h1. 100.000195: nvme_complete_rq: nvme0: disk=nvme0n1, qid=1, cmdid=5, res=0x0, retries=0, flags=0x0, status=0x0\n"
	root, inst := fakeTracefs(t, []string{"nvme/nvme_setup_cmd", "nvme/nvme_complete_rq"}, pipe)

	s, err := Start("nvme0n1", Options{TracefsRoot: root, InstanceName: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := read(t, filepath.Join(inst, "tracing_on")); got != "1" {
		t.Errorf("tracing_on = %q while running", got)
	}
	if got := read(t, filepath.Join(inst, "events/nvme/nvme_setup_cmd/filter")); !strings.Contains(got, "opcode == 0") {
		t.Errorf("setup_cmd filter = %q", got)
	}
	sum, _ := s.Stop() // instance dir is not empty in the fake, so a cleanup error is expected
	if sum.Source != "nvme" || sum.Count != 1 || sum.MeanNs != 195_000 || !sum.Exact {
		t.Errorf("summary = %+v", sum)
	}
	// Sample times are relative to the start marker (t=1.0s), not the raw clock.
	got := s.Samples()
	if len(got) != 1 || got[0].LatencyNs != 195_000 || got[0].TimeNs != 99_000_195_000 {
		t.Errorf("samples = %+v", got)
	}
	if got := read(t, filepath.Join(inst, "tracing_on")); got != "0" {
		t.Errorf("tracing_on = %q after Stop", got)
	}
	if got := read(t, filepath.Join(inst, "events/nvme/nvme_complete_rq/enable")); got != "0" {
		t.Errorf("event left enabled: %q", got)
	}
}

func TestSession_BlockDeviceUsesDevNumbers(t *testing.T) {
	pipe := "fio-1 [000] ..... 10.000000: block_rq_issue: 8,16 F 0 () 0 + 0 none,0,0 [fio]\n" +
		"<idle>-0 [000] d.h1. 10.000300: block_rq_complete: 8,16 F () 0 + 0 [0]\n"
	root, _ := fakeTracefs(t, []string{"block/block_rq_issue", "block/block_rq_complete"}, pipe)
	sysBlock := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysBlock, "sdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysBlock, "sdb", "dev"), []byte("8:16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Start("sdb", Options{TracefsRoot: root, SysBlock: sysBlock, InstanceName: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sum, _ := s.Stop()
	if sum.Source != "block" || sum.Count != 1 || sum.MeanNs != 300_000 || sum.Exact {
		t.Errorf("summary = %+v", sum)
	}
}

// blockSession starts a session on name (sysfs dev = devnum) with the block
// events present and the given trace_pipe, and returns it with the instance.
func blockSession(t *testing.T, name, devnum, pipe string) (*Session, string) {
	t.Helper()
	root, inst := fakeTracefs(t, []string{"block/block_rq_issue", "block/block_rq_complete"}, pipe)
	sysBlock := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysBlock, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysBlock, name, "dev"), []byte(devnum+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Start(name, Options{TracefsRoot: root, SysBlock: sysBlock, InstanceName: "test"})
	if err != nil {
		t.Fatalf("Start %s: %v", name, err)
	}
	return s, inst
}

// An md array or a dm device (an LV) takes the generic Block path, and the
// collector keeps only the events of its own dev numbers.
func checkBlockSession(t *testing.T, name, devnum, mm string) {
	t.Helper()
	pipe := "fio-1 [000] ..... 9.000000: block_rq_issue: 8,16 F 0 () 0 + 0 none,0,0 [fio]\n" +
		"<idle>-0 [000] d.h1. 9.000900: block_rq_complete: 8,16 F () 0 + 0 [0]\n" +
		"fio-1 [000] ..... 10.000000: block_rq_issue: " + mm + " F 0 () 0 + 0 none,0,0 [fio]\n" +
		"<idle>-0 [000] d.h1. 10.000300: block_rq_complete: " + mm + " F () 0 + 0 [0]\n"
	s, inst := blockSession(t, name, devnum, pipe)
	for _, e := range []string{"block_rq_issue", "block_rq_complete"} {
		if got := read(t, filepath.Join(inst, "events/block", e, "enable")); got != "1" {
			t.Errorf("%s: %s enable = %q while running", name, e, got)
		}
		if got := read(t, filepath.Join(inst, "events/block", e, "filter")); !strings.Contains(got, `rwbs ~ "F*"`) {
			t.Errorf("%s: %s filter = %q", name, e, got)
		}
	}
	sum, _ := s.Stop()
	if sum.Source != "block" || sum.Count != 1 || sum.MeanNs != 300_000 || sum.Exact {
		t.Errorf("%s: summary = %+v", name, sum)
	}
}

func TestSession_MdUsesBlockCollector(t *testing.T) { checkBlockSession(t, "md127", "9:127", "9,127") }

func TestSession_DmUsesBlockCollector(t *testing.T) { checkBlockSession(t, "dm-3", "253:3", "253,3") }

// UNVERIFIED on hardware: md and dm are bio-based, so the request events may
// never fire for them. The session then ends with a block summary of 0
// Flushes (the phase shows the "ftrace saw 0 Flush commands" note).
func TestSession_MdNoRequestEventsGivesZeroFlushes(t *testing.T) {
	s, _ := blockSession(t, "md127", "9:127", "")
	sum, _ := s.Stop()
	if sum.Source != "block" || sum.Count != 0 {
		t.Errorf("summary = %+v", sum)
	}
}

func TestSession_NVMeFallsBackToBlockWithoutNVMeTracepoints(t *testing.T) {
	root, _ := fakeTracefs(t, []string{"block/block_rq_issue", "block/block_rq_complete"}, "")
	sysBlock := t.TempDir()
	_ = os.MkdirAll(filepath.Join(sysBlock, "nvme0n1"), 0o755)
	_ = os.WriteFile(filepath.Join(sysBlock, "nvme0n1", "dev"), []byte("259:0\n"), 0o644)
	s, err := Start("nvme0n1", Options{TracefsRoot: root, SysBlock: sysBlock, InstanceName: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sum, _ := s.Stop(); sum.Source != "block" {
		t.Errorf("want block fallback, got %q", sum.Source)
	}
}

// The per-CPU ring buffer is Options.BufferKB, 8192 KB by default.
func TestStart_BufferKB(t *testing.T) {
	for _, c := range []struct{ opt, want int }{{0, 8192}, {2048, 2048}} {
		root, inst := fakeTracefs(t, []string{"nvme/nvme_setup_cmd", "nvme/nvme_complete_rq"}, "")
		s, err := Start("nvme0n1", Options{TracefsRoot: root, InstanceName: "test", BufferKB: c.opt})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, _ = s.Stop()
		if got := read(t, filepath.Join(inst, "buffer_size_kb")); got != strconv.Itoa(c.want) {
			t.Errorf("BufferKB %d: buffer_size_kb = %q, want %d", c.opt, got, c.want)
		}
	}
}

func TestStart_Unsupported(t *testing.T) {
	_, err := Start("sda", Options{TracefsRoot: t.TempDir()})
	if !errors.Is(err, ErrUnsupported) || !errors.Is(err, ErrNoTracefs) ||
		err.Error() != "flush tracing unsupported: tracefs not found (is it mounted, and are you root?)" {
		t.Errorf("no tracefs: err = %v", err)
	}
	// FindTracefs itself names no feature: each package wraps it once.
	if _, err := FindTracefs(Options{TracefsRoot: t.TempDir()}); err != ErrNoTracefs || errors.Is(err, ErrUnsupported) {
		t.Errorf("FindTracefs: err = %v", err)
	}
	root, _ := fakeTracefs(t, nil, "")
	if _, err := Start("sda", Options{TracefsRoot: root}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("no tracepoints: err = %v", err)
	}
}

// A real trace_pipe never reports EOF when tracing stops: a read blocks until
// new data arrives. Stop must therefore end the reader with the stop marker,
// not by waiting for EOF (it used to time out with "trace reader did not
// finish" on a real kernel).
func TestSession_StopDoesNotNeedEOF(t *testing.T) {
	root, inst := fakeTracefs(t, []string{"nvme/nvme_setup_cmd", "nvme/nvme_complete_rq"}, "")
	pipePath := filepath.Join(inst, "trace_pipe")
	if err := os.Remove(pipePath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(pipePath, 0o644); err != nil {
		t.Fatal(err)
	}
	// Read-write open: the fifo never reports EOF, like the kernel's pipe.
	kernel, err := os.OpenFile(pipePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer kernel.Close()
	write := func(line string) { kernel.WriteString(line + "\n") }
	write("bbbench-1 [000] ..... 1.000000: tracing_mark_write: bbbench-start")
	write("fio-1 [001] ..... 100.000000: nvme_setup_cmd: nvme0: disk=nvme0n1, qid=1, cmdid=5, nsid=1, flags=0x0, meta=0x0, cmd=(nvme_cmd_flush cdw10=00000000)")
	write("<idle>-0 [001] d.h1. 100.000195: nvme_complete_rq: nvme0: disk=nvme0n1, qid=1, cmdid=5, res=0x0, retries=0, flags=0x0, status=0x0")

	// Stand-in for the kernel: a write to trace_marker shows up in the pipe.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if b, err := os.ReadFile(filepath.Join(inst, "trace_marker")); err == nil &&
				strings.Contains(string(b), stopMarker) {
				write("bbbench-1 [000] ..... 400.000000: tracing_mark_write: " + stopMarker)
				return
			}
		}
	}()

	s, err := Start("nvme0n1", Options{TracefsRoot: root, InstanceName: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the reader consume the events
	began := time.Now()
	sum, err := s.Stop()
	if err != nil && strings.Contains(err.Error(), "reader did not finish") {
		t.Fatalf("Stop timed out waiting for EOF: %v", err)
	}
	if d := time.Since(began); d > time.Second {
		t.Errorf("Stop took %v", d)
	}
	if sum.Count != 1 || sum.MeanNs != 195_000 {
		t.Errorf("summary = %+v", sum)
	}
}

// bioSession starts a session on a bio-based node (queue/ without mq/) with
// the request and the bio events present.
func bioSession(t *testing.T, name, devnum, pipe string) (*Session, string) {
	t.Helper()
	root, inst := fakeTracefs(t, []string{"block/block_rq_issue", "block/block_rq_complete",
		"block/block_bio_queue", "block/block_bio_complete"}, pipe)
	sysBlock := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysBlock, name, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysBlock, name, "dev"), []byte(devnum+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Start(name, Options{TracefsRoot: root, SysBlock: sysBlock, InstanceName: "test"})
	if err != nil {
		t.Fatalf("Start %s: %v", name, err)
	}
	return s, inst
}

// A bio-based md or dm node has no request events: its own Flushes are the
// empty flush bios submitted to it (rwbs F..., 0 sectors), timed from
// block_bio_queue to block_bio_complete; its members' flushes are included.
func TestSession_BioNodeFlush(t *testing.T) {
	for _, c := range []struct{ name, devnum, mm string }{{"md127", "9:127", "9,127"}, {"dm-3", "253:3", "253,3"}} {
		pipe := "fio-1 [000] ..... 10.000000: block_bio_queue: " + c.mm + " FWS 0 + 0 [fio]\n" +
			"fio-1 [000] ..... 10.000010: block_bio_queue: 8,16 FWS 0 + 0 [md127_raid1]\n" + // a member's flush bio
			"<idle>-0 [000] d.h1. 10.000200: block_bio_complete: 8,16 FWS 0 + 0 [0]\n" +
			"fio-1 [000] ..... 10.000300: block_bio_queue: " + c.mm + " WS 2048 + 8 [fio]\n" + // a write: not a Flush
			"<idle>-0 [000] d.h1. 10.000400: block_bio_complete: " + c.mm + " WS 2048 + 8 [0]\n" +
			"<idle>-0 [000] d.h1. 10.000500: block_bio_complete: " + c.mm + " FWS 0 + 0 [0]\n" +
			"fio-1 [000] ..... 11.000000: block_bio_queue: " + c.mm + " FWS 0 + 0 [fio]\n"
		s, inst := bioSession(t, c.name, c.devnum, pipe)
		maj, min, _ := strings.Cut(c.devnum, ":")
		mj, _ := strconv.Atoi(maj)
		mn, _ := strconv.Atoi(min)
		for _, e := range []string{"block_bio_queue", "block_bio_complete"} {
			if got := read(t, filepath.Join(inst, "events/block", e, "enable")); got != "1" {
				t.Errorf("%s: %s enable = %q while running", c.name, e, got)
			}
			want := "dev == " + strconv.Itoa(mj<<20|mn) + ` && rwbs ~ "F*" && nr_sector == 0`
			if got := read(t, filepath.Join(inst, "events/block", e, "filter")); got != want {
				t.Errorf("%s: %s filter = %q, want %q", c.name, e, got, want)
			}
		}
		if _, err := os.Stat(filepath.Join(inst, "events/block/block_rq_issue/enable")); err == nil {
			t.Errorf("%s: the request events were enabled on a bio-based node", c.name)
		}
		sum, _ := s.Stop()
		if sum.Source != "bio" || sum.Count != 1 || sum.MeanNs != 500_000 || sum.Exact || sum.Lost != 1 {
			t.Errorf("%s: summary = %+v", c.name, sum)
		}
		if strings.Join(sum.Events, "|") != "block_bio_queue -> block_bio_complete" {
			t.Errorf("%s: events = %q", c.name, sum.Events)
		}
		if got, want := SourceLine(&sum), "ftrace: block_bio_queue -> block_bio_complete (Flush bios of the node, its members' flushes included) · pairing: first-in first-out · lost events: 1"; got != want {
			t.Errorf("%s: source line = %q\nwant %q", c.name, got, want)
		}
	}
}

// Without the bio tracepoints a bio-based node takes the request path as
// before (which sees no Flush).
func TestSession_BioNodeWithoutBioTracepoints(t *testing.T) {
	root, _ := fakeTracefs(t, []string{"block/block_rq_issue", "block/block_rq_complete"}, "")
	sysBlock := t.TempDir()
	_ = os.MkdirAll(filepath.Join(sysBlock, "md127", "queue"), 0o755)
	_ = os.WriteFile(filepath.Join(sysBlock, "md127", "dev"), []byte("9:127\n"), 0o644)
	s, err := Start("md127", Options{TracefsRoot: root, SysBlock: sysBlock, InstanceName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if sum, _ := s.Stop(); sum.Source != "block" {
		t.Errorf("source = %q", sum.Source)
	}
}
