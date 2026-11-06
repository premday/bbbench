package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"bbbench/internal/diskinfo"
	"bbbench/internal/flushtrace"
)

// ExecutionCoordinator manages benchmark execution across multiple drives.
type ExecutionCoordinator struct {
	drives      []DriveInfo
	workloads   map[string]*FioWorkload // keyed by device name
	mode        string
	outputDir   string
	dryRun      bool
	verbose     bool
	results     map[string][]*FioResult
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	interrupted bool
	tempFiles   []string
	tempFilesMu sync.Mutex
	stateFile   string
	resumeState *ExecutionState
	startTime   time.Time
	liveResult  *BenchmarkResult // updated after each phase for live web view
	logAvgMsec  int              // fio --log_avg_msec value (0 → default 1000ms)
	flushTrace  bool             // trace device Flush latency during phases that fsync

	skipWriteThrough bool   // do not run fsync phases on write-through devices (default: warn)
	sysBlock         string // sysfs block dir override for tests ("" = /sys/block)

	// collectDisk reads a drive's configuration and its text dump (nil:
	// diskinfo.CollectDump).
	collectDisk func(drive DriveInfo) (diskinfo.Info, string)
}

// fioStampSlackMs is how early (ms) a fio log stamp can be for its window.
const fioStampSlackMs = 10

// defaultLogAvgMsec is fio's log averaging interval when none is set (the
// --log-avg-msec default); also the Flush series window for logs that do not
// record one.
const defaultLogAvgMsec = flushtrace.DefaultWindowMs

// avgMsec is the fio log averaging interval of the run.
func (ec *ExecutionCoordinator) avgMsec() int {
	if ec.logAvgMsec <= 0 {
		return defaultLogAvgMsec
	}
	return ec.logAvgMsec
}

// coalesceMsec returns the timestamp bucket size used when coalescing log files.
// It is logAvgMsec/10, with a minimum of 1000ms (1 second).
func (ec *ExecutionCoordinator) coalesceMsec() int64 {
	threshold := int64(ec.avgMsec()) / 10
	if threshold < 1000 {
		threshold = 1000
	}
	return threshold
}

// FioResult represents the result of a single fio execution.
type FioResult struct {
	DeviceName string
	JobName    string
	Phase      int
	OutputPath string
	LogPrefix  string // base prefix for fio time-series log files
	JsonOutput []byte
	Error      error
	StartTime  time.Time
	EndTime    time.Time
	// FlushTrace is the device Flush latency measured from kernel trace
	// events, for phases that issue fsyncs. Nil when not measured.
	FlushTrace *flushtrace.Summary
	// Skipped is why the phase was not run on this device (not an error).
	Skipped string
	// Warning is a caveat about a phase that did run (e.g. no Flush to trace).
	Warning string
}

// checkFioInstalled verifies that the fio binary is available in PATH.
func checkFioInstalled() error {
	path, err := exec.LookPath("fio")
	if err != nil {
		return fmt.Errorf("fio is not installed or not in PATH. Please install fio before running benchmarks")
	}
	logger.Debug("found fio binary", "path", path)
	return nil
}

// validateFioFile validates a fio file using fio --parse-only.
func validateFioFile(path string) error {
	cmd := exec.Command("fio", "--parse-only", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Include fio's error output in the error message
		return fmt.Errorf("fio validation failed: %w\nfio output:\n%s", err, string(output))
	}
	logger.Debug("validated fio file", "path", path)
	return nil
}

// newExecutionCoordinator creates a new coordinator and parses all fio files.
func newExecutionCoordinator(drives []DriveInfo, mode, outputDir string) (*ExecutionCoordinator, error) {
	// Check if fio is installed
	if err := checkFioInstalled(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	ec := &ExecutionCoordinator{
		drives:    drives,
		workloads: make(map[string]*FioWorkload),
		mode:      mode,
		outputDir: outputDir,
		results:   make(map[string][]*FioResult),
		ctx:       ctx,
		cancel:    cancel,
		startTime: time.Now(),
	}

	// Set up signal handling for graceful shutdown
	go ec.handleSignals()

	// Parse and validate all fio files
	for _, drive := range drives {
		// Validate fio file exists
		if _, err := os.Stat(drive.FioFilePath); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("fio configuration file not found for drive %s: %s\n"+
					"Expected file: %s\n"+
					"This should have been auto-generated. Please report this issue.",
					drive.Device.Name, drive.Device.Model, drive.FioFilePath)
			}
			return nil, fmt.Errorf("check fio file for %s: %w", drive.Device.Name, err)
		}

		workload, err := parseFioFile(drive.FioFilePath)
		if err != nil {
			return nil, fmt.Errorf("parse fio file for %s: %w", drive.Device.Name, err)
		}

		// Validate fio file syntax using fio --parse-only
		if err := validateFioFile(drive.FioFilePath); err != nil {
			return nil, fmt.Errorf("validate fio file for %s (%s): %w", drive.Device.Name, drive.FioFilePath, err)
		}

		ec.workloads[drive.Device.Name] = workload
		logger.Info("parsed fio file",
			"device", drive.Device.Name,
			"jobs", len(workload.Jobs),
			"phases", len(workload.Phases))
	}

	return ec, nil
}

