package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"bbbench/internal/flushtrace"
)

// findTracefs is the tracefs mount of the host.
func findTracefs() (string, error) { return flushtrace.FindTracefs(flushtrace.Options{}) }

// removeStaleTraces removes, once at orchestrator start and whatever the
// flags, the tracefs instances a killed or crashed bbbench left behind: their
// events stay enabled and tax every I/O until they are removed. It names what
// it removed; without tracefs it does nothing.
func removeStaleTraces(find func() (string, error), remove func(root string) []string, w io.Writer) {
	root, err := find()
	if err != nil {
		return
	}
	if removed := remove(root); len(removed) > 0 {
		fmt.Fprintf(w, "Removed trace instances left by a bbbench that is no longer running: %s\n", strings.Join(removed, ", "))
	}
}

// staleTracesMessage is doctor's report of stale trace instances ("" = none).
func staleTracesMessage(root string, stale []string) string {
	if len(stale) == 0 {
		return ""
	}
	cmds := make([]string, len(stale))
	for i, n := range stale {
		cmds[i] = "rmdir " + filepath.Join(root, "instances", n)
	}
	return fmt.Sprintf("trace instances left by a bbbench that is no longer running (their events are still enabled and cost CPU on every I/O): %s; "+
		"the next `bbbench orchestrator` start removes them, or remove them by hand: %s",
		strings.Join(stale, ", "), strings.Join(cmds, "; "))
}

// cleanStaleTraces is removeStaleTraces on the host.
func cleanStaleTraces(w io.Writer) { removeStaleTraces(findTracefs, flushtrace.RemoveStale, w) }
