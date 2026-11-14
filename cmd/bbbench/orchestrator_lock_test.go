package main

import (
	"os"
	"testing"
)

func TestAcquireOrchestratorLock(t *testing.T) {
	// Clean up any existing lock
	defer releaseOrchestratorLock()

	err := acquireOrchestratorLock()
	if err != nil {
		t.Fatalf("Failed to acquire lock: %v", err)
	}

	// Verify lock file exists
	if !isOrchestratorRunning() {
		t.Error("isOrchestratorRunning should return true after acquiring lock")
	}

	// Try to acquire again - should fail
	err = acquireOrchestratorLock()
	if err == nil {
		t.Error("Should not be able to acquire lock twice")
	}

	// Release lock
	releaseOrchestratorLock()

	// Should be able to acquire again
	err = acquireOrchestratorLock()
	if err != nil {
		t.Fatalf("Should be able to acquire lock after release: %v", err)
	}
}

func TestReleaseOrchestratorLock(t *testing.T) {
	defer releaseOrchestratorLock()

	// Acquire lock
	err := acquireOrchestratorLock()
	if err != nil {
		t.Fatalf("Failed to acquire lock: %v", err)
	}

	// Release it
	releaseOrchestratorLock()

	// Should not be running anymore
	if isOrchestratorRunning() {
		t.Error("isOrchestratorRunning should return false after release")
	}

	// Lock file should be gone
	lockPath := getLockFilePath()
	if _, err := os.Stat(lockPath); err == nil {
		t.Error("Lock file should be removed after release")
	}
}

func TestIsOrchestratorRunning(t *testing.T) {
	defer releaseOrchestratorLock()

	// Should be false initially
	if isOrchestratorRunning() {
		t.Error("isOrchestratorRunning should be false initially")
	}

	// Acquire lock
	err := acquireOrchestratorLock()
	if err != nil {
		t.Fatalf("Failed to acquire lock: %v", err)
	}

	// Should be true now
	if !isOrchestratorRunning() {
		t.Error("isOrchestratorRunning should be true after acquiring lock")
	}

	// Release lock
	releaseOrchestratorLock()

	// Should be false again
	if isOrchestratorRunning() {
		t.Error("isOrchestratorRunning should be false after release")
	}
}

func TestStaleLockFile(t *testing.T) {
	defer releaseOrchestratorLock()

	lockPath := getLockFilePath()

	// Create a stale lock file with an invalid PID
	err := os.WriteFile(lockPath, []byte("999999"), 0644)
	if err != nil {
		t.Fatalf("Failed to create stale lock file: %v", err)
	}

	// Should be able to acquire lock (stale lock is removed)
	err = acquireOrchestratorLock()
	if err != nil {
		t.Fatalf("Should be able to acquire lock with stale lock file: %v", err)
	}

	// Verify we now own the lock
	if !isOrchestratorRunning() {
		t.Error("isOrchestratorRunning should return true after acquiring stale lock")
	}
}

func TestGetLockFilePath(t *testing.T) {
	path := getLockFilePath()

	if path == "" {
		t.Error("Lock file path should not be empty")
	}

	if !contains(path, ".bbbench.orchestrator.lock") {
		t.Errorf("Lock file path should contain lock file name, got: %s", path)
	}
}

func TestSameListener(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.0.0.0:12345", "0.0.0.0:12345", true},
		{"127.0.0.1:12345", "0.0.0.0:12345", true}, // a wildcard covers it
		{":12345", "127.0.0.1:12345", true},
		{"[::]:12345", "10.0.0.1:12345", true},
		{"127.0.0.1:18766", "0.0.0.0:12345", false}, // another port: no collision
		{"127.0.0.1:12345", "127.0.0.2:12345", false},
		{"10.0.0.1:12345", "10.0.0.1:12345", true},
		{"nonsense", "0.0.0.0:12345", true}, // unparsable: assume the worst
	} {
		if got := sameListener(c.a, c.b); got != c.want {
			t.Errorf("sameListener(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// serve next to a running orchestrator: refused only on the same address; the
// orchestrator's recorded address is used, the default for an old lock file.
func TestCheckServeAddr(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if note, err := checkServeAddr("0.0.0.0:12345"); note != "" || err != nil {
		t.Fatalf("no orchestrator: note %q err %v", note, err)
	}
	if err := acquireOrchestratorLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseOrchestratorLock()

	// A lock without an address (an older bbbench) means the default one.
	if _, err := checkServeAddr("0.0.0.0:12345"); err == nil || !contains(err.Error(), "0.0.0.0:12345") {
		t.Errorf("same default address must be refused with the address named: %v", err)
	}
	note, err := checkServeAddr("127.0.0.1:18766")
	if err != nil || !contains(note, "saved results only") || !contains(note, "0.0.0.0:12345") {
		t.Errorf("another address must run next to it with a note: %q, %v", note, err)
	}

	// The recorded address wins.
	recordOrchestratorAddr("10.1.2.3:9000")
	if pid, addr, running := runningOrchestrator(); !running || addr != "10.1.2.3:9000" || pid != os.Getpid() {
		t.Errorf("recorded lock: pid %d addr %q running %v", pid, addr, running)
	}
	if _, err := checkServeAddr("0.0.0.0:12345"); err != nil {
		t.Errorf("the default address is free when the orchestrator listens elsewhere: %v", err)
	}
	if _, err := checkServeAddr("10.1.2.3:9000"); err == nil {
		t.Error("the orchestrator's own address must be refused")
	}
	// The lock is still a valid lock file after the address was added.
	if !isOrchestratorRunning() {
		t.Error("lock with an address must still count as running")
	}
}

// A lock file that carries the web address still refuses a second orchestrator.
func TestLockWithAddrStillLocks(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if err := acquireOrchestratorLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseOrchestratorLock()
	recordOrchestratorAddr("0.0.0.0:12345")
	if err := acquireOrchestratorLock(); err == nil {
		t.Error("a second orchestrator must be refused while the lock names a live pid and an address")
	}
}