// exitCodeSIGINT is the conventional exit code for processes terminated by SIGINT (128 + signal number).
const exitCodeSIGINT = 128 + int(syscall.SIGINT)

// handleSignals sets up signal handling for graceful shutdown.
func (ec *ExecutionCoordinator) handleSignals() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	fmt.Println("\n\nReceived interrupt signal. Gracefully shutting down...")
	fmt.Println("Waiting for current phase to complete...")
	fmt.Println("Press Ctrl+C again to force quit (may leave temporary files)")

	ec.mu.Lock()
	ec.interrupted = true
	ec.mu.Unlock()

	ec.cancel()

	// Set up second signal handler for force quit
	go func() {
		<-sigChan
		fmt.Println("\nForce quitting...")
		ec.cleanup()
		os.Exit(exitCodeSIGINT)
	}()
}

// cleanup removes temporary files.
func (ec *ExecutionCoordinator) cleanup() {
	ec.tempFilesMu.Lock()
	defer ec.tempFilesMu.Unlock()

	for _, tmpFile := range ec.tempFiles {
		if err := os.Remove(tmpFile); err != nil {
			logger.Debug("cleanup temp file", "path", tmpFile, "err", err)
		}
	}
}

// registerTempFile adds a temporary file to the cleanup list.
func (ec *ExecutionCoordinator) registerTempFile(path string) {
	ec.tempFilesMu.Lock()
	defer ec.tempFilesMu.Unlock()
	ec.tempFiles = append(ec.tempFiles, path)
}

// unregisterTempFile removes a temporary file from the cleanup list.
func (ec *ExecutionCoordinator) unregisterTempFile(path string) {
	ec.tempFilesMu.Lock()
	defer ec.tempFilesMu.Unlock()
	for i, f := range ec.tempFiles {
		if f == path {
			ec.tempFiles = append(ec.tempFiles[:i], ec.tempFiles[i+1:]...)
			break
		}
	}
}

// parseFioLogFiles reads all three fio log types for a given prefix and returns
// time-series data, or nil if no log files exist.
func parseFioLogFiles(prefix string, thresholdMs int64) *DeviceTimeSeries {
	ts := &DeviceTimeSeries{
		// IOPS and BW: sum across jobs (each job contributes to total throughput).
		// Lat: average across jobs (each job's latency is an independent observation).
		IOPS: parseFioSingleLog(prefix, "iops", 1.0, false, thresholdMs),      // IOPS as-is, sum jobs
		BW:   parseFioSingleLog(prefix, "bw", 1.0/1024.0, false, thresholdMs), // KiB/s → MB/s, sum jobs
		Lat:  parseFioSingleLog(prefix, "lat", 1.0/1000.0, true, thresholdMs), // ns → µs, avg jobs
	}
	if len(ts.IOPS) == 0 && len(ts.BW) == 0 && len(ts.Lat) == 0 {
		return nil
	}
	return ts
}

