package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

func makeTestResult() *BenchmarkResult {
	return &BenchmarkResult{
		ID:        "test-123",
		Timestamp: time.Now(),
		Drives: []DriveInfo{
			{Device: blockdev.Device{Name: "sda", Vendor: "ACME", Model: "Disk1000"}},
			{Device: blockdev.Device{Name: "sdb", Vendor: "ACME", Model: "Disk1000"}},
		},
		Phases: []PhaseResult{
			{
				PhaseNumber: 1,
				PhaseName:   "seq-read",
				Jobs: []JobResult{
					{Device: "sda", ReadStats: &IOStats{IOPS: 50000, Bandwidth: 204800, AvgLatNS: 10000}},
					{Device: "sdb", ReadStats: &IOStats{IOPS: 48000, Bandwidth: 196608, AvgLatNS: 12000}},
				},
			},
			{
				PhaseNumber: 2,
				PhaseName:   "seq-write",
				Jobs: []JobResult{
					{Device: "sda", WriteStats: &IOStats{IOPS: 40000, Bandwidth: 163840, AvgLatNS: 15000}},
					{Device: "sdb", WriteStats: &IOStats{IOPS: 38000, Bandwidth: 155648, AvgLatNS: 18000}},
				},
			},
		},
	}
}

func TestGenerateDiskGraphs(t *testing.T) {
	result := makeTestResult()
	graphs := generateDiskGraphs(result)

	if len(graphs) != 2 {
		t.Fatalf("graphs length = %d, want 2 (one per disk)", len(graphs))
	}

	sda := graphs[0]
	if sda.Disk != "sda" {
		t.Errorf("disk = %q, want sda", sda.Disk)
	}
	if len(sda.Phases) != 2 {
		t.Fatalf("phases = %d, want 2", len(sda.Phases))
	}

	p0 := sda.Phases[0]
	if !p0.HasRead {
		t.Error("phase 0 should have read data")
	}
	if p0.ReadIOPS != 50000 {
		t.Errorf("ReadIOPS = %f, want 50000", p0.ReadIOPS)
	}
	if p0.HasWrite {
		t.Error("phase 0 should not have write data")
	}

	p1 := sda.Phases[1]
	if p1.HasRead {
		t.Error("phase 1 should not have read data")
	}
	if !p1.HasWrite {
		t.Error("phase 1 should have write data")
	}
	if p1.WriteIOPS != 40000 {
		t.Errorf("WriteIOPS = %f, want 40000", p1.WriteIOPS)
	}
}

func TestGenerateDiskGraphs_NoPhases(t *testing.T) {
	result := &BenchmarkResult{Phases: []PhaseResult{}}
	graphs := generateDiskGraphs(result)
	if len(graphs) != 0 {
		t.Errorf("expected 0 graphs for empty result, got %d", len(graphs))
	}
}

func TestGenerateDiskGraphs_NoDrives(t *testing.T) {
	result := &BenchmarkResult{
		Phases: []PhaseResult{
			{PhaseNumber: 1, Jobs: []JobResult{
				{Device: "sda", ReadStats: &IOStats{IOPS: 1000}},
			}},
		},
	}
	// No drives listed → disk order comes from drives slice, so no graphs
	graphs := generateDiskGraphs(result)
	if len(graphs) != 0 {
		t.Errorf("expected 0 graphs when drives list is empty, got %d", len(graphs))
	}
}

func TestHandleResultGraphs(t *testing.T) {
	result := makeTestResult()
	globalResultsStore.AddResult(result)
	defer globalResultsStore.Clear()

	req := httptest.NewRequest("GET", "/api/results/graphs?id=test-123", nil)
	w := httptest.NewRecorder()
	handleResultGraphs(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var graphs []DiskGraphData
	if err := json.NewDecoder(resp.Body).Decode(&graphs); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(graphs) == 0 {
		t.Error("expected at least one disk graph")
	}
}

func TestHandleResultGraphs_MissingID(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/results/graphs", nil)
	w := httptest.NewRecorder()
	handleResultGraphs(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Result().StatusCode)
	}
}

func TestHandleResultGraphs_NotFound(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/results/graphs?id=nonexistent", nil)
	w := httptest.NewRecorder()
	handleResultGraphs(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Result().StatusCode)
	}
}

