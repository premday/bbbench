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
	// DurationSec is the phase's expected length when Deterministic.
	DurationSec   int  `json:"duration_sec"`
	Deterministic bool `json:"deterministic"`
	// Open explains a phase without a known length.
	Open string `json:"open,omitempty"`
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
		pc := PhaseConfig{
			Index:   i,
			Name:    strings.Join(names, ", "),
			Summary: phaseSummary(global, phase),
			Config:  stripFioComments(buildPhaseFioFile(w, phase)),
		}
		pc.DurationSec, pc.Deterministic, pc.Open = phaseDuration(global, phase)
		out = append(out, pc)
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

// stripFioComments drops comment lines (; or #) and blank lines from a job
// file, leaving only what fio acts on.
func stripFioComments(config string) string {
	var out []string
	for _, line := range strings.Split(config, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || t[0] == ';' || t[0] == '#' {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// phaseDuration estimates how long a phase takes. Jobs of a phase run side by
// side, so it is the longest job: ramp_time + runtime for a time_based job.
// A job without time_based ends when its I/O is done (e.g. a full-disk
// write), which depends on the drive, so the phase has no known length.
func phaseDuration(global map[string]string, phase []FioJob) (sec int, deterministic bool, open string) {
	for _, job := range phase {
		opts := map[string]string{}
		for k, v := range global {
			opts[k] = v
		}
		for k, v := range parseFioOptions(job.Section) {
			opts[k] = v
		}
		runtime, ok := parseFioSeconds(opts["runtime"])
		if opts["time_based"] == "" || !ok || runtime <= 0 {
			if opts["size"] == "100%" || opts["rw"] == "trim" {
				return 0, false, "runs until the whole disk is processed; the time depends on the drive"
			}
			return 0, false, "no time limit; it ends when its I/O is done"
		}
		ramp, _ := parseFioSeconds(opts["ramp_time"])
		if d := runtime + ramp; d > sec {
			sec = d
		}
	}
	return sec, len(phase) > 0, ""
}

// parseFioSeconds reads an fio time value: a bare number is seconds, else a
// suffix d, h, m, s, ms or us ("5m", "1h", "250ms").
func parseFioSeconds(v string) (int, bool) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return 0, false
	}
	units := []struct {
		suffix string
		sec    float64
	}{{"ms", 0.001}, {"us", 0.000001}, {"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			v, mult = strings.TrimSuffix(v, u.suffix), u.sec
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return int(n*mult + 0.5), true
}

// phaseRampMs is the ramp_time of the phase in ms: fio starts its logs and
// statistics only when the ramp ends (a job's value overrides [global]).
func phaseRampMs(w *FioWorkload, phase []FioJob) int {
	ramp := 0
	global := parseFioOptions(w.GlobalSection)
	for _, job := range phase {
		opts := map[string]string{"ramp_time": global["ramp_time"]}
		if v, ok := parseFioOptions(job.Section)["ramp_time"]; ok {
			opts["ramp_time"] = v
		}
		if sec, ok := parseFioSeconds(opts["ramp_time"]); ok && sec*1000 > ramp {
			ramp = sec * 1000
		}
	}
	return ramp
}
