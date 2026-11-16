package main

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"bbbench/internal/buildinfo"
	"bbbench/internal/iotrace"
)

// runLogPath is the file a run's output is written to, next to its summary
// (<runID>_summary.json).
func runLogPath(outDir, runID string) string {
	return filepath.Join(outDir, runID+".log")
}

// startRunLog writes everything the run prints (stdout, stderr, and the
// output of logger and of the slog default and log package, which the
// internal packages use) to <outDir>/<runID>.log as well as to the terminal.
// The first line is header, prefixed with "# ". It must be called when no TUI
// owns the terminal. stop restores the streams, waits until everything
// written before it is in the file and closes it; it can be called again.
func startRunLog(outDir, runID, header string) (stop func(), path string, err error) {
	path = runLogPath(outDir, runID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, path, fmt.Errorf("open run log: %w", err)
	}
	if _, err := fmt.Fprintf(f, "# %s\n", header); err != nil {
		_ = f.Close()
		return nil, path, fmt.Errorf("write run log: %w", err)
	}
	ro, wo, err := os.Pipe()
	if err != nil {
		_ = f.Close()
		return nil, path, fmt.Errorf("run log pipe: %w", err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		_ = f.Close()
		_ = ro.Close()
		_ = wo.Close()
		return nil, path, fmt.Errorf("run log pipe: %w", err)
	}

	origOut, origErr := os.Stdout, os.Stderr
	prevLogger, prevDefault := logger, slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()

	var (
		mu sync.Mutex // keeps one write of each stream whole in the file
		wg sync.WaitGroup
	)
	pump := func(r *os.File, terminal io.Writer) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				mu.Lock()
				_, _ = terminal.Write(buf[:n])
				_, _ = f.Write(buf[:n])
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(ro, origOut)
	go pump(re, origErr)

	os.Stdout, os.Stderr = wo, we
	if logger != nil {
		logger = slog.New(slog.NewTextHandler(we, &slog.HandlerOptions{Level: logLevel}))
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(we, &slog.HandlerOptions{Level: slog.LevelInfo})))

	var once sync.Once
	stop = func() {
		once.Do(func() {
			os.Stdout, os.Stderr = origOut, origErr
			logger = prevLogger
			slog.SetDefault(prevDefault)
			// SetDefault of a custom handler redirected the log package.
			log.SetOutput(prevOut)
			log.SetFlags(prevFlags)
			_ = wo.Close()
			_ = we.Close()
			wg.Wait()
			_ = ro.Close()
			_ = re.Close()
			_ = f.Close()
		})
	}
	return stop, path, nil
}

// runLogHeader is the first line of a run log: what produced it.
func runLogHeader(opts runOptions) string {
	ioTrace := "off"
	if opts.IOTrace {
		ioTrace = string(opts.IOTraceMode)
		if ioTrace == "" {
			ioTrace = string(iotrace.ModeAuto)
		}
	}
	return fmt.Sprintf("bbbench %s, args: %s, mode: %s, io-trace: %s", buildinfo.Version, strings.Join(os.Args[1:], " "), opts.Mode, ioTrace)
}
