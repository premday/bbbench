package main

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"strings"
	"testing"
)

type recordHandler struct{ msgs *[]string }

func (h recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordHandler) Handle(_ context.Context, r slog.Record) error {
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}
func (h recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordHandler) WithGroup(string) slog.Handler      { return h }

// While a TUI owns the terminal, logger and the slog default are held in a
// buffer; release puts the previous ones back and writes what was held.
func TestHoldLogs(t *testing.T) {
	var defMsgs, logMsgs []string
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	def := slog.New(recordHandler{&defMsgs})
	slog.SetDefault(def)
	prevLogger := logger
	t.Cleanup(func() { logger = prevLogger })
	mine := slog.New(recordHandler{&logMsgs})
	logger = mine
	prevOut, prevFlags := log.Writer(), log.Flags()

	var replay bytes.Buffer
	release := holdLogs(&replay)
	slog.Info("from the settings package")
	logger.Warn("from bbbench")
	if len(defMsgs) != 0 || len(logMsgs) != 0 || replay.Len() != 0 {
		t.Fatalf("written while held: %v %v %q", defMsgs, logMsgs, replay.String())
	}
	release()
	if slog.Default() != def {
		t.Error("the slog default handler was not restored")
	}
	if logger != mine {
		t.Error("logger was not restored")
	}
	if log.Writer() != prevOut || log.Flags() != prevFlags {
		t.Error("the log package output was not restored")
	}
	got := replay.String()
	if !strings.Contains(got, "from the settings package") || !strings.Contains(got, "from bbbench") ||
		strings.Index(got, "from the settings package") > strings.Index(got, "from bbbench") {
		t.Errorf("held logs not written in order:\n%s", got)
	}
	slog.Info("after")
	if len(defMsgs) != 1 || defMsgs[0] != "after" {
		t.Errorf("after release: %v", defMsgs)
	}
}
