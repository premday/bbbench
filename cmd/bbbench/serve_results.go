package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"bbbench/internal/flushtrace"
)

// BenchmarkResult represents a complete benchmark run.
type BenchmarkResult struct {
	ID        string        `json:"id"`         // Unique identifier (timestamp-based)
	Timestamp time.Time     `json:"timestamp"`  // When the benchmark started
	Mode      string        `json:"mode"`       // parallel-sync or sequential
	Drives    []DriveInfo   `json:"drives"`     // Drives that were benchmarked
	Phases    []PhaseResult `json:"phases"`     // Results for each phase
	OutputDir string        `json:"output_dir"` // Directory containing JSON files
}

// PhaseResult represents results for a single phase.
type PhaseResult struct {
	PhaseNumber int                          `json:"phase_number"`
	PhaseName   string                       `json:"phase_name"`
	Duration    float64                      `json:"duration_seconds"`
	Jobs        []JobResult                  `json:"jobs"`
	FioFiles    map[string]string            `json:"fio_files"`            // device -> fio output file path
	LogSeries   map[string]*DeviceTimeSeries `json:"log_series,omitempty"` // device -> time-series data
	// FlushTrace holds the kernel-measured Flush latency per device, for
	// phases that fsync and where tracing was available.
	FlushTrace map[string]*flushtrace.Summary `json:"flush_trace,omitempty"`
	// Skipped says why the phase was not run, per device (not an error).
	Skipped map[string]string `json:"skipped,omitempty"`
	// RampSec is the fio ramp_time of the phase: fio logs start when it ends.
	RampSec int `json:"ramp_sec,omitempty"`
	// LogAvgMsec is the fio polling interval (--log-avg-msec) of the phase.
	LogAvgMsec int `json:"log_avg_msec,omitempty"`
	// Warnings are caveats about a phase that did run, per device.
	Warnings map[string]string `json:"warnings,omitempty"`
	// FlushSeries is the Flush latency over time (mean µs per second, W
	// column) per device, read back from the stored <prefix>_flush.1.log.
	FlushSeries map[string][]TimePoint `json:"flush_series,omitempty"`
}

