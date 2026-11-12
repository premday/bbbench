// Package main implements the orchestrator command for running fio benchmarks
// across multiple drives with synchronized or sequential execution.
//
// Usage:
//
//	sudo ./bbbench orchestrator [flags]
//
// The orchestrator command:
//  1. Discovers block devices and matches them with generated fio files
//  2. Presents an interactive TUI for drive selection
//  3. Executes benchmarks in the chosen mode (parallel-sync or sequential)
//  4. Saves JSON results for each phase
//  5. Displays a summary of results
//
// Execution modes:
//   - parallel-sync: All drives run phase 1, then all run phase 2, etc.
//   - sequential: Complete benchmark on drive 1, then drive 2, etc.
//
// Flags:
//
//	--mode string        Execution mode: parallel-sync or sequential (default "parallel-sync")
//	--output string      Output directory for results (default: from config)
//	--config string      Config file path
//	--dist string        Dist directory
//
// Example:
//
//	sudo ./bbbench generate                              # Generate fio files first
//	sudo ./bbbench orchestrator --mode=parallel-sync    # Run benchmarks
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/config"
	"bbbench/internal/diskinfo"
	"bbbench/internal/dmi"
	"bbbench/internal/inuse"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
	"bbbench/internal/templating"
	"bbbench/internal/util"
)

// DriveInfo holds information about a drive and its associated fio file.
type DriveInfo struct {
	Device      blockdev.Device
	FioFilePath string
	FioExists   bool
	Selected    bool
	// Members are the disks of a composite target (an array); none for a drive.
	Members []rungroup.Member `json:"Members,omitempty"`
	UUID    string            `json:"UUID,omitempty"`
}

// IsArray reports whether the target is a composite of member disks.
func (d DriveInfo) IsArray() bool { return len(d.Members) > 0 }

