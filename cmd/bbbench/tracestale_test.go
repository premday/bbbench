package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// At orchestrator start the stale instances are removed and named; nothing
// is printed when there are none or tracefs is missing.
func TestRemoveStaleTraces(t *testing.T) {
	var gotRoot string
	remove := func(root string) []string { gotRoot = root; return []string{"bbbench-iolat-4343-1"} }
	var out bytes.Buffer
	removeStaleTraces(func() (string, error) { return "/t", nil }, remove, &out)
	if gotRoot != "/t" || !strings.Contains(out.String(), "bbbench-iolat-4343-1") {
		t.Errorf("root %q, out %q", gotRoot, out.String())
	}

	out.Reset()
	removeStaleTraces(func() (string, error) { return "/t", nil }, func(string) []string { return nil }, &out)
	removeStaleTraces(func() (string, error) { return "", errors.New("no tracefs") }, func(string) []string {
		t.Error("remove called without tracefs")
		return nil
	}, &out)
	if out.Len() != 0 {
		t.Errorf("out = %q", out.String())
	}
}

// doctor names the stale instances and what removes them.
func TestStaleTracesMessage(t *testing.T) {
	if m := staleTracesMessage("/sys/kernel/tracing", nil); m != "" {
		t.Errorf("no stale: %q", m)
	}
	m := staleTracesMessage("/sys/kernel/tracing", []string{"bbbench-flush-12-1", "bbbench-iolat-12-2"})
	for _, want := range []string{"bbbench-flush-12-1", "bbbench-iolat-12-2", "bbbench orchestrator", "rmdir /sys/kernel/tracing/instances/bbbench-flush-12-1"} {
		if !strings.Contains(m, want) {
			t.Errorf("message %q lacks %q", m, want)
		}
	}
}