// parseFioSingleLog parses fio log files matching <prefix>_<kind>.*.log.
// Lines format: time_ms, value, direction(0=R 1=W 2=T), block_size, offset, ...
//
// fio writes one log entry per I/O (without log_avg_msec) or one entry per
// averaging window (with log_avg_msec). In both cases, multiple entries in the
// same file at the same timestamp must be averaged (not summed) because they are
// repeated samples of the same rate/latency, not independent contributions.
//
// avgAcrossFiles controls how per-file averages are combined:
//   - false (IOPS, BW): sum per-file averages → total across all jobs
//   - true  (Lat):      average per-file averages → mean latency across jobs
func parseFioSingleLog(prefix, kind string, scale float64, avgAcrossFiles bool, thresholdMs int64) []TimePoint {
	files, _ := filepath.Glob(prefix + "_" + kind + ".*.log")
	if len(files) == 0 {
		// Legacy fio: no job number suffix
		alt := prefix + "_" + kind + ".log"
		if _, err := os.Stat(alt); err == nil {
			files = []string{alt}
		}
	}
	if len(files) == 0 {
		return nil
	}

	// combined[tms] accumulates the per-file averages across all files.
	type combined struct {
		rSum, wSum     float64 // sum of per-file averages
		rFiles, wFiles int     // number of files that contributed (for avgAcrossFiles)
	}
	global := make(map[int64]*combined)

	type acc struct {
		rSum, wSum float64
		rN, wN     int
	}

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}

		// First pass: collect per-timestamp sums and counts for this file.
		perTime := make(map[int64]*acc)
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, ",", 5)
			if len(parts) < 3 {
				continue
			}
			tms, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
			val, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
			dir, err3 := strconv.Atoi(strings.TrimSpace(parts[2]))
			if err1 != nil || err2 != nil || err3 != nil {
				continue
			}
			a := perTime[tms]
			if a == nil {
				a = &acc{}
				perTime[tms] = a
			}
			v := val * scale
			switch dir {
			case 0:
				a.rSum += v
				a.rN++
			case 1:
				a.wSum += v
				a.wN++
			}
		}

		// Second pass: coalesce this file's timestamps into per-bucket accumulators,
		// then commit to global.  Doing it in two steps ensures each file increments
		// rFiles/wFiles exactly once per bucket, even when multiple sub-interval
		// timestamps from the same file land in the same bucket (e.g. when
		// log_avg_msec < thresholdMs).
		type bucketAcc struct {
			rSum, wSum float64
			rN, wN     int
		}
		fileBuckets := make(map[int64]*bucketAcc, len(perTime))
		for tms, a := range perTime {
			// Floor, with a few ms of slack: fio stamps a window late (up to
			// about a second after the boundary, when several jobs log) or a
			// millisecond early (9999 for 10 s). Plain flooring sends the early
			// stamp to the previous bucket, merging two windows and leaving a gap.
			bucket := ((tms + fioStampSlackMs) / thresholdMs) * thresholdMs
			ba := fileBuckets[bucket]
			if ba == nil {
				ba = &bucketAcc{}
				fileBuckets[bucket] = ba
			}
			if a.rN > 0 {
				ba.rSum += a.rSum / float64(a.rN)
				ba.rN++
			}
			if a.wN > 0 {
				ba.wSum += a.wSum / float64(a.wN)
				ba.wN++
			}
		}
		for bucket, ba := range fileBuckets {
			c := global[bucket]
			if c == nil {
				c = &combined{}
				global[bucket] = c
			}
			if ba.rN > 0 {
				c.rSum += ba.rSum / float64(ba.rN)
				c.rFiles++
			}
			if ba.wN > 0 {
				c.wSum += ba.wSum / float64(ba.wN)
				c.wFiles++
			}
		}
	}

	if len(global) == 0 {
		return nil
	}

	buckets := make([]int64, 0, len(global))
	for b := range global {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })

	nFiles := int64(len(files))
	points := make([]TimePoint, len(buckets))
	for i, b := range buckets {
		c := global[b]
		r := c.rSum
		w := c.wSum
		if avgAcrossFiles {
			if c.rFiles > 0 {
				r /= float64(c.rFiles)
			}
			if c.wFiles > 0 {
				w /= float64(c.wFiles)
			}
		}
		// Mark incomplete if any active direction has fewer contributors than expected.
		incomplete := (c.rFiles > 0 && int64(c.rFiles) < nFiles) ||
			(c.wFiles > 0 && int64(c.wFiles) < nFiles)
		points[i] = TimePoint{T: float64(b) / 1000.0, R: r, W: w, Incomplete: incomplete}
	}
	return points
}

