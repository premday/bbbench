package iotrace

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"bbbench/internal/flushtrace"
)

func kernelResult() Result {
	r := Result{Summary: Summary{
		AvgMsec: 1000, Overrun: 100,
		Devices: []DeviceSummary{{Name: "md0", Source: SourceBio, BioBased: true}},
		Kernel: &flushtrace.KernelInfo{
			BufferKB: "65536 (expanded: 98304)",
			Filters: []flushtrace.FilterState{
				{Event: "events/block/block_bio_queue", Wanted: "dev == 9", Applied: "dev == 9"},
				{Event: "events/block/block_bio_complete", Wanted: "dev == 9", Applied: "none", Err: "write error: invalid argument"},
			},
			CPUs: []flushtrace.CPUStats{{CPU: 0, Entries: 1, Overrun: 7000, CommitOverrun: 2, Bytes: 3, Dropped: 4, Read: 5}, {CPU: 1, Overrun: 1}},
		},
	}}
	r.Summary.Fill()
	return r
}

func TestKernelLogRoundTrip(t *testing.T) {
	r := kernelResult()
	var b bytes.Buffer
	if err := WriteLog(&b, r); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLog(&b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Summary.Kernel, r.Summary.Kernel) {
		t.Errorf("kernel view changed:\n got %+v\nwant %+v", got.Summary.Kernel, r.Summary.Kernel)
	}
	if got.Summary.KernelNote != r.Summary.KernelNote || got.Summary.KernelNote == "" {
		t.Errorf("note %q vs %q", got.Summary.KernelNote, r.Summary.KernelNote)
	}
}

func TestKernelNote(t *testing.T) {
	n := kernelResult().Summary.KernelNote
	for _, want := range []string{"did not apply the filter of events/block/block_bio_complete", "kernel overrun 7001, bbbench counted 100"} {
		if !strings.Contains(n, want) {
			t.Errorf("note %q lacks %q", n, want)
		}
	}
	s := Summary{Overrun: 5, Kernel: &flushtrace.KernelInfo{CPUs: []flushtrace.CPUStats{{Overrun: 5}}}}
	if got := s.kernelNote(); got != "" {
		t.Errorf("agreeing counts: note %q", got)
	}
	if got := (&Summary{}).kernelNote(); got != "" {
		t.Errorf("no kernel view: note %q", got)
	}
}

// A log without kernel lines (older) reads as before.
func TestOldLogHasNoKernel(t *testing.T) {
	r := kernelResult()
	r.Summary.Kernel = nil
	var b bytes.Buffer
	_ = WriteLog(&b, r)
	got, err := ReadLog(&b)
	if err != nil || got.Summary.Kernel != nil {
		t.Errorf("err %v kernel %+v", err, got.Summary.Kernel)
	}
}