func runOrchestrator(args []string) error {
	fs := flag.NewFlagSet("orchestrator", flag.ExitOnError)
	configPath := fs.String("config", "", "config file (default: search ~/.bbbench/config, /etc/bbbench, dist)")
	distDir := fs.String("dist", "", "dist directory (default ./dist or BBBENCH_DIST_DIR)")
	mode := fs.String("mode", "parallel-sync", "execution mode: parallel-sync or sequential")
	outputDir := fs.String("output", "", "output directory for results (default: from config fioplot.output.path)")
	dryRun := fs.Bool("dry-run", false, "show what would be executed without running fio")
	resume := fs.Bool("resume", false, "resume from previous interrupted run")
	verbose := fs.Bool("verbose", false, "show detailed execution information")
	filterType := fs.String("filter-type", "", "filter drives by type: hdd or ssd")
	filterVendor := fs.String("filter-vendor", "", "filter drives by vendor (case-insensitive substring match)")
	filterModel := fs.String("filter-model", "", "filter drives by model (case-insensitive substring match)")
	filterMinCap := fs.Uint64("filter-min-capacity", 0, "filter drives by minimum capacity in GB")
	filterMaxCap := fs.Uint64("filter-max-capacity", 0, "filter drives by maximum capacity in GB (0 = no limit)")
	skipWriteThrough := fs.Bool("skip-write-through", false, "do not run fsync (Flush-latency) phases on write-through devices, where the kernel sends no Flush (default: run them and warn)")
	noFlushTrace := fs.Bool("no-flush-trace", false, "do not trace device Flush latency (kernel trace events) during fsync phases")
	noIOTrace := fs.Bool("no-io-trace", false, "do not trace read and write latency (kernel trace events) during phases")
	logAvgMsec := fs.Int("log-avg-msec", 1000, "fio log averaging interval in milliseconds (--log_avg_msec)")
	dmiPath := fs.String("dmi-path", "", "override /sys/class/dmi/id (tests)")
	sysBlock := fs.String("sys-block", "", "override /sys/block (tests)")
	_ = fs.Parse(args)

	if os.Geteuid() != 0 {
		return errors.New("bbbench orchestrator must run as root")
	}

	// Every external program is checked before anything interactive happens.
	if err := printDependencies(checkDependencies(exec.LookPath, programVersion)); err != nil {
		return err
	}

	// Acquire lock to prevent multiple orchestrator instances
	if err := acquireOrchestratorLock(); err != nil {
		return err
	}
	defer releaseOrchestratorLock()
	cleanStaleTraces(os.Stdout)
	defer ResetStatus() // Reset web status when done

	if *mode != "parallel-sync" && *mode != "sequential" {
		return fmt.Errorf("invalid mode: %s (must be parallel-sync or sequential)", *mode)
	}

	if *filterType != "" && *filterType != "hdd" && *filterType != "ssd" {
		return fmt.Errorf("invalid filter-type: %s (must be hdd or ssd)", *filterType)
	}

	dir := resolveDistDir(*distDir)

	cfg, err := loadConfigWithSearch(*configPath, dir)
	if err != nil {
		return err
	}

	// Load DMI info for host identification (needed for auto-generation and discovery)
	dmiInfo, err := dmi.Load(dmi.OSReader{}, *dmiPath)
	if err != nil {
		return fmt.Errorf("read dmi: %w", err)
	}
	chassis, err := dmiInfo.ChassisSerial()
	if err != nil {
		return fmt.Errorf("read chassis_serial: %w", err)
	}
	host := util.SanitizeFilename(chassis, "_")

	// Discover drives and match with fio files
	drives, err := discoverDrivesWithFioFiles(cfg, host, *sysBlock, *verbose)
	if err != nil {
		return err
	}

	if len(drives) == 0 {
		return errors.New("no drives with generated fio files found. Run 'bbbench generate' first")
	}

	if *verbose {
		fmt.Printf("\nDiscovered %d drive(s) with fio files:\n", len(drives))
		for _, drive := range drives {
			fmt.Printf("  - %s: %s %s %s (%d GB, %s)\n",
				drive.Device.Name,
				drive.Device.Vendor,
				drive.Device.Model,
				drive.Device.Serial,
				drive.Device.CapacityGB(),
				drive.Device.WorkloadType())
		}
		fmt.Println()
	}

	allDrives := drives

	// Apply filters
	filter := DriveFilter{
		Type:        *filterType,
		Vendor:      *filterVendor,
		Model:       *filterModel,
		MinCapacity: *filterMinCap,
		MaxCapacity: *filterMaxCap,
	}

	if filter.HasFilters() {
		originalCount := len(drives)
		drives = filter.Apply(drives)
		if *verbose {
			fmt.Printf("Applied filters: %d drive(s) remaining (filtered out %d)\n\n", len(drives), originalCount-len(drives))
		}
	}

	if len(drives) == 0 {
		return errors.New("no drives match the specified filters")
	}

	// Determine output directory
	outDir := *outputDir
	if outDir == "" {
		// Extract output path from fioplot map
		if fioplot, ok := cfg.Fioplot.(map[string]any); ok {
			if output, ok := fioplot["output"].(map[string]any); ok {
				if path, ok := output["path"].(string); ok {
					outDir = path
				}
			}
		}
		if outDir == "" {
			outDir = "~/.bbbench/output" // default fallback
		}
	}
	outDir = expandUser(outDir)

	// A drive setting left changed by an earlier setting test is offered for
	// restoring before anything else; all discovered drives are candidates.
	// The disks a layout must not erase include those holding this binary
	// and the output directory; the layout re-checks them before each build.
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	scanInUse := func() (*inuse.Report, error) {
		return inuse.Scan(inuse.Options{Paths: inUsePaths(exe, outDir)})
	}
	available := settings.All(settings.Env{InUse: scanInUse})
	targets := targetsFor(allDrives, diskinfo.Options{})
	if err := offerRecovery(context.Background(), os.Stdin, os.Stdout, groupRecoveryPath, available, targets); err != nil {
		return err
	}

	// What to run: a group to resume, a setting test, or a single run.
	var group *groupChoice
	var resumeGroup *rungroup.GroupFile
	var selectedDrives []DriveInfo
	if *resume {
		if g := findResumableGroup(outDir); g != nil {
			if resumesGroup(g, stateIfAny(getStateFilePath(outDir))) {
				if *dryRun {
					return errors.New("a dry run cannot be combined with a setting test")
				}
				c, err := prepareGroupResume(os.Stdin, os.Stdout, g, available, allDrives, scanInUse)
				if err != nil {
					return err
				}
				group, resumeGroup, selectedDrives = c, g, c.Drives
			} else {
				fmt.Printf("Note: setting test %s is left unfinished (%s); resuming the single run instead.\n", g.ID, groupFileName(g))
			}
		}
	}
	if group == nil {
		choice, err := runModeTUI(available, drives, targets)
		if err != nil {
			return err
		}
		if choice.Setting != nil && *dryRun {
			return errors.New("a dry run cannot be combined with a setting test")
		}
		if note := resumeIgnoredNote(*resume, choice.Setting); note != "" {
			fmt.Println(note)
		}

		if choice.Setting != nil && choice.Setting.Input() == settings.InputVariants {
			// A layout test picks its disks per layout: no drive selection.
			ls, ok := choice.Setting.(settings.LayoutSetting)
			if !ok {
				return fmt.Errorf("%s takes layouts but has no layout screens", choice.Setting.Name())
			}
			busy, berr := scanInUse()
			// x on the list destroys an existing configuration and re-runs the
			// scan; the discovered drives stay the same disks.
			group, err = runLayoutSetupTUI(ls, drives, targets, busy, berr, scanInUse)
			if err != nil {
				return err
			}
			selectedDrives = group.Drives
		} else {
			// Launch TUI for drive selection
			selectedDrives, err = runDriveSelectionTUI(drives)
			if err != nil {
				return err
			}

			if len(selectedDrives) == 0 {
				return errors.New("no drives selected")
			}

			if choice.Setting != nil {
				group, err = runGroupSetupTUI(choice.Setting, selectedDrives, targets)
				if err != nil {
					return err
				}
				selectedDrives = group.Drives
			}
		}
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// Handle resume mode
	var resumeState *ExecutionState
	stateFile := getStateFilePath(outDir)
	if *resume && group == nil {
		state, err := loadState(stateFile)
		if err != nil {
			return fmt.Errorf("load state for resume: %w", err)
		}
		resumeState = state

		// Validate state matches current request
		if state.Mode != *mode {
			return fmt.Errorf("cannot resume: mode mismatch (previous: %s, requested: %s)", state.Mode, *mode)
		}

		fmt.Printf("Resuming previous run from %s\n", state.StartTime.Format("2006-01-02 15:04:05"))
		fmt.Printf("Mode: %s\n", state.Mode)
		fmt.Printf("Drives: %d\n", len(state.Drives))

		// Count completed work
		totalCompleted := 0
		for _, phases := range state.CompletedWork {
			totalCompleted += len(phases)
		}
		fmt.Printf("Already completed phases: %d\n\n", totalCompleted)
	}

	logger.Info("orchestrator start", "mode", *mode, "drives", len(selectedDrives), "output", outDir, "dry_run", *dryRun, "resume", *resume)

	// Auto-generate missing fio files
	var missingDevices []blockdev.Device
	for _, drive := range selectedDrives {
		if !drive.FioExists {
			missingDevices = append(missingDevices, drive.Device)
		}
	}

	if len(missingDevices) > 0 {
		fmt.Printf("Auto-generating fio configuration files for %d drive(s)...\n", len(missingDevices))

		// Load templates
		eng, err := loadTemplatesWithSearch("", dir)
		if err != nil {
			return fmt.Errorf("load templates for auto-generation: %w", err)
		}

		// Determine output base directory
		outBase := cfg.Fio.Generated.Path
		outBase = expandUser(outBase)

		// Generate files
		if err := generateFioFilesForDrives(cfg, eng, missingDevices, host, outBase); err != nil {
			return fmt.Errorf("auto-generate fio files: %w", err)
		}

		fmt.Printf("Auto-generation complete.\n\n")
	}

	if *dryRun {
		fmt.Printf("DRY RUN: Would run benchmarks on %d drive(s) in %s mode\n", len(selectedDrives), *mode)
	} else {
		fmt.Printf("Running benchmarks on %d drive(s) in %s mode\n", len(selectedDrives), *mode)
	}
	fmt.Printf("Output directory: %s\n\n", outDir)

	opts := runOptions{
		Mode:             *mode,
		OutDir:           outDir,
		Resume:           resumeState,
		DryRun:           *dryRun,
		Verbose:          *verbose,
		FlushTrace:       !*noFlushTrace,
		IOTrace:          !*noIOTrace,
		SkipWriteThrough: *skipWriteThrough,
		LogAvgMsec:       *logAvgMsec,
	}
	spec := runSpec{Drives: selectedDrives}

	// The web server starts when the run is registered (see OnStarted), once
	// per process; a dry run never gets there.
	var srv *http.Server
	var startOnce sync.Once
	opts.OnStarted = func() {
		startOnce.Do(func() {
			srv = newWebServer("0.0.0.0:12345")
			go func(srv *http.Server) {
				logger.Info("starting web server from orchestrator", "addr", srv.Addr)
				fmt.Printf("Web server available at http://0.0.0.0:12345\n\n")
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("web server error", "err", err)
				}
			}(srv)
		})
	}
	defer func() {
		if srv == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	var runErr error
	if group != nil {
		// A composite target built by a layout gets its fio file rendered
		// from the same templates; they are loaded on the first composite
		// only, so a group of drive values loads nothing more.
		var eng *templating.Engine
		makeDrive := func(bt settings.BenchTarget) (DriveInfo, error) {
			if eng == nil {
				e, err := loadTemplatesWithSearch("", dir)
				if err != nil {
					return DriveInfo{}, fmt.Errorf("load templates: %w", err)
				}
				eng = e
			}
			return arrayDrive(bt, drivesByName(group.Drives), *sysBlock, fioGenerator(cfg, eng, host))
		}
		cfg := newGroupConfig(group, targets, opts, host, resumeGroup, makeDrive)
		cfg.Identify = func(name string) (blockdev.Device, error) {
			return blockdev.Lookup(blockdev.OSFS{}, *sysBlock, name)
		}
		if group.Variants != nil {
			fmt.Printf("Setting test: %s, %d layouts, one after the other\n\n", group.Setting.Name(), len(group.Variants))
		} else {
			fmt.Printf("Setting test: %s, %d values, strategy %s\n\n", group.Setting.Name(), len(group.Values), group.Strategy)
		}
		g, err := runGroup(context.Background(), cfg)
		if g == nil {
			// Setup failed before the first run: nothing to wait for.
			return err
		}
		printGroupSummary(os.Stdout, g)
		runErr = err
	} else {
		result, err := executeRun(context.Background(), opts, spec)
		if result == nil {
			// Setup failed, or a dry run: nothing was executed, nothing to wait for.
			return err
		}
		runErr = err
	}

	// Keep web server running until user interrupts
	fmt.Printf("\nWeb server still running at http://0.0.0.0:12345\n")
	fmt.Printf("Results available at: %s\n", outDir)
	fmt.Printf("\nPress Ctrl+C to stop...\n\n")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	fmt.Println("\n\nShutting down gracefully...")
	logger.Info("received interrupt signal")

	return runErr
}

// runOptions are the settings shared by every run of an orchestrator process.
type runOptions struct {
	Mode, OutDir     string
	Resume           *ExecutionState
	DryRun, Verbose  bool
	FlushTrace       bool
	IOTrace          bool
	SkipWriteThrough bool
	LogAvgMsec       int
	// OnStarted, when non-nil, is called once the run's live result is in
	// the results store, just before the phases execute (the web server
	// starts here).
	OnStarted func()
}

// registerLiveRun creates the live result for the web view (updated
// incrementally as phases complete), adds it to the results store, and then
// calls opts.OnStarted.
func registerLiveRun(opts runOptions, spec runSpec, coordinator *ExecutionCoordinator) *BenchmarkResult {
	runID := fmt.Sprintf("run_%d", time.Now().Unix())
	liveResult := &BenchmarkResult{
		ID:        runID,
		Timestamp: time.Now(),
		Mode:      opts.Mode,
		Drives:    spec.Drives,
		Phases:    make([]PhaseResult, 0),
		OutputDir: opts.OutDir,
		GroupID:   spec.GroupID,
		Labels:    spec.Labels,
	}
	globalResultsStore.AddResult(liveResult)
	coordinator.liveResult = liveResult
	if opts.OnStarted != nil {
		opts.OnStarted()
	}
	return liveResult
}

// runSpec describes one run: its drives and, for a run group, its labels.
type runSpec struct {
	Drives  []DriveInfo
	Labels  map[string]rungroup.Label // device -> label; nil for a plain run
	GroupID string
}

// executeRun runs the whole test plan once on spec.Drives: it builds the
// coordinator, executes, displays the results and saves the summary. It
// returns the saved result. The result is nil (with a nil error) for a dry
// run, and nil with an error when setup failed before anything ran; once
// execution started the result is non-nil and the error joins any execution
// and display errors.
func executeRun(ctx context.Context, opts runOptions, spec runSpec) (*BenchmarkResult, error) {
	coordinator, err := newExecutionCoordinator(spec.Drives, opts.Mode, opts.OutDir)
	if err != nil {
		return nil, err
	}
	// Ending the coordinator's context when the run returns also stops its
	// signal handler, and finish stops it after a first signal.
	defer coordinator.finish()
	defer coordinator.cancel()
	coordinator.dryRun = opts.DryRun
	coordinator.stateFile = getStateFilePath(opts.OutDir)
	coordinator.resumeState = opts.Resume
	coordinator.verbose = opts.Verbose
	coordinator.logAvgMsec = opts.LogAvgMsec
	coordinator.flushTrace = opts.FlushTrace
	coordinator.ioTrace = opts.IOTrace
	coordinator.labels = spec.Labels
	coordinator.groupID = spec.GroupID
	if err := coordinator.checkEnvironment(); err != nil {
		return nil, err
	}
	coordinator.skipWriteThrough = opts.SkipWriteThrough

	if opts.DryRun {
		// In dry-run mode, just show what would be executed
		return nil, displayDryRun(coordinator)
	}

	// Stop the coordinator when the caller cancels ctx, reported as an
	// interruption like a signal.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			coordinator.setInterrupted()
			coordinator.cancel()
		case <-done:
		}
	}()

	liveResult := registerLiveRun(opts, spec, coordinator)

	// Execute based on mode
	var execErr error
	if opts.Mode == "parallel-sync" {
		execErr = coordinator.executeParallelSync()
	} else {
		execErr = coordinator.executeSequential()
	}

	// Display results even if interrupted (partial results)
	var displayErr error
	if err := displayResults(coordinator); err != nil {
		logger.Error("display results", "err", err)
		displayErr = err
	}

	if execErr != nil {
		logger.Error("execution error", "err", execErr)
		fmt.Printf("\nBenchmark execution failed: %v\n", execErr)
	} else {
		logger.Info("orchestrator complete")
		fmt.Println("\nBenchmarks complete!")
	}

	// Save benchmark summary to output directory
	if err := saveBenchmarkSummary(coordinator, spec.Drives, opts.Mode, opts.OutDir); err != nil {
		logger.Error("save benchmark summary", "err", err)
		fmt.Printf("Warning: failed to save benchmark summary: %v\n", err)
	}

	return liveResult, errors.Join(execErr, displayErr)
}