// updateLivePhase appends completed phase data to the live result in the store.
func (ec *ExecutionCoordinator) updateLivePhase(phaseIdx int) {
	if ec.liveResult == nil {
		return
	}

	phaseResult := PhaseResult{
		PhaseNumber: phaseIdx + 1,
		FioFiles:    make(map[string]string),
		LogAvgMsec:  ec.avgMsec(),
	}

	for _, drive := range ec.drives {
		deviceName := drive.Device.Name
		workload := ec.workloads[deviceName]
		if phaseIdx >= len(workload.Phases) {
			continue
		}
		phase := workload.Phases[phaseIdx]
		if len(phase) == 0 {
			continue
		}

		if sec := phaseRampMs(workload, phase) / 1000; sec > phaseResult.RampSec {
			phaseResult.RampSec = sec
		}
		for _, result := range ec.results[deviceName] {
			if result.Phase == phaseIdx && result.Error == nil {
				if result.Warning != "" {
					if phaseResult.Warnings == nil {
						phaseResult.Warnings = make(map[string]string)
					}
					phaseResult.Warnings[deviceName] = result.Warning
				}
				if result.Skipped != "" {
					if phaseResult.Skipped == nil {
						phaseResult.Skipped = make(map[string]string)
					}
					phaseResult.Skipped[deviceName] = result.Skipped
				}
				if result.OutputPath != "" {
					phaseResult.FioFiles[deviceName] = result.OutputPath
				}
				if result.FlushTrace != nil {
					if phaseResult.FlushTrace == nil {
						phaseResult.FlushTrace = make(map[string]*flushtrace.Summary)
					}
					phaseResult.FlushTrace[deviceName] = result.FlushTrace
				}
				if result.LogPrefix != "" {
					// Same path as reloading a saved run: from the stored log.
					applyFlushLog(&phaseResult, deviceName, result.LogPrefix)
				}
				if len(result.JsonOutput) > 0 {
					jobs, err := parseFioJSON(result.JsonOutput, result.OutputPath)
					if err == nil {
						for j := range jobs {
							jobs[j].Device = deviceName
						}
						phaseResult.Jobs = append(phaseResult.Jobs, jobs...)
					}
				}
				if result.LogPrefix != "" {
					if ts := parseFioLogFiles(result.LogPrefix, ec.coalesceMsec()); ts != nil {
						if phaseResult.LogSeries == nil {
							phaseResult.LogSeries = make(map[string]*DeviceTimeSeries)
						}
						phaseResult.LogSeries[deviceName] = ts
					}
				}
				break
			}
		}

		if phaseResult.PhaseName == "" && len(phase) > 0 {
			phaseResult.PhaseName = phase[0].Name
		}
	}

	ec.liveResult.Phases = append(ec.liveResult.Phases, phaseResult)
	globalResultsStore.AddResult(ec.liveResult)
}

