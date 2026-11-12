package main

import (
	"bytes"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
)

var logger *slog.Logger

// logLevel is the level of logger, from BBBENCH_LOG_LEVEL.
var logLevel = slog.LevelInfo

func initLoggerFromEnv() {
	level := strings.ToLower(os.Getenv("BBBENCH_LOG_LEVEL"))
	if level == "" {
		level = "info"
	}

	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	logLevel = lvl
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// heldLog is the buffer logs go to while a TUI owns the terminal; the TUI's
// commands log from their own goroutines.
type heldLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (h *heldLog) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.Write(p)
}

func (h *heldLog) bytes() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return bytes.Clone(h.buf.Bytes())
}

// holdLogs sends logger, the slog default (the internal packages log through
// it) and the log package into a buffer, so that nothing writes over a TUI
// on the alternate screen. release puts the previous ones back and writes
// what was held to replay (the terminal, once the TUI has left it).
func holdLogs(replay io.Writer) (release func()) {
	held := &heldLog{}
	prevLogger, prevDefault := logger, slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	if logger != nil {
		logger = slog.New(slog.NewTextHandler(held, &slog.HandlerOptions{Level: logLevel}))
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(held, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return func() {
		logger = prevLogger
		slog.SetDefault(prevDefault)
		// SetDefault of a custom handler redirected the log package.
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		if b := held.bytes(); len(b) > 0 {
			_, _ = replay.Write(b)
		}
	}
}
