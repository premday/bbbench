package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"bbbench/internal/flushtrace"
	"bbbench/internal/iotrace"
)

// FioJsonOutput represents the structure of fio's JSON output.
type FioJsonOutput struct {
	FioVersion string `json:"fio version"`
	Timestamp  int64  `json:"timestamp"`
	Jobs       []struct {
		JobName string `json:"jobname"`
		Read    struct {
			IoBytes  int64   `json:"io_bytes"`
			BwBytes  int64   `json:"bw_bytes"`
			Iops     float64 `json:"iops"`
			Runtime  int64   `json:"runtime"`
			TotalIos int64   `json:"total_ios"`
		} `json:"read"`
		Write struct {
			IoBytes  int64   `json:"io_bytes"`
			BwBytes  int64   `json:"bw_bytes"`
			Iops     float64 `json:"iops"`
			Runtime  int64   `json:"runtime"`
			TotalIos int64   `json:"total_ios"`
		} `json:"write"`
		Trim struct {
			IoBytes  int64   `json:"io_bytes"`
			BwBytes  int64   `json:"bw_bytes"`
			Iops     float64 `json:"iops"`
			Runtime  int64   `json:"runtime"`
			TotalIos int64   `json:"total_ios"`
		} `json:"trim"`
		// Sync holds fsync completion stats; only populated by jobs that set fsync=.
		Sync struct {
			TotalIos int64 `json:"total_ios"`
			LatNs    struct {
				Mean float64 `json:"mean"`
			} `json:"lat_ns"`
		} `json:"sync"`
	} `json:"jobs"`
}

// fsyncLatency returns the mean fsync latency in microseconds across all jobs,
// weighted by each job's number of fsyncs, and that number. It returns
// (0, 0) when no job issued fsyncs.
func fsyncLatency(o *FioJsonOutput) (meanUs float64, count int64) {
	var sumNs float64
	for _, j := range o.Jobs {
		sumNs += j.Sync.LatNs.Mean * float64(j.Sync.TotalIos)
		count += j.Sync.TotalIos
	}
	if count == 0 {
		return 0, 0
	}
	return sumNs / float64(count) / 1000, count
}

// stripFioPrefix removes any non-JSON preamble that fio may write to the output
// file before the JSON object (e.g. "fio: WARNING: ..."). If source is non-empty,
// each non-blank preamble line is logged as a warning with its line number.
func stripFioPrefix(data []byte, source string) []byte {
	idx := bytes.IndexByte(data, '{')
	if idx <= 0 {
		return data
	}
	if logger != nil && source != "" {
		for lineNum, line := range bytes.Split(bytes.TrimRight(data[:idx], "\n"), []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) > 0 {
				logger.Warn("fio output contained non-JSON preamble",
					"file", source, "line", lineNum+1, "message", string(line))
			}
		}
	}
	return data[idx:]
}

// parseFioJsonOutput parses fio's JSON output. source is the file path used in
// warning logs when fio writes preamble text before the JSON.
func parseFioJsonOutput(jsonData []byte, source string) (*FioJsonOutput, error) {
	var output FioJsonOutput
	if err := json.Unmarshal(stripFioPrefix(jsonData, source), &output); err != nil {
		return nil, fmt.Errorf("unmarshal fio json: %w", err)
	}
	return &output, nil
}