// executeParallelSync runs all drives phase-by-phase with synchronization.
func (ec *ExecutionCoordinator) executeParallelSync() error {
	// Find maximum number of phases across all drives
	maxPhases := 0
	for _, workload := range ec.workloads {
		if len(workload.Phases) > maxPhases {
			maxPhases = len(workload.Phases)
		}
	}

	logger.Info("parallel-sync execution", "phases", maxPhases, "drives", len(ec.drives))
	fmt.Printf("Executing %d phase(s) across %d drive(s)\n\n", maxPhases, len(ec.drives))

	// Initialize web status
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = true
		s.StartTime = ec.startTime
		s.Mode = ec.mode
		s.TotalPhases = maxPhases
		s.CompletedPhases = 0
		s.CurrentPhase = 0
		s.phaseConfigs = ec.statusPhaseConfigs()
		// Build drive status list
		s.Drives = make([]DriveStatus, len(ec.drives))
		for i, drive := range ec.drives {
			driveType := "SSD"
			if drive.Device.Rotational {
				driveType = "HDD"
			}
			capacityGB := drive.Device.CapacityBytes / (1024 * 1024 * 1024)
			s.Drives[i] = DriveStatus{
				Name:         drive.Device.Name,
				Vendor:       drive.Device.Vendor,
				Model:        drive.Device.Model,
				Serial:       drive.Device.Serial,
				Capacity:     capacityGB,
				Type:         driveType,
				Status:       "pending",
				CurrentPhase: 0,
			}
		}
	})

	ec.collectDiskConfig()

	// Execute each phase
	for phaseIdx := 0; phaseIdx < maxPhases; phaseIdx++ {
		// Check for interruption
		select {
		case <-ec.ctx.Done():
			fmt.Println("\nExecution interrupted. Cleaning up...")
			ec.cleanup()
			ec.saveState()
			return fmt.Errorf("execution interrupted by user")
		default:
		}

		// Check if all drives completed this phase (for resume)
		allCompleted := true
		for _, drive := range ec.drives {
			workload := ec.workloads[drive.Device.Name]
			if phaseIdx < len(workload.Phases) && !ec.isPhaseCompleted(drive.Device.Name, phaseIdx) {
				allCompleted = false
				break
			}
		}

		if allCompleted {
			fmt.Printf("Phase %d/%d: SKIPPED (already completed)\n", phaseIdx+1, maxPhases)
			continue
		}

		// Get phase name from first drive's workload for display
		phaseName := ""
		for _, drive := range ec.drives {
			workload := ec.workloads[drive.Device.Name]
			if phaseIdx < len(workload.Phases) && len(workload.Phases[phaseIdx]) > 0 {
				// Use first job name from the phase
				jobNames := make([]string, len(workload.Phases[phaseIdx]))
				for i, job := range workload.Phases[phaseIdx] {
					jobNames[i] = job.Name
				}
				phaseName = strings.Join(jobNames, ", ")
				break
			}
		}

		if phaseName != "" {
			fmt.Printf("Phase %d/%d (%s): ", phaseIdx+1, maxPhases, phaseName)
		} else {
			fmt.Printf("Phase %d/%d: ", phaseIdx+1, maxPhases)
		}

		// Update web status for current phase
		UpdateStatus(func(s *BenchmarkStatus) {
			s.CurrentPhase = phaseIdx + 1
			// Mark all drives as running this phase
			for i := range s.Drives {
				s.Drives[i].Status = "running"
				s.Drives[i].clearNotes()
				s.Drives[i].CurrentPhase = phaseIdx + 1
				s.Drives[i].PhaseName = phaseName
				s.Drives[i].PhaseStart = time.Now()
			}
		})

		// Count active drives so we know when all have finished setup.
		activeCount := 0
		for _, drive := range ec.drives {
			workload := ec.workloads[drive.Device.Name]
			if phaseIdx < len(workload.Phases) && !ec.isPhaseCompleted(drive.Device.Name, phaseIdx) {
				activeCount++
			}
		}

		var wg sync.WaitGroup
		var phaseErrors []error
		var mu sync.Mutex

		// setupDone is buffered so goroutines never block signalling readiness.
		setupDone := make(chan struct{}, activeCount)
		startGate := make(chan struct{})

		for _, drive := range ec.drives {
			workload := ec.workloads[drive.Device.Name]

			// Skip if this drive has fewer phases
			if phaseIdx >= len(workload.Phases) {
				continue
			}

			// Skip if already completed in previous run
			if ec.isPhaseCompleted(drive.Device.Name, phaseIdx) {
				continue
			}

			ec.noteFlushGate(drive.Device.Name, workload.Phases[phaseIdx])

			wg.Add(1)
			go func(drv DriveInfo, wl *FioWorkload, idx int) {
				defer wg.Done()

				phase := wl.Phases[idx]
				result := ec.executePhase(drv, phase, idx, setupDone, startGate)

				ec.mu.Lock()
				ec.results[drv.Device.Name] = append(ec.results[drv.Device.Name], result)
				ec.mu.Unlock()

				if result.Error != nil {
					mu.Lock()
					phaseErrors = append(phaseErrors, fmt.Errorf("%s: %w", drv.Device.Name, result.Error))
					mu.Unlock()
				}
			}(drive, workload, phaseIdx)
		}

		// Wait for all goroutines to finish setup, then open the gate so all
		// fio processes start at the same time.
		for i := 0; i < activeCount; i++ {
			<-setupDone
		}
		startTime := time.Now()
		close(startGate)

		wg.Wait()
		elapsed := time.Since(startTime)

		// Update completed phases count and live result
		UpdateStatus(func(s *BenchmarkStatus) {
			s.CompletedPhases = phaseIdx + 1
		})
		ec.updateLivePhase(phaseIdx)

		if len(phaseErrors) > 0 {
			fmt.Printf("FAILED (%.1fs) - errors:\n", elapsed.Seconds())
			for _, err := range phaseErrors {
				fmt.Printf("  - %v\n", err)
			}
		} else if skipped := ec.skippedInPhase(phaseIdx); len(skipped) > 0 {
			if len(skipped) >= activeCount {
				fmt.Printf("SKIPPED on %d drive(s):\n", len(skipped))
			} else {
				fmt.Printf("COMPLETE (%.1fs), skipped on %d drive(s):\n", elapsed.Seconds(), len(skipped))
			}
			for _, s := range skipped {
				fmt.Printf("  - %s\n", s)
			}
		} else {
			fmt.Printf("COMPLETE (%.1fs)\n", elapsed.Seconds())
		}
		ec.printPhaseWarnings(phaseIdx, "  ")

		// Save state after each phase
		if err := ec.saveState(); err != nil {
			logger.Error("save state", "err", err)
		} else if ec.verbose {
			fmt.Printf("  [Saved state]\n")
		}
	}

	ec.cleanup()
	ec.cleanupStateFile()
	fmt.Println("\nAll phases complete!")

	// Mark all drives as complete
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = false
		for i := range s.Drives {
			s.Drives[i].Status = "complete"
			s.Drives[i].PhaseName = ""
		}
	})

	return nil
}

