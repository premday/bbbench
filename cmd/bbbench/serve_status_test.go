package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/diskinfo"
	"bbbench/internal/settings"
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
		drives:    []DriveInfo{{Device: blockdev.Device{Name: "nvme0n1"}}, {Device: blockdev.Device{Name: "sda"}}},
		outputDir: t.TempDir(),
		startTime: time.Unix(1793969936, 0),
		collectDisk: func(d DriveInfo) (diskinfo.Info, string) {
			return diskinfo.Info{Interface: "iface-" + d.Device.Name, Smart: diskinfo.Smart{Status: "PASSED"}}, "dump of " + d.Device.Name + "\n"
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

	// One text dump per disk lands in the output directory.
	for _, dev := range []string{"nvme0n1", "sda"} {
		path := filepath.Join(ec.outputDir, dev+"_diskinfo_1793969936.txt")
		var b []byte
		for time.Now().Before(deadline) { // the file is written right after the status update
			if b, _ = os.ReadFile(path); len(b) > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if string(b) != "dump of "+dev+"\n" {
			t.Errorf("%s: dump file = %q", path, b)
		}
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

// The real time of a finished phase lands on the drive's status entry, per
// phase; skipped phases ran nothing and get none. Snapshots keep their own
// map, so encoding one cannot race with a later update.
func TestRecordPhaseTime(t *testing.T) {
	ResetStatus()
	defer ResetStatus()
	UpdateStatus(func(s *BenchmarkStatus) { s.Drives = []DriveStatus{{Name: "nvme0n1"}, {Name: "sda"}} })
	t0 := time.Unix(1000, 0)

	recordPhaseTime("nvme0n1", 0, &FioResult{StartTime: t0, EndTime: t0.Add(61500 * time.Millisecond)})
	before := GetStatus().Drives[0].PhaseSeconds
	recordPhaseTime("nvme0n1", 2, &FioResult{StartTime: t0, EndTime: t0.Add(5 * time.Minute)})
	recordPhaseTime("nvme0n1", 1, &FioResult{StartTime: t0, EndTime: t0, Skipped: "write through"})
	recordPhaseTime("sda", 0, &FioResult{}) // never ran

	got := GetStatus().Drives
	if m := got[0].PhaseSeconds; len(m) != 2 || m[0] != 61.5 || m[2] != 300 {
		t.Errorf("nvme0n1 = %v, want phases 0 and 2 only", m)
	}
	if len(before) != 1 {
		t.Errorf("an earlier snapshot changed: %v", before)
	}
	if got[1].PhaseSeconds != nil {
		t.Errorf("sda = %v, want none", got[1].PhaseSeconds)
	}
	w := httptest.NewRecorder()
	handleStatus(w, httptest.NewRequest("GET", "/api/status", nil))
	if !strings.Contains(w.Body.String(), `"phase_seconds":{"0":61.5,"2":300}`) {
		t.Errorf("/api/status = %s", w.Body.String())
	}
}

// The snapshot holds its own copy of the group state, and rebuilding the
// drive list at the start of a run keeps each drive's setting state.
func TestGetStatus_GroupCopiedAndSettingKept(t *testing.T) {
	ResetStatus()
	defer ResetStatus()
	valid := true
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Group = &GroupStatus{ID: "g1", Setting: "cache", Strategy: "sequential", Run: 1, Runs: 2, Step: "applying"}
		s.Drives = []DriveStatus{{Name: "sda", Setting: &DriveSetting{Value: "enabled", Valid: &valid}}}
	})
	snap := GetStatus()
	UpdateStatus(func(s *BenchmarkStatus) { s.Group.Step = "running" })
	if snap.Group == nil || snap.Group.Step != "applying" {
		t.Errorf("snapshot group changed with the status: %+v", snap.Group)
	}

	drives := []DriveInfo{{Device: blockdev.Device{Name: "sda"}}, {Device: blockdev.Device{Name: "sdb"}}}
	UpdateStatus(func(s *BenchmarkStatus) { s.Drives = newDriveStatuses(drives, s.Drives) })
	got := GetStatus().Drives
	if len(got) != 2 || got[0].Setting == nil || got[0].Setting.Value != "enabled" || got[1].Setting != nil {
		t.Errorf("drives after rebuild = %+v", got)
	}
	if got[0].Status != "pending" {
		t.Errorf("rebuilt drive status %q, want pending", got[0].Status)
	}

	w := httptest.NewRecorder()
	handleStatus(w, httptest.NewRequest("GET", "/api/status", nil))
	body := w.Body.String()
	for _, want := range []string{`"group":{`, `"step":"running"`, `"setting":{"value":"enabled"`, `"valid":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("/api/status lacks %s: %s", want, body)
		}
	}
	ResetStatus()
	if GetStatus().Group != nil {
		t.Error("ResetStatus kept the group")
	}
}

// /api/status carries the group and the per-drive setting the page draws.
func TestStatusAPI_CarriesGroup(t *testing.T) {
	ResetStatus()
	defer ResetStatus()
	invalid := false
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = true
		s.Group = &GroupStatus{ID: "g1", Setting: "write-cache", Strategy: "sequential", Run: 2, Runs: 3, Step: "verifying-before"}
		s.Drives = []DriveStatus{{Name: "sda", Setting: &DriveSetting{
			Value: "disabled", Before: &settings.Reading{Value: "enabled", Kernel: "write back"}, Valid: &invalid}}}
	})
	w := httptest.NewRecorder()
	handleStatus(w, httptest.NewRequest("GET", "/api/status", nil))
	var got struct {
		Group struct {
			Step string `json:"step"`
			Run  int    `json:"run"`
			Runs int    `json:"runs"`
		} `json:"group"`
		Drives []struct {
			Setting struct {
				Value  string `json:"value"`
				Before struct {
					Value string `json:"value"`
				} `json:"before"`
				Valid *bool `json:"valid"`
			} `json:"setting"`
		} `json:"drives"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Group.Step != "verifying-before" || got.Group.Run != 2 || got.Group.Runs != 3 {
		t.Errorf("group = %+v", got.Group)
	}
	if len(got.Drives) != 1 || got.Drives[0].Setting.Value != "disabled" ||
		got.Drives[0].Setting.Before.Value != "enabled" || got.Drives[0].Setting.Valid == nil || *got.Drives[0].Setting.Valid {
		t.Errorf("drives = %+v", got.Drives)
	}
}

// pageScript is the status page's <script> content.
func pageScript(t *testing.T) string {
	t.Helper()
	w := httptest.NewRecorder()
	handleStatusPage(w, httptest.NewRequest("GET", "/status", nil))
	body := w.Body.String()
	i := strings.Index(body, "<script>")
	j := strings.LastIndex(body, "</script>")
	if i < 0 || j < i {
		t.Fatal("no script in the status page")
	}
	return body[i+len("<script>") : j]
}

// The group banner, badges and ETA run in node against sample data; the page
// script must also pass node --check. Skipped without node.
func TestStatusPage_GroupScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	page := filepath.Join(dir, "page.js")
	if err := os.WriteFile(page, []byte(pageScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", page).CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
	harness := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(harness, []byte(groupHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, harness, page).CombinedOutput(); err != nil {
		t.Fatalf("harness: %v\n%s", err, out)
	}
}

const groupHarness = `
const fs = require('fs'), vm = require('vm'), assert = require('assert');
const ctx = {
  document: { addEventListener() {}, getElementById() { return { style: {}, set innerHTML(v) {}, set textContent(v) {} }; } },
  fetch: () => new Promise(() => {}), setInterval() {}, console, Date, Math, String, Set, JSON, Object, Array,
};
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), ctx);
const run = s => vm.runInContext(s, ctx);

// no group: nothing at all
assert.strictEqual(run('groupBannerHTML(undefined, [], {})'), '');
assert.strictEqual(run('groupBannerHTML(null, [], {})'), '');
assert.strictEqual(run('settingBadgesHTML({name:"sda"}, undefined)'), '');

const group = '{id:"g",setting:"write <b>cache</b>",strategy:"sequential",run:2,runs:3,step:"running"}';
const phases = '{sda:[{index:0,deterministic:true,duration_sec:60},{index:1,deterministic:true,duration_sec:40}]}';
const drv = '[{name:"sda",status:"running",current_phase:1,phase_start:new Date().toISOString()}]';
let b = run('groupBannerHTML(' + group + ',' + drv + ',' + phases + ')');
assert(b.includes('Setting test: <b>write &lt;b&gt;cache&lt;/b&gt;</b>, run 2 of 3, running'), b);
assert(b.includes('sequential'), b);
assert(!b.includes('write <b>'), b);
// 1 run left * 100s + current run (~100s) => about 3m 20s
assert(b.includes('ETA ~3m'), b);

// unknown-length phase makes the total unknown, said like the existing text
const phasesU = '{sda:[{index:0,deterministic:true,duration_sec:60},{index:1,deterministic:false}]}';
b = run('groupBannerHTML(' + group + ',' + drv + ',' + phasesU + ')');
assert(b.includes('of unknown length') && !/ETA ~[^+]*\(about/.test(b), b);

// mixed drives: A fully known, B past its unknown phase (phase 2 of 2 running)
// but with an unknown phase in its future runs: no confident plain number.
const mixPh = '{sda:[{index:0,deterministic:true,duration_sec:60}],sdb:[{index:0,deterministic:false},{index:1,deterministic:true,duration_sec:40}]}';
const mixDr = '[{name:"sda",status:"running",current_phase:1,phase_start:new Date().toISOString()},{name:"sdb",status:"running",current_phase:2,phase_start:new Date().toISOString()}]';
b = run('groupBannerHTML(' + group + ',' + mixDr + ',' + mixPh + ')');
assert(b.includes('1 phase of unknown length'), b);
assert(!/ETA ~[^+<]*\(about/.test(b), b);
// every drive fully known: the plain number
const knownPh = '{sda:[{index:0,deterministic:true,duration_sec:60}],sdb:[{index:0,deterministic:true,duration_sec:80}]}';
b = run('groupBannerHTML(' + group + ',' + mixDr + ',' + knownPh + ')');
assert(/ETA ~[^+<]*\(about/.test(b) && !b.includes('unknown'), b);

// last run: no extra runs
b = run('groupBannerHTML({id:"g",setting:"s",strategy:"sequential",run:3,runs:3,step:"running"},' + drv + ',' + phases + ')');
assert(!b.includes('ETA unknown'), b);

// restoring and done steps
b = run('groupBannerHTML({id:"g",setting:"s",strategy:"sequential",run:3,runs:3,step:"restoring"},' + drv + ',' + phases + ')');
assert(b.includes('restoring'), b);
b = run('groupBannerHTML({id:"g",setting:"s",strategy:"sequential",run:3,runs:3,step:"done"},' + drv + ',' + phases + ')');
assert(b.includes('done'), b);
assert(!/restored|restore failed/.test(b), b);

// badges: failed before, pending after
let d = '{name:"sda",setting:{value:"disabled",before:{value:"enabled",kernel:"write back"}}}';
let h = run('settingBadgesHTML(' + d + ',' + group + ')');
assert(h.includes('disabled'), h);
assert(/before[^<]*<\/span>|before: failed/.test(h) && h.includes('failed'), h);
assert(h.includes('pending'), h);
assert(h.includes('kernel write back'), h);

// after ok, valid
d = '{name:"sda",setting:{value:"disabled",before:{value:"disabled"},after:{value:"disabled",kernel:"write through"},valid:true}}';
h = run('settingBadgesHTML(' + d + ',' + group + ')');
assert(!h.includes('failed') && !h.includes('pending') && !h.includes('invalid'), h);
assert(h.includes('>before ok<') && h.includes('>after ok<'), h);

// invalid card
d = '{name:"sda",setting:{value:"disabled",before:{value:"disabled"},after:{value:"enabled"},valid:false}}';
h = run('settingBadgesHTML(' + d + ',' + group + ')');
assert(h.includes('invalid') && h.includes('failed'), h);

// restoring step
h = run('settingBadgesHTML(' + d + ',{step:"restoring",run:1,runs:1})');
assert(h.includes('restoring'), h);

// markup in value and reading is escaped
d = '{name:"sda",setting:{value:"<img src=x onerror=1>",before:{value:"\\"><script>",kernel:"<i>"}}}';
h = run('settingBadgesHTML(' + d + ',' + group + ')');
assert(!h.includes('<img') && !h.includes('<script') && !h.includes('<i>'), h);
console.log('ok');
`

func TestNewDriveStatuses_ArrayMembers(t *testing.T) {
	got := newDriveStatuses(arrayDrives(), nil)
	if got[0].Kind != "array" || len(got[0].Members) != 2 || got[0].Members[1].Name != "sdc" || got[0].Members[1].Serial != "S2" || got[0].Members[1].Role != settings.RoleData {
		t.Errorf("array status = %+v", got[0])
	}
	if got[1].Kind != "" || got[1].Members != nil {
		t.Errorf("drive status = %+v", got[1])
	}
}

func TestDriveInfo_JSONUnchangedForDrives(t *testing.T) {
	data, err := json.Marshal(DriveInfo{Device: blockdev.Device{Name: "sda"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"Members"`, `"UUID"`} {
		if strings.Contains(string(data), k) {
			t.Errorf("drive JSON has %s: %s", k, data)
		}
	}
}