// DriveFilter contains criteria for filtering drives.
type DriveFilter struct {
	Type        string // "hdd" or "ssd"
	Vendor      string // case-insensitive substring match
	Model       string // case-insensitive substring match
	MinCapacity uint64 // in GB
	MaxCapacity uint64 // in GB (0 = no limit)
}

// HasFilters returns true if any filter is specified.
func (f *DriveFilter) HasFilters() bool {
	return f.Type != "" || f.Vendor != "" || f.Model != "" || f.MinCapacity > 0 || f.MaxCapacity > 0
}

// Apply filters the drive list based on the filter criteria.
func (f *DriveFilter) Apply(drives []DriveInfo) []DriveInfo {
	if !f.HasFilters() {
		return drives
	}

	filtered := make([]DriveInfo, 0, len(drives))
	for _, drive := range drives {
		if f.matches(drive) {
			filtered = append(filtered, drive)
		}
	}
	return filtered
}

// matches checks if a drive matches all filter criteria.
func (f *DriveFilter) matches(drive DriveInfo) bool {
	// Type filter
	if f.Type != "" {
		driveType := drive.Device.WorkloadType()
		if !strings.EqualFold(f.Type, driveType) {
			return false
		}
	}

	// Vendor filter (case-insensitive substring match)
	if f.Vendor != "" {
		if !strings.Contains(strings.ToLower(drive.Device.Vendor), strings.ToLower(f.Vendor)) {
			return false
		}
	}

	// Model filter (case-insensitive substring match)
	if f.Model != "" {
		if !strings.Contains(strings.ToLower(drive.Device.Model), strings.ToLower(f.Model)) {
			return false
		}
	}

	// Capacity filters
	capacityGB := drive.Device.CapacityGB()
	if f.MinCapacity > 0 && capacityGB < f.MinCapacity {
		return false
	}
	if f.MaxCapacity > 0 && capacityGB > f.MaxCapacity {
		return false
	}

	return true
}

