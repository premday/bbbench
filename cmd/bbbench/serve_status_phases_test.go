package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const phasesTestFio = `[global]
ioengine=libaio
iodepth=128
direct=1

[seq_write]
rw=write
bs=1M
runtime=60
time_based

[rand_write]
stonewall
rw=randwrite
bs=4k
iodepth=1
fsync=1
runtime=300
`

func phasesTestWorkload(t *testing.T) *FioWorkload {
	t.Helper()
	path := t.TempDir() + "/w.fio"
	if err := os.WriteFile(path, []byte(phasesTestFio), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := parseFioFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPhaseConfigs_SummaryAndConfig(t *testing.T) {
	got := phaseConfigs(phasesTestWorkload(t))
	if len(got) != 2 {
		t.Fatalf("got %d phases, want 2", len(got))
	}
	if got[0].Name != "seq_write" || got[1].Name != "rand_write" {
		t.Errorf("names = %q, %q", got[0].Name, got[1].Name)
	}
	// Job options override [global]; unset ones fall back to it.
	want := "rw=randwrite bs=4k iodepth=1 ioengine=libaio direct=1 fsync=1 runtime=300"
	if got[1].Summary != want {
		t.Errorf("summary = %q, want %q", got[1].Summary, want)
	}
	if !strings.Contains(got[0].Summary, "time_based=1") {
		t.Errorf("flag option missing from %q", got[0].Summary)
	}
	for i, p := range got {
		if !strings.Contains(p.Config, "[global]") || !strings.Contains(p.Config, "["+p.Name+"]") {
			t.Errorf("phase %d config lacks global or job section:\n%s", i, p.Config)
		}
		if p.Index != i {
			t.Errorf("index = %d, want %d", p.Index, i)
		}
	}
	if strings.Contains(got[0].Config, "rand_write") {
		t.Error("phase 0 config contains another phase's job")
	}
}

func TestStripFioComments(t *testing.T) {
	got := stripFioComments("; a comment\n[global]\n# other\n\nbs=4k\n  ; indented\n")
	if got != "[global]\nbs=4k" {
		t.Errorf("got %q", got)
	}
}

func TestPhaseConfigs_NilWorkload(t *testing.T) {
	if got := phaseConfigs(nil); got != nil {
		t.Errorf("phaseConfigs(nil) = %v", got)
	}
}

func TestHandleStatusPhases(t *testing.T) {
	ResetStatus()
	UpdateStatus(func(s *BenchmarkStatus) {
		s.phaseConfigs = map[string][]PhaseConfig{"nvme0n1": phaseConfigs(phasesTestWorkload(t))}
	})
	defer ResetStatus()

	w := httptest.NewRecorder()
	handleStatusPhases(w, httptest.NewRequest("GET", "/api/status/phases", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string][]PhaseConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got["nvme0n1"]) != 2 {
		t.Errorf("got %v", got)
	}

	// The 2-second status poll must not carry the configs.
	w = httptest.NewRecorder()
	handleStatus(w, httptest.NewRequest("GET", "/api/status", nil))
	if strings.Contains(w.Body.String(), "rand_write") {
		t.Error("/api/status leaks phase configs")
	}
}
