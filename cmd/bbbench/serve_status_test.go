package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/diskinfo"
)

func TestGetStatus(t *testing.T) {
	// Reset to known state
	ResetStatus()

	status := GetStatus()

	if status.Running {
		t.Error("Initial status should not be running")
	}

	if status.TotalPhases != 0 {
		t.Errorf("TotalPhases = %d, want 0", status.TotalPhases)
	}
}

func TestUpdateStatus(t *testing.T) {
	ResetStatus()

	// Update status
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = true
		s.Mode = "parallel-sync"
		s.TotalPhases = 10
		s.CurrentPhase = 3
		s.CompletedPhases = 2
	})

	status := GetStatus()

	if !status.Running {
		t.Error("Status should be running after update")
	}

	if status.Mode != "parallel-sync" {
		t.Errorf("Mode = %q, want parallel-sync", status.Mode)
	}

	if status.TotalPhases != 10 {
		t.Errorf("TotalPhases = %d, want 10", status.TotalPhases)
	}

	if status.CurrentPhase != 3 {
		t.Errorf("CurrentPhase = %d, want 3", status.CurrentPhase)
	}

	if status.CompletedPhases != 2 {
		t.Errorf("CompletedPhases = %d, want 2", status.CompletedPhases)
	}

	// Verify LastUpdate was set
	if status.LastUpdate.IsZero() {
		t.Error("LastUpdate should be set")
	}
}

func TestResetStatus(t *testing.T) {
	// Set some status
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = true
		s.Mode = "sequential"
		s.TotalPhases = 5
		s.Drives = []DriveStatus{
			{Name: "drive1", Device: "/dev/sda"},
		}
	})

	// Reset
	ResetStatus()

	status := GetStatus()

	if status.Running {
		t.Error("Status should not be running after reset")
	}

	if status.Mode != "" {
		t.Errorf("Mode = %q, want empty", status.Mode)
	}

	if status.TotalPhases != 0 {
		t.Errorf("TotalPhases = %d, want 0", status.TotalPhases)
	}

	if len(status.Drives) != 0 {
		t.Errorf("Drives length = %d, want 0", len(status.Drives))
	}
}

func TestUpdateStatusWithDrives(t *testing.T) {
	ResetStatus()

	drives := []DriveStatus{
		{
			Name:   "drive1",
			Device: "/dev/sda",
			Vendor: "Samsung",
			Model:  "SSD 980",
			Status: "running",
		},
		{
			Name:   "drive2",
			Device: "/dev/nvme0n1",
			Vendor: "Intel",
			Model:  "Optane",
			Status: "pending",
		},
	}

	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = true
		s.Drives = drives
	})

	status := GetStatus()

	if len(status.Drives) != 2 {
		t.Fatalf("Drives length = %d, want 2", len(status.Drives))
	}

	if status.Drives[0].Name != "drive1" {
		t.Errorf("Drive[0].Name = %q, want drive1", status.Drives[0].Name)
	}

	if status.Drives[1].Vendor != "Intel" {
		t.Errorf("Drive[1].Vendor = %q, want Intel", status.Drives[1].Vendor)
	}
}

func TestHandleStatus(t *testing.T) {
	ResetStatus()

	// Set up test status
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = true
		s.Mode = "parallel-sync"
		s.TotalPhases = 5
		s.CurrentPhase = 2
		s.CompletedPhases = 1
		s.Drives = []DriveStatus{
			{Name: "test-drive", Device: "/dev/sda", Status: "running"},
		}
	})

	// Create test request
	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()

	// Handle request
	handleStatus(w, req)

	// Check response
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}

	// Decode response
	var status BenchmarkStatusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if !status.Running {
		t.Error("Status should be running")
	}

	if status.Mode != "parallel-sync" {
		t.Errorf("Mode = %q, want parallel-sync", status.Mode)
	}

	if status.TotalPhases != 5 {
		t.Errorf("TotalPhases = %d, want 5", status.TotalPhases)
	}

	if len(status.Drives) != 1 {
		t.Fatalf("Drives length = %d, want 1", len(status.Drives))
	}

	if status.Drives[0].Name != "test-drive" {
		t.Errorf("Drive name = %q, want test-drive", status.Drives[0].Name)
	}
}

