package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapStd replaces os.Stdout and os.Stderr by pipes read into the returned
// collectors and restores them with the returned function.
func swapStd(t *testing.T) (out, errb *strings.Builder, restore func()) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	out, errb = &strings.Builder{}, &strings.Builder{}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(out, ro); done <- struct{}{} }()
	go func() { _, _ = io.Copy(errb, re); done <- struct{}{} }()
	return out, errb, func() {
		_ = wo.Close()
		_ = we.Close()
		<-done
		<-done
		os.Stdout, os.Stderr = oldOut, oldErr
	}
}

func TestRunLogTeesStdoutStderrAndSlog(t *testing.T) {
	dir := t.TempDir()
	out, errb, restore := swapStd(t)
	prevLogger, prevDefault := logger, slog.Default()
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	stop, path, err := startRunLog(dir, "run_42", "bbbench test header")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "run_42.log") {
		t.Fatalf("path %q", path)
	}
	fmt.Println("to stdout")
	fmt.Fprintln(os.Stderr, "to stderr")
	slog.Info("hello from slog")
	logger.Info("hello from logger")
	stop()
	stop() // idempotent
	logger = prevLogger
	slog.SetDefault(prevDefault)
	restore()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "# bbbench test header\n") {
		t.Errorf("header missing: %q", got)
	}
	for _, want := range []string{"to stdout", "to stderr", "hello from slog", "hello from logger"} {
		if !strings.Contains(got, want) {
			t.Errorf("run log lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(out.String(), "to stdout") {
		t.Errorf("terminal stdout lost: %q", out.String())
	}
	if !strings.Contains(errb.String(), "to stderr") || !strings.Contains(errb.String(), "hello from slog") {
		t.Errorf("terminal stderr lost: %q", errb.String())
	}
}

func TestRunLogCompleteWhenStoppedRightAfterWriting(t *testing.T) {
	dir := t.TempDir()
	_, _, restore := swapStd(t)
	stop, path, err := startRunLog(dir, "run_43", "h")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		fmt.Printf("line %d\n", i)
	}
	stop()
	restore()
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "line "); n != 2000 {
		t.Fatalf("run log has %d of 2000 lines", n)
	}
}

func TestRunLogUnwritableDirIsAnError(t *testing.T) {
	if _, _, err := startRunLog(filepath.Join(t.TempDir(), "missing", "dir"), "run_1", "h"); err == nil {
		t.Fatal("want an error for an unwritable directory")
	}
}

func TestCloseRunLogFlushesBeforeExit(t *testing.T) {
	dir := t.TempDir()
	_, _, restore := swapStd(t)
	_, path, err := startRunLog(dir, "run_44", "h")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("Force quitting...")
	closeRunLog() // what the force-quit path calls before exit
	closeRunLog() // nothing left to close
	restore()
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "Force quitting...") {
		t.Errorf("run log lost the last line: %q", data)
	}
}
