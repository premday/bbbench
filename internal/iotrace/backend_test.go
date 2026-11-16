package iotrace

import (
	"errors"
	"strings"
	"testing"
)

func failingEBPF(t *testing.T, msg string) {
	t.Helper()
	prev := startEBPF
	startEBPF = func([]string, Options) (Session, error) { return nil, errors.New(msg) }
	t.Cleanup(func() { startEBPF = prev })
}

func TestAutoFallsBackToFtrace(t *testing.T) {
	failingEBPF(t, "ebpf: no BTF")
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	res, _ := run(t, []string{"sdb"}, blockEvents, sys, Options{Mode: ModeAuto},
		marker(start, "bbbench-start"),
		blk(10*sec, "block_rq_issue", "8,16", "R", 1000, 8),
		blk(10*sec+5_000_000, "block_rq_complete", "8,16", "R", 1000, 8),
	)
	if res.Summary.Mode != ModeFtrace || !strings.Contains(res.Summary.ModeNote, "no BTF") {
		t.Errorf("mode %q note %q, want ftrace with the ebpf failure", res.Summary.Mode, res.Summary.ModeNote)
	}
	wantStat(t, dev(t, res.Summary, "sdb"), KindDevice, DirRead, 1, 5e6, 5_000_000, 5_000_000)
}

func TestForcedFtraceNeverTriesEBPF(t *testing.T) {
	prev := startEBPF
	startEBPF = func([]string, Options) (Session, error) { t.Error("ebpf started"); return nil, errors.New("x") }
	t.Cleanup(func() { startEBPF = prev })
	sys := fakeSys(t, map[string]string{"sdb": "8:16"}, "sdb")
	res, _ := run(t, []string{"sdb"}, blockEvents, sys, Options{Mode: ModeFtrace}, marker(start, "bbbench-start"))
	if res.Summary.Mode != ModeFtrace || res.Summary.ModeNote != "" {
		t.Errorf("mode %q note %q", res.Summary.Mode, res.Summary.ModeNote)
	}
}

func TestForcedEBPFFailureIsAnError(t *testing.T) {
	failingEBPF(t, "ebpf: verifier rejected the program")
	_, err := Start([]string{"sdb"}, Options{Mode: ModeEBPF})
	if err == nil || !strings.Contains(err.Error(), "verifier rejected") {
		t.Fatalf("err = %v", err)
	}
}

type fakeSession struct{ marked, closed bool }

func (f *fakeSession) MarkStart()            { f.marked = true }
func (f *fakeSession) Stop() (Result, error) { return Result{Summary: Summary{Mode: ModeEBPF}}, nil }
func (f *fakeSession) Close() error          { f.closed = true; return nil }

func TestAutoUsesEBPFWhenItStarts(t *testing.T) {
	prev := startEBPF
	fs := &fakeSession{}
	startEBPF = func([]string, Options) (Session, error) { return fs, nil }
	t.Cleanup(func() { startEBPF = prev })
	s, err := Start([]string{"sdb"}, Options{Mode: ModeAuto, TracefsRoot: t.TempDir()})
	if err != nil || s != Session(fs) {
		t.Fatalf("Start = %v, %v; want the ebpf session", s, err)
	}
}