// JobResult represents results for a single fio job.
type JobResult struct {
	JobName    string        `json:"job_name"`
	Device     string        `json:"device"`
	ReadStats  *IOStats      `json:"read_stats,omitempty"`
	WriteStats *IOStats      `json:"write_stats,omitempty"`
	TrimStats  *IOStats      `json:"trim_stats,omitempty"`
	Latency    *LatencyStats `json:"latency,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// IOStats represents I/O statistics from fio.
type IOStats struct {
	IOPS      float64 `json:"iops"`
	Bandwidth float64 `json:"bandwidth_kbps"` // KB/s
	AvgLatNS  float64 `json:"avg_lat_ns"`     // Average latency in nanoseconds
	MinLatNS  float64 `json:"min_lat_ns"`
	MaxLatNS  float64 `json:"max_lat_ns"`
	Runtime   float64 `json:"runtime_ms"` // Runtime in milliseconds
}

// LatencyStats represents detailed latency information.
type LatencyStats struct {
	Mean   float64 `json:"mean_ns"`
	StdDev float64 `json:"stddev_ns"`
	P50    float64 `json:"p50_ns"`
	P95    float64 `json:"p95_ns"`
	P99    float64 `json:"p99_ns"`
}

// TimePoint is one data point from a fio time-series log file.
type TimePoint struct {
	T          float64 `json:"t"`           // seconds from phase start
	R          float64 `json:"r"`           // read value (IOPS, MB/s, or µs)
	W          float64 `json:"w"`           // write value
	Incomplete bool    `json:"i,omitempty"` // true if fewer threads contributed than expected
}

// DeviceTimeSeries holds per-device time-series log data for one phase.
type DeviceTimeSeries struct {
	IOPS []TimePoint `json:"iops,omitempty"`
	BW   []TimePoint `json:"bw,omitempty"`  // MB/s
	Lat  []TimePoint `json:"lat,omitempty"` // µs
}

// ResultsStore manages benchmark results.
type ResultsStore struct {
	mu      sync.RWMutex
	results map[string]*BenchmarkResult // keyed by ID
}

var globalResultsStore = &ResultsStore{
	results: make(map[string]*BenchmarkResult),
}

// AddResult adds a benchmark result to the store.
func (rs *ResultsStore) AddResult(result *BenchmarkResult) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.results[result.ID] = result
}

// GetResult retrieves a result by ID.
func (rs *ResultsStore) GetResult(id string) (*BenchmarkResult, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	result, ok := rs.results[id]
	return result, ok
}

// ListResults returns all results sorted by timestamp (newest first).
func (rs *ResultsStore) ListResults() []*BenchmarkResult {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	results := make([]*BenchmarkResult, 0, len(rs.results))
	for _, r := range rs.results {
		results = append(results, r)
	}

	// Sort by timestamp, newest first
	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.After(results[j].Timestamp)
	})

	return results
}

// Clear removes all results from the store.
func (rs *ResultsStore) Clear() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.results = make(map[string]*BenchmarkResult)
}

// LoadResultsFromDir scans a directory for fio JSON output files and loads them.
func LoadResultsFromDir(dir string) error {
	// Expand user home directory
	dir = expandUser(dir)

	// Check if directory exists
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if logger != nil {
			logger.Debug("results directory does not exist", "dir", dir)
		}
		return nil // Not an error, just no results yet
	}

	// Find all JSON files
	pattern := filepath.Join(dir, "*.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob results: %w", err)
	}

	if logger != nil {
		logger.Info("loading results", "dir", dir, "files", len(files))
	}

	for _, file := range files {
		if err := loadFioJSONFile(file); err != nil {
			if logger != nil {
				logger.Error("failed to load fio result", "file", file, "err", err)
			}
			// Continue loading other files
		}
	}

	return nil
}

// loadFioJSONFile parses a fio JSON output file.
func loadFioJSONFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	// Parse fio JSON format
	var fioOutput struct {
		Jobs []struct {
			JobName string `json:"jobname"`
			Read    struct {
				IOPS    float64 `json:"iops"`
				BWBytes int64   `json:"bw_bytes"` // Bandwidth in bytes/sec
				LatNS   struct {
					Mean   float64 `json:"mean"`
					StdDev float64 `json:"stddev"`
					Min    float64 `json:"min"`
					Max    float64 `json:"max"`
				} `json:"lat_ns"`
				Clat struct {
					Percentile map[string]float64 `json:"percentile"`
				} `json:"clat_ns"`
			} `json:"read"`
			Write struct {
				IOPS    float64 `json:"iops"`
				BWBytes int64   `json:"bw_bytes"`
				LatNS   struct {
					Mean   float64 `json:"mean"`
					StdDev float64 `json:"stddev"`
					Min    float64 `json:"min"`
					Max    float64 `json:"max"`
				} `json:"lat_ns"`
				Clat struct {
					Percentile map[string]float64 `json:"percentile"`
				} `json:"clat_ns"`
			} `json:"write"`
			Trim struct {
				IOPS    float64 `json:"iops"`
				BWBytes int64   `json:"bw_bytes"`
			} `json:"trim"`
		} `json:"jobs"`
	}

	if err := json.Unmarshal(data, &fioOutput); err != nil {
		return fmt.Errorf("parse json: %w", err)
	}

	// For now, just log that we parsed it successfully
	if logger != nil {
		logger.Debug("parsed fio output", "file", path, "jobs", len(fioOutput.Jobs))
	}

	// TODO: Store parsed results in globalResultsStore
	// This will be implemented when we integrate with the orchestrator

	return nil
}

// parseFioJSON extracts statistics from fio JSON output. source is the file path
// used in warning logs when fio writes preamble text before the JSON.
func parseFioJSON(data []byte, source string) ([]JobResult, error) {
	data = stripFioPrefix(data, source)
	var fioOutput struct {
		Jobs []struct {
			JobName string `json:"jobname"`
			Read    struct {
				IOPS      float64 `json:"iops"`
				BWBytes   int64   `json:"bw_bytes"`
				RuntimeMS int64   `json:"runtime"`
				LatNS     struct {
					Mean   float64 `json:"mean"`
					StdDev float64 `json:"stddev"`
					Min    float64 `json:"min"`
					Max    float64 `json:"max"`
				} `json:"lat_ns"`
				Clat struct {
					Percentile map[string]float64 `json:"percentile"`
				} `json:"clat_ns"`
			} `json:"read"`
			Write struct {
				IOPS      float64 `json:"iops"`
				BWBytes   int64   `json:"bw_bytes"`
				RuntimeMS int64   `json:"runtime"`
				LatNS     struct {
					Mean   float64 `json:"mean"`
					StdDev float64 `json:"stddev"`
					Min    float64 `json:"min"`
					Max    float64 `json:"max"`
				} `json:"lat_ns"`
				Clat struct {
					Percentile map[string]float64 `json:"percentile"`
				} `json:"clat_ns"`
			} `json:"write"`
			Trim struct {
				IOPS      float64 `json:"iops"`
				BWBytes   int64   `json:"bw_bytes"`
				RuntimeMS int64   `json:"runtime"`
			} `json:"trim"`
		} `json:"jobs"`
	}

	if err := json.Unmarshal(data, &fioOutput); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	results := make([]JobResult, 0, len(fioOutput.Jobs))
	for _, job := range fioOutput.Jobs {
		result := JobResult{
			JobName: job.JobName,
		}

		// Extract read stats
		if job.Read.IOPS > 0 {
			result.ReadStats = &IOStats{
				IOPS:      job.Read.IOPS,
				Bandwidth: float64(job.Read.BWBytes) / 1024, // Convert to KB/s
				AvgLatNS:  job.Read.LatNS.Mean,
				MinLatNS:  job.Read.LatNS.Min,
				MaxLatNS:  job.Read.LatNS.Max,
				Runtime:   float64(job.Read.RuntimeMS),
			}

			// Extract percentiles
			if len(job.Read.Clat.Percentile) > 0 {
				result.Latency = &LatencyStats{
					Mean:   job.Read.LatNS.Mean,
					StdDev: job.Read.LatNS.StdDev,
					P50:    job.Read.Clat.Percentile["50.000000"],
					P95:    job.Read.Clat.Percentile["95.000000"],
					P99:    job.Read.Clat.Percentile["99.000000"],
				}
			}
		}

		// Extract write stats
		if job.Write.IOPS > 0 {
			result.WriteStats = &IOStats{
				IOPS:      job.Write.IOPS,
				Bandwidth: float64(job.Write.BWBytes) / 1024,
				AvgLatNS:  job.Write.LatNS.Mean,
				MinLatNS:  job.Write.LatNS.Min,
				MaxLatNS:  job.Write.LatNS.Max,
				Runtime:   float64(job.Write.RuntimeMS),
			}

			// Use write latency if no read latency
			if result.Latency == nil && len(job.Write.Clat.Percentile) > 0 {
				result.Latency = &LatencyStats{
					Mean:   job.Write.LatNS.Mean,
					StdDev: job.Write.LatNS.StdDev,
					P50:    job.Write.Clat.Percentile["50.000000"],
					P95:    job.Write.Clat.Percentile["95.000000"],
					P99:    job.Write.Clat.Percentile["99.000000"],
				}
			}
		}

		// Extract trim stats
		if job.Trim.IOPS > 0 {
			result.TrimStats = &IOStats{
				IOPS:      job.Trim.IOPS,
				Bandwidth: float64(job.Trim.BWBytes) / 1024,
				Runtime:   float64(job.Trim.RuntimeMS),
			}
		}

		results = append(results, result)
	}

	return results, nil
}
