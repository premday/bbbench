package flushtrace

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// staleKinds are the instance kinds bbbench creates (InstanceName).
var staleKinds = []string{"flush", "iolat"}

// procRoot is where process liveness is read (tests override it).
var procRoot = "/proc"

// RemoveStale removes the instances a bbbench process left behind when it
// was killed or crashed: instances/bbbench-<flush|iolat>-<pid>-<n> whose pid
// is not running (FindStale). Their events are switched off first, then the
// instance is removed. An instance of this process or of a running process is
// never touched, nor is anything when liveness cannot be read (no /proc).
// Liveness is read from /proc of this PID namespace: a bbbench in another
// container sharing the tracefs looks dead. Best effort: it returns the
// instances it removed.
func RemoveStale(root string) []string {
	var removed []string
	for _, name := range FindStale(root) {
		dir := filepath.Join(root, "instances", name)
		writeExisting(filepath.Join(dir, "events", "enable"), "0")
		writeExisting(filepath.Join(dir, "tracing_on"), "0")
		if err := os.Remove(dir); err == nil || errors.Is(err, os.ErrNotExist) {
			removed = append(removed, name)
		}
	}
	return removed
}

// FindStale returns the instances RemoveStale would remove, changing nothing
// (for doctor).
func FindStale(root string) []string {
	if _, err := os.Stat(filepath.Join(procRoot, "self")); err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "instances"))
	if err != nil {
		return nil
	}
	var stale []string
	for _, e := range entries {
		pid, ok := instancePID(e.Name())
		if !ok || pid == os.Getpid() || pidAlive(pid) {
			continue
		}
		stale = append(stale, e.Name())
	}
	return stale
}

// instancePID returns the pid of a bbbench-<kind>-<pid>-<n> instance name.
func instancePID(name string) (int, bool) {
	for _, k := range staleKinds {
		rest, ok := strings.CutPrefix(name, "bbbench-"+k+"-")
		if !ok {
			continue
		}
		p, n, ok := strings.Cut(rest, "-")
		if !ok {
			return 0, false
		}
		pid, err := strconv.Atoi(p)
		if err != nil || pid <= 0 {
			return 0, false
		}
		if _, err := strconv.Atoi(n); err != nil {
			return 0, false
		}
		return pid, true
	}
	return 0, false
}

// pidAlive reports whether a process exists; anything but "does not exist"
// counts as alive.
func pidAlive(pid int) bool {
	_, err := os.Stat(filepath.Join(procRoot, strconv.Itoa(pid)))
	return !errors.Is(err, os.ErrNotExist)
}

// writeExisting writes to a file only if it exists (tracefs control files
// always do; nothing is created).
func writeExisting(path, val string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return
	}
	_, _ = f.WriteString(val)
	_ = f.Close()
}
