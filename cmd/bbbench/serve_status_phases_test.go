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

func TestPhaseDuration(t *testing.T) {
	w := phasesTestWorkload(t)
	got := phaseConfigs(w)
	// seq_write: runtime=60 time_based, no ramp in this file.
	if !got[0].Deterministic || got[0].DurationSec != 60 {
		t.Errorf("phase 0 = %+v, want deterministic 60s", got[0])
	}
	// rand_write: runtime=300 without time_based ends with its I/O.
	if got[1].Deterministic || got[1].Open == "" {
		t.Errorf("phase 1 = %+v, want unknown length with a reason", got[1])
	}
}

func TestPhaseDuration_RampAndFullDisk(t *testing.T) {
	g := map[string]string{"ramp_time": "10"}
	sec, det, _ := phaseDuration(g, []FioJob{{Section: "[a]\ntime_based\nruntime=5m"}, {Section: "[b]\ntime_based\nruntime=30"}})
	if !det || sec != 310 {
		t.Errorf("got %d,%v want 310,true", sec, det)
	}
	_, det, open := phaseDuration(g, []FioJob{{Section: "[a]\nrw=write\nsize=100%"}})
	if det || !strings.Contains(open, "whole disk") {
		t.Errorf("full disk: det=%v open=%q", det, open)
	}
	_, det, _ = phaseDuration(g, []FioJob{{Section: "[a]\nrw=trim\nsize=100%"}})
	if det {
		t.Error("trim must not be deterministic")
	}
}

func TestParseFioSeconds(t *testing.T) {
	for in, want := range map[string]int{"300": 300, "5m": 300, "1h": 3600, "250ms": 0, "2d": 172800, "90s": 90} {
		if got, ok := parseFioSeconds(in); !ok || got != want {
			t.Errorf("%q = %d,%v want %d", in, got, ok, want)
		}
	}
	if _, ok := parseFioSeconds("abc"); ok {
		t.Error("abc parsed")
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