// executeSequential runs benchmarks on drives one at a time.
func (ec *ExecutionCoordinator) executeSequential() error {
	logger.Info("sequential execution", "drives", len(ec.drives))
	fmt.Printf("Executing benchmarks sequentially on %d drive(s)\n\n", len(ec.drives))

	// Count total phases across all drives
	totalPhases := 0
	for _, workload := range ec.workloads {
		totalPhases += len(workload.Phases)
	}

	// Initialize web status
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = true
		s.StartTime = ec.startTime
		s.Mode = ec.mode
		s.TotalPhases = totalPhases
		s.CompletedPhases = 0
		s.CurrentPhase = 0
		s.phaseConfigs = ec.statusPhaseConfigs()
		// Build drive status list
		s.Drives = make([]DriveStatus, len(ec.drives))
		for i, drive := range ec.drives {
			driveType := "SSD"
			if drive.Device.Rotational {
				driveType = "HDD"
			}
			capacityGB := drive.Device.CapacityBytes / (1024 * 1024 * 1024)
			s.Drives[i] = DriveStatus{
				Name:         drive.Device.Name,
				Vendor:       drive.Device.Vendor,
				Model:        drive.Device.Model,
				Serial:       drive.Device.Serial,
				Capacity:     capacityGB,
				Type:         driveType,
				Status:       "pending",
				CurrentPhase: 0,
			}
		}
	})

	ec.collectDiskConfig()

	completedPhases := 0

	for i, drive := range ec.drives {
		// Check for interruption
		select {
		case <-ec.ctx.Done():
			fmt.Println("\nExecution interrupted. Cleaning up...")
			ec.cleanup()
			return fmt.Errorf("execution interrupted by user")
		default:
		}

		fmt.Printf("Drive %d/%d: %s\n", i+1, len(ec.drives), drive.Device.Name)

		// Mark this drive as active
		UpdateStatus(func(s *BenchmarkStatus) {
			for j := range s.Drives {
				if s.Drives[j].Name == drive.Device.Name {
					s.Drives[j].Status = "running"
					break
				}
			}
		})

		workload := ec.workloads[drive.Device.Name]
		startTime := time.Now()

		for phaseIdx, phase := range workload.Phases {
			// Check for interruption before each phase
			select {
			case <-ec.ctx.Done():
				fmt.Println("\n  Interrupted during execution. Cleaning up...")
				ec.cleanup()
				ec.saveState()
				return fmt.Errorf("execution interrupted by user")
			default:
			}

			// Skip if already completed in previous run
			if ec.isPhaseCompleted(drive.Device.Name, phaseIdx) {
				fmt.Printf("  Phase %d/%d: SKIPPED (already completed)\n", phaseIdx+1, len(workload.Phases))
				continue
			}

			// Get phase name from job names
			jobNames := make([]string, len(phase))
			for i, job := range phase {
				jobNames[i] = job.Name
			}
			phaseName := strings.Join(jobNames, ", ")

			fmt.Printf("  Phase %d/%d (%s): ", phaseIdx+1, len(workload.Phases), phaseName)
			phaseStart := time.Now()

			// Update web status - mark this drive as running this phase
			UpdateStatus(func(s *BenchmarkStatus) {
				s.CurrentPhase = completedPhases + 1
				for j := range s.Drives {
					if s.Drives[j].Name == drive.Device.Name {
						s.Drives[j].CurrentPhase = phaseIdx + 1
						s.Drives[j].PhaseName = phaseName
						s.Drives[j].PhaseStart = time.Now()
						s.Drives[j].Status = "running"
						s.Drives[j].clearNotes()
						break
					}
				}
			})

			ec.noteFlushGate(drive.Device.Name, phase)
			result := ec.executePhase(drive, phase, phaseIdx, nil, nil)

			ec.mu.Lock()
			ec.results[drive.Device.Name] = append(ec.results[drive.Device.Name], result)
			ec.mu.Unlock()

			elapsed := time.Since(phaseStart)
			if result.Error != nil {
				fmt.Printf("FAILED (%.1fs) - %v\n", elapsed.Seconds(), result.Error)
			} else if result.Skipped != "" {
				fmt.Printf("SKIPPED - %s\n", result.Skipped)
			} else {
				fmt.Printf("COMPLETE (%.1fs)\n", elapsed.Seconds())
			}
			ec.printPhaseWarnings(phaseIdx, "    ")

			// Update completed phases count and live result
			completedPhases++
			UpdateStatus(func(s *BenchmarkStatus) {
				s.CompletedPhases = completedPhases
			})
			ec.updateLivePhase(phaseIdx)

			// Save state after each phase
			if err := ec.saveState(); err != nil {
				logger.Error("save state", "err", err)
			} else if ec.verbose {
				fmt.Printf("    [Saved state]\n")
			}
		}

		totalElapsed := time.Since(startTime)
		fmt.Printf("  Total: %.1f minutes\n\n", totalElapsed.Minutes())

		// Mark drive as complete
		UpdateStatus(func(s *BenchmarkStatus) {
			for j := range s.Drives {
				if s.Drives[j].Name == drive.Device.Name {
					s.Drives[j].Status = "complete"
					s.Drives[j].PhaseName = ""
					break
				}
			}
		})
	}

	ec.cleanup()
	ec.cleanupStateFile()
	fmt.Println("All drives complete!")

	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = false
	})

	return nil
}