func TestHandleResultsList(t *testing.T) {
	globalResultsStore.Clear()

	now := time.Now()
	results := []*BenchmarkResult{
		{
			ID: "1", Timestamp: now, Mode: "parallel-sync",
			Drives: []DriveInfo{{Device: blockdev.Device{Name: "sda"}}},
			Phases: []PhaseResult{{}, {}},
		},
		{
			ID: "2", Timestamp: now.Add(-1 * time.Hour), Mode: "sequential",
			Drives: []DriveInfo{{Device: blockdev.Device{Name: "nvme0n1"}}},
			Phases: []PhaseResult{{}},
		},
	}
	for _, r := range results {
		globalResultsStore.AddResult(r)
	}
	defer globalResultsStore.Clear()

	req := httptest.NewRequest("GET", "/api/results", nil)
	w := httptest.NewRecorder()
	handleResultsList(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var summaries []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&summaries); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(summaries) != 2 {
		t.Errorf("summaries length = %d, want 2", len(summaries))
	}
	if summaries[0]["id"] != "1" {
		t.Errorf("first result ID = %v, want 1", summaries[0]["id"])
	}
}

func TestHandleGraphsPage(t *testing.T) {
	result := &BenchmarkResult{
		ID:        "test-456",
		Timestamp: time.Now(),
		Phases:    []PhaseResult{{}},
	}
	globalResultsStore.AddResult(result)
	defer globalResultsStore.Clear()

	req := httptest.NewRequest("GET", "/graphs?id=test-456", nil)
	w := httptest.NewRecorder()
	handleGraphsPage(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	body := w.Body.String()
	if !contains(body, "chart.js") {
		t.Error("page should include Chart.js")
	}
	if !contains(body, "test-456") {
		t.Error("page should include result ID")
	}
}

func TestHandleGraphsPage_MissingID(t *testing.T) {
	req := httptest.NewRequest("GET", "/graphs", nil)
	w := httptest.NewRecorder()
	handleGraphsPage(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Result().StatusCode)
	}
}

func TestGenerateDiskPhaseData_Labels(t *testing.T) {
	r := makeTestResult()
	r.Labels = map[string]rungroup.Label{
		"sdb": {Setting: "write-cache", Value: "write through", Valid: false},
	}
	var sda, sdb int
	for _, d := range generateDiskPhaseData(r) {
		switch d.Disk {
		case "sdb":
			sdb++
			if d.Setting != "write-cache" || d.SettingValue != "write through" || d.RunID != "test-123" || !d.Invalid {
				t.Errorf("sdb entry = %+v", d)
			}
		case "sda":
			sda++
			if d.Setting != "" || d.SettingValue != "" || d.RunID != "" || d.Invalid {
				t.Errorf("unlabelled sda carries label data: %+v", d)
			}
		}
	}
	if sda == 0 || sdb == 0 {
		t.Fatalf("entries: sda=%d sdb=%d", sda, sdb)
	}
	r.Labels["sdb"] = rungroup.Label{Setting: "write-cache", Value: "write back", Valid: true}
	for _, d := range generateDiskPhaseData(r) {
		if d.Disk == "sdb" && d.Invalid {
			t.Errorf("valid label marked invalid: %+v", d)
		}
	}
}

func arrayDrives() []DriveInfo {
	return []DriveInfo{
		{Device: blockdev.Device{Name: "md127", Model: "raid1", Serial: "0a:0b"}, UUID: "0a:0b", Members: []rungroup.Member{
			{Name: "sdb", Serial: "S1", Model: "M", Role: settings.RoleData},
			{Name: "sdc", Serial: "S2", Model: "M", Role: settings.RoleData},
		}},
		{Device: blockdev.Device{Name: "sdd", Model: "M", Serial: "S3"}},
	}
}

