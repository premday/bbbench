package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleBrowsePage(t *testing.T) {
	req := httptest.NewRequest("GET", "/results", nil)
	w := httptest.NewRecorder()

	handleBrowsePage(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", contentType)
	}

	body := w.Body.String()

	// Check for expected elements
	if !contains(body, "Browse Benchmark Results") {
		t.Error("Page missing title")
	}

	if !contains(body, "/api/results") {
		t.Error("Page missing API endpoint")
	}

	if !contains(body, "refreshResults()") {
		t.Error("Page missing refresh function")
	}

	if !contains(body, "filter-mode") {
		t.Error("Page missing mode filter")
	}
}

func TestHandleResultDetailPage(t *testing.T) {
	globalResultsStore.Clear()

	// Add test result
	result := &BenchmarkResult{
		ID:        "detail-test-123",
		Timestamp: time.Now(),
		Mode:      "parallel-sync",
		Phases: []PhaseResult{
			{PhaseName: "Test Phase 1"},
			{PhaseName: "Test Phase 2"},
		},
	}
	globalResultsStore.AddResult(result)
	defer globalResultsStore.Clear()

	req := httptest.NewRequest("GET", "/result?id=detail-test-123", nil)
	w := httptest.NewRecorder()

	handleResultDetailPage(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body := w.Body.String()

	if !contains(body, "detail-test-123") {
		t.Error("Page should contain result ID")
	}

	if !contains(body, "parallel-sync") {
		t.Error("Page should contain mode")
	}

	if !contains(body, "/graphs?id=") {
		t.Error("Page should have graphs link")
	}

	if !contains(body, "/export?id=") {
		t.Error("Page should have export link")
	}
}

func TestHandleResultDetailPage_MissingID(t *testing.T) {
	req := httptest.NewRequest("GET", "/result", nil)
	w := httptest.NewRecorder()

	handleResultDetailPage(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHandleResultDetailPage_NotFound(t *testing.T) {
	req := httptest.NewRequest("GET", "/result?id=nonexistent", nil)
	w := httptest.NewRecorder()

	handleResultDetailPage(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestHandleResultDetail(t *testing.T) {
	globalResultsStore.Clear()

	// Add test result
	result := &BenchmarkResult{
		ID:        "api-test-456",
		Timestamp: time.Now(),
		Mode:      "sequential",
		Phases: []PhaseResult{
			{
				PhaseName: "Test Phase",
				Jobs: []JobResult{
					{JobName: "job1"},
				},
			},
		},
	}
	globalResultsStore.AddResult(result)
	defer globalResultsStore.Clear()

	req := httptest.NewRequest("GET", "/api/results/detail?id=api-test-456", nil)
	w := httptest.NewRecorder()

	handleResultDetail(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}

	var decodedResult BenchmarkResult
	if err := json.NewDecoder(resp.Body).Decode(&decodedResult); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decodedResult.ID != "api-test-456" {
		t.Errorf("ID = %q, want api-test-456", decodedResult.ID)
	}

	if decodedResult.Mode != "sequential" {
		t.Errorf("Mode = %q, want sequential", decodedResult.Mode)
	}

	if len(decodedResult.Phases) != 1 {
		t.Errorf("Phases length = %d, want 1", len(decodedResult.Phases))
	}
}

func TestHandleResultDetail_MissingID(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/results/detail", nil)
	w := httptest.NewRecorder()

	handleResultDetail(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHandleResultDetail_NotFound(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/results/detail?id=missing", nil)
	w := httptest.NewRecorder()

	handleResultDetail(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestEscapeHTML(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"<script>alert('xss')</script>", "&lt;script&gt;alert(&#39;xss&#39;)&lt;/script&gt;"},
		{"normal text", "normal text"},
		{"a & b", "a &amp; b"},
		{`"quoted"`, "&quot;quoted&quot;"},
		{"<tag attr='value'>", "&lt;tag attr=&#39;value&#39;&gt;"},
	}

	for _, tt := range tests {
		result := escapeHTML(tt.input)
		if result != tt.expected {
			t.Errorf("escapeHTML(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestBrowseIntegration(t *testing.T) {
	globalResultsStore.Clear()

	// Add multiple results
	now := time.Now()
	results := []*BenchmarkResult{
		{
			ID:        "browse-1",
			Timestamp: now,
			Mode:      "parallel-sync",
			Phases:    []PhaseResult{{}, {}},
		},
		{
			ID:        "browse-2",
			Timestamp: now.Add(-1 * time.Hour),
			Mode:      "sequential",
			Phases:    []PhaseResult{{}},
		},
	}

	for _, r := range results {
		globalResultsStore.AddResult(r)
	}
	defer globalResultsStore.Clear()

	// Test browse page loads
	req1 := httptest.NewRequest("GET", "/results", nil)
	w1 := httptest.NewRecorder()
	handleBrowsePage(w1, req1)

	if w1.Code != http.StatusOK {
		t.Errorf("Browse page status = %d, want %d", w1.Code, http.StatusOK)
	}

	// Test detail page loads
	req2 := httptest.NewRequest("GET", "/result?id=browse-1", nil)
	w2 := httptest.NewRecorder()
	handleResultDetailPage(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("Detail page status = %d, want %d", w2.Code, http.StatusOK)
	}

	// Test API returns data
	req3 := httptest.NewRequest("GET", "/api/results/detail?id=browse-1", nil)
	w3 := httptest.NewRecorder()
	handleResultDetail(w3, req3)

	if w3.Code != http.StatusOK {
		t.Errorf("API detail status = %d, want %d", w3.Code, http.StatusOK)
	}

	var result BenchmarkResult
	if err := json.NewDecoder(w3.Body).Decode(&result); err != nil {
		t.Fatalf("API decode failed: %v", err)
	}

	if result.ID != "browse-1" {
		t.Errorf("API returned wrong result: %s", result.ID)
	}
}

// The Details page lists each traced device's source text and each note of
// an untraced drive.
func TestDetailsPageShowsIOTrace(t *testing.T) {
	globalResultsStore.Clear()
	globalResultsStore.AddResult(&BenchmarkResult{ID: "detail-iotrace", Timestamp: time.Now(), Phases: []PhaseResult{{PhaseName: "p"}}})
	defer globalResultsStore.Clear()
	w := httptest.NewRecorder()
	handleResultDetailPage(w, httptest.NewRequest("GET", "/result?id=detail-iotrace", nil))
	body := w.Body.String()
	for _, want := range []string{"Object.entries(phase.io_trace || {})", "Object.entries(phase.io_trace_notes || {})", "s.text"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
