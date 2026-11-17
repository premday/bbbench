//go:build linux && (amd64 || arm64)

package ebpf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf/btf"
)

func TestObjectHasTheProgramsAndMaps(t *testing.T) {
	spec, err := loadIolat()
	if err != nil {
		t.Fatal(err)
	}
	for name := range progTracepoints {
		if spec.Programs[name] == nil {
			t.Errorf("object lacks program %s", name)
		}
	}
	for _, m := range []string{"devs", "starts", "hists", "stats"} {
		if spec.Maps[m] == nil {
			t.Errorf("object lacks map %s", m)
		}
	}
	if h := spec.Maps["hists"]; h != nil && h.MaxEntries != maxSlots {
		t.Errorf("hists has %d entries, want %d", h.MaxEntries, maxSlots)
	}
}

func tracepointType(params int) btf.Type {
	ps := make([]btf.FuncParam, params)
	for i := range ps {
		ps[i] = btf.FuncParam{Type: &btf.Void{}}
	}
	return &btf.Typedef{Name: "btf_trace_block_bio_complete", Type: &btf.Pointer{Target: &btf.FuncProto{Params: ps, Return: &btf.Void{}}}}
}

func TestHasQueueArg(t *testing.T) {
	if !hasQueueArg(tracepointType(3)) {
		t.Error("(void*, q, bio) must have the queue argument")
	}
	if hasQueueArg(tracepointType(2)) {
		t.Error("(void*, bio) must not")
	}
}

func TestSelectProgramsOnTheRunningKernel(t *testing.T) {
	kspec, err := btf.LoadKernelSpec()
	if err != nil {
		t.Skipf("no kernel BTF: %v", err)
	}
	spec, err := loadIolat()
	if err != nil {
		t.Fatal(err)
	}
	if err := selectPrograms(spec, kspec); err != nil {
		t.Skipf("this kernel lacks a required tracepoint: %v", err)
	}
	bio := 0
	for name := range spec.Programs {
		if progTracepoints[name] == "block_bio_complete" {
			bio++
		}
	}
	if bio != 1 {
		t.Errorf("%d bio_complete variants kept, want exactly 1", bio)
	}
}

// The NVMe tracepoints are in the nvme-core module's BTF: they are kept when
// the module is loaded (or built in) and both go when it is not.
func TestSelectProgramsKeepsTheNVMePairWhenTheModuleHasIt(t *testing.T) {
	kspec, err := btf.LoadKernelSpec()
	if err != nil {
		t.Skipf("no kernel BTF: %v", err)
	}
	var td *btf.Typedef
	have := kspec.TypeByName("btf_trace_nvme_setup_cmd", &td) == nil
	if !have {
		if _, err := os.Stat("/sys/kernel/btf/" + nvmeModule); err != nil {
			t.Skip("no nvme-core BTF on this machine")
		}
	}
	spec, err := loadIolat()
	if err != nil {
		t.Fatal(err)
	}
	if err := selectPrograms(spec, kspec); err != nil {
		t.Skipf("this kernel lacks a required tracepoint: %v", err)
	}
	for _, name := range []string{"nvme_setup", "nvme_done"} {
		if _, ok := spec.Programs[name]; !ok {
			t.Errorf("program %s was dropped although nvme-core's BTF has its tracepoint", name)
		}
	}
}

func TestBTFHintNamesWhyTheFileIsUnusable(t *testing.T) {
	got := btfHint(filepath.Join(t.TempDir(), "vmlinux"))
	if !strings.Contains(got, "no such file") || !strings.Contains(got, "container") {
		t.Errorf("hint for a missing file: %q", got)
	}
	f := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := btfHint(f); !strings.Contains(got, "opens") {
		t.Errorf("hint for a readable file: %q", got)
	}
}