func TestGenerateDiskPhaseData_ArrayMembers(t *testing.T) {
	r := &BenchmarkResult{
		ID: "x", Drives: arrayDrives(),
		Phases: []PhaseResult{{PhaseNumber: 1, PhaseName: "p", Jobs: []JobResult{
			{Device: "md127", ReadStats: &IOStats{IOPS: 1, Bandwidth: 1}},
			{Device: "sdd", ReadStats: &IOStats{IOPS: 1, Bandwidth: 1}},
		}}},
	}
	var md, sdd *DiskPhaseData
	for _, d := range generateDiskPhaseData(r) {
		d := d
		switch d.Disk {
		case "md127":
			md = &d
		case "sdd":
			sdd = &d
		}
	}
	if md == nil || sdd == nil {
		t.Fatalf("entries: md=%v sdd=%v", md, sdd)
	}
	if md.Kind != "array" || len(md.Members) != 2 || md.Members[0].Role != settings.RoleData || md.Model != "raid1" || md.Serial != "0a:0b" {
		t.Errorf("md127 = %+v", md)
	}
	if sdd.Kind != "" || sdd.Members != nil {
		t.Errorf("sdd = %+v", sdd)
	}
}

// graphsPageBody renders the graphs page for a stored result.
func graphsPageBody(t *testing.T) string {
	t.Helper()
	globalResultsStore.AddResult(&BenchmarkResult{ID: "members-page", Timestamp: time.Now(), Phases: []PhaseResult{{}}})
	defer globalResultsStore.Clear()
	w := httptest.NewRecorder()
	handleGraphsPage(w, httptest.NewRequest("GET", "/graphs?id=members-page", nil))
	return w.Body.String()
}

// jsFunction returns the source of the page function name, up to the next
// function or comment at the same indentation.
func jsFunction(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, "function "+name+"(")
	if start < 0 {
		t.Fatalf("page has no function %s", name)
	}
	rest := body[start+1:]
	end := len(rest)
	for _, next := range []string{"\n    function ", "\n    //"} {
		if i := strings.Index(rest, next); i >= 0 && i < end {
			end = i
		}
	}
	return rest[:end]
}

func TestGraphsPageStacksMembers(t *testing.T) {
	body := graphsPageBody(t)
	// Read and write stack separately: Chart.js adds a 0 to every earlier
	// value of its stack, whatever their sign.
	for _, want := range []string{"'members'", "'members-write'", "stack = 'array'", "y: { stacked: stacked,",
		"mode: 'index', intersect: false", "Stack total", "'Array '", "ts_members", "fill: first ? 'origin' : '-1'"} {
		if !strings.Contains(body, want) {
			t.Errorf("graphs page lacks %q", want)
		}
	}
	// The array line is not filled, so it is drawn over the stack, not into it.
	arr := jsFunction(t, body, "arrayDatasets")
	if !strings.Contains(arr, "fill: false") {
		t.Errorf("array datasets are filled:\n%s", arr)
	}
}

func TestGraphsPageLatencyNotStacked(t *testing.T) {
	body := graphsPageBody(t)
	lat := jsFunction(t, body, "memberLatencyDatasets")
	if strings.Contains(lat, "stack") {
		t.Errorf("member latency datasets are stacked:\n%s", lat)
	}
	if !strings.Contains(lat, "fill: false") {
		t.Errorf("member latency lines are filled:\n%s", lat)
	}
	// The scale stacks only for IOPS and bandwidth.
	if !strings.Contains(body, "metric.key !== 'lat'") {
		t.Error("page does not keep latency off the stacked scale")
	}
}

func TestGraphsPageNoMembersUnchanged(t *testing.T) {
	body := graphsPageBody(t)
	// The plain Read/Write series of a drive, as before.
	for _, want := range []string{
		"const readPts  = ts.filter(p => p.r > 0).map(p => ({x: Math.round(p.t + shift), y:  p.r}));",
		"const writePts = ts.filter(p => p.w > 0).map(p => ({x: Math.round(p.t + shift), y: -p.w}));",
		"label: 'Read',  data: readPts,",
		"label: 'Write', data: writePts,",
		"if (memberMetric(d, metric)) return makeMembersChart(",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("graphs page lacks %q", want)
		}
	}
	// Only phases with members take the member path.
	mm := jsFunction(t, body, "memberMetric")
	if !strings.Contains(mm, "d.ts_members") {
		t.Errorf("memberMetric does not test ts_members:\n%s", mm)
	}
}
