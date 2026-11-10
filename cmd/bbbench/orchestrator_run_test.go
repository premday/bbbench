package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

func TestBenchmarkResult_LabelsRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	in := BenchmarkResult{
		ID:      "run_1",
		GroupID: "grp_1",
		Labels: map[string]rungroup.Label{
			"sda": {Setting: "write-cache", Value: "write back", Valid: true,
				Before: &settings.Reading{Value: "write through", At: at},
				After:  &settings.Reading{Value: "write back", At: at}},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out BenchmarkResult
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.GroupID != "grp_1" || !reflect.DeepEqual(out.Labels, in.Labels) {
		t.Errorf("round trip lost data: %+v", out)
	}

	var old BenchmarkResult
	if err := json.Unmarshal([]byte(`{"id":"run_0","mode":"sequential"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.GroupID != "" || old.Labels != nil {
		t.Errorf("old summary: group=%q labels=%v", old.GroupID, old.Labels)
	}
}

func TestExecuteRun_OnStartedNotCalledOnSetupFailure(t *testing.T) {
	called := false
	opts := runOptions{Mode: "sequential", OutDir: t.TempDir(), OnStarted: func() { called = true }}
	spec := runSpec{Drives: []DriveInfo{{FioFilePath: "/nonexistent/none.fio"}}}
	result, err := executeRun(context.Background(), opts, spec)
	if err == nil || result != nil {
		t.Fatalf("want setup error and nil result, got %v, %v", result, err)
	}
	if called {
		t.Error("OnStarted called although setup failed")
	}
}

func TestRegisterLiveRun_OnStartedOnceAfterStore(t *testing.T) {
	calls := 0
	var inStore bool
	var live *BenchmarkResult
	opts := runOptions{Mode: "sequential", OutDir: t.TempDir()}
	opts.OnStarted = func() {
		calls++
		if live == nil {
			// live is assigned after return; look the run up by scanning the store.
			for _, r := range globalResultsStore.ListResults() {
				if r.GroupID == "g-onstarted" {
					inStore = true
				}
			}
		}
	}
	ec := &ExecutionCoordinator{}
	live = registerLiveRun(opts, runSpec{GroupID: "g-onstarted"}, ec)
	if calls != 1 || !inStore {
		t.Errorf("calls=%d inStore=%v", calls, inStore)
	}
	if ec.liveResult != live {
		t.Error("coordinator liveResult not set")
	}
}
