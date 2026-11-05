package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// PhaseConfig describes one phase of a drive's workload for the status page:
// a one-line summary of the options that matter and the fio job file exactly
// as the phase runs it.
type PhaseConfig struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Config  string `json:"config"`
}

// summaryOptions are the fio options shown in a phase's one-line summary, in
// display order. A job's value overrides the [global] one.
var summaryOptions = []string{
	"rw", "bs", "iodepth", "numjobs", "ioengine", "direct", "fsync",
	"runtime", "time_based", "size",
}

// phaseConfigs builds the status-page description of every phase of a workload.
func phaseConfigs(w *FioWorkload) []PhaseConfig {
	if w == nil {
		return nil
	}
	global := parseFioOptions(w.GlobalSection)
	out := make([]PhaseConfig, 0, len(w.Phases))
	for i, phase := range w.Phases {
		names := make([]string, len(phase))
		for j, job := range phase {
			names[j] = job.Name
		}
		out = append(out, PhaseConfig{
			Index:   i,
			Name:    strings.Join(names, ", "),
			Summary: phaseSummary(global, phase),
			Config:  strings.TrimSpace(buildPhaseFioFile(w, phase)),
		})
	}
	return out
}

// phaseSummary renders "rw=randwrite bs=4k ..." from the first job of the
// phase (its jobs share the options that matter), falling back to [global].
func phaseSummary(global map[string]string, phase []FioJob) string {
	opts := map[string]string{}
	for k, v := range global {
		opts[k] = v
	}
	if len(phase) > 0 {
		for k, v := range parseFioOptions(phase[0].Section) {
			opts[k] = v
		}
	}
	var parts []string
	for _, k := range summaryOptions {
		if v, ok := opts[k]; ok && v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if len(phase) > 1 {
		parts = append(parts, "("+strconv.Itoa(len(phase))+" jobs)")
	}
	return strings.Join(parts, " ")
}

// parseFioOptions reads key=value lines of an fio section; flags without a
// value (e.g. "time_based") map to "1".
func parseFioOptions(section string) map[string]string {
	opts := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '[' || line[0] == ';' || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			opts[strings.TrimSpace(k)] = strings.TrimSpace(v)
		} else {
			opts[line] = "1"
		}
	}
	return opts
}

// handleStatusPhases serves each drive's phase descriptions as JSON, keyed by
// device name. It is separate from /api/status because the configs are static
// for a run and the status is polled every 2 seconds.
func handleStatusPhases(w http.ResponseWriter, r *http.Request) {
	globalStatusMu.RLock()
	phases := make(map[string][]PhaseConfig, len(globalStatus.phaseConfigs))
	for k, v := range globalStatus.phaseConfigs {
		phases[k] = v
	}
	globalStatusMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	if err := json.NewEncoder(w).Encode(phases); err != nil {
		logger.Error("encode status phases", "err", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}
