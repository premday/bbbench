package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// defaultWebAddr is where the web server listens unless -addr says otherwise
// (serve and orchestrator).
const defaultWebAddr = "0.0.0.0:12345"

// runServe starts the web server for the bbbench GUI.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", defaultWebAddr, "address to bind web server (all interfaces by default)")
	outputDir := fs.String("output", "~/.bbbench/output", "output directory containing benchmark results")
	_ = fs.Parse(args)

	// Check if orchestrator is running
	if isOrchestratorRunning() {
		return errors.New("orchestrator is already running with web interface enabled\nConnect to its web address (default http://" + defaultWebAddr + ", set by its -addr) to view status")
	}

	// Load existing benchmark results from output directory
	if err := loadBenchmarkSummaries(expandUser(*outputDir)); err != nil {
		logger.Warn("failed to load benchmark summaries", "err", err)
		fmt.Printf("Warning: failed to load existing results: %v\n", err)
	}

	if err := loadGroups(expandUser(*outputDir)); err != nil {
		logger.Warn("failed to load run groups", "err", err)
	}

	srv := newWebServer(*addr)

	// Start server in goroutine
	errChan := make(chan error, 1)
	go func() {
		logger.Info("starting web server", "addr", *addr)
		fmt.Printf("Web server starting on http://%s\n", *addr)
		fmt.Printf("Press Ctrl+C to stop\n\n")

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errChan:
		return fmt.Errorf("server error: %w", err)
	case sig := <-sigChan:
		logger.Info("received signal, shutting down", "signal", sig)
		fmt.Println("\nShutting down gracefully...")

		// Graceful shutdown with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			return fmt.Errorf("server shutdown error: %w", err)
		}

		logger.Info("server stopped")
		return nil
	}
}

// newWebServer creates and configures the HTTP server.
func newWebServer(addr string) *http.Server {
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/health", handleHealth)

	// API endpoints
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/status/phases", handleStatusPhases)
	mux.HandleFunc("/api/results", handleResultsList)
	mux.HandleFunc("/api/results/detail", handleResultDetail)
	mux.HandleFunc("/api/results/graphs", handleResultGraphs)
	mux.HandleFunc("/api/results/summary", handleResultSummaryAPI)
	mux.HandleFunc("/api/groups", handleGroupsList)
	mux.HandleFunc("/api/groups/detail", handleGroupDetail)
	mux.HandleFunc("/api/groups/graphs", handleGroupGraphs)

	// HTML pages
	mux.HandleFunc("/status", handleStatusPage)
	mux.HandleFunc("/results", handleBrowsePage)
	mux.HandleFunc("/result", handleResultDetailPage)
	mux.HandleFunc("/graphs", handleGraphsPage)
	mux.HandleFunc("/compare", handleComparePage)
	mux.HandleFunc("/summary", handleSummaryPage)
	mux.HandleFunc("/group", handleGroupPage)
	mux.HandleFunc("/export", handleExportHTML)
	mux.HandleFunc("/", handleIndex)

	return &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// navbarCSS is embedded in every page's <style> block.
const navbarCSS = `
        nav.topnav {
            background: #1e293b;
            display: flex;
            align-items: center;
            padding: 0 24px;
            height: 52px;
            position: sticky;
            top: 0;
            z-index: 100;
            box-shadow: 0 2px 6px rgba(0,0,0,0.2);
        }
        nav.topnav .brand {
            color: #fff;
            font-weight: 700;
            font-size: 18px;
            text-decoration: none;
            margin-right: 32px;
            letter-spacing: 0.3px;
            flex-shrink: 0;
        }
        nav.topnav .nav-links { display: flex; gap: 4px; }
        nav.topnav a.nav-link {
            color: #94a3b8;
            text-decoration: none;
            padding: 6px 14px;
            border-radius: 6px;
            font-size: 14px;
            font-weight: 500;
            transition: background 0.15s, color 0.15s;
        }
        nav.topnav a.nav-link:hover { background: #334155; color: #e2e8f0; }
        nav.topnav a.nav-link.active { background: #2563eb; color: #fff; }
`

// navbarHTML returns the top navigation bar, marking the given path as active.
func navbarHTML(active string) string {
	link := func(href, label string) string {
		cls := "nav-link"
		if href == active {
			cls += " active"
		}
		return fmt.Sprintf(`<a href="%s" class="%s">%s</a>`, href, cls, label)
	}
	return fmt.Sprintf(`<nav class="topnav">
    <a class="brand" href="/">bbbench</a>
    <div class="nav-links">%s%s%s</div>
</nav>`, link("/", "Home"), link("/status", "Status"), link("/results", "Results"))
}

// handleHealth responds to health check requests.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","service":"bbbench"}`)
}

