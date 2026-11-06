package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ExecutionState represents the saved state of an orchestrator run.
type ExecutionState struct {
	Version       string           `json:"version"`
	Mode          string           `json:"mode"`
	OutputDir     string           `json:"output_dir"`
	StartTime     time.Time        `json:"start_time"`
	LastUpdate    time.Time        `json:"last_update"`
	Drives        []string         `json:"drives"`         // drive names
	CompletedWork map[string][]int `json:"completed_work"` // drive -> completed phase indices
	Interrupted   bool             `json:"interrupted"`
}

const stateFileVersion = "1"

// saveState saves the current execution state to disk.
func (ec *ExecutionCoordinator) saveState() error {
	if ec.stateFile == "" {
		return nil // No state file configured
	}

	ec.mu.Lock()
	defer ec.mu.Unlock()

	state := ExecutionState{
		Version:       stateFileVersion,
		Mode:          ec.mode,
		OutputDir:     ec.outputDir,
		StartTime:     ec.startTime,
		LastUpdate:    time.Now(),
		Drives:        make([]string, len(ec.drives)),
		CompletedWork: make(map[string][]int),
		Interrupted:   ec.interrupted,
	}

	for i, drive := range ec.drives {
		state.Drives[i] = drive.Device.Name
	}

	// Record completed phases for each drive
	for driveName, results := range ec.results {
		completed := make([]int, 0, len(results))
		for _, result := range results {
			// A skipped phase is not done: after fixing the cause, --resume runs it.
			if result.Error == nil && result.Skipped == "" {
				completed = append(completed, result.Phase)
			}
		}
		state.CompletedWork[driveName] = completed
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	// Write atomically by writing to temp file then renaming
	tmpFile := ec.stateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}

	if err := os.Rename(tmpFile, ec.stateFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("rename state file: %w", err)
	}

	logger.Debug("saved execution state", "file", ec.stateFile)
	return nil
}

// loadState loads execution state from disk.
func loadState(stateFile string) (*ExecutionState, error) {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}

	var state ExecutionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal state: %w", err)
	}

	if state.Version != stateFileVersion {
		return nil, fmt.Errorf("incompatible state file version: %s (expected %s)", state.Version, stateFileVersion)
	}

	return &state, nil
}

// isPhaseCompleted checks if a phase was completed in a previous run.
func (ec *ExecutionCoordinator) isPhaseCompleted(driveName string, phaseIdx int) bool {
	if ec.resumeState == nil {
		return false
	}

	completed, ok := ec.resumeState.CompletedWork[driveName]
	if !ok {
		return false
	}

	for _, idx := range completed {
		if idx == phaseIdx {
			return true
		}
	}

	return false
}

// getStateFilePath returns the path for the state file.
func getStateFilePath(outputDir string) string {
	return filepath.Join(outputDir, ".bbbench-orchestrator-state.json")
}

// cleanupStateFile removes the state file after successful completion.
func (ec *ExecutionCoordinator) cleanupStateFile() {
	if ec.stateFile != "" && !ec.interrupted {
		if err := os.Remove(ec.stateFile); err != nil {
			logger.Debug("cleanup state file", "err", err)
		} else {
			logger.Debug("removed state file", "file", ec.stateFile)
		}
	}
}
