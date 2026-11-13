package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"bbbench/internal/flushtrace"
	"bbbench/internal/iotrace"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// DiskPhaseData holds metrics for one disk × one phase combination.
type DiskPhaseData struct {
	Disk       string `json:"disk"` // OS name, e.g. "sdq"
	Path       string `json:"path"` // full path, e.g. "/dev/sdq"
	Vendor     string `json:"vendor"`
	Model      string `json:"model"`
	Serial     string `json:"serial"`
	Technology string `json:"technology"` // "hdd" or "ssd"
	PhaseNum   int    `json:"phase_num"`
	PhaseName  string `json:"phase_name"`
	// Run-group label of the disk (empty for an unlabelled drive).
	Setting      string `json:"setting,omitempty"`
	SettingValue string `json:"setting_value,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	Invalid      bool   `json:"invalid,omitempty"`
	// Round is the 1-based round of the run in its group (set only in a
	// group's data, /api/groups/graphs).
	Round int `json:"round,omitempty"`
	// Kind is "" for a drive, "array" for a composite target with Members.
	Kind    string            `json:"kind,omitempty"`
	Members []rungroup.Member `json:"members,omitempty"`
	// Summary stats (averages for the full phase)
	ReadIOPS    float64 `json:"read_iops"`
	WriteIOPS   float64 `json:"write_iops"`
	ReadBWMbps  float64 `json:"read_bw_mbps"`
	WriteBWMbps float64 `json:"write_bw_mbps"`
	ReadLatUS   float64 `json:"read_lat_us"`
	WriteLatUS  float64 `json:"write_lat_us"`
	HasRead     bool    `json:"has_read"`
	HasWrite    bool    `json:"has_write"`
	// Time-series data from fio log files (nil for old results)
	TSiops []TimePoint `json:"ts_iops,omitempty"`
	TSbw   []TimePoint `json:"ts_bw,omitempty"`
	TSlat  []TimePoint `json:"ts_lat,omitempty"`
	// Device Flush latency (µs, W column), from the kernel trace samples
	// stored with the run; nil when the phase did not fsync or tracing was
	// unavailable.
	TSflush []TimePoint `json:"ts_flush,omitempty"`
	// TSmembers is the I/O of each member of an array (member -> series).
	TSmembers map[string]MemberSeries `json:"ts_members,omitempty"`
	// TSmd and MDEvents are the md state of an array over time and its changes.
	TSmd     []MDPoint           `json:"ts_md,omitempty"`
	MDEvents []MDEvent           `json:"md_events,omitempty"`
	Flush    *flushtrace.Summary `json:"flush,omitempty"`
	// FlushSource is the Flush source line (tracepoints, pairing, lost events).
	FlushSource string `json:"flush_source,omitempty"`
	// TSiolat is the traced device's read and write latency per window,
	// TSiolatMembers the same for every other traced device (array members).
	TSiolat        []IOLatPoint            `json:"ts_iolat,omitempty"`
	TSiolatMembers map[string][]IOLatPoint `json:"ts_iolat_members,omitempty"`
	// IOTrace is the phase's ftrace summary; IOTraceNote says why there is none.
	IOTrace     *iotrace.Summary `json:"io_trace,omitempty"`
	IOTraceNote string           `json:"io_trace_note,omitempty"`
	// RampSec is the length of fio's ramp-up at the start of the phase; fio's
	// series (t) start after it, the Flush series (t) include it.
	RampSec int `json:"ramp_sec,omitempty"`
	// LogAvgMsec is the width of one fio log point (its polling interval).
	LogAvgMsec int `json:"log_avg_msec,omitempty"`
	// Skipped is why this phase was not run on the disk ("" = it ran).
	Skipped string `json:"skipped,omitempty"`
}

// DiskGraphData holds per-disk metrics across all phases (used by export).
type DiskGraphData struct {
	Disk   string         `json:"disk"`
	Title  string         `json:"title"`
	Phases []PhaseMetrics `json:"phases"`
}

// PhaseMetrics holds aggregated I/O metrics for one phase of one disk.
type PhaseMetrics struct {
	PhaseName   string  `json:"phase_name"`
	PhaseNum    int     `json:"phase_num"`
	ReadIOPS    float64 `json:"read_iops"`
	WriteIOPS   float64 `json:"write_iops"`
	ReadBWMbps  float64 `json:"read_bw_mbps"`
	WriteBWMbps float64 `json:"write_bw_mbps"`
	ReadLatUS   float64 `json:"read_lat_us"`
	WriteLatUS  float64 `json:"write_lat_us"`
	HasRead     bool    `json:"has_read"`
	HasWrite    bool    `json:"has_write"`
}

// generateDiskPhaseData returns a flat list of one entry per disk × phase.
func generateDiskPhaseData(result *BenchmarkResult) []DiskPhaseData {
	// Build ordered unique disk list and info lookup.
	type diskInfo struct {
		path, vendor, model, serial, technology string
		kind                                    string
		members                                 []rungroup.Member
	}
	seen := map[string]bool{}
	diskOrder := []string{}
	info := map[string]diskInfo{}
	for _, drive := range result.Drives {
		n := drive.Device.Name
		if seen[n] {
			continue
		}
		seen[n] = true
		diskOrder = append(diskOrder, n)
		tech := "ssd"
		if drive.Device.Rotational {
			tech = "hdd"
		}
		info[n] = diskInfo{
			path:       drive.Device.Path,
			vendor:     strings.TrimSpace(drive.Device.Vendor),
			model:      strings.TrimSpace(drive.Device.Model),
			serial:     strings.TrimSpace(drive.Device.Serial),
			technology: tech,
			members:    drive.Members,
		}
		if drive.IsArray() {
			d := info[n]
			d.kind = string(settings.TargetArray)
			info[n] = d
		}
	}

	var out []DiskPhaseData
	for _, phase := range result.Phases {
		// Aggregate jobs per device for this phase.
		byDisk := map[string]*DiskPhaseData{}
		for _, job := range phase.Jobs {
			di, ok := info[job.Device]
			if !ok {
				continue
			}
			dp, exists := byDisk[job.Device]
			if !exists {
				dp = &DiskPhaseData{
					Disk:       job.Device,
					Path:       di.path,
					Vendor:     di.vendor,
					Model:      di.model,
					Serial:     di.serial,
					Technology: di.technology,
					PhaseNum:   phase.PhaseNumber,
					PhaseName:  phase.PhaseName,
					Kind:       di.kind,
					Members:    di.members,
				}
				byDisk[job.Device] = dp
			}
			if job.ReadStats != nil && job.ReadStats.IOPS > 0 {
				dp.ReadIOPS += job.ReadStats.IOPS
				dp.ReadBWMbps += job.ReadStats.Bandwidth / 1024
				if job.ReadStats.AvgLatNS > 0 {
					dp.ReadLatUS = job.ReadStats.AvgLatNS / 1000
				}
				dp.HasRead = true
			}
			if job.WriteStats != nil && job.WriteStats.IOPS > 0 {
				dp.WriteIOPS += job.WriteStats.IOPS
				dp.WriteBWMbps += job.WriteStats.Bandwidth / 1024
				if job.WriteStats.AvgLatNS > 0 {
					dp.WriteLatUS = job.WriteStats.AvgLatNS / 1000
				}
				dp.HasWrite = true
			}
		}
		// Phases skipped on a disk have no jobs; still emit an entry that
		// carries the reason, so the UI can show it.
		for dev, reason := range phase.Skipped {
			di, ok := info[dev]
			if !ok || byDisk[dev] != nil {
				continue
			}
			byDisk[dev] = &DiskPhaseData{
				Disk: dev, Path: di.path, Vendor: di.vendor, Model: di.model,
				Serial: di.serial, Technology: di.technology,
				PhaseNum: phase.PhaseNumber, PhaseName: phase.PhaseName,
				Skipped: reason,
				Kind:    di.kind, Members: di.members,
			}
		}
		// Emit in disk order, attaching time-series if available.
		for _, name := range diskOrder {
			if dp, ok := byDisk[name]; ok {
				if phase.LogSeries != nil {
					if ts, ok2 := phase.LogSeries[name]; ok2 && ts != nil {
						dp.TSiops = ts.IOPS
						dp.TSbw = ts.BW
						dp.TSlat = ts.Lat
					}
				}
				dp.TSflush = phase.FlushSeries[name]
				dp.TSmembers = phase.MemberSeries[name]
				dp.TSmd = phase.MDSeries[name].Points
				dp.MDEvents = phase.MDSeries[name].Events
				dp.RampSec = phase.RampSec
				dp.LogAvgMsec = phase.LogAvgMsec
				dp.Flush = phase.FlushTrace[name]
				if dp.Flush != nil {
					dp.FlushSource = flushtrace.SourceLine(dp.Flush)
				}
				dp.IOTrace = phase.IOTrace[name]
				dp.IOTraceNote = phase.IOTraceNotes[name]
				for dev, pts := range phase.IOTraceSeries[name] {
					if dev == name {
						dp.TSiolat = pts
						continue
					}
					if dp.TSiolatMembers == nil {
						dp.TSiolatMembers = map[string][]IOLatPoint{}
					}
					dp.TSiolatMembers[dev] = pts
				}
				if l, ok := result.Labels[name]; ok {
					dp.Setting = l.Setting
					dp.SettingValue = l.Value
					dp.RunID = result.ID
					dp.Invalid = l.Value != "" && !l.Valid
				}
				out = append(out, *dp)
			}
		}
	}
	return out
}

// generateDiskGraphs returns per-disk data with phases on the X axis (used by export).
func generateDiskGraphs(result *BenchmarkResult) []DiskGraphData {
	seen := map[string]bool{}
	diskOrder := []string{}
	diskTitle := map[string]string{}
	for _, drive := range result.Drives {
		name := drive.Device.Name
		if seen[name] {
			continue
		}
		seen[name] = true
		diskOrder = append(diskOrder, name)
		title := name
		v := strings.TrimSpace(drive.Device.Vendor)
		m := strings.TrimSpace(drive.Device.Model)
		if v != "" || m != "" {
			title = fmt.Sprintf("%s  ·  %s %s", name, v, m)
		}
		diskTitle[name] = title
	}

	graphs := make([]DiskGraphData, 0, len(diskOrder))
	for _, diskName := range diskOrder {
		phases := make([]PhaseMetrics, 0, len(result.Phases))
		for _, phase := range result.Phases {
			pm := PhaseMetrics{PhaseName: phase.PhaseName, PhaseNum: phase.PhaseNumber}
			for _, job := range phase.Jobs {
				if job.Device != diskName {
					continue
				}
				if job.ReadStats != nil && job.ReadStats.IOPS > 0 {
					pm.ReadIOPS += job.ReadStats.IOPS
					pm.ReadBWMbps += job.ReadStats.Bandwidth / 1024
					if job.ReadStats.AvgLatNS > 0 {
						pm.ReadLatUS = job.ReadStats.AvgLatNS / 1000
					}
					pm.HasRead = true
				}
				if job.WriteStats != nil && job.WriteStats.IOPS > 0 {
					pm.WriteIOPS += job.WriteStats.IOPS
					pm.WriteBWMbps += job.WriteStats.Bandwidth / 1024
					if job.WriteStats.AvgLatNS > 0 {
						pm.WriteLatUS = job.WriteStats.AvgLatNS / 1000
					}
					pm.HasWrite = true
				}
			}
			phases = append(phases, pm)
		}
		if len(phases) > 0 {
			graphs = append(graphs, DiskGraphData{
				Disk:   diskName,
				Title:  diskTitle[diskName],
				Phases: phases,
			})
		}
	}
	return graphs
}

// handleResultGraphs serves the flat disk×phase data as JSON.
func handleResultGraphs(w http.ResponseWriter, r *http.Request) {
	resultID := r.URL.Query().Get("id")
	if resultID == "" {
		http.Error(w, "Missing result ID", http.StatusBadRequest)
		return
	}
	result, ok := globalResultsStore.GetResult(resultID)
	if !ok {
		http.Error(w, "Result not found", http.StatusNotFound)
		return
	}
	data := generateDiskPhaseData(result)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		if logger != nil {
			logger.Error("encode graphs", "err", err)
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// handleResultsList serves a list of all benchmark results.
func handleResultsList(w http.ResponseWriter, r *http.Request) {
	results := globalResultsStore.ListResults()
	summaries := make([]map[string]interface{}, len(results))
	for i, result := range results {
		summaries[i] = map[string]interface{}{
			"id":          result.ID,
			"timestamp":   result.Timestamp,
			"mode":        result.Mode,
			"drive_count": len(result.Drives),
			"phase_count": len(result.Phases),
			"output_dir":  result.OutputDir,
		}
		// A member run of a run group: its group and the value(s) it ran under.
		if result.GroupID != "" {
			summaries[i]["group_id"] = result.GroupID
			summaries[i]["setting_values"] = labelValues(result)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(summaries); err != nil {
		if logger != nil {
			logger.Error("encode results list", "err", err)
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// labelValues returns the distinct setting values of a run's labels, sorted.
func labelValues(result *BenchmarkResult) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range result.Labels {
		if l.Value != "" && !seen[l.Value] {
			seen[l.Value] = true
			out = append(out, l.Value)
		}
	}
	sort.Strings(out)
	return out
}

// filterGroup returns the HTML for one sidebar filter group.
func filterGroup(dim, label string) string {
	return fmt.Sprintf(`
        <div class="fg">
            <div class="fg-hdr">
                <span class="fg-title">%s</span>
                <span class="fg-allnone">
                    <a href="#" onclick="setAll('%s',true);return false">All</a>
                    <a href="#" onclick="setAll('%s',false);return false">None</a>
                </span>
            </div>
            <ul class="flist" id="list-%s"></ul>
        </div>`, label, dim, dim, dim)
}

// groupFilters is the sidebar block for a run group's data: the setting value
// filter and the invalid toggle. It stays hidden until the data carries
// setting values (see addSettingDim).
var groupFilters = `
        <div id="group-filters" style="display:none">` + filterGroup("setting", "Setting value") + `
        <div class="fg"><ul class="flist"><li><label><input type="checkbox" id="hide-invalid" checked>&nbsp;Hide invalid results</label></li></ul></div>
        </div>`

// scaleSelector is the sidebar's Y scale selector (SCALE_CRITERIA in the
// page): the y range and SI prefix of the cards, per phase (default), one
// per metric over the whole page, or per card. It applies at once
// (setScaleCriterion), without Show Graphs; autocomplete off so a page
// restored from the browser cache does not show another choice than the
// one in use.
var scaleSelector = `
        <div class="fg">
            <div class="fg-hdr"><label class="fg-title" for="y-scale">Y scale</label></div>
            <select id="y-scale" class="fsel" autocomplete="off"
                    title="Global: one range per graph type over every phase; Per phase: one range per phase; Per graph: each graph its own range">
                <option value="global">Global</option>
                <option value="phase" selected>Per phase</option>
                <option value="graph">Per graph</option>
            </select>
        </div>`

// handleGraphsPage serves the graphs visualization page (?id=<run> or
// ?group=<group>).
func handleGraphsPage(w http.ResponseWriter, r *http.Request) {
	dataURL, _, ok := chartSource(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// The setting filter and the invalid toggle are shown only for data of
	// labelled drives (a run group); a plain run never sees them.
	sidebar := filterGroup("graph_type", "Graph Type") +
		scaleSelector +
		groupFilters +
		filterGroup("technology", "Drive Type") +
		filterGroup("vendor", "Vendor") +
		filterGroup("model", "Model") +
		filterGroup("serial", "Serial") +
		filterGroup("disk", "Device") +
		filterGroup("phase", "Phase")

	html := `<!DOCTYPE html>
<html>
<head>
    <title>bbbench - Benchmark Graphs</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.0/dist/chart.umd.min.js"></script>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            display: flex; flex-direction: column; height: 100vh; overflow: hidden;
            background: #f1f5f9;
        }
        __NAVBAR_CSS__
        .workspace { display: flex; flex: 1; overflow: hidden; }

        /* ── Sidebar ── */
        .sidebar {
            width: 240px; flex-shrink: 0;
            background: #fff; border-right: 1px solid #e2e8f0;
            overflow-y: auto; padding: 12px 12px 0;
            display: flex; flex-direction: column; gap: 0;
        }
        .fg { border-bottom: 1px solid #f1f5f9; padding: 10px 0 8px; }
        .fg-hdr {
            display: flex; justify-content: space-between; align-items: center;
            margin-bottom: 5px;
        }
        .fg-title {
            font-size: 10px; font-weight: 700; color: #64748b;
            text-transform: uppercase; letter-spacing: 0.08em;
        }
        .fg-allnone { display: flex; gap: 6px; }
        .fg-allnone a { font-size: 11px; color: #2563eb; text-decoration: none; }
        .fg-allnone a:hover { text-decoration: underline; }
        .flist { list-style: none; }
        .fsel { width: 100%; font: inherit; font-size: 11px; color: #334155; padding: 3px 4px;
                border: 1px solid #cbd5e1; border-radius: 5px; background: #fff; cursor: pointer; }
        .flist li label {
            display: flex; align-items: flex-start; gap: 5px;
            font-size: 11px; color: #334155; cursor: pointer;
            padding: 2px 0; line-height: 1.4; word-break: break-all;
        }
        .flist li label input[type=checkbox] { flex-shrink: 0; margin-top: 2px; accent-color: #2563eb; }
        .sidebar-bottom {
            padding: 12px 0; display: flex; flex-direction: column; gap: 8px;
            position: sticky; bottom: 0; background: #fff;
            border-top: 1px solid #e2e8f0;
        }
        .count { font-size: 11px; color: #64748b; }
        .btn-show {
            width: 100%; padding: 8px; background: #2563eb; color: #fff;
            border: none; border-radius: 6px; font-size: 13px; font-weight: 600;
            cursor: pointer; transition: background 0.15s;
        }
        .btn-show:hover { background: #1d4ed8; }

        /* ── Main ── */
        .main { flex: 1; overflow-y: auto; padding: 16px; display: flex; flex-direction: column; gap: 16px; }

        /* Metric-level foldable section */
        .metric-section { display: flex; flex-direction: column; }
        .sec-hdr {
            display: flex; align-items: center; gap: 8px;
            cursor: pointer; user-select: none;
            font-size: 14px; font-weight: 700; color: #1e293b;
            padding: 10px 14px; border-bottom: 2px solid #e2e8f0;
            background: #fff; border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.06);
        }
        .sec-hdr .sec-trace { font-size: 11px; font-weight: 400; color: #64748b; margin-top: 2px; }
        .sec-hdr:hover { background: #f8fafc; }
        .sec-hdr .chev { display: inline-block; font-size: 10px; color: #94a3b8; transition: transform 0.18s; }
        .sec-hdr.collapsed .chev { transform: rotate(-90deg); }
        .sec-body { padding: 8px 0 4px; display: flex; flex-direction: column; gap: 0; }
        .sec-body.hidden { display: none; }

        /* Phase-level foldable sub-section */
        .phase-hdr {
            display: flex; align-items: center; gap: 7px;
            cursor: pointer; user-select: none;
            font-size: 12px; font-weight: 700; color: #475569;
            padding: 7px 12px; margin: 8px 0 6px;
            background: #f1f5f9; border-left: 3px solid #2563eb;
            border-radius: 0 5px 5px 0;
        }
        .phase-hdr:hover { background: #e2e8f0; }
        .phase-hdr .chev { display: inline-block; font-size: 9px; color: #94a3b8; transition: transform 0.18s; }
        .phase-hdr.collapsed .chev { transform: rotate(-90deg); }
        .phase-count { font-weight: 400; color: #94a3b8; font-size: 11px; }
        .mode-toggle { margin-left: auto; display: inline-flex; border: 1px solid #cbd5e1; border-radius: 5px; overflow: hidden; }
        .mode-toggle button { font: inherit; font-size: 11px; font-weight: 600; padding: 2px 9px; border: 0; background: #fff; color: #475569; cursor: pointer; }
        .mode-toggle button + button { border-left: 1px solid #cbd5e1; }
        .mode-toggle button.on { background: #2563eb; color: #fff; }
        .phase-body { padding-bottom: 8px; }
        .phase-body.hidden { display: none; }

        .chart-grid {
            display: grid;
            grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
            gap: 12px;
        }
        .chart-card {
            background: #fff; border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.08);
            padding: 10px 12px 8px; display: flex; flex-direction: column;
            cursor: pointer;
        }
        .chart-card:hover { box-shadow: 0 4px 12px rgba(0,0,0,0.15); }
        .card-title {
            font-size: 12px; font-weight: 700; color: #1e293b;
            white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
        }
        .src {
            font-size: 9px; font-weight: 600; letter-spacing: 0.03em; vertical-align: middle;
            padding: 0 5px; border-radius: 8px; border: 1px solid #cbd5e1;
            color: #64748b; background: #f8fafc; cursor: help;
        }
        .none-note.skipped { color: #1e3a5f; background: #e0ecff; }
        .none-note { display: flex; align-items: center; font-size: 11px; color: #92400e;
                     background: #fef3c7; border-radius: 4px; padding: 6px 8px; height: auto; min-height: 60px; }
        .src-ftrace { color: #6d28d9; border-color: #c4b5fd; background: #f5f3ff; }
        .card-subtitle {
            font-size: 10px; color: #64748b; margin-top: 2px;
            white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
        }
        .chart-wrap { position: relative; height: 160px; margin-top: 6px; }
        .md-badge {
            font-size: 9px; font-weight: 600; vertical-align: middle;
            padding: 0 5px; border-radius: 8px; border: 1px solid #fcd34d;
            color: #b45309; background: #fffbeb; cursor: help;
        }
        .md-badge.md-degraded { color: #be123c; border-color: #fda4af; background: #fff1f2; }
        .md-note { font-size: 10px; color: #b45309; margin-top: 2px; }
        .src-line { font-size: 10px; color: #6d28d9; margin-top: 2px; word-break: break-word; }
        /* Series toggles: one checkbox and swatch per series of a card. */
        .series-row { display: flex; flex-wrap: wrap; gap: 1px 8px; margin-top: 4px; font-size: 10px; color: #334155; cursor: default; }
        .series-row:empty { display: none; }
        .series-row label { display: inline-flex; align-items: center; gap: 3px; cursor: pointer; white-space: nowrap; }
        .series-row label.off { color: #94a3b8; }
        .series-row input { margin: 0; width: 11px; height: 11px; accent-color: #2563eb; }
        .series-row .swatch { display: inline-block; width: 12px; height: 8px; border-radius: 2px; }
        .series-row .swatch.dash { height: 0; border-top: 2px dashed; border-radius: 0; background: none; }
        .series-row label.off .swatch { opacity: 0.35; }
        .modal-series .series-row { font-size: 12px; gap: 2px 12px; margin-top: 6px; }
        .modal-series .series-row input { width: 13px; height: 13px; }
        .modal-src { font-size: 11px; color: #6d28d9; margin-top: 3px; word-break: break-word; }
        .modal-md { margin-top: 4px; }
        .modal-md .md-badge { font-size: 11px; }
        .modal-md .md-note { font-size: 12px; }
        .ts-warn {
            font-size: 10px; color: #92400e; background: #fef3c7;
            border: 1px solid #fcd34d; border-radius: 3px;
            padding: 2px 6px; margin-top: 4px;
        }
        .placeholder {
            display: flex; align-items: center; justify-content: center;
            flex: 1; color: #94a3b8; font-size: 14px; padding: 60px;
            text-align: center;
        }

        /* ── Modal overlay ── */
        .modal-overlay {
            display: none; position: fixed; inset: 0; z-index: 1000;
            background: rgba(0,0,0,0.65);
            align-items: center; justify-content: center;
        }
        .modal-overlay.active { display: flex; }
        .modal-card {
            background: #fff; border-radius: 12px;
            box-shadow: 0 8px 40px rgba(0,0,0,0.3);
            padding: 16px 20px 14px;
            width: calc(100vw - 40px); height: calc(100vh - 40px);
            display: flex; flex-direction: column;
        }
        .modal-card-title  { font-size: 16px; font-weight: 700; color: #1e293b; }
        .modal-card-subtitle { font-size: 12px; color: #64748b; margin-top: 3px; }
        .modal-hint { font-size: 11px; color: #94a3b8; margin-top: 4px; }
        .modal-chart-wrap  { position: relative; flex: 1; margin-top: 10px; }
        .modal-ts-warn {
            font-size: 11px; color: #92400e; background: #fef3c7;
            border: 1px solid #fcd34d; border-radius: 3px;
            padding: 3px 8px; margin-top: 6px;
        }
    </style>
</head>
<body>
__NAVBAR_HTML__
<div class="workspace">
    <aside class="sidebar">
        __SIDEBAR__
        <div class="sidebar-bottom">
            <div class="count" id="count"></div>
            <button class="btn-show" id="btn-show">Show Graphs</button>
        </div>
    </aside>
    <main class="main" id="main">
        <div class="placeholder">Select filters and click "Show Graphs".</div>
    </main>
</div>

<div class="modal-overlay" id="modal-overlay">
    <div class="modal-card" id="modal-card">
        <div class="modal-card-title" id="modal-title"></div>
        <div class="modal-card-subtitle" id="modal-subtitle"></div>
        <div class="modal-src" id="modal-src"></div>
        <div class="modal-series" id="modal-series"></div>
        <div class="modal-md" id="modal-md"></div>
        <div class="modal-hint">Press Esc or click outside to close</div>
        <div id="modal-ts-warn"></div>
        <div class="modal-chart-wrap">
            <canvas id="modal-canvas"></canvas>
        </div>
    </div>
</div>

<script>
    const DATA_URL = __DATA_URL__;
    let allData = [];
    const charts = {};

    const METRICS = [
        // label names the metric without a unit: each card's y axis shows its
        // base unit, the numbers the SI prefix of its phase (siScale).
        { key: 'iops', label: 'IOPS',          unit: 'IOPS',  tsKey: 'ts_iops', source: 'fio',
          sumR: d => d.read_iops||0,    sumW: d => d.write_iops||0    },
        { key: 'bw',   label: 'Bandwidth',     unit: 'MB/s',  tsKey: 'ts_bw', source: 'fio',
          sumR: d => d.read_bw_mbps||0, sumW: d => d.write_bw_mbps||0 },
        { key: 'lat',  label: 'Latency',       unit: '\u03bcs', tsKey: 'ts_lat', source: 'fio',
          sumR: d => d.read_lat_us||0,  sumW: d => d.write_lat_us||0  },
        // Device Flush latency, timed from kernel trace events (not fio).
        { key: 'flush', label: 'Flush latency', unit: '\u03bcs', tsKey: 'ts_flush', source: 'ftrace',
          has:  d => !!d.flush || !!d.skipped || !!(d.ts_flush && d.ts_flush.length),
          srcTitle: 'Measured from kernel ftrace events (device Flush commands), not by fio',
          sumR: d => 0, sumW: d => d.flush ? (d.flush.mean_ns||0)/1000 : 0 },
        // Read and write request latency of the device, timed from kernel
        // trace events (any device: NVMe, SATA, SAS, md members).
        { key: 'iolat', label: 'Device latency (ftrace)', unit: '\u03bcs', tsKey: 'ts_iolat', source: 'ftrace',
          srcTitle: 'Measured from kernel ftrace events (each read and write request, issue to completion), not by fio',
          has:  d => !!d.io_trace || !!d.io_trace_note,
          sumR: d => iolatMean(d, 'r', 'device'), sumW: d => iolatMean(d, 'w', 'device') },
        // The same requests timed from their block-layer insert (time in
        // the I/O scheduler and the software queues included).
        { key: 'iolatblk', label: 'Block-layer latency (ftrace)', unit: '\u03bcs', tsKey: 'ts_iolat', source: 'ftrace',
          srcTitle: 'Measured from kernel ftrace events (each read and write request, block-layer insert to completion), not by fio',
          has:  (d, all) => iolatBlkHas(d, all), none: data => iolatBlkNoneText(data),
          sumR: d => iolatMean(d, 'r', 'blocklayer'), sumW: d => iolatMean(d, 'w', 'blocklayer') },
    ];
    const SRC_TITLE = {
        fio: 'Measured by fio',
    };
    // flushNone explains a Flush phase that was traced but saw no Flush.
    function flushNone(d, metric) {
        if (metric.key !== 'flush' || !d.flush || d.flush.count > 0) return '';
        return 'ftrace saw 0 Flush commands' + (d.flush.write_cache === 'write through'
            ? ': the kernel sends no Flush to this drive (write cache: write through, i.e. its volatile write cache is disabled or absent).'
            : '.');
    }
    // iolatKind is the ftrace latency kind a card draws on its own: device
    // for Device latency, blocklayer for Block-layer latency, '' otherwise
    // (the Latency card overlays the means of both).
    function iolatKind(metric) {
        return metric.key === 'iolat' ? 'device' : metric.key === 'iolatblk' ? 'blocklayer' : '';
    }
    // iolatMean is the traced device's summary mean latency (us) of a
    // direction and kind: the first traced device is the target.
    function iolatMean(d, dir, kind) {
        const dev = d.io_trace && d.io_trace.devices ? d.io_trace.devices[0] : null;
        const k = dev && dev.stats && dev.stats[kind];
        const st = k && k[dir];
        return st && st.mean_ns > 0 ? st.mean_ns / 1000 : 0;
    }
    // iolatPtsHave: some point of that kind saw a request.
    function iolatPtsHave(pts, kind) {
        return (pts || []).some(p => p.kind === kind && p.n > 0);
    }
    // iolatMembersHave: some member has points of that kind.
    function iolatMembersHave(d, kind) {
        return !!d.ts_iolat_members && Object.values(d.ts_iolat_members).some(pts => iolatPtsHave(pts, kind));
    }
    // iolatStatsHas: a traced device (io_trace.devices[]) has a summary of
    // that kind.
    function iolatStatsHas(dev, kind) {
        return !!(dev.stats && dev.stats[kind] && Object.keys(dev.stats[kind]).length > 0);
    }
    // iolatHasKind: the entry has latency of that kind, the target's or a
    // member's, as a series or a summary.
    function iolatHasKind(d, kind) {
        if (iolatPtsHave(d.ts_iolat, kind) || iolatMembersHave(d, kind)) return true;
        const devs = (d.io_trace && d.io_trace.devices) || [];
        return devs.some(dev => iolatStatsHas(dev, kind));
    }
    // iolatBlkHas: an entry gets a Block-layer latency card when it has
    // block-layer latency, or when it was traced (or ftrace was off) and
    // another entry shown has some: its card says why it has none. With no
    // block-layer data at all, no card.
    function iolatBlkHas(d, all) {
        if (iolatHasKind(d, 'blocklayer')) return true;
        return (!!d.io_trace || !!d.io_trace_note) && (all || []).some(x => iolatHasKind(x, 'blocklayer'));
    }
    // iolatBlkNoneText is the text of a Block-layer latency section without
    // a card: the reasons of the shown entries, once each.
    function iolatBlkNoneText(data) {
        const why = [];
        data.forEach(d => {
            const w = iolatNone(d, {key: 'iolatblk'}).replace(/^no block-layer series: /, '');
            if (w && why.indexOf(w) === -1) why.push(w);
        });
        return 'No block-layer data for the selected disks and phases' + (why.length ? ': ' + why.join('; ') : '.');
    }
    // iolatNone explains a Device or Block-layer latency card that has
    // nothing to draw: the reason ftrace was off, that the target saw no
    // read or write request (a bio-based md/dm node has no request events;
    // its members do), or why it has no block-layer series (the device's
    // block_layer_note: no insert tracepoint, a multipath head, ...).
    function iolatNone(d, metric) {
        const kind = iolatKind(metric);
        if (!kind) return '';
        if (!d.io_trace) return d.io_trace_note || '';
        const dev = d.io_trace.devices && d.io_trace.devices[0];
        if (!dev) return 'no traced device';
        // A bio-based node's own series on the Device card are its bios.
        const own = iolatStatsHas(dev, kind) || (kind === 'device' && iolatStatsHas(dev, 'bio'));
        if (own || iolatMembersHave(d, kind)) return '';
        if (kind === 'device') return dev.note || 'ftrace saw no read or write requests on ' + dev.name;
        return 'no block-layer series: ' + (dev.block_layer_note || dev.note || 'ftrace saw no block-layer requests on ' + dev.name);
    }
    // iolatNodeNote: the Block-layer card of a bio-based target (an md or
    // dm node: no request queue) draws its members only; the target's
    // block_layer_note says so and where its bio series is. '' otherwise.
    function iolatNodeNote(d, metric) {
        if (metric.key !== 'iolatblk' || !d.io_trace) return '';
        const dev = (d.io_trace.devices || [])[0];
        return dev && dev.bio_based && dev.block_layer_note ? dev.name + ': ' + dev.block_layer_note : '';
    }
    // iolatPoints turns the points of one kind and direction into chart
    // points: x on the member window grid, y = stat in us (negative for
    // writes); windows without a request are left out.
    function iolatPoints(d, pts, kind, dir, stat) {
        const step = memberStep(d);
        return (pts || []).filter(p => p.kind === kind && p.dir === dir && p.n > 0)
            .map(p => ({x: memberKey(p.t, step), y: (dir === 'w' ? -1 : 1) * p[stat]}));
    }
    // ftraceDatasets are the ftrace lines of a chart: the Latency card gets
    // the mean device and block layer latency of the target next to fio's,
    // the Device latency card mean and p99 of the device kind plus each
    // member's device mean, the Block-layer latency card the same of the
    // block-layer kind. A bio-based target (an md or dm node) has bio-level
    // series instead (kind bio): on the Latency card its mean, on the Device
    // latency card mean and p99, in the array's fio colours. A kind and
    // direction without points has no dataset.
    function ftraceDatasets(d, metric) {
        const out = [], only = iolatKind(metric);
        if (metric.key !== 'lat' && !only) return out;
        const full = !!only;
        const base = {fill: false, tension: 0.2, pointRadius: 0, pointHoverRadius: 3, borderWidth: 1.5, spanGaps: true};
        const rgb = {r: [37, 99, 235], w: [220, 38, 38]}, P99 = [8, 3, 2, 3];
        const mk = (label, data, skey, dir, color, dash, extra) => Object.assign({}, base,
            {label, data, skey, dir, borderColor: color, backgroundColor: color, borderDash: dash}, extra);
        const kinds = [['device', 'device', [6, 3]], ['blocklayer', 'block layer', [2, 2]]].filter(k => !only || k[0] === only);
        // The target's rows: [kind, stat, dir, label, dash, alpha, extra],
        // the card's kinds first, then a bio-based node's bios.
        const rows = [];
        ['r', 'w'].forEach(dir => kinds.forEach(([kind, kname, dash]) => {
            const name = (dir === 'r' ? 'Read ' : 'Write ') + kname;
            rows.push([kind, 'mean', dir, name + (full ? ' mean' : '') + ' (ftrace)', dash, 0.9, {}]);
            if (full) rows.push([kind, 'p99', dir, name + ' p99 (ftrace)', P99, 0.6, {}]);
        }));
        if (!only || only === 'device') ['r', 'w'].forEach(dir => {
            const name = 'Array ' + (dir === 'r' ? 'read' : 'write') + ' bio ';
            rows.push(['bio', 'mean', dir, name + 'mean (ftrace)', [6, 3], 0.95, {borderWidth: 2}]);
            if (full) rows.push(['bio', 'p99', dir, name + 'p99 (ftrace)', P99, 0.6, {borderWidth: 2}]);
        });
        rows.forEach(([kind, stat, dir, label, dash, a, extra]) => {
            const data = iolatPoints(d, d.ts_iolat, kind, dir, stat);
            if (data.length) out.push(mk(label, data, 'ftrace.' + kind + '.' + stat + '.' + dir, dir, rgba(rgb[dir], a), dash, extra));
        });
        if (full && d.ts_iolat_members) {
            const names = Object.keys(d.ts_iolat_members).sort(), colors = memberColors(names);
            const kname = kinds[0][1];
            ['r', 'w'].forEach(dir => names.forEach(n => {
                const data = iolatPoints(d, d.ts_iolat_members[n], only, dir, 'mean');
                if (!data.length) return;
                out.push(mk(n + (dir === 'r' ? ' read ' : ' write ') + kname + ' mean (ftrace)', data, 'ftrace.member:' + n, dir,
                    rgba(colors[n], 0.9), dir === 'w' ? [4, 2] : [], {borderWidth: 1.2}));
            }));
        }
        return out;
    }
    // sourceLine says where each series of a card comes from: fio, the
    // kernel's per-disk counters of the members, the monitored ftrace
    // tracepoints (their names come from the data, never from here).
    function sourceLine(d, metric) {
        const trace = d.io_trace ? (d.io_trace.text || '') : (d.io_trace_note || '');
        const members = d.ts_members && Object.keys(d.ts_members).length > 0
            ? ' \u00b7 members: kernel stat (/sys/block/<dev>/stat)' : '';
        switch (metric.key) {
        case 'iops':
        case 'bw':
            return 'fio' + members;
        case 'lat':
            return 'fio' + members + (trace ? ' \u00b7 ' + trace : '');
        case 'flush':
            return d.flush_source || '';
        case 'iolat':
            return (d.io_trace && d.io_trace.text_device) || trace;
        case 'iolatblk':
            return (d.io_trace && d.io_trace.text_blocklayer) || trace;
        }
        return '';
    }
    // lossNote warns when the kernel dropped trace events of this phase.
    function lossNote(d, metric) {
        if (metric.key !== 'lat' && !iolatKind(metric)) return '';
        const n = d.io_trace ? d.io_trace.lost || 0 : 0;
        return n > 0 ? '\u26a0 ' + n + ' trace events lost: the ftrace series are incomplete' : '';
    }
    // addLossNote appends the loss note to warnEl (which tsWarn may have filled).
    function addLossNote(warnEl, d, metric) {
        const n = lossNote(d, metric);
        if (!n || !warnEl) return;
        warnEl.className = warnEl.className.includes('modal') ? 'modal-ts-warn' : 'ts-warn';
        warnEl.textContent = (warnEl.textContent ? warnEl.textContent + ' \u00b7 ' : '') + n;
    }
    // ftraceRef is the static reference of the kernel tracepoints (trace
    // events) behind an ftrace metric and the pair each latency spans, per
    // source: NVMe vs everything else. '' for a metric without ftrace data.
    function ftraceRef(metric) {
        const bio = 'a bio-based md/dm node (no request queue): block_bio_queue -> block_bio_complete';
        const flush = 'NVMe: nvme_setup_cmd -> nvme_complete_rq (flush opcode); any other device, or NVMe without those tracepoints: block_rq_issue -> block_rq_complete (rwbs F, 0 sectors); ' + bio + ' (its flush bios: rwbs F, 0 sectors)';
        const dev = 'NVMe: nvme_setup_cmd -> nvme_complete_rq (read and write commands); any other device, or NVMe without those tracepoints: block_rq_issue -> block_rq_complete (read and write requests); ' + bio + ' (its read and write bios)';
        const blk = 'every device, NVMe included: block_rq_insert -> block_rq_complete (a request issued without an insert counts from block_rq_issue; merges via block_rq_merge); none on a bio-based md/dm node';
        switch (metric.key) {
        case 'flush': return flush;
        case 'iolat': return dev;
        case 'iolatblk': return blk;
        case 'lat': return 'device: ' + dev + ' \u00b7 block layer: ' + blk;
        }
        return '';
    }
    // ftraceEvents is the union of the tracepoint pairs actually monitored in
    // the shown runs for a metric (Flush summary events, io_trace device
    // events; the insert pair is the block-layer one), in first-seen order.
    function ftraceEvents(data, metric) {
        const out = [];
        const add = e => { if (e && !out.includes(e)) out.push(e); };
        (data || []).forEach(d => {
            if (metric.key === 'flush') {
                ((d.flush && d.flush.events) || []).forEach(add);
                return;
            }
            if (metric.key !== 'iolat' && metric.key !== 'iolatblk' && metric.key !== 'lat') return;
            ((d.io_trace && d.io_trace.devices) || []).forEach(dev => (dev.events || []).forEach((e, i) => {
                // the block-layer pair is the last event when the device has one
                const blk = !!dev.block_layer && i === dev.events.length - 1;
                if (metric.key === 'lat' || (metric.key === 'iolatblk') === blk) add(e);
            }));
        });
        return out;
    }
    // srcTitleOf is the tooltip of a metric's source badge: what measures it
    // and, for the ftrace metrics, the kernel tracepoints (static reference)
    // plus those monitored in the shown runs (data, when there are any).
    function srcTitleOf(metric, data) {
        let t = metric.srcTitle || SRC_TITLE[metric.source];
        const ref = ftraceRef(metric);
        if (!ref) return t;
        t += metric.key === 'lat' ? '\nDashed ftrace lines, kernel tracepoints: ' + ref
            : '\nKernel tracepoints: ' + ref;
        const mon = ftraceEvents(data, metric);
        if (mon.length) t += '\nMonitored in the shown runs: ' + mon.join('; ');
        return t;
    }
    function srcBadge(metric, data) {
        const b = document.createElement('span');
        b.className = 'src src-' + metric.source;
        b.textContent = metric.source;
        b.title = srcTitleOf(metric, data);
        return b;
    }

    // Selections: Set of checked values per dimension.
    const SEL = {
        graph_type: new Set(),
        technology: new Set(),
        vendor:     new Set(),
        model:      new Set(),
        serial:     new Set(),
        disk:       new Set(),
        phase:      new Set(),
    };

    // Data dimensions and their field accessors. The setting dimension (the
    // value labels of a run group) is added by addSettingDim only when the
    // entries carry setting_value.
    const DATA_DIMS = ['technology', 'vendor', 'model', 'serial', 'disk', 'phase'];
    const DIM_F = {
        technology: d => d.technology || '',
        vendor:     d => d.vendor     || '',
        model:      d => d.model      || '',
        serial:     d => d.serial     || '',
        disk:       d => d.disk       || '',
        phase:      d => d.phase_name || '',
    };
    // Group data: hide invalid results unless asked; RPV counts the runs per value.
    let SHOW_INVALID = false;
    let RPV = {};
    function addSettingDim() {
        if (!hasSettingData(allData) || DATA_DIMS.indexOf('setting') !== -1) return;
        DATA_DIMS.unshift('setting');
        DIM_F.setting = d => d.setting_value || '';
        SEL.setting = new Set();
        RPV = runsPerValue(allData);
        document.getElementById('group-filters').style.display = '';
    }
    function setShowInvalid(on) {
        SHOW_INVALID = on;
        updateLists();
    }
    // Entries the filters work on: invalid ones only when shown.
    function pool() {
        return visibleEntries(allData, SHOW_INVALID);
    }

    // Entries matching all dims except the given one.
    function dataExcept(dim) {
        return pool().filter(d =>
            DATA_DIMS.every(fd => fd === dim || SEL[fd].has(DIM_F[fd](d)))
        );
    }

    // Sorted unique values for dim that appear in dataExcept(dim).
    function availableFor(dim) {
        const seen = new Set();
        dataExcept(dim).forEach(d => seen.add(DIM_F[dim](d)));
        return [...seen].sort();
    }

    // Build or rebuild one filter list.
    function buildList(dim, values, isStatic) {
        const ul = document.getElementById('list-' + dim);
        ul.innerHTML = '';
        values.forEach(v => {
            const li  = document.createElement('li');
            const lbl = document.createElement('label');
            const cb  = document.createElement('input');
            cb.type    = 'checkbox';
            cb.value   = v;
            cb.checked = SEL[dim].has(v);
            cb.addEventListener('change', () => {
                if (cb.checked) SEL[dim].add(v); else SEL[dim].delete(v);
                if (isStatic) { updateCount(); } else { updateLists(); }
            });
            lbl.appendChild(cb);
            lbl.appendChild(document.createTextNode('\u00a0' + (v || '(unknown)')));
            li.appendChild(lbl);
            ul.appendChild(li);
        });
    }

    function updateLists() {
        DATA_DIMS.forEach(dim => buildList(dim, availableFor(dim), false));
        updateCount();
    }

    function updateCount() {
        const n = filteredData().length;
        document.getElementById('count').textContent =
            n + ' combination' + (n !== 1 ? 's' : '') + ' match';
    }

    // Check/uncheck all visible items in a list.
    function setAll(dim, checked) {
        const ul = document.getElementById('list-' + dim);
        ul.querySelectorAll('input[type=checkbox]').forEach(cb => {
            cb.checked = checked;
            if (checked) SEL[dim].add(cb.value); else SEL[dim].delete(cb.value);
        });
        if (dim === 'graph_type') updateCount(); else updateLists();
    }

    function filteredData() {
        return pool().filter(d =>
            DATA_DIMS.every(fd => SEL[fd].has(DIM_F[fd](d)))
        );
    }

    function initFilters() {
        // graph_type: static list
        const GT = [['iops','IOPS'],['bw','Bandwidth'],['lat','Latency'],['flush','Flush latency'],['iolat', 'Device latency (ftrace)'],
            ['iolatblk', 'Block-layer latency (ftrace)']];
        const ul = document.getElementById('list-graph_type');
        GT.forEach(([v, label]) => {
            SEL.graph_type.add(v);
            const li = document.createElement('li');
            const lbl = document.createElement('label');
            const cb = document.createElement('input');
            cb.type = 'checkbox'; cb.value = v; cb.checked = true;
            cb.addEventListener('change', () => {
                if (cb.checked) SEL.graph_type.add(v); else SEL.graph_type.delete(v);
                updateCount();
            });
            lbl.appendChild(cb);
            lbl.appendChild(document.createTextNode('\u00a0' + label));
            lbl.appendChild(document.createTextNode(' '));
            lbl.appendChild(srcBadge(METRICS.find(m => m.key === v), allData));
            li.appendChild(lbl);
            ul.appendChild(li);
        });
        addSettingDim();
        // Data dims: all values selected by default, except trim phases.
        DATA_DIMS.forEach(dim => {
            const vals = [...new Set(allData.map(d => DIM_F[dim](d)))].sort();
            vals.forEach(v => {
                if (dim !== 'phase' || !isTrimPhase(v)) SEL[dim].add(v);
            });
            buildList(dim, vals, false);
        });
        updateCount();
    }

    function isTrimPhase(name) {
        const n = (name || '').toLowerCase();
        return n.indexOf('trim') !== -1 || n.indexOf('blkdiscard') !== -1;
    }

    // ── Chart rendering ──


    // The graph of a phase: GRAPH_MODES lists the choices (more can be
    // added); the choice is kept per metric and phase. 'normal' draws
    // every member and the array as plain lines, 'stacked' stacks the members
    // of IOPS and bandwidth (not the default, and never offered for latency,
    // which is never stacked; a drive has no members, so no toggle).
    const GRAPH_MODES = [['normal', 'Normal'], ['stacked', 'Stacked']];
    const graphModeOf = {};
    function graphMode(metric, pname) { return graphModeOf[metric.key + ':' + pname] || 'normal'; }

    function destroyAllCharts() {
        Object.keys(charts).forEach(id => { charts[id].destroy(); delete charts[id]; });
    }

    function toggleSection(hdr, body) {
        const collapsed = body.classList.toggle('hidden');
        hdr.classList.toggle('collapsed', collapsed);
    }

    // Flush latency: one positive series (mean per second) from the stored
    // ftrace samples, or the phase mean as a single bar when no series exists.
    // ── Shared time axis ──
    // Every time-series chart counts seconds from the start of the phase and
    // spans 0..PHASE_X[phase], so the graphs of a phase line up. fio's own logs start when its
    // ramp-up ends, so their points are shifted by the ramp; the Flush series
    // already counts from the start of tracing. The ramp-up is shaded.
    let PHASE_X = {};
    // phaseKey names the phase of a drive's data (the grouping of the cards).
    function phaseKey(d) { return d.phase_name || ('Phase ' + d.phase_num); }
    // phaseXMax gives, per phase, the end of the longest series of every
    // shown metric over all drives: fio (shifted), members, ftrace. Computed
    // from the data, so hidden series and the Y scale criterion do not matter.
    function phaseXMax(data, selMetrics) {
        const out = {};
        data.forEach(d => {
            const k = phaseKey(d);
            let m = out[k] || 0;
            selMetrics.forEach(me => {
                m = Math.max(m, seriesMaxT(d, me), memberMetric(d, me) ? memberMaxT(d, me) : 0);
                ftraceDatasets(d, me).forEach(ds => ds.data.forEach(p => { m = Math.max(m, p.x); }));
            });
            out[k] = m;
        });
        return out;
    }
    __CHART_SHARED_JS__

    // yAxisTitle is the y axis title of a card: the base unit of its ticks,
    // whose numbers carry the SI prefix (2.4G = 2.4 * 10^9 bytes/s).
    function yAxisTitle(sc) {
        return { display: true, text: sc.unit, color: '#94a3b8', font: {size: 9} };
    }
    // lineScales are the axes of a line chart: seconds on x (0..the phase's
    // maximum of d, shared by every card of the phase), y from yLow to yHigh in the base unit with the scale's
    // ticks and a darker zero line; extra overrides or adds y options.
    function lineScales(d, sc, yLow, yHigh, extra) {
        return {
            x: { type: 'linear', min: 0, max: PHASE_X[phaseKey(d)] || undefined,
                 title: { display: true, text: 'seconds', color: '#94a3b8', font: {size: 9} },
                 ticks: { font: {size: 9}, color: '#64748b', maxTicksLimit: 6 } },
            y: Object.assign({ min: yLow * sc.mul, max: yHigh * sc.mul, title: yAxisTitle(sc),
                 ticks: { callback: v => fmtScaled(v, sc), font: {size: 9}, color: '#64748b' },
                 grid: { color: ctx => ctx.tick.value === 0 ? '#94a3b8' : '#f1f5f9' } }, extra)
        };
    }
    // barAxis is the y axis of a bar chart (barChart).
    function barAxis(sc, yLow, yHigh) {
        return { min: yLow * sc.mul, max: yHigh * sc.mul, title: yAxisTitle(sc),
            ticks: { callback: v => fmtScaled(v, sc), font: {size: 9} } };
    }
    // barChart draws a card without series as bars (barData) on the y axis
    // y, the tooltip giving each value in the scale's unit.
    function barChart(canvas, datasets, sc, y) {
        return new Chart(canvas, {
            type: 'bar',
            data: barData(datasets),
            options: {
                responsive: true, maintainAspectRatio: false, animation: false,
                plugins: { legend: { display: false },
                    tooltip: { callbacks: { label: ctx => fmtScaled(ctx.raw, sc, true) + ' ' + sc.unit } } },
                scales: { y }
            }
        });
    }
    // windowTitle is the tooltip title of a time-series chart: the hovered
    // window, or its time without a window.
    function windowTitle(d) {
        return items => windowOf(d) ? 'window ' + items[0].raw.x + '\u2013' + (items[0].raw.x + windowOf(d)) + ' s' : items[0].raw.x + ' s';
    }

    // phaseScale is the SI scale of a card (siScale): its prefix is chosen
    // from the data maximum of its y range (the phase's, the metric's or the
    // card's own, per the Y scale criterion), not from the axis maximum, which
    // has 15% headroom (a 900 MiB/s peak must stay 944M, not become an axis
    // in G that stops at 1.09). A card plots in the base unit: its values and
    // its yLow/yHigh are multiplied by sc.mul.
    const AXIS_HEADROOM = 1.15;
    function phaseScale(metric, yLow, yHigh) {
        return siScale(metric, Math.max(yHigh || 0, -(yLow || 0)) / AXIS_HEADROOM);
    }

    function makeFlushChart(canvas, d, metric, yHigh) {
        const sc = phaseScale(metric, 0, yHigh);
        // Flush has no member breakdown: always the normal datasets.
        const cd = chartDatasets(d, metric, 'normal');
        const datasets = toBase(cd.datasets, sc);
        if (!cd.bars) {
            return new Chart(canvas, {
                type: 'line', plugins: [rampPlugin],
                data: { datasets },
                options: {
                    responsive: true, maintainAspectRatio: false, animation: false,
                    plugins: {
                        rampZone: { sec: rampOf(d) },
                        legend: { display: false },
                        tooltip: { callbacks: {
                            title: items => 'window ' + items[0].raw.x + '\u2013' + (items[0].raw.x + (d.flush && d.flush.avg_msec ? d.flush.avg_msec / 1000 : 1)) + ' s',
                            label: ctx => 'Flush latency: ' + fmtScaled(ctx.raw.y, sc, true) + ' ' + sc.unit +
                                ' (mean' + (d.flush && d.flush.avg_msec ? ' per ' + d.flush.avg_msec / 1000 + ' s' : '') + ')' } }
                    },
                    scales: lineScales(d, sc, 0, yHigh, { grid: { color: '#f1f5f9' } })
                }
            });
        }
        return barChart(canvas, datasets, sc, barAxis(sc, 0, yHigh));
    }
    // barData is the bar chart of a card without series: one bar per shown
    // dataset (its one value), a grey dash when none is left.
    function barData(datasets) {
        const bars = datasets.length ? datasets : [{label: '\u2014', data: [0], backgroundColor: '#e2e8f0'}];
        return { labels: bars.map(b => b.label),
            datasets: [{ data: bars.map(b => b.data[0]), backgroundColor: bars.map(b => b.backgroundColor), borderWidth: 1 }] };
    }

    // tsWarn tells warnEl (optional) how many points of ts miss thread
    // contributions.
    function tsWarn(warnEl, ts) {
        if (!warnEl) return;
        const incompleteCount = ts.filter(p => p.i).length;
        if (incompleteCount > 0) {
            warnEl.className = warnEl.className.includes('modal') ? 'modal-ts-warn' : 'ts-warn';
            warnEl.textContent = '\u26a0 ' + incompleteCount + ' point' +
                (incompleteCount !== 1 ? 's are' : ' is') +
                ' missing thread contributions';
        } else {
            warnEl.textContent = '';
        }
    }

    // ── members (begin) ──
    // An array phase carries ts_members (member -> {role, iops, bw, lat}, each
    // a series of {t, r, w}), stamped at window starts on the phase clock like
    // the Flush series: no fioShift. IOPS and bandwidth stack the members
    // (read up, write down) under the array's own series, drawn as unfilled
    // lines in a stack of their own so they are not added to the members.
    // Latency is never summed: one line per member and direction.
    const MEMBER_PALETTE = [
        [22,163,74], [217,119,6], [124,58,237], [8,145,178], [219,39,119],
        [101,163,13], [234,88,12], [13,148,136], [147,51,234], [161,98,7],
    ];
    function memberMetric(d, metric) {
        return ['iops', 'bw', 'lat'].indexOf(metric.key) !== -1 &&
            !!d.ts_members && Object.keys(d.ts_members).length > 0;
    }
    function memberNames(d) {
        return Object.keys(d.ts_members).sort();
    }
    // memberColors gives each member a colour from its name (none of them the
    // array's blue or red); two members that would share one take the next.
    function memberColors(names) {
        const used = {}, out = {};
        names.forEach(n => {
            let h = 0;
            for (let i = 0; i < n.length; i++) h = (h * 31 + n.charCodeAt(i)) >>> 0;
            let k = h % MEMBER_PALETTE.length;
            for (let j = 0; j < MEMBER_PALETTE.length && used[k]; j++) k = (k + 1) % MEMBER_PALETTE.length;
            used[k] = true;
            out[n] = MEMBER_PALETTE[k];
        });
        return out;
    }
    function rgba(c, a) {
        return 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + a + ')';
    }
    // memberStep is the width of one log window in seconds (the sampler
    // uses fio's --log-avg-msec); 1 when unknown.
    function memberStep(d) {
        const w = d.log_avg_msec ? d.log_avg_msec / 1000 : windowOf(d);
        return w > 0 ? w : 1;
    }
    // memberKey puts a time on the window grid: the nearest multiple of the
    // window, in whole ms. The sampler's stamps jitter by a few ms and fio's
    // are bucketed, so both land on the same key; a sub-second window keeps
    // its own key instead of sharing one with its neighbour. With 1 s windows
    // it is Math.round(t).
    function memberKey(t, step) {
        return Math.round(Math.round(t / step) * step * 1000) / 1000;
    }
    // memberGrid is everything a member chart draws, built once: the x grid
    // every dataset shares (the tooltip, mode 'index', pairs the points of
    // the datasets by position), each shown member's and the array's point
    // per x, and the stack totals summed from those same member points, so
    // the totals and the Y scale cannot differ from the drawn stack top.
    // shown (optional) lists the members to draw, all when omitted; the grid
    // keeps the windows of every member, so hiding one does not move it.
    function memberGrid(d, metric, shown) {
        const step = memberStep(d), shift = fioShift(d), keys = new Set();
        const members = {}, array = {}, totals = {};
        const names = memberNames(d).filter(n => !shown || shown.indexOf(n) !== -1);
        memberNames(d).forEach(n => {
            const m = {};
            (d.ts_members[n][metric.key] || []).forEach(p => {
                const x = memberKey(p.t, step);
                m[x] = p;
                keys.add(x);
            });
            if (names.indexOf(n) !== -1) members[n] = m;
        });
        (d[metric.tsKey] || []).forEach(p => {
            const x = memberKey(p.t + shift, step);
            array[x] = p;
            keys.add(x);
        });
        // The ftrace lines share the grid (the tooltip pairs points by position).
        ftraceDatasets(d, metric).forEach(ds => ds.data.forEach(p => keys.add(p.x)));
        Object.values(members).forEach(m => Object.keys(m).forEach(x => {
            const s = totals[x] || (totals[x] = {r: 0, w: 0});
            s.r += m[x].r || 0;
            s.w += m[x].w || 0;
        }));
        return { xs: [...keys].sort((a, b) => a - b), names, members, array, totals };
    }
    function memberMaxT(d, metric) {
        let m = 0;
        memberNames(d).forEach(n => (d.ts_members[n][metric.key] || []).forEach(p => { m = Math.max(m, p.t); }));
        return m;
    }
    function memberLabel(d, n, dir) {
        const role = d.ts_members[n].role;
        return n + (role ? ' (' + role + ')' : '') + (dir === 'r' ? ' read' : ' write') + ' (kernel stat)';
    }
    function hasDir(pts, dir) {
        return Object.values(pts).some(p => (p[dir] || 0) > 0);
    }
    // stackedMemberDatasets: one filled band per shown member (g.names) and
    // direction, the first of each direction from the origin. Read
    // and write are two stacks: Chart.js adds a 0 to every earlier value of
    // its stack whatever their sign, so an idle write would sit on the reads.
    // Zeros are kept: a missing point would let the bands above it drop.
    function stackedMemberDatasets(d, g, colors) {
        const out = [], names = g.names;
        ['r', 'w'].forEach(dir => {
            if (!names.some(n => hasDir(g.members[n], dir))) return;
            let first = true;
            names.forEach(n => {
                const c = colors[n], pts = g.members[n];
                out.push({
                    label: memberLabel(d, n, dir), member: n, skey: 'member:' + n, dir,
                    data: g.xs.map(x => ({x, y: pts[x] ? (dir === 'r' ? 1 : -1) * (pts[x][dir] || 0) : null})),
                    stack: dir === 'r' ? 'members' : 'members-write',
                    fill: first ? 'origin' : '-1',
                    borderColor: rgba(c, 0.9), backgroundColor: rgba(c, dir === 'r' ? 0.35 : 0.22),
                    tension: 0.2, pointRadius: 0, pointHoverRadius: 3, borderWidth: 1, spanGaps: true,
                });
                first = false;
            });
        });
        return out;
    }
    // memberLatencyDatasets: one line per shown member and direction. A
    // window without I/O has no latency (0): no point.
    // keepZero (IOPS and bandwidth in Normal): an idle window is a real 0.
    function memberLatencyDatasets(d, g, colors, keepZero) {
        const out = [], names = g.names;
        ['r', 'w'].forEach(dir => names.forEach(n => {
            const pts = g.members[n];
            if (!hasDir(pts, dir)) return;
            const c = colors[n];
            out.push({
                label: memberLabel(d, n, dir), member: n, skey: 'member:' + n, dir,
                data: g.xs.map(x => ({x, y: pts[x] && (keepZero || pts[x][dir] > 0) ? (dir === 'r' ? 1 : -1) * (pts[x][dir] || 0) : null})),
                fill: false, borderColor: rgba(c, 0.9), backgroundColor: rgba(c, 0.9),
                borderDash: dir === 'w' ? [4, 2] : [],
                tension: 0.2, pointRadius: 0, pointHoverRadius: 3, borderWidth: 1.2, spanGaps: true,
            });
        }));
        return out;
    }
    // arrayDatasets: the array's own fio series, unfilled lines drawn over
    // the members (order -1). Under the stacked scale they get their own
    // stack, so their values are not added to the members'.
    function arrayDatasets(g, stacked) {
        const pts = g.array, out = [];
        [['r', 'Read', 'rgba(37,99,235,0.95)'], ['w', 'Write', 'rgba(220,38,38,0.95)']].forEach(([dir, name, color]) => {
            if (!hasDir(pts, dir)) return;
            const ds = {
                label: 'Array ' + name.toLowerCase() + ' (fio)', array: true, skey: 'fio.' + dir, dir, order: -1,
                data: g.xs.map(x => ({x, y: pts[x] && pts[x][dir] > 0 ? (dir === 'r' ? 1 : -1) * pts[x][dir] : null})),
                fill: false, borderColor: color, backgroundColor: color,
                tension: 0.2, pointRadius: 0, pointHoverRadius: 3, borderWidth: 2, spanGaps: true,
            };
            if (stacked) ds.stack = 'array';
            out.push(ds);
        });
        return out;
    }
    // ftraceOnGrid: the ftrace lines on the member chart's x grid (null where
    // a line has no window).
    function ftraceOnGrid(d, metric, xs) {
        return ftraceDatasets(d, metric).map(ds => {
            const at = {};
            ds.data.forEach(p => { at[p.x] = p.y; });
            return Object.assign({}, ds, {data: xs.map(x => ({x, y: at[x] === undefined ? null : at[x]}))});
        });
    }
    function makeMembersChart(canvas, d, metric, yLow, yHigh, warnEl, mode) {
        tsWarn(warnEl, d[metric.tsKey] || []);
        const cd = chartDatasets(d, metric, mode), stacked = cd.stacked, g = cd.grid, xs = g.xs;
        const sc = phaseScale(metric, yLow, yHigh);
        const datasets = toBase(cd.datasets, sc);
        // The shown members come first, then the array and the ftrace lines.
        const nMembers = datasets.filter(ds => ds.member).length;
        const totals = stacked ? g.totals : {};
        const dirs = ['r', 'w'].filter(dir => datasets.some(m => m.member && m.dir === dir));
        // v in the base unit; the stack totals are in the metric's units.
        const fmt = v => fmtScaled(v, sc, true) + ' ' + sc.unit;
        const totalLines = i => {
            const s = totals[xs[i]];
            if (!s) return [];
            return dirs.map(dir => 'Stack total ' + (dir === 'r' ? 'read' : 'write') + ': ' + fmt(s[dir] * sc.mul));
        };
        // The first array item shown at index i, which carries the stack totals.
        const firstArray = (chart, i) => {
            for (let k = nMembers; k < datasets.length; k++) {
                if (chart.isDatasetVisible(k) && datasets[k].data[i].y !== null) return k;
            }
            return -1;
        };
        return new Chart(canvas, {
            type: 'line', data: { datasets }, plugins: chartPlugins(d, metric),
            options: {
                responsive: true, maintainAspectRatio: false, animation: false,
                interaction: { mode: 'index', intersect: false },
                plugins: {
                    rampZone: { sec: rampOf(d) },
                    mdTrack: mdOptions(d, metric),
                    legend: { display: false },
                    tooltip: {
                        mode: 'index', intersect: false,
                        // Members first, then the stack total, then the array.
                        itemSort: (a, b) => (a.dataset.array ? 1 : 0) - (b.dataset.array ? 1 : 0) || a.datasetIndex - b.datasetIndex,
                        callbacks: {
                            title: windowTitle(d),
                            beforeLabel: ctx => stacked && ctx.dataset.array && firstArray(ctx.chart, ctx.dataIndex) === ctx.datasetIndex
                                ? totalLines(ctx.dataIndex) : '',
                            label: ctx => ctx.dataset.label + ': ' + fmt(ctx.raw.y),
                            afterBody: items => (stacked && items.length && firstArray(items[0].chart, items[0].dataIndex) === -1
                                ? totalLines(items[0].dataIndex) : []).concat(mdTipLines(d, items)),
                        },
                    },
                },
                scales: lineScales(d, sc, yLow, yHigh, { stacked: stacked })
            }
        });
    }

    // ── members (end) ──

    // ── md track (begin) ──
    // An md array phase carries ts_md (per window: action, pct, speed KiB/s,
    // degraded, state; t = window start on the phase clock, like ts_members)
    // and md_events ({t, text}: every change of the md state). The IOPS,
    // bandwidth and latency charts of such a phase shade the windows of a sync
    // (any action but idle), mark the events, add the window's md state to the
    // tooltip, and the card header gets badges and the "array busy" note. A
    // phase without md data takes none of this. Everything up to "md track
    // (end)" belongs to it.
    const MD_BAND = 'rgba(245,158,11,0.16)', MD_STRIP = 'rgba(217,119,6,0.9)', MD_INK = '#b45309', MD_EVENT = '#be123c';
    // mdPlugin draws a light full-height tint of each sync band under the
    // datasets; over them, so stacked fills cannot hide it, an opaque strip in
    // the top 6 px of the chart area with the action on a white box, then the
    // event markers (options: {bands, markers}, from mdOptions).
    const mdPlugin = {
        id: 'mdTrack',
        beforeDatasetsDraw(chart, args, opts) {
            if (!opts || !opts.bands || !opts.bands.length) return;
            const x = chart.scales.x, a = chart.chartArea, c = chart.ctx;
            c.save();
            c.fillStyle = MD_BAND;
            opts.bands.forEach(b => {
                const x0 = Math.max(a.left, x.getPixelForValue(b.x0));
                const x1 = Math.min(a.right, x.getPixelForValue(b.x1));
                if (x1 > x0) c.fillRect(x0, a.top, x1 - x0, a.bottom - a.top);
            });
            c.restore();
        },
        afterDatasetsDraw(chart, args, opts) {
            if (!opts) return;
            const x = chart.scales.x, a = chart.chartArea, c = chart.ctx;
            c.save();
            c.font = '9px sans-serif';
            c.textBaseline = 'top';
            const bands = opts.bands || [];
            bands.forEach((b, i) => {
                const x0 = Math.max(a.left, x.getPixelForValue(b.x0));
                const x1 = Math.min(a.right, x.getPixelForValue(b.x1));
                if (x1 <= x0) return;
                c.fillStyle = MD_STRIP;
                c.fillRect(x0, a.top, x1 - x0, 6);
                // The label may run past a narrow band, up to the next one.
                const next = bands[i + 1] ? Math.min(a.right, x.getPixelForValue(bands[i + 1].x0)) : a.right;
                const s = mdFit(c, b.action, next - x0 - 6);
                if (!s) return;
                c.fillStyle = 'rgba(255,255,255,0.85)';
                c.fillRect(x0 + 1, a.top + 7, c.measureText(s).width + 4, 11);
                c.fillStyle = MD_INK;
                c.fillText(s, x0 + 3, a.top + 8);
            });
            const vis = (opts.markers || []).map(m => ({m, x: x.getPixelForValue(m.t)}))
                .filter(v => v.x >= a.left && v.x <= a.right);
            const lay = mdLabelLayout(vis.map(v => ({x: v.x, w: c.measureText(v.m.text).width})), a.right);
            vis.forEach((v, i) => {
                c.strokeStyle = MD_EVENT;
                c.lineWidth = 1;
                c.setLineDash([3, 2]);
                c.beginPath();
                c.moveTo(v.x, a.top);
                c.lineTo(v.x, a.bottom);
                c.stroke();
                c.setLineDash([]);
                const s = mdFit(c, v.m.text, lay[i].maxW - 2);
                if (!s) return;
                const y = a.top + 21 + lay[i].row * 11;
                c.fillStyle = 'rgba(255,255,255,0.8)';
                c.fillRect(v.x + 1, y - 1, c.measureText(s).width + 3, 11);
                c.fillStyle = MD_EVENT;
                c.fillText(s, v.x + 2, y);
            });
            c.restore();
        }
    };
    function mdMetric(d, metric) {
        return ['iops', 'bw', 'lat'].indexOf(metric.key) !== -1 && hasMD(d);
    }
    function hasMD(d) {
        return !!(d.ts_md && d.ts_md.length) || !!(d.md_events && d.md_events.length);
    }
    // mdBusy: the window runs an administrative operation (sync_action not
    // idle; '' = unreadable).
    function mdBusy(p) {
        return !!p.action && p.action !== 'idle';
    }
    // mdActions is the action of each window, an unreadable one ('') taking
    // the last known (idle at the start), as the events do.
    function mdActions(d) {
        let last = 'idle';
        return (d.ts_md || []).map(p => (last = p.action || last));
    }
    // mdBands: consecutive windows of one non-idle action, as one band from
    // the first window's start to the last window's end.
    function mdBands(d) {
        const pts = d.ts_md || [], acts = mdActions(d), step = memberStep(d), out = [];
        let cur = null;
        pts.forEach((p, i) => {
            const a = acts[i];
            if (a === 'idle') { cur = null; return; }
            if (cur && cur.action === a) { cur.x1 = p.t + step; return; }
            cur = {action: a, x0: p.t, x1: p.t + step};
            out.push(cur);
        });
        return out;
    }
    // mdBadges: per activity of an action, its progress at its first and
    // last window with a known progress (pct 0 = total unknown, skipped; no
    // known progress: the action alone); a drop in progress is a restart and
    // starts a new badge. Then the highest degraded count ('degraded' for 1).
    function mdBadges(d) {
        const segs = [], cur = {};
        let deg = 0;
        (d.ts_md || []).forEach(p => {
            if (mdBusy(p)) {
                const pct = p.pct || 0;
                let g = cur[p.action];
                if (!g || (pct > 0 && g.last !== null && pct < g.last)) {
                    g = cur[p.action] = {action: p.action, first: null, last: null};
                    segs.push(g);
                }
                if (pct > 0) {
                    if (g.first === null) g.first = pct;
                    g.last = pct;
                }
            }
            deg = Math.max(deg, p.degraded > 0 ? p.degraded : 0); // < 0 = unreadable
        });
        const out = segs.map(g => {
            if (g.first === null) return {text: g.action, kind: 'sync'};
            const f = Math.round(g.first), l = Math.round(g.last);
            return {text: g.action + ' ' + f + '%' + (f === l ? '' : ' -> ' + l + '%'), kind: 'sync'};
        });
        if (deg > 0) out.push({text: deg === 1 ? 'degraded' : 'degraded ' + deg, kind: 'degraded'});
        return out;
    }
    // mdBusyNote: the phase ran a sync or its md state changed.
    function mdBusyNote(d) {
        return (d.ts_md || []).some(mdBusy) || !!(d.md_events && d.md_events.length);
    }
    // mdIndex keys the md windows and the event texts like the member grid
    // (memberKey): {step, windows, events}. Built once per entry and kept on
    // it as a hidden (non-enumerable) property, so a hover is a lookup.
    function mdIndex(d) {
        if (d._mdIndex) return d._mdIndex;
        const step = memberStep(d), windows = {}, events = {};
        (d.ts_md || []).forEach(p => { windows[memberKey(p.t, step)] = p; });
        (d.md_events || []).forEach(e => {
            const k = memberKey(e.t, step);
            (events[k] || (events[k] = [])).push(e.text);
        });
        const idx = {step, windows, events};
        Object.defineProperty(d, '_mdIndex', {value: idx});
        return idx;
    }
    // mdTipLines: the hovered window's md state and its events, for a
    // tooltip's afterBody; [] without md data or without an md window there.
    function mdTipLines(d, items) {
        if (!hasMD(d) || !items || !items.length) return [];
        const idx = mdIndex(d), k = memberKey(items[0].raw.x, idx.step), out = [];
        const p = idx.windows[k];
        if (p) {
            out.push('md: ' + (mdBusy(p)
                ? p.action + ' ' + Math.round(p.pct || 0) + '% ' + ((p.speed || 0) / 1024).toFixed(1) + ' MiB/s'
                : (p.action || 'unknown')) + ', degraded ' + (p.degraded < 0 ? 'unknown' : (p.degraded || 0)));
        }
        (idx.events[k] || []).forEach(text => out.push('md event: ' + text));
        return out;
    }
    // mdMarkers: one marker per event time, its texts joined, by time.
    function mdMarkers(d) {
        const out = [];
        (d.md_events || []).slice().sort((a, b) => a.t - b.t).forEach(e => {
            const m = out[out.length - 1];
            if (m && m.t === e.t) m.text += '; ' + e.text;
            else out.push({t: e.t, text: e.text});
        });
        return out;
    }
    // mdLabelLayout staggers the marker labels (items by x, in pixels) over
    // three rows; each may use the room up to the next label of its row (or
    // right). The full text is in the tooltip.
    function mdLabelLayout(items, right) {
        const rows = 3;
        return items.map((it, i) => {
            const next = items[i + rows];
            return {row: i % rows, maxW: (next ? next.x : right) - it.x - 4};
        });
    }
    // mdFit cuts s to w pixels with an ellipsis; '' when not even that fits.
    function mdFit(c, s, w) {
        if (c.measureText(s).width <= w) return s;
        while (s.length && c.measureText(s + '…').width > w) s = s.slice(0, -1);
        return s ? s + '…' : '';
    }
    function mdOptions(d, metric) {
        return mdMetric(d, metric) ? {bands: mdBands(d), markers: mdMarkers(d)} : false;
    }
    function chartPlugins(d, metric) {
        return mdMetric(d, metric) ? [rampPlugin, mdPlugin] : [rampPlugin];
    }
    // mdHeader: the card header's badges and the busy note of an md phase;
    // null without md data.
    function mdHeader(d, metric) {
        if (!mdMetric(d, metric)) return null;
        const badges = mdBadges(d).map(b => {
            const s = document.createElement('span');
            s.className = 'md-badge md-' + b.kind;
            s.textContent = b.text;
            s.title = b.kind === 'sync'
                ? 'md sync_action during this phase, progress at its first and last window'
                : 'md degraded count during this phase';
            return s;
        });
        let note = null;
        if (mdBusyNote(d)) {
            note = document.createElement('div');
            note.className = 'md-note';
            note.textContent = 'array busy: results include administrative work';
        }
        return {badges, note};
    }
    // ── md track (end) ──

    // makeChart renders a Chart.js instance onto canvas for (d, metric).
    // Returns the Chart instance. warnEl (optional) will receive a ts-warn message.
    function makeChart(canvas, d, metric, yLow, yHigh, warnEl, mode) {
        const ts = d[metric.tsKey];
        if (metric.key === 'flush') return makeFlushChart(canvas, d, metric, yHigh);
        if (iolatKind(metric)) return makeIOLatChart(canvas, d, metric, yLow, yHigh);
        if (memberMetric(d, metric)) return makeMembersChart(canvas, d, metric, yLow, yHigh, warnEl, mode);
        // The card's unit: chosen from its range (the Y scale criterion).
        const sc = phaseScale(metric, yLow, yHigh);
        const cd = chartDatasets(d, metric, mode);
        const datasets = toBase(cd.datasets, sc);
        if (!cd.bars) {
            tsWarn(warnEl, ts || []);
            return new Chart(canvas, {
                type: 'line', data: { datasets }, plugins: chartPlugins(d, metric),
                options: {
                    responsive: true, maintainAspectRatio: false, animation: false,
                    plugins: {
                        rampZone: { sec: rampOf(d) },
                        mdTrack: mdOptions(d, metric),
                        legend: { display: false },
                        tooltip: { callbacks: {
                            title: windowTitle(d),
                            label: ctx => ctx.dataset.label + ': ' +
                                          fmtScaled(ctx.raw.y, sc, true) + ' ' + sc.unit,
                            afterBody: items => mdTipLines(d, items)
                        }}
                    },
                    scales: lineScales(d, sc, yLow, yHigh)
                }
            });
        }
        return barChart(canvas, datasets, sc, barAxis(sc, yLow, yHigh));
    }

    // makeIOLatChart draws the Device and the Block-layer latency cards: the
    // ftrace lines of the card's kind alone (bars of the summary means when
    // the phase has no series).
    function makeIOLatChart(canvas, d, metric, yLow, yHigh) {
        const sc = phaseScale(metric, yLow, yHigh);
        // These cards have no member stack: always the normal datasets.
        const cd = chartDatasets(d, metric, 'normal');
        const datasets = toBase(cd.datasets, sc);
        // The bars keep the line chart's y axis (tick colour, zero line).
        const scales = lineScales(d, sc, yLow, yHigh);
        if (!cd.bars) {
            return new Chart(canvas, {
                type: 'line', data: { datasets }, plugins: [rampPlugin],
                options: {
                    responsive: true, maintainAspectRatio: false, animation: false,
                    plugins: {
                        rampZone: { sec: rampOf(d) },
                        legend: { display: false },
                        tooltip: { callbacks: {
                            title: windowTitle(d),
                            label: ctx => ctx.dataset.label + ': ' + fmtScaled(ctx.raw.y, sc, true) + ' ' + sc.unit,
                        }}
                    },
                    scales: scales
                }
            });
        }
        return barChart(canvas, datasets, sc, scales.y);
    }

    // ── Series visibility ──
    // Each series of a card has a key (skey): fio.r and fio.w (a drive's
    // read and write, an array's own fio lines, the bars), member:<name>
    // (both directions of a member, kernel stat), ftrace.<kind>.<stat>.<dir>
    // (ftrace.device.mean.r; ftrace.bio.mean.r for a bio-based node's bios),
    // ftrace.member:<name> (a member's device latency
    // on the Device card, its block-layer latency on the Block-layer card)
    // and flush. HIDDEN maps a metric key to the Set of the keys
    // hidden on every card of that metric: page memory only (not stored), so
    // it survives a re-render and is gone on reload.
    const HIDDEN = {};
    function isShown(metricKey, skey) {
        return !(HIDDEN[metricKey] && HIDDEN[metricKey].has(skey));
    }
    function setShown(metricKey, skey, on) {
        const s = HIDDEN[metricKey] || (HIDDEN[metricKey] = new Set());
        if (on) s.delete(skey); else s.add(skey);
    }
    // chartDatasets is the one place the datasets of a card are built, in
    // the metric's units (a chart converts them with toBase): {datasets,
    // stacked, bars, grid}. Only shown series are built (all of them with
    // all), so the member fill chain ('origin' then '-1') and the stack
    // totals of grid (memberGrid, member charts only) run over the shown
    // members. Whether a card draws lines or bars depends on the data, not
    // on what is shown: hiding every line does not turn it into bars. A bar
    // dataset holds its one value in data[0] (writes negative).
    function chartDatasets(d, metric, mode, all) {
        const shown = ds => all || isShown(metric.key, ds.skey);
        const ts = d[metric.tsKey];
        const line = {tension: 0.2, pointRadius: 0, pointHoverRadius: 3, borderWidth: 1.5};
        const bar = (skey, label, dir, v, color) => ({skey, label, dir, data: [v], borderColor: color, backgroundColor: color});
        // barsFor: the summary means as bars (no series): read up, write
        // down, a direction without I/O left out; skey and label by direction.
        const barsFor = (skey, label) => {
            const r = metric.sumR(d), w = metric.sumW(d), out = [];
            if (r > 0) out.push(bar(skey('r'), label('Read'), 'r', r, 'rgba(37,99,235,0.75)'));
            if (w > 0) out.push(bar(skey('w'), label('Write'), 'w', -w, 'rgba(220,38,38,0.75)'));
            return {stacked: false, bars: true, datasets: out.filter(shown)};
        };
        if (metric.key === 'flush') {
            if (ts && ts.length > 0) return {stacked: false, bars: false, datasets: [Object.assign({
                label: 'Flush (ftrace)', skey: 'flush',
                data: ts.filter(p => p.w > 0).map(p => ({x: Math.round(p.t), y: p.w})),
                borderColor: 'rgba(124,58,237,0.9)', backgroundColor: 'rgba(124,58,237,0.07)', fill: 'origin',
            }, line)].filter(shown)};
            return {stacked: false, bars: true,
                datasets: [bar('flush', 'Flush (ftrace)', 'w', metric.sumW(d), 'rgba(124,58,237,0.75)')].filter(shown)};
        }
        if (memberMetric(d, metric)) {
            // Normal draws every member and the array as plain lines; only
            // Stacked stacks (latency is never stacked, see memberLatencyDatasets).
            const stacked = mode !== 'normal' && metric.key !== 'lat', colors = memberColors(memberNames(d));
            const g = memberGrid(d, metric, memberNames(d).filter(n => all || isShown(metric.key, 'member:' + n)));
            const members = stacked ? stackedMemberDatasets(d, g, colors) : memberLatencyDatasets(d, g, colors, metric.key !== 'lat');
            return {stacked, bars: false, grid: g,
                datasets: members.concat(arrayDatasets(g, stacked).filter(shown), ftraceOnGrid(d, metric, g.xs).filter(shown))};
        }
        const ftrace = ftraceDatasets(d, metric);
        const kind = iolatKind(metric);
        if (kind) {
            if (ftrace.length > 0) return {stacked: false, bars: false, datasets: ftrace.filter(shown)};
            const kname = kind === 'device' ? 'device' : 'block layer';
            return barsFor(dir => 'ftrace.' + kind + '.mean.' + dir, name => name + ' ' + kname + ' (ftrace)');
        }
        if ((ts && ts.length > 0) || ftrace.length > 0) {
            const shift = fioShift(d), out = [];
            const readPts  = (ts || []).filter(p => p.r > 0).map(p => ({x: Math.round(p.t + shift), y:  p.r}));
            const writePts = (ts || []).filter(p => p.w > 0).map(p => ({x: Math.round(p.t + shift), y: -p.w}));
            if (readPts.length > 0) out.push(Object.assign({
                label: 'Read (fio)',  data: readPts, skey: 'fio.r', dir: 'r',
                borderColor: 'rgba(37,99,235,0.9)',  backgroundColor: 'rgba(37,99,235,0.07)', fill: 'origin',
            }, line));
            if (writePts.length > 0) out.push(Object.assign({
                label: 'Write (fio)', data: writePts, skey: 'fio.w', dir: 'w',
                borderColor: 'rgba(220,38,38,0.9)', backgroundColor: 'rgba(220,38,38,0.07)', fill: 'origin',
            }, line));
            return {stacked: false, bars: false, datasets: out.concat(ftrace).filter(shown)};
        }
        return barsFor(dir => 'fio.' + dir, name => name + ' (fio)');
    }
    // cardSeries lists every series a card could draw, shown or not, once
    // per key: {skey, label, color, dash}. A member's two directions are one
    // series, its label without the direction.
    function cardSeries(d, metric, mode) {
        const out = [], seen = {};
        chartDatasets(d, metric, mode, true).datasets.forEach(ds => {
            if (!ds.skey || seen[ds.skey]) return;
            seen[ds.skey] = true;
            const both = ds.skey.indexOf('member:') !== -1;
            out.push({skey: ds.skey, label: both ? ds.label.replace(/ (read|write) /, ' ') : ds.label,
                color: ds.borderColor || ds.backgroundColor,
                dash: ds.borderDash && ds.borderDash.length ? ds.borderDash : null});
        });
        return out;
    }
    // phaseRange is the data range {yMax, yMin} (metric units, no headroom,
    // 0 when nothing is above or below zero) of the cards of phaseData over
    // the series they show: each value, or for a stacked chart the sum of
    // each stack per window (read and write apart, as Chart.js stacks them).
    function phaseRange(phaseData, metric, mode) {
        let yMax = 0, yMin = 0;
        const see = v => { if (v > yMax) yMax = v; if (v < yMin) yMin = v; };
        const val = p => typeof p === 'number' ? p : (p && p.y !== null && p.y !== undefined ? p.y : 0);
        phaseData.forEach(d => {
            const cd = chartDatasets(d, metric, mode);
            if (!cd.stacked) {
                cd.datasets.forEach(ds => ds.data.forEach(p => see(val(p))));
                return;
            }
            const sums = {};
            cd.datasets.forEach((ds, k) => {
                const s = sums[ds.stack || '#' + k] || (sums[ds.stack || '#' + k] = []);
                ds.data.forEach((p, i) => {
                    const v = val(p), e = s[i] || (s[i] = {pos: 0, neg: 0});
                    if (v > 0) e.pos += v; else e.neg += v;
                });
            });
            Object.values(sums).forEach(s => s.forEach(e => { see(e.pos); see(e.neg); }));
        });
        return {yMax, yMin};
    }
    // rangeExtent turns a data range (phaseRange) into the y range of a card
    // {yLow, yHigh}, with AXIS_HEADROOM. Nothing shown, or only zeros: 0..1.
    function rangeExtent(r) {
        return {
            yHigh: r.yMax > 0 ? r.yMax * AXIS_HEADROOM : (r.yMin < 0 ? 0 : 1),
            yLow: r.yMin < 0 ? r.yMin * AXIS_HEADROOM : 0,
        };
    }
    // phaseExtent is the y range of a phase's cards (phaseRange, rangeExtent).
    function phaseExtent(phaseData, metric, mode) {
        return rangeExtent(phaseRange(phaseData, metric, mode));
    }

    // ── Y scale criterion ──
    // SCALE_CRITERIA are the choices of the sidebar's Y scale selector, the
    // y range and SI prefix a card is drawn on: 'phase' (default) one per
    // phase, shared by its cards; 'global' one per metric over all its
    // phases on the page, each phase contributing in its own graph mode, so
    // every card of the metric compares directly; 'graph' each card its own.
    // Hidden series never count. The time axis (PHASE_X) is not part of it.
    // scaleCriterion is page memory only, like HIDDEN and graphModeOf.
    const SCALE_CRITERIA = [['global', 'Global'], ['phase', 'Per phase'], ['graph', 'Per graph']];
    let scaleCriterion = 'phase';
    // scaleScope gives the y range of every card under criterion: phases is
    // [{phaseData, mode}] (the phases of one metric), the result one array
    // per phase with one {yLow, yHigh} per card of phaseData. An unknown
    // criterion is 'phase'. Global takes the union of the phases' data
    // ranges, so a phase with nothing to draw does not widen it to 0..1.
    function scaleScope(criterion, phases, metric) {
        if (criterion === 'graph') return phases.map(p => p.phaseData.map(d => phaseExtent([d], metric, p.mode)));
        if (criterion === 'global') {
            const all = {yMax: 0, yMin: 0};
            phases.forEach(p => {
                const r = phaseRange(p.phaseData, metric, p.mode);
                all.yMax = Math.max(all.yMax, r.yMax);
                all.yMin = Math.min(all.yMin, r.yMin);
            });
            const e = rangeExtent(all);
            return phases.map(p => p.phaseData.map(() => e));
        }
        return phases.map(p => { const e = phaseExtent(p.phaseData, metric, p.mode); return p.phaseData.map(() => e); });
    }
    // SCALE_PHASES maps a metric key to its phases on the page, [{pname,
    // phaseData}] in the order of REFILL[metric.key] (renderAll resets it).
    let SCALE_PHASES = {};
    // metricScales is scaleScope of the metric's phases on the page, each in
    // its current graph mode, under the active criterion.
    function metricScales(metric) {
        return scaleScope(scaleCriterion, (SCALE_PHASES[metric.key] || []).map(p =>
            ({phaseData: p.phaseData, mode: graphMode(metric, p.pname)})), metric);
    }
    // setScaleCriterion selects a criterion (an unknown one is 'phase') and
    // redraws every metric on the page (applyShown: cards and open modal);
    // series toggles, graph modes and folded sections are kept.
    function setScaleCriterion(v) {
        scaleCriterion = SCALE_CRITERIA.some(c => c[0] === v) ? v : 'phase';
        METRICS.filter(m => REFILL[m.key]).forEach(m => applyShown(m));
    }
    // REFILL maps a metric key to the fill(ext) of each of its phases, ext
    // being that phase's card ranges (metricScales; renderAll resets it): a
    // toggle redraws every phase of that metric only, so under Global the
    // untouched phases follow the new range.
    let REFILL = {};
    // ROW_BOXES maps '<where>|<skey>' to the checkbox of the series row now
    // shown there (where = a card's chart id or 'modal:<metric>'), so a
    // toggle gives the focus back to the rebuilt checkbox; reset in renderAll.
    let ROW_BOXES = {};
    // seriesRow is the row of checkboxes above a card's chart (and in the
    // modal): one per series with its colour; a change applies to every
    // card of the metric. Its clicks do not reach the card, which opens the
    // modal. where names the row's place (the card's chart id, or
    // 'modal:<metric>') so the focus follows a toggle across the redraw.
    function seriesRow(d, metric, mode, where) {
        const row = document.createElement('div');
        row.className = 'series-row';
        row.addEventListener('click', e => e.stopPropagation());
        cardSeries(d, metric, mode).forEach(s => {
            const lbl = document.createElement('label');
            const on = isShown(metric.key, s.skey);
            lbl.className = on ? '' : 'off';
            lbl.title = s.label;
            const cb = document.createElement('input');
            cb.type = 'checkbox';
            cb.checked = on;
            cb.dataset.skey = s.skey;
            const at = where + '|' + s.skey;
            ROW_BOXES[at] = cb;
            cb.addEventListener('change', () => {
                setShown(metric.key, s.skey, cb.checked);
                applyShown(metric);
                const nb = ROW_BOXES[at];
                if (nb && nb !== cb && nb.focus) nb.focus();
            });
            const sw = document.createElement('span');
            sw.className = 'swatch' + (s.dash ? ' dash' : '');
            if (s.dash) sw.style.borderTopColor = s.color; else sw.style.background = s.color;
            lbl.appendChild(cb);
            lbl.appendChild(sw);
            lbl.appendChild(document.createTextNode(s.label));
            row.appendChild(lbl);
        });
        return row;
    }
    // refillMetric redraws every phase of the metric on its card ranges
    // under the active criterion (metricScales) and returns those ranges.
    function refillMetric(metric) {
        const ext = metricScales(metric);
        (REFILL[metric.key] || []).forEach((f, i) => f(ext[i]));
        return ext;
    }
    // applyShown redraws every phase of the metric, and the modal when it
    // shows that metric, on the scale of the series now shown, the graph
    // modes and the criterion.
    function applyShown(metric) {
        const ext = refillMetric(metric);
        if (modalCtx && modalCtx.metric.key === metric.key) {
            const c = modalCtx, e = modalExtent(c, ext);
            openModal(c.d, c.metric, e.yLow, e.yHigh, c.mode, c.pname, c.phaseData);
        }
    }
    // modalExtent is the y range of the open modal's card c: its entry in
    // ext (metricScales), or its phase's own range when the card is not on
    // the page.
    function modalExtent(c, ext) {
        const phases = SCALE_PHASES[c.metric.key] || [];
        const pi = phases.findIndex(p => p.pname === c.pname);
        const j = pi < 0 ? -1 : phases[pi].phaseData.indexOf(c.d);
        if (j >= 0 && ext && ext[pi] && ext[pi][j]) return ext[pi][j];
        return phaseExtent(c.phaseData || [c.d], c.metric, c.mode);
    }

    // ── Modal ──
    let modalChart = null;
    // modalCtx is what the open modal shows ({d, metric, pname, mode,
    // phaseData}), so a series toggle can draw it again; null when closed.
    let modalCtx = null;

    function openModal(d, metric, yLow, yHigh, mode, pname, phaseData) {
        modalCtx = {d, metric, pname, mode, phaseData};
        const mt = membersText(d);
        document.getElementById('modal-title').textContent = d.disk + settingTag(d, RPV) + (mt ? ' \u00b7 ' + mt : '');
        const tech = d.technology ? d.technology.toUpperCase() : '';
        document.getElementById('modal-subtitle').textContent =
            [d.vendor, d.model, d.serial, tech, metric.label, 'source: ' + metric.source].filter(Boolean).join('  \u00b7  ');
        const mdEl = document.getElementById('modal-md');
        mdEl.innerHTML = '';
        const mdh = mdHeader(d, metric);
        if (mdh) {
            mdh.badges.forEach(b => { mdEl.appendChild(b); mdEl.appendChild(document.createTextNode(' ')); });
            if (mdh.note) mdEl.appendChild(mdh.note);
        }

        if (modalChart) { modalChart.destroy(); modalChart = null; }

        // Replace canvas to avoid Chart.js "canvas already in use" error.
        const wrap = document.querySelector('.modal-chart-wrap');
        const oldCanvas = document.getElementById('modal-canvas');
        const newCanvas = document.createElement('canvas');
        newCanvas.id = 'modal-canvas';
        wrap.replaceChild(newCanvas, oldCanvas);

        const warnEl = document.getElementById('modal-ts-warn');
        warnEl.className = 'modal-ts-warn';
        warnEl.textContent = '';
        modalChart = makeChart(newCanvas, d, metric, yLow, yHigh, warnEl, mode);
        addLossNote(warnEl, d, metric);
        const seriesEl = document.getElementById('modal-series');
        seriesEl.innerHTML = '';
        seriesEl.appendChild(seriesRow(d, metric, mode, 'modal:' + metric.key));
        document.getElementById('modal-src').textContent = [sourceLine(d, metric), iolatNodeNote(d, metric)].filter(Boolean).join(' \u00b7 ');
        document.getElementById('modal-overlay').classList.add('active');
    }

    function closeModal() {
        modalCtx = null;
        document.getElementById('modal-overlay').classList.remove('active');
        if (modalChart) { modalChart.destroy(); modalChart = null; }
    }

    document.getElementById('modal-overlay').addEventListener('click', e => {
        if (e.target === e.currentTarget) closeModal();
    });
    document.addEventListener('keydown', e => {
        if (e.key === 'Escape') closeModal();
    });

    function makeChartCard(d, metric, cid, yLow, yHigh, mode, pname, phaseData) {
        const card = document.createElement('div');
        card.className = 'chart-card';

        const t1 = document.createElement('div');
        t1.className = 'card-title';
        t1.textContent = d.disk + settingTag(d, RPV);
        t1.appendChild(document.createTextNode(' '));
        t1.appendChild(srcBadge(metric, [d]));
        const mdh = mdHeader(d, metric);
        if (mdh) mdh.badges.forEach(b => { t1.appendChild(document.createTextNode(' ')); t1.appendChild(b); });
        card.appendChild(t1);

        const t2 = document.createElement('div');
        t2.className = 'card-subtitle';
        const tech = d.technology ? d.technology.toUpperCase() : '';
        t2.textContent = [d.vendor, d.model, d.serial, tech, membersText(d)].filter(Boolean).join('  \u00b7  ');
        card.appendChild(t2);
        const src = sourceLine(d, metric);
        if (src) {
            const t3 = document.createElement('div');
            t3.className = 'src-line';
            t3.textContent = src;
            card.appendChild(t3);
        }
        if (mdh && mdh.note) card.appendChild(mdh.note);
        const why = d.skipped ? 'Skipped: ' + d.skipped : flushNone(d, metric) || iolatNone(d, metric);
        const nodeNote = why ? '' : iolatNodeNote(d, metric);
        if (nodeNote) {
            const t4 = document.createElement('div');
            t4.className = 'src-line';
            t4.textContent = nodeNote;
            card.appendChild(t4);
        }
        if (!why) card.appendChild(seriesRow(d, metric, mode, cid));

        const wrap = document.createElement('div');
        wrap.className = 'chart-wrap';
        const canvas = document.createElement('canvas');
        wrap.appendChild(canvas);
        card.appendChild(wrap);

        const warnEl = document.createElement('div');
        card.appendChild(warnEl);
        if (why) {
            // Traced, but no Flush command reached the device: say why
            // instead of drawing an empty chart.
            wrap.removeChild(canvas);
            wrap.className = 'chart-wrap none-note' + (d.skipped ? ' skipped' : '');
            wrap.textContent = why;
            return card;
        }
        charts[cid] = makeChart(canvas, d, metric, yLow, yHigh, warnEl, mode);
        addLossNote(warnEl, d, metric);

        card.addEventListener('click', () => openModal(d, metric, yLow, yHigh, mode, pname, phaseData));
        return card;
    }

    function renderAll() {
        destroyAllCharts();
        REFILL = {};
        SCALE_PHASES = {};
        ROW_BOXES = {};
        const main = document.getElementById('main');
        main.innerHTML = '';

        const data = filteredData();
        const selMetrics = METRICS.filter(m => SEL.graph_type.has(m.key));

        if (selMetrics.length === 0) {
            main.innerHTML = '<div class="placeholder">Select at least one graph type.</div>';
            return;
        }
        if (data.length === 0) {
            main.innerHTML = '<div class="placeholder">No data matches the current filters.</div>';
            return;
        }

        // One time axis per phase, over every chart of the phase.
        PHASE_X = phaseXMax(data, selMetrics);

        selMetrics.forEach(metric => {
            // ── Metric-level foldable section ──
            const section = document.createElement('div');
            section.className = 'metric-section';

            const secHdr = document.createElement('div');
            secHdr.className = 'sec-hdr';
            secHdr.innerHTML = '<span class="chev">▼</span>' + metric.label + ' ';
            secHdr.appendChild(srcBadge(metric, data));
            const mon = ftraceEvents(data, metric);
            if (mon.length) {
                const sub = document.createElement('div');
                sub.className = 'sec-trace';
                sub.textContent = 'tracepoints monitored: ' + mon.join('; ');
                secHdr.appendChild(sub);
            }
            section.appendChild(secHdr);

            const secBody = document.createElement('div');
            secBody.className = 'sec-body';
            section.appendChild(secBody);
            main.appendChild(section);

            secHdr.addEventListener('click', () => toggleSection(secHdr, secBody));

            // Group data by phase, preserving encounter order. Metrics with a
            // "has" test (Flush, ftrace latency) only show disks that have
            // that data (has may look at the other entries shown); "none"
            // gives the text of a section without a card.
            const phaseOrder = [];
            const byPhase = {};
            const mdata = metric.has ? data.filter(d => metric.has(d, data)) : data;
            if (mdata.length === 0) {
                const none = document.createElement('div');
                none.className = 'placeholder';
                none.textContent = metric.none ? metric.none(data) : 'No ' + metric.source + ' data for the selected disks and phases.';
                secBody.appendChild(none);
            }
            mdata.forEach(d => {
                const pname = d.phase_name || ('Phase ' + d.phase_num);
                if (!byPhase[pname]) { phaseOrder.push(pname); byPhase[pname] = []; }
                byPhase[pname].push(d);
            });

            // ── Phase-level foldable sub-section ──
            phaseOrder.forEach(pname => {
                const phaseData = byPhase[pname];
                const diskCount = phaseData.length;

                const phaseHdr = document.createElement('div');
                phaseHdr.className = 'phase-hdr';
                phaseHdr.innerHTML = '<span class="chev">▼</span>'
                    + esc(pname)
                    + ' <span class="phase-count">(' + diskCount + ' disk' + (diskCount !== 1 ? 's' : '') + ')</span>';
                secBody.appendChild(phaseHdr);

                const phaseBody = document.createElement('div');
                phaseBody.className = 'phase-body';
                secBody.appendChild(phaseBody);

                phaseHdr.addEventListener('click', () => toggleSection(phaseHdr, phaseBody));

                const grid = document.createElement('div');
                grid.className = 'chart-grid';
                phaseBody.appendChild(grid);

                // (Re)draws this phase's cards in its current graph mode, each
                // on its range in ext (metricScales: the Y scale criterion).
                const fill = ext => {
                    const mode = graphMode(metric, pname);
                    Object.keys(charts).forEach(id => {
                        if (id.indexOf(metric.key + ':' + pname + ':') === 0) { charts[id].destroy(); delete charts[id]; }
                    });
                    grid.innerHTML = '';
                    phaseData.forEach((d, i) => {
                        const cid = metric.key + ':' + pname + ':' + i;
                        const sc = (ext && ext[i]) || phaseExtent([d], metric, mode);
                        grid.appendChild(makeChartCard(d, metric, cid, sc.yLow, sc.yHigh, mode, pname, phaseData));
                    });
                    toggle.querySelectorAll('button').forEach(b => b.classList.toggle('on', b.dataset.mode === mode));
                };
                // Only a phase with a per-member breakdown has a choice.
                const toggle = document.createElement('span');
                toggle.className = 'mode-toggle';
                GRAPH_MODES.forEach(([key, label]) => {
                    const b = document.createElement('button');
                    b.type = 'button'; b.dataset.mode = key; b.textContent = label;
                    // Every phase of the metric is redrawn: under Global the
                    // mode of one phase moves the range of the others.
                    b.addEventListener('click', e => { e.stopPropagation(); graphModeOf[metric.key + ':' + pname] = key; applyShown(metric); });
                    toggle.appendChild(b);
                });
                if (metric.key !== 'lat' && phaseData.some(d => memberMetric(d, metric))) phaseHdr.appendChild(toggle);
                (REFILL[metric.key] || (REFILL[metric.key] = [])).push(fill);
                (SCALE_PHASES[metric.key] || (SCALE_PHASES[metric.key] = [])).push({pname, phaseData});
            });
            // Drawn once all its phases are known (a Global range needs them all).
            refillMetric(metric);
        });
    }

    document.getElementById('btn-show').addEventListener('click', renderAll);
    document.getElementById('y-scale').addEventListener('change', e => setScaleCriterion(e.target.value));
    document.getElementById('hide-invalid').addEventListener('change', e => setShowInvalid(!e.target.checked));

    fetch(DATA_URL)
        .then(r => r.json())
        .then(data => { allData = data || []; initFilters(); })
        .catch(err => {
            document.getElementById('main').innerHTML =
                '<div class="placeholder">Error loading data: ' + err.message + '</div>';
        });
</script>
</body>
</html>`

	html = strings.ReplaceAll(html, "__NAVBAR_CSS__", navbarCSS)
	html = strings.ReplaceAll(html, "__CHART_SHARED_JS__", chartSharedJS)
	html = strings.ReplaceAll(html, "__NAVBAR_HTML__", navbarHTML("/results"))
	html = strings.ReplaceAll(html, "__DATA_URL__", jsString(dataURL))
	html = strings.ReplaceAll(html, "__SIDEBAR__", sidebar)

	w.Write([]byte(html))
}