// handleIndex serves the main page.
func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
    <title>bbbench - Block Device Benchmark</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        * { box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            margin: 0;
            padding: 0;
            background: #f5f5f5;
        }
        .page-content {
            max-width: 1200px;
            margin: 0 auto;
            padding: 24px 20px;
        }
        .content {
            background: #fff;
            padding: 24px;
            border-radius: 8px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        h2 { margin-top: 0; color: #333; }
        code { background: #f0f0f0; padding: 2px 6px; border-radius: 3px; font-size: 13px; }
        %s
    </style>
</head>
<body>
%s
    <div class="page-content">
        <div class="content">
            <h2>Welcome</h2>
            <p>The web interface is running.</p>
            <h3>Available Pages:</h3>
            <ul>
                <li><a href="/status">Status</a> - View current benchmark execution status</li>
                <li><a href="/results">Browse Results</a> - Browse and search benchmark results</li>
                <li><a href="/api/results">Results API</a> - List all benchmark results (JSON)</li>
            </ul>
            <p>Use the CLI to start benchmarks: <code>bbbench orchestrator</code></p>
        </div>
    </div>
</body>
</html>`, navbarCSS, navbarHTML("/"))
}

// getListenAddr extracts the actual listening address from the server.
// This is useful for testing when binding to :0 to get a random port.
func getListenAddr(srv *http.Server) (string, error) {
	// Try to extract from listener if server is already running
	if srv.Addr == "" {
		return "", fmt.Errorf("server address not set")
	}
	return srv.Addr, nil
}

// isServerListening checks if a server is listening on the given address.
func isServerListening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// loadBenchmarkSummaries loads all benchmark summary files from the output directory.
func loadBenchmarkSummaries(outputDir string) error {
	// Check if directory exists
	if _, err := os.Stat(outputDir); os.IsNotExist(err) {
		logger.Info("output directory does not exist", "dir", outputDir)
		return nil
	}

	// Find all summary files
	pattern := filepath.Join(outputDir, "run_*_summary.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob summary files: %w", err)
	}

	logger.Info("loading benchmark summaries", "dir", outputDir, "files", len(files))
	fmt.Printf("Loading %d benchmark result(s) from %s\n", len(files), outputDir)

	loadedCount := 0
	for _, file := range files {
		if err := loadBenchmarkSummary(file); err != nil {
			logger.Error("failed to load summary", "file", file, "err", err)
			// Continue loading other files
		} else {
			loadedCount++
		}
	}

	fmt.Printf("Successfully loaded %d benchmark result(s)\n\n", loadedCount)
	return nil
}

// loadBenchmarkSummary loads a single benchmark summary file.
func loadBenchmarkSummary(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	var summary BenchmarkResult
	if err := json.Unmarshal(data, &summary); err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}

	// If phases have fio output files but no parsed jobs (old summary format),
	// read and parse the fio JSON files now to populate job stats for graphs.
	// Also attempt to backfill time-series from log files if present.
	for i := range summary.Phases {
		phase := &summary.Phases[i]
		if len(phase.Jobs) == 0 && len(phase.FioFiles) > 0 {
			for device, fioPath := range phase.FioFiles {
				data, err := os.ReadFile(fioPath)
				if err != nil {
					logger.Debug("read fio output for summary backfill", "file", fioPath, "err", err)
					continue
				}
				jobs, err := parseFioJSON(data, fioPath)
				if err != nil {
					logger.Debug("parse fio json for summary backfill", "file", fioPath, "err", err)
					continue
				}
				for j := range jobs {
					jobs[j].Device = device
				}
				phase.Jobs = append(phase.Jobs, jobs...)
			}
		}
		// Flush samples stored with the fio logs: rebuild the summary and the
		// series from them (also for runs saved before the series existed).
		for device, fioPath := range phase.FioFiles {
			applyFlushLog(phase, device, strings.TrimSuffix(fioPath, ".json")+"_log")
			applyMembersLog(phase, device, strings.TrimSuffix(fioPath, ".json")+"_log")
			applyIOLatLog(phase, device, strings.TrimSuffix(fioPath, ".json")+"_log")
		}
		// Backfill time-series from log files if not already present
		if phase.LogSeries == nil && len(phase.FioFiles) > 0 {
			for device, fioPath := range phase.FioFiles {
				logPrefix := strings.TrimSuffix(fioPath, ".json") + "_log"
				if ts := parseFioLogFiles(logPrefix, 1000); ts != nil {
					if phase.LogSeries == nil {
						phase.LogSeries = make(map[string]*DeviceTimeSeries)
					}
					phase.LogSeries[device] = ts
				}
			}
		}
	}

	// Add to global results store
	globalResultsStore.AddResult(&summary)

	logger.Debug("loaded benchmark summary", "id", summary.ID, "timestamp", summary.Timestamp)
	return nil
}