// executePhase executes a single phase for a single drive.
// setupDone and startGate are used to synchronize parallel launches: the
// goroutine signals setupDone after writing the temp file, then waits on
// startGate before starting fio. Pass nil channels for sequential execution.
func (ec *ExecutionCoordinator) executePhase(drive DriveInfo, phase []FioJob, phaseIdx int, setupDone chan<- struct{}, startGate <-chan struct{}) *FioResult {
	result := &FioResult{
		DeviceName: drive.Device.Name,
		Phase:      phaseIdx,
	}

	// Get first job name for logging
	if len(phase) > 0 {
		result.JobName = phase[0].Name
	}

	workload := ec.workloads[drive.Device.Name]

	// signalReady signals setup done and waits for the start gate.
	// Always called before returning to avoid deadlock when using a gate.
	signalReady := func() {
		if setupDone != nil {
			setupDone <- struct{}{}
			<-startGate
		}
	}

	// With --skip-write-through, refuse a test that cannot measure a Flush.
	// Otherwise it runs and carries a warning (printed by the caller, not
	// logged here: a log line would land inside the "Phase N/M (...): " line).
	warn, skip := ec.flushGate(drive.Device.Name, phase)
	result.Warning = warn
	if skip != "" {
		result.Skipped = skip
		signalReady()
		result.StartTime = time.Now()
		result.EndTime = result.StartTime
		return result
	}

	// Build temporary fio file for this phase
	phaseFioContent := buildPhaseFioFile(workload, phase)
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("bbbench-phase-%s-*.fio", drive.Device.Name))
	if err != nil {
		result.Error = fmt.Errorf("create temp fio file: %w", err)
		signalReady()
		result.EndTime = time.Now()
		return result
	}
	tmpPath := tmpFile.Name()
	ec.registerTempFile(tmpPath)
	defer func() {
		os.Remove(tmpPath)
		ec.unregisterTempFile(tmpPath)
	}()

	if _, err := tmpFile.WriteString(phaseFioContent); err != nil {
		tmpFile.Close()
		result.Error = fmt.Errorf("write temp fio file: %w", err)
		signalReady()
		result.EndTime = time.Now()
		return result
	}
	tmpFile.Close()

	// Construct output path and log prefix
	timestamp := time.Now().Unix()
	outputFile := filepath.Join(ec.outputDir, fmt.Sprintf("%s_phase%d_%d.json",
		drive.Device.Name, phaseIdx, timestamp))
	logPrefix := strings.TrimSuffix(outputFile, ".json") + "_log"
	result.OutputPath = outputFile
	result.LogPrefix = logPrefix

	// All setup done — wait for start gate so all drives launch fio together.
	signalReady()
	result.StartTime = time.Now()

	logAvgMsec := ec.avgMsec()

	// For phases that fsync, time the device's Flush commands from kernel
	// trace events while fio runs. Failure to trace never fails the phase.
	stopTrace := func() {}
	rampMs := phaseRampMs(workload, phase)
	if ec.flushTrace && phaseHasFsync(phase) {
		if sess, err := flushtrace.Start(drive.Device.Name, flushtrace.Options{}); err != nil {
			logger.Warn("flush tracing unavailable",
				"device", drive.Device.Name, "phase", phaseIdx, "err", err)
		} else {
			stopTrace = func() {
				sum, err := sess.Stop()
				if err != nil {
					logger.Warn("flush tracing cleanup", "device", drive.Device.Name, "err", err)
				}
				sum.AvgMsec = logAvgMsec // series follow fio's polling interval
				sum.OffsetMs = rampMs    // and fio's clock, which starts after its ramp
				samples := sess.Samples()
				// Same statistics as a later load of the log: without the ramp.
				final := flushtrace.Finish(sum, samples)
				result.FlushTrace = &final
				// Keep the samples with the fio logs, so the run can be
				// re-summarized and graphed later.
				if err := writeFlushLog(logPrefix, final, samples); err != nil {
					logger.Warn("storing flush samples", "device", drive.Device.Name, "err", err)
				}
				stopTrace = func() {}
			}
		}
	}

	// Execute fio with context for cancellation support
	cmd := exec.CommandContext(ec.ctx, "fio",
		"--output-format=json",
		"--output="+outputFile,
		"--write_iops_log="+logPrefix,
		"--write_bw_log="+logPrefix,
		"--write_lat_log="+logPrefix,
		fmt.Sprintf("--log_avg_msec=%d", logAvgMsec),
		tmpPath)

	if ec.verbose {
		fmt.Printf("\n[%s Phase %d] Command: fio --output-format=json --output=%s %s\n",
			drive.Device.Name, phaseIdx, outputFile, tmpPath)
		fmt.Printf("[%s Phase %d] Jobs: ", drive.Device.Name, phaseIdx)
		jobNames := make([]string, len(phase))
		for i, job := range phase {
			jobNames[i] = job.Name
		}
		fmt.Printf("%s\n", strings.Join(jobNames, ", "))
	}

	logger.Debug("executing fio",
		"device", drive.Device.Name,
		"phase", phaseIdx,
		"output", outputFile)

	output, err := cmd.CombinedOutput()
	stopTrace()
	if err != nil {
		// Check if it was cancelled
		if ec.ctx.Err() != nil {
			result.Error = fmt.Errorf("fio execution cancelled")
		} else {
			result.Error = fmt.Errorf("fio execution failed: %w\nOutput: %s", err, string(output))
			if ec.verbose {
				fmt.Printf("[%s Phase %d] Error output:\n%s\n", drive.Device.Name, phaseIdx, string(output))
			}
		}
		result.EndTime = time.Now()
		return result
	}

	if ec.verbose && len(output) > 0 {
		fmt.Printf("[%s Phase %d] fio stderr/stdout:\n%s\n", drive.Device.Name, phaseIdx, string(output))
	}

	// Read the JSON output
	jsonData, err := os.ReadFile(outputFile)
	if err != nil {
		result.Error = fmt.Errorf("read fio output: %w", err)
		result.EndTime = time.Now()
		return result
	}

	result.JsonOutput = jsonData
	result.EndTime = time.Now()
	return result
}

// statusPhaseConfigs describes every drive's phases for the status page.
func (ec *ExecutionCoordinator) statusPhaseConfigs() map[string][]PhaseConfig {
	out := make(map[string][]PhaseConfig, len(ec.drives))
	for _, drive := range ec.drives {
		out[drive.Device.Name] = phaseConfigs(ec.workloads[drive.Device.Name])
	}
	return out
}

// markDrive updates a drive's entry on the status page.
func markDrive(device string, f func(*DriveStatus)) {
	UpdateStatus(func(s *BenchmarkStatus) {
		for i := range s.Drives {
			if s.Drives[i].Name == device {
				f(&s.Drives[i])
				return
			}
		}
	})
}