func TestHandleStatusPage(t *testing.T) {
	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()

	handleStatusPage(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", contentType)
	}

	// Read body
	body := w.Body.String()

	// Check for expected content
	if !contains(body, "Benchmark Status") {
		t.Error("Page missing title")
	}

	if !contains(body, "/api/status") {
		t.Error("Page missing status API endpoint")
	}

	if !contains(body, "updateStatus()") {
		t.Error("Page missing status update JavaScript")
	}
}

func TestStatusConcurrency(t *testing.T) {
	ResetStatus()

	// Run concurrent updates
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(n int) {
			UpdateStatus(func(s *BenchmarkStatus) {
				s.CurrentPhase = n
			})
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Status should be consistent (no race conditions)
	status := GetStatus()
	if status.CurrentPhase < 0 || status.CurrentPhase >= 10 {
		t.Errorf("CurrentPhase = %d, should be 0-9", status.CurrentPhase)
	}
}

func TestStatusLastUpdate(t *testing.T) {
	ResetStatus()

	before := time.Now()
	time.Sleep(10 * time.Millisecond)

	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = true
	})

	time.Sleep(10 * time.Millisecond)
	after := time.Now()

	status := GetStatus()

	if status.LastUpdate.Before(before) {
		t.Error("LastUpdate should be after the 'before' timestamp")
	}

	if status.LastUpdate.After(after) {
		t.Error("LastUpdate should be before the 'after' timestamp")
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 &&
		(s == substr || len(s) > len(substr) &&
			(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
				findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestMarkDrive(t *testing.T) {
	ResetStatus()
	defer ResetStatus()
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Drives = []DriveStatus{{Name: "nvme0n1", Status: "running"}, {Name: "nvme1n1", Status: "running"}}
	})

	markDrive("nvme1n1", func(d *DriveStatus) { d.Status, d.Skipped = "skipped", "write through" })

	got := GetStatus().Drives
	if got[0].Status != "running" || got[0].Skipped != "" {
		t.Errorf("other drive changed: %+v", got[0])
	}
	if got[1].Status != "skipped" || got[1].Skipped != "write through" {
		t.Errorf("skipped drive = %+v", got[1])
	}
}

// The disk configuration is read in the background and lands on the status
// page's drive entries as it arrives; /api/status carries it.
func TestCollectDiskConfig_LandsOnStatus(t *testing.T) {
	ResetStatus()
	defer ResetStatus()
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Drives = []DriveStatus{{Name: "nvme0n1"}, {Name: "sda"}}
	})
	ec := &ExecutionCoordinator{
		drives: []DriveInfo{{Device: blockdev.Device{Name: "nvme0n1"}}, {Device: blockdev.Device{Name: "sda"}}},
		collectDisk: func(name string) diskinfo.Info {
			return diskinfo.Info{Interface: "iface-" + name, Smart: diskinfo.Smart{Status: "PASSED"}}
		},
	}
	ec.collectDiskConfig()

	deadline := time.Now().Add(2 * time.Second)
	for {
		d := GetStatus().Drives
		if d[0].Config != nil && d[1].Config != nil {
			if d[0].Config.Interface != "iface-nvme0n1" || d[1].Config.Interface != "iface-sda" {
				t.Fatalf("configs mixed up: %+v %+v", d[0].Config, d[1].Config)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("config never arrived: %+v", d)
		}
		time.Sleep(5 * time.Millisecond)
	}

	w := httptest.NewRecorder()
	handleStatus(w, httptest.NewRequest("GET", "/api/status", nil))
	var got struct {
		Drives []struct {
			Name   string `json:"name"`
			Config *struct {
				Interface string `json:"interface"`
				Smart     struct {
					Status string `json:"status"`
				} `json:"smart"`
			} `json:"config"`
		} `json:"drives"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Drives) != 2 || got.Drives[0].Config == nil || got.Drives[0].Config.Smart.Status != "PASSED" {
		t.Errorf("/api/status = %s", w.Body.String())
	}
}