// discoverDrivesWithFioFiles discovers drives and matches them with generated fio files.
func discoverDrivesWithFioFiles(cfg *config.Root, host, sysBlock string, verbose bool) ([]DriveInfo, error) {
	// Discover block devices
	devs, err := blockdev.Discover(blockdev.OSFS{}, sysBlock)
	if err != nil {
		return nil, fmt.Errorf("discover block devices: %w", err)
	}

	// Get the base output path from config
	outBase := expandUser(cfg.Fio.Generated.Path)
	hostDir := filepath.Join(outBase, host)

	// Match devices with fio files
	var drives []DriveInfo
	for _, disk := range devs {
		fioPath := filepath.Join(hostDir, fioFileName(host, disk))
		fioExists := false
		if _, err := os.Stat(fioPath); err == nil {
			fioExists = true
		}

		drives = append(drives, DriveInfo{
			Device:      disk,
			FioFilePath: fioPath,
			FioExists:   fioExists,
			Selected:    false,
		})
	}

	return drives, nil
}

// displayDryRun shows what would be executed in dry-run mode.
func displayDryRun(coordinator *ExecutionCoordinator) error {
	fmt.Println(repeatString("=", 80))
	fmt.Println("DRY RUN - EXECUTION PLAN")
	fmt.Println(repeatString("=", 80))
	fmt.Println()

	fmt.Printf("Mode: %s\n", coordinator.mode)
	fmt.Printf("Drives: %d\n", len(coordinator.drives))
	fmt.Printf("Output directory: %s\n\n", coordinator.outputDir)

	// Calculate total phases
	maxPhases := 0
	for _, workload := range coordinator.workloads {
		if len(workload.Phases) > maxPhases {
			maxPhases = len(workload.Phases)
		}
	}

	if coordinator.mode == "parallel-sync" {
		fmt.Printf("Would execute %d phase(s) across all drives in parallel-sync mode\n\n", maxPhases)

		for phaseIdx := 0; phaseIdx < maxPhases; phaseIdx++ {
			fmt.Printf("Phase %d/%d:\n", phaseIdx+1, maxPhases)

			for _, drive := range coordinator.drives {
				workload := coordinator.workloads[drive.Device.Name]
				if phaseIdx >= len(workload.Phases) {
					continue
				}

				phase := workload.Phases[phaseIdx]
				fmt.Printf("  %s: %d job(s) - ", drive.Device.Name, len(phase))
				jobNames := make([]string, len(phase))
				for i, job := range phase {
					jobNames[i] = job.Name
				}
				fmt.Printf("%s\n", strings.Join(jobNames, ", "))
			}
			fmt.Println()
		}
	} else {
		fmt.Println("Would execute benchmarks sequentially, one drive at a time:")
		fmt.Println()

		for i, drive := range coordinator.drives {
			workload := coordinator.workloads[drive.Device.Name]
			fmt.Printf("%d. %s (%s %s)\n", i+1, drive.Device.Name, drive.Device.Vendor, drive.Device.Model)
			fmt.Printf("   Phases: %d\n", len(workload.Phases))
			fmt.Printf("   Total jobs: %d\n", len(workload.Jobs))
			fmt.Printf("   Fio file: %s\n\n", drive.FioFilePath)
		}
	}

	fmt.Println(repeatString("=", 80))
	fmt.Println("Commands that would be executed:")
	fmt.Println(repeatString("=", 80))
	fmt.Println()

	// Show sample fio command
	if len(coordinator.drives) > 0 {
		drive := coordinator.drives[0]
		workload := coordinator.workloads[drive.Device.Name]
		if len(workload.Phases) > 0 {
			fmt.Println("Example command for first phase of first drive:")
			timestamp := 1234567890
			outputFile := filepath.Join(coordinator.outputDir, fmt.Sprintf("%s_phase%d_%d.json",
				drive.Device.Name, 0, timestamp))
			fmt.Printf("  fio --output-format=json --output=%s <temp-phase-file>\n\n", outputFile)
		}
	}

	fmt.Println("No actual benchmarks were run (dry-run mode)")
	fmt.Println(repeatString("=", 80))

	return nil
}

// saveBenchmarkSummary saves a summary of the benchmark run to the output directory.
func saveBenchmarkSummary(coordinator *ExecutionCoordinator, drives []DriveInfo, mode, outDir string) error {
	// Reuse liveResult (already populated incrementally during execution).
	summary := coordinator.liveResult
	if summary == nil {
		summary = &BenchmarkResult{
			ID:        fmt.Sprintf("run_%d", time.Now().Unix()),
			Timestamp: time.Now(),
			Mode:      mode,
			Drives:    drives,
			Phases:    make([]PhaseResult, 0),
			OutputDir: outDir,
		}
	}

	// Save to JSON file
	summaryPath := filepath.Join(outDir, fmt.Sprintf("%s_summary.json", summary.ID))
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}

	if err := os.WriteFile(summaryPath, data, 0644); err != nil {
		return fmt.Errorf("write summary file: %w", err)
	}

	logger.Info("saved benchmark summary", "path", summaryPath)
	fmt.Printf("Saved benchmark summary: %s\n", summaryPath)

	globalResultsStore.AddResult(summary)

	return nil
}
