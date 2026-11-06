package main

import (
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