// displayResults shows a summary of all benchmark results.
func displayResults(coordinator *ExecutionCoordinator) error {
	fmt.Println("\n" + repeatString("=", 80))
	if coordinator.isInterrupted() {
		fmt.Println("BENCHMARK RESULTS SUMMARY (INTERRUPTED - PARTIAL RESULTS)")
	} else {
		fmt.Println("BENCHMARK RESULTS SUMMARY")
	}
	fmt.Println(repeatString("=", 80))
	fmt.Println()

	// Sort drives by name for consistent output
	sortedDrives := make([]DriveInfo, len(coordinator.drives))
	copy(sortedDrives, coordinator.drives)
	sort.Slice(sortedDrives, func(i, j int) bool {
		return sortedDrives[i].Device.Name < sortedDrives[j].Device.Name
	})

	for _, drive := range sortedDrives {
		results := coordinator.results[drive.Device.Name]
		if len(results) == 0 {
			continue
		}

		fmt.Printf("Drive: %s (%s %s)\n", drive.Device.Name, drive.Device.Vendor, drive.Device.Model)
		fmt.Printf("%s\n", repeatString("-", 80))

		var totalDuration time.Duration
		successCount := 0
		errorCount := 0

		for _, result := range results {
			duration := result.EndTime.Sub(result.StartTime)
			totalDuration += duration

			if result.Error != nil {
				errorCount++
				fmt.Printf("  Phase %d: ERROR - %v\n", result.Phase, result.Error)
				continue
			}

			if result.Skipped != "" {
				fmt.Printf("  Phase %d: SKIPPED - %s\n", result.Phase, result.Skipped)
				continue
			}

			successCount++

			// Parse and display key metrics
			fioOutput, err := parseFioJsonOutput(result.JsonOutput, result.OutputPath)
			if err != nil {
				fmt.Printf("  Phase %d: WARNING - failed to parse results: %v\n", result.Phase, err)
				continue
			}

			// Aggregate metrics across all jobs in this phase
			var totalReadIops, totalWriteIops, totalTrimIops float64
			var totalReadBw, totalWriteBw, totalTrimBw int64

			for _, job := range fioOutput.Jobs {
				totalReadIops += job.Read.Iops
				totalWriteIops += job.Write.Iops
				totalTrimIops += job.Trim.Iops
				totalReadBw += job.Read.BwBytes
				totalWriteBw += job.Write.BwBytes
				totalTrimBw += job.Trim.BwBytes
			}

			fmt.Printf("  Phase %d: ", result.Phase)
			parts := []string{}

			if totalReadIops > 0 {
				parts = append(parts, fmt.Sprintf("Read: %.0f IOPS (%.1f MB/s)",
					totalReadIops, float64(totalReadBw)/(1024*1024)))
			}
			if totalWriteIops > 0 {
				parts = append(parts, fmt.Sprintf("Write: %.0f IOPS (%.1f MB/s)",
					totalWriteIops, float64(totalWriteBw)/(1024*1024)))
			}
			if totalTrimIops > 0 {
				parts = append(parts, fmt.Sprintf("Trim: %.0f IOPS (%.1f MB/s)",
					totalTrimIops, float64(totalTrimBw)/(1024*1024)))
			}

			if us, n := fsyncLatency(fioOutput); n > 0 {
				parts = append(parts, fmt.Sprintf("fsync avg: %.1f us (%d fsyncs)", us, n))
			}
			if result.FlushTrace != nil {
				parts = append(parts, formatFlushSummary(result.FlushTrace))
			}
			if result.IOTrace != nil {
				parts = append(parts, formatIOTraceSummary(result.IOTrace))
			} else if result.IOTraceNote != "" {
				parts = append(parts, result.IOTraceNote)
			}

			if len(parts) > 0 {
				fmt.Printf("%s (%.1fs)\n", joinStrings(parts, ", "), duration.Seconds())
			} else {
				fmt.Printf("Complete (%.1fs)\n", duration.Seconds())
			}

			fmt.Printf("    Output: %s\n", result.OutputPath)
		}

		fmt.Printf("\nTotal: %d phases, %d successful, %d errors, %.1f minutes\n",
			len(results), successCount, errorCount, totalDuration.Minutes())
		fmt.Println()
	}

	fmt.Println(repeatString("=", 80))
	fmt.Printf("All results saved to: %s\n", coordinator.outputDir)
	fmt.Println(repeatString("=", 80))

	return nil
}

// Helper function to repeat a string n times.
func repeatString(s string, n int) string {
	var result string
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}

// Helper function to join strings with a separator.
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for i := 1; i < len(parts); i++ {
		result += sep + parts[i]
	}
	return result
}

// formatFlushSummary renders a kernel-measured Flush latency for the run
// summary. Percentiles are shown only when the source paired events exactly.
func formatFlushSummary(s *flushtrace.Summary) string {
	if s.Count == 0 {
		if s.WriteCache == "write through" {
			return fmt.Sprintf("Flush (%s): none seen (the kernel sends no Flush to a write-through device: its volatile write cache is disabled or absent)", s.Source)
		}
		return fmt.Sprintf("Flush (%s): none seen", s.Source)
	}
	out := fmt.Sprintf("Flush (%s): avg %.1f us", s.Source, s.MeanNs/1000)
	if s.Exact {
		out += fmt.Sprintf(", p99 %.1f us, max %.1f us", float64(s.P99Ns)/1000, float64(s.MaxNs)/1000)
	}
	return fmt.Sprintf("%s (%d flushes)", out, s.Count)
}

// formatIOTraceSummary renders the kernel-measured device latency of the
// target (the first traced device) for the run summary; an array's members
// are on the pages. A target without request events (an md or dm node is
// bio-based) says so.
func formatIOTraceSummary(s *iotrace.Summary) string {
	var dev map[iotrace.Dir]iotrace.Stats
	if len(s.Devices) > 0 {
		dev = s.Devices[0].Stats[iotrace.KindDevice]
	}
	var dirs []string
	for _, d := range []struct {
		dir  iotrace.Dir
		name string
	}{{iotrace.DirRead, "read"}, {iotrace.DirWrite, "write"}} {
		if st, ok := dev[d.dir]; ok && st.Count > 0 {
			dirs = append(dirs, fmt.Sprintf("%s avg %.1f us p99 %.1f us", d.name, st.MeanNs/1000, float64(st.P99Ns)/1000))
		}
	}
	if len(dirs) == 0 {
		return "ftrace: no request events seen"
	}
	out := "ftrace device: " + strings.Join(dirs, ", ")
	if s.Lost > 0 {
		out += fmt.Sprintf(" (%d events lost)", s.Lost)
	}
	return out
}
