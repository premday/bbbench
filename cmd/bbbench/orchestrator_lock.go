package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const lockFileName = ".bbbench.orchestrator.lock"

// getLockFilePath returns the path to the orchestrator lock file.
func getLockFilePath() string {
	tmpDir := os.TempDir()
	return filepath.Join(tmpDir, lockFileName)
}

// acquireOrchestratorLock creates a lock file to indicate orchestrator is running.
// Returns an error if another orchestrator instance is already running.
func acquireOrchestratorLock() error {
	lockPath := getLockFilePath()

	// A lock file of a live process (with or without a recorded web address)
	// refuses; anything else is stale and replaced.
	if pid, _, running := runningOrchestrator(); running {
		return fmt.Errorf("orchestrator is already running (PID %d)", pid)
	}
	os.Remove(lockPath)

	// Create new lock file with current PID
	pid := os.Getpid()
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return fmt.Errorf("create lock file: %w", err)
	}

	return nil
}

// releaseOrchestratorLock removes the lock file.
func releaseOrchestratorLock() {
	lockPath := getLockFilePath()
	os.Remove(lockPath)
}

// recordOrchestratorAddr adds the address of the orchestrator's web server to
// its lock file (second line), so a `serve` can tell whether it would collide.
func recordOrchestratorAddr(addr string) {
	_ = os.WriteFile(getLockFilePath(), []byte(strconv.Itoa(os.Getpid())+"\n"+addr+"\n"), 0644)
}

// runningOrchestrator reads the lock file: the pid, the recorded web address
// ("" when the lock predates it) and whether that process is alive.
func runningOrchestrator() (pid int, addr string, running bool) {
	data, err := os.ReadFile(getLockFilePath())
	if err != nil {
		return 0, "", false
	}
	lines := strings.Fields(string(data))
	if len(lines) == 0 {
		return 0, "", false
	}
	pid, err = strconv.Atoi(lines[0])
	if err != nil {
		return 0, "", false
	}
	if len(lines) > 1 {
		addr = lines[1]
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return pid, addr, false
	}
	// Send signal 0 to check if process is alive
	return pid, addr, process.Signal(syscall.Signal(0)) == nil
}

// isOrchestratorRunning checks if an orchestrator instance is currently running.
func isOrchestratorRunning() bool {
	_, _, running := runningOrchestrator()
	return running
}

// sameListener reports whether two listen addresses would collide: the same
// port on the same host, or on a wildcard (0.0.0.0, ::, empty) that covers it.
func sameListener(a, b string) bool {
	ha, pa, errA := net.SplitHostPort(a)
	hb, pb, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil || pa != pb {
		return errA != nil || errB != nil // unparsable: assume the worst
	}
	wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" }
	return ha == hb || wild(ha) || wild(hb)
}

// checkServeAddr is serve's look at a running orchestrator, which serves its
// own web UI for the run: refuse only when `serve` would listen on the same
// address (the orchestrator's recorded one, else the default); on another one
// it may run next to it (saved results only, no live status). The returned
// note is printed when it does.
func checkServeAddr(addr string) (note string, err error) {
	pid, orchAddr, running := runningOrchestrator()
	if !running {
		return "", nil
	}
	if orchAddr == "" {
		orchAddr = defaultWebAddr
	}
	if sameListener(addr, orchAddr) {
		return "", fmt.Errorf("an orchestrator (PID %d) is already serving the web interface on %s\nConnect to http://%s to view its status, or give serve another -addr", pid, orchAddr, orchAddr)
	}
	return fmt.Sprintf("Note: an orchestrator (PID %d) is running with its own web interface on %s; this server shows saved results only, not the live run.", pid, orchAddr), nil
}
