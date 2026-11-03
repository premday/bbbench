package flushtrace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestStart_Unsupported(t *testing.T) {
	if _, err := Start("sda", Options{TracefsRoot: t.TempDir()}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("no tracefs: err = %v", err)
	}
	root, _ := fakeTracefs(t, nil, "")
	if _, err := Start("sda", Options{TracefsRoot: root}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("no tracepoints: err = %v", err)
	}
}
