package iotrace

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"auto": ModeAuto, "ebpf": ModeEBPF, "ftrace": ModeFtrace, "": ModeAuto} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	_, err := ParseMode("perf")
	if err == nil {
		t.Fatal("want an error for an unknown mode")
	}
	for _, w := range []string{"auto", "ebpf", "ftrace", "perf"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not mention %q", err, w)
		}
	}
}

func TestLogModeRoundTrip(t *testing.T) {
	r := Result{Summary: Summary{AvgMsec: 1000, Mode: ModeFtrace, ModeNote: "ebpf: no BTF; try x=y"}}
	var b bytes.Buffer
	if err := WriteLog(&b, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.SplitN(b.String(), "\n", 2)[0], " mode=ftrace ") {
		t.Errorf("header lacks mode: %q", strings.SplitN(b.String(), "\n", 2)[0])
	}
	got, err := ReadLog(&b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Mode != ModeFtrace || got.Summary.ModeNote != "ebpf: no BTF; try x=y" {
		t.Errorf("mode %q note %q", got.Summary.Mode, got.Summary.ModeNote)
	}
}

func TestOldLogModeIsFtrace(t *testing.T) {
	got, err := ReadLog(strings.NewReader("# bbbench-iolat-log v1 avg_msec=1000 offset_ms=0 overrun=0 lost=0 reader=raw\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Mode != ModeFtrace {
		t.Errorf("old log mode %q, want ftrace", got.Summary.Mode)
	}
}

func TestSourceLineNamesTheMode(t *testing.T) {
	s := Summary{Mode: ModeEBPF, Devices: []DeviceSummary{{Name: "sda", Source: SourceBlock}}}
	if got := SourceLine(&s); !strings.HasPrefix(got, "ebpf: ") {
		t.Errorf("mode ebpf: %q", got)
	}
	s.Mode = ""
	if got := SourceLine(&s); !strings.HasPrefix(got, "ftrace: ") {
		t.Errorf("no mode: %q", got)
	}
}

func TestEBPFLogRoundTripAndNote(t *testing.T) {
	r := Result{Summary: Summary{AvgMsec: 1000, Mode: ModeEBPF, EBPF: &EBPFInfo{
		Started: 100, Completed: 98, NoStart: 3, Full: 7, Programs: []string{"block_rq_issue", "block_rq_complete"}}}}
	r.Summary.Fill()
	var b bytes.Buffer
	if err := WriteLog(&b, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "# ebpf started=100 completed=98 no_start=3 full=7 programs=block_rq_issue,block_rq_complete\n") {
		t.Errorf("log lacks the ebpf line:\n%s", b.String())
	}
	got, err := ReadLog(&b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.EBPF == nil || got.Summary.EBPF.Full != 7 ||
		strings.Join(got.Summary.EBPF.Programs, ",") != "block_rq_issue,block_rq_complete" {
		t.Errorf("ebpf info %+v", got.Summary.EBPF)
	}
	if !strings.Contains(got.Summary.KernelNote, "start map") || !strings.Contains(got.Summary.KernelNote, "7") {
		t.Errorf("kernel note %q does not say the start map overflowed", got.Summary.KernelNote)
	}
	clean := Summary{Mode: ModeEBPF, EBPF: &EBPFInfo{Started: 5, Completed: 5}}
	clean.Fill()
	if clean.KernelNote != "" {
		t.Errorf("note %q without loss", clean.KernelNote)
	}
}
