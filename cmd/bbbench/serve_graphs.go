package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"bbbench/internal/flushtrace"
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
        // label names the metric without a unit: each card's y axis shows the
        // unit chosen from its phase's scale (unitScale).
        { key: 'iops', label: 'IOPS',          unit: 'IOPS',  tsKey: 'ts_iops', source: 'fio',
          sumR: d => d.read_iops||0,    sumW: d => d.write_iops||0    },
        { key: 'bw',   label: 'Bandwidth',     unit: 'MB/s',  tsKey: 'ts_bw', source: 'fio',
          sumR: d => d.read_bw_mbps||0, sumW: d => d.write_bw_mbps||0 },
        { key: 'lat',  label: 'Latency',       unit: '\u03bcs', tsKey: 'ts_lat', source: 'fio',
          sumR: d => d.read_lat_us||0,  sumW: d => d.write_lat_us||0  },
        // Device Flush latency, timed from kernel trace events (not fio).
        { key: 'flush', label: 'Flush latency', unit: '\u03bcs', tsKey: 'ts_flush', source: 'ftrace',
          has:  d => !!d.flush || !!d.skipped || !!(d.ts_flush && d.ts_flush.length),
          sumR: d => 0, sumW: d => d.flush ? (d.flush.mean_ns||0)/1000 : 0 },
    ];
    const SRC_TITLE = {
        fio:    'Measured by fio',
        ftrace: 'Measured from kernel ftrace events (device Flush commands), not by fio',
    };
    // flushNone explains a Flush phase that was traced but saw no Flush.
    function flushNone(d, metric) {
        if (metric.key !== 'flush' || !d.flush || d.flush.count > 0) return '';
        return 'ftrace saw 0 Flush commands' + (d.flush.write_cache === 'write through'
            ? ': the kernel sends no Flush to this drive (write cache: write through, i.e. its volatile write cache is disabled or absent).'
            : '.');
    }
    function srcBadge(metric) {
        const b = document.createElement('span');
        b.className = 'src src-' + metric.source;
        b.textContent = metric.source;
        b.title = SRC_TITLE[metric.source];
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
        const GT = [['iops','IOPS'],['bw','Bandwidth'],['lat','Latency'],['flush','Flush latency']];
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
            lbl.appendChild(srcBadge(METRICS.find(m => m.key === v)));
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
    // spans 0..X_MAX, so graphs line up. fio's own logs start when its
    // ramp-up ends, so their points are shifted by the ramp; the Flush series
    // already counts from the start of tracing. The ramp-up is shaded.
    let X_MAX = 0;
    __CHART_SHARED_JS__

    // yAxisTitle is the y axis title of a card: the unit its ticks are in.
    function yAxisTitle(sc) {
        return { display: true, text: sc.unit, color: '#94a3b8', font: {size: 9} };
    }

    function makeFlushChart(canvas, d, metric, yHigh) {
        const ts = d[metric.tsKey];
        const sc = unitScale(metric.unit, yHigh || 0), tick = v => fmtScaled(v, sc);
        const color = 'rgba(124,58,237,0.9)';
        if (ts && ts.length > 0) {
            const pts = ts.filter(p => p.w > 0).map(p => ({x: Math.round(p.t), y: p.w}));
            return new Chart(canvas, {
                type: 'line', plugins: [rampPlugin],
                data: { datasets: [{ label: 'Flush (ftrace)', data: pts,
                    borderColor: color, backgroundColor: 'rgba(124,58,237,0.07)',
                    fill: 'origin', tension: 0.2, pointRadius: 0, pointHoverRadius: 3, borderWidth: 1.5 }] },
                options: {
                    responsive: true, maintainAspectRatio: false, animation: false,
                    plugins: {
                        rampZone: { sec: rampOf(d) },
                        legend: { display: true, position: 'top',
                            labels: { boxWidth: 10, font: {size: 10}, padding: 5 } },
                        tooltip: { callbacks: {
                            title: items => 'window ' + items[0].raw.x + '\u2013' + (items[0].raw.x + (d.flush && d.flush.avg_msec ? d.flush.avg_msec / 1000 : 1)) + ' s',
                            label: ctx => 'Flush latency: ' + fmtScaled(ctx.raw.y, sc) + ' ' + sc.unit +
                                ' (mean' + (d.flush && d.flush.avg_msec ? ' per ' + d.flush.avg_msec / 1000 + ' s' : '') + ')' } }
                    },
                    scales: {
                        x: { type: 'linear', min: 0, max: X_MAX || undefined,
                             title: { display: true, text: 'seconds', color: '#94a3b8', font: {size: 9} },
                             ticks: { font: {size: 9}, color: '#64748b', maxTicksLimit: 6 } },
                        y: { min: 0, max: yHigh, title: yAxisTitle(sc),
                             ticks: { callback: tick, font: {size: 9}, color: '#64748b' },
                             grid: { color: '#f1f5f9' } }
                    }
                }
            });
        }
        return new Chart(canvas, {
            type: 'bar',
            data: { labels: ['Flush (ftrace)'], datasets: [{ data: [metric.sumW(d)],
                backgroundColor: 'rgba(124,58,237,0.75)', borderWidth: 1 }] },
            options: {
                responsive: true, maintainAspectRatio: false, animation: false,
                plugins: { legend: { display: false },
                    tooltip: { callbacks: { label: ctx => fmtScaled(ctx.raw, sc) + ' ' + sc.unit } } },
                scales: { y: { min: 0, max: yHigh, title: yAxisTitle(sc),
                    ticks: { callback: tick, font: {size: 9} } } }
            }
        });
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
    // the datasets by position), each member's and the array's point per x,
    // and the stack totals summed from those same member points, so the
    // totals and the Y scale cannot differ from the drawn stack top.
    function memberGrid(d, metric) {
        const step = memberStep(d), shift = fioShift(d), keys = new Set();
        const members = {}, array = {}, totals = {};
        memberNames(d).forEach(n => {
            const m = members[n] = {};
            (d.ts_members[n][metric.key] || []).forEach(p => {
                const x = memberKey(p.t, step);
                m[x] = p;
                keys.add(x);
            });
        });
        (d[metric.tsKey] || []).forEach(p => {
            const x = memberKey(p.t + shift, step);
            array[x] = p;
            keys.add(x);
        });
        Object.values(members).forEach(m => Object.keys(m).forEach(x => {
            const s = totals[x] || (totals[x] = {r: 0, w: 0});
            s.r += m[x].r || 0;
            s.w += m[x].w || 0;
        }));
        return { xs: [...keys].sort((a, b) => a - b), members, array, totals };
    }
    // memberExtent is the highest read and write a member chart draws: the
    // stack totals for IOPS and bandwidth, each member's own for latency.
    function memberExtent(d, metric) {
        const g = memberGrid(d, metric);
        let r = 0, w = 0;
        if (metric.key === 'lat') {
            Object.values(g.members).forEach(m => Object.values(m).forEach(p => {
                r = Math.max(r, p.r || 0);
                w = Math.max(w, p.w || 0);
            }));
        } else {
            Object.values(g.totals).forEach(s => {
                r = Math.max(r, s.r);
                w = Math.max(w, s.w);
            });
        }
        return {r, w};
    }
    function memberMaxT(d, metric) {
        let m = 0;
        memberNames(d).forEach(n => (d.ts_members[n][metric.key] || []).forEach(p => { m = Math.max(m, p.t); }));
        return m;
    }
    function memberLabel(d, n, dir) {
        const role = d.ts_members[n].role;
        return n + (role ? ' (' + role + ')' : '') + (dir === 'r' ? ' read' : ' write');
    }
    function hasDir(pts, dir) {
        return Object.values(pts).some(p => (p[dir] || 0) > 0);
    }
    // stackedMemberDatasets: one filled band per member and direction. Read
    // and write are two stacks: Chart.js adds a 0 to every earlier value of
    // its stack whatever their sign, so an idle write would sit on the reads.
    // Zeros are kept: a missing point would let the bands above it drop.
    function stackedMemberDatasets(d, g, colors) {
        const out = [];
        ['r', 'w'].forEach(dir => {
            if (!memberNames(d).some(n => hasDir(g.members[n], dir))) return;
            let first = true;
            memberNames(d).forEach(n => {
                const c = colors[n], pts = g.members[n];
                out.push({
                    label: memberLabel(d, n, dir), member: n, dir,
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
    // memberLatencyDatasets: one line per member and direction. A window
    // without I/O has no latency (0): no point.
    function memberLatencyDatasets(d, g, colors) {
        const out = [];
        ['r', 'w'].forEach(dir => memberNames(d).forEach(n => {
            const pts = g.members[n];
            if (!hasDir(pts, dir)) return;
            const c = colors[n];
            out.push({
                label: memberLabel(d, n, dir), member: n, dir,
                data: g.xs.map(x => ({x, y: pts[x] && pts[x][dir] > 0 ? (dir === 'r' ? 1 : -1) * pts[x][dir] : null})),
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
                label: 'Array ' + name.toLowerCase(), array: true, dir, order: -1,
                data: g.xs.map(x => ({x, y: pts[x] && pts[x][dir] > 0 ? (dir === 'r' ? 1 : -1) * pts[x][dir] : null})),
                fill: false, borderColor: color, backgroundColor: color,
                tension: 0.2, pointRadius: 0, pointHoverRadius: 3, borderWidth: 2, spanGaps: true,
            };
            if (stacked) ds.stack = 'array';
            out.push(ds);
        });
        return out;
    }
    function makeMembersChart(canvas, d, metric, yLow, yHigh, warnEl) {
        tsWarn(warnEl, d[metric.tsKey] || []);
        const stacked = metric.key !== 'lat';
        const g = memberGrid(d, metric), xs = g.xs, colors = memberColors(memberNames(d));
        const members = stacked ? stackedMemberDatasets(d, g, colors) : memberLatencyDatasets(d, g, colors);
        const array = arrayDatasets(g, stacked);
        const datasets = members.concat(array);
        const totals = stacked ? g.totals : {};
        const dirs = ['r', 'w'].filter(dir => members.some(m => m.dir === dir));
        const sc = unitScale(metric.unit, Math.max(yHigh || 0, -(yLow || 0))), tick = v => fmtScaled(v, sc);
        const fmt = v => fmtScaled(v, sc) + ' ' + sc.unit;
        const totalLines = i => {
            const s = totals[xs[i]];
            if (!s) return [];
            return dirs.map(dir => 'Stack total ' + (dir === 'r' ? 'read' : 'write') + ': ' + fmt(s[dir]));
        };
        // The first array item shown at index i, which carries the stack totals.
        const firstArray = (chart, i) => {
            for (let k = members.length; k < datasets.length; k++) {
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
                    legend: { display: true, position: 'top',
                        labels: { boxWidth: 10, font: {size: 10}, padding: 5 } },
                    tooltip: {
                        mode: 'index', intersect: false,
                        // Members first, then the stack total, then the array.
                        itemSort: (a, b) => (a.dataset.array ? 1 : 0) - (b.dataset.array ? 1 : 0) || a.datasetIndex - b.datasetIndex,
                        callbacks: {
                            title: items => windowOf(d) ? 'window ' + items[0].raw.x + '\u2013' + (items[0].raw.x + windowOf(d)) + ' s' : items[0].raw.x + ' s',
                            beforeLabel: ctx => stacked && ctx.dataset.array && firstArray(ctx.chart, ctx.dataIndex) === ctx.datasetIndex
                                ? totalLines(ctx.dataIndex) : '',
                            label: ctx => ctx.dataset.label + ': ' + fmt(ctx.raw.y),
                            afterBody: items => (stacked && items.length && firstArray(items[0].chart, items[0].dataIndex) === -1
                                ? totalLines(items[0].dataIndex) : []).concat(mdTipLines(d, items)),
                        },
                    },
                },
                scales: {
                    x: { type: 'linear', min: 0, max: X_MAX || undefined,
                         title: { display: true, text: 'seconds', color: '#94a3b8', font: {size: 9} },
                         ticks: { font: {size: 9}, color: '#64748b', maxTicksLimit: 6 } },
                    y: { stacked: stacked, min: yLow, max: yHigh, title: yAxisTitle(sc),
                         ticks: { callback: tick, font: {size: 9}, color: '#64748b' },
                         grid: { color: ctx => ctx.tick.value === 0 ? '#94a3b8' : '#f1f5f9' } }
                }
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
    // mdIndex keys the md windows like the member grid (memberKey).
    function mdIndex(d) {
        const step = memberStep(d), idx = {};
        (d.ts_md || []).forEach(p => { idx[memberKey(p.t, step)] = p; });
        return idx;
    }
    // mdTipLines: the hovered window's md state and its events, for a
    // tooltip's afterBody; [] without md data or without an md window there.
    function mdTipLines(d, items) {
        if (!hasMD(d) || !items || !items.length) return [];
        const step = memberStep(d), k = memberKey(items[0].raw.x, step), out = [];
        const p = mdIndex(d)[k];
        if (p) {
            out.push('md: ' + (mdBusy(p)
                ? p.action + ' ' + Math.round(p.pct || 0) + '% ' + ((p.speed || 0) / 1024).toFixed(1) + ' MiB/s'
                : (p.action || 'unknown')) + ', degraded ' + (p.degraded < 0 ? 'unknown' : (p.degraded || 0)));
        }
        (d.md_events || []).forEach(e => {
            if (memberKey(e.t, step) === k) out.push('md event: ' + e.text);
        });
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
    function makeChart(canvas, d, metric, yLow, yHigh, warnEl) {
        const ts = d[metric.tsKey];
        if (metric.key === 'flush') return makeFlushChart(canvas, d, metric, yHigh);
        if (memberMetric(d, metric)) return makeMembersChart(canvas, d, metric, yLow, yHigh, warnEl);
        // One unit for every card of the phase: chosen from the phase's scale.
        const sc = unitScale(metric.unit, Math.max(yHigh || 0, -(yLow || 0))), tick = v => fmtScaled(v, sc);
        if (ts && ts.length > 0) {
            tsWarn(warnEl, ts);
            const ramp = rampOf(d), shift = fioShift(d);
            const readPts  = ts.filter(p => p.r > 0).map(p => ({x: Math.round(p.t + shift), y:  p.r}));
            const writePts = ts.filter(p => p.w > 0).map(p => ({x: Math.round(p.t + shift), y: -p.w}));
            const datasets = [];
            if (readPts.length > 0) datasets.push({
                label: 'Read',  data: readPts,
                borderColor: 'rgba(37,99,235,0.9)',  backgroundColor: 'rgba(37,99,235,0.07)',
                fill: 'origin', tension: 0.2, pointRadius: 0, pointHoverRadius: 3, borderWidth: 1.5,
            });
            if (writePts.length > 0) datasets.push({
                label: 'Write', data: writePts,
                borderColor: 'rgba(220,38,38,0.9)', backgroundColor: 'rgba(220,38,38,0.07)',
                fill: 'origin', tension: 0.2, pointRadius: 0, pointHoverRadius: 3, borderWidth: 1.5,
            });
            return new Chart(canvas, {
                type: 'line', data: { datasets }, plugins: chartPlugins(d, metric),
                options: {
                    responsive: true, maintainAspectRatio: false, animation: false,
                    plugins: {
                        rampZone: { sec: ramp },
                        mdTrack: mdOptions(d, metric),
                        legend: { display: true, position: 'top',
                            labels: { boxWidth: 10, font: {size: 10}, padding: 5 } },
                        tooltip: { callbacks: {
                            title: items => windowOf(d) ? 'window ' + items[0].raw.x + '\u2013' + (items[0].raw.x + windowOf(d)) + ' s' : items[0].raw.x + ' s',
                            label: ctx => (ctx.raw.y >= 0 ? 'Read: ' : 'Write: ') +
                                          fmtScaled(ctx.raw.y, sc) + ' ' + sc.unit,
                            afterBody: items => mdTipLines(d, items)
                        }}
                    },
                    scales: {
                        x: { type: 'linear', min: 0, max: X_MAX || undefined,
                             title: { display: true, text: 'seconds', color: '#94a3b8', font: {size: 9} },
                             ticks: { font: {size: 9}, color: '#64748b', maxTicksLimit: 6 } },
                        y: { min: yLow, max: yHigh, title: yAxisTitle(sc),
                             ticks: { callback: tick, font: {size: 9}, color: '#64748b' },
                             grid: { color: ctx => ctx.tick.value === 0 ? '#94a3b8' : '#f1f5f9' } }
                    }
                }
            });
        } else {
            const r = metric.sumR(d), w = metric.sumW(d);
            const lbls = [], vals = [], cols = [];
            if (r > 0) { lbls.push('Read');  vals.push(r);  cols.push('rgba(37,99,235,0.75)'); }
            if (w > 0) { lbls.push('Write'); vals.push(-w); cols.push('rgba(220,38,38,0.75)'); }
            if (!lbls.length) { lbls.push('\u2014'); vals.push(0); cols.push('#e2e8f0'); }
            return new Chart(canvas, {
                type: 'bar',
                data: { labels: lbls, datasets: [{ data: vals, backgroundColor: cols, borderWidth: 1 }] },
                options: {
                    responsive: true, maintainAspectRatio: false, animation: false,
                    plugins: { legend: { display: false },
                        tooltip: { callbacks: { label: ctx =>
                            fmtScaled(ctx.raw, sc) + ' ' + sc.unit } }
                    },
                    scales: { y: { min: yLow, max: yHigh, title: yAxisTitle(sc),
                        ticks: { callback: tick, font: {size: 9} } } }
                }
            });
        }
    }

    // ── Modal ──
    let modalChart = null;

    function openModal(d, metric, yLow, yHigh) {
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
        modalChart = makeChart(newCanvas, d, metric, yLow, yHigh, warnEl);
        document.getElementById('modal-overlay').classList.add('active');
    }

    function closeModal() {
        document.getElementById('modal-overlay').classList.remove('active');
        if (modalChart) { modalChart.destroy(); modalChart = null; }
    }

    document.getElementById('modal-overlay').addEventListener('click', e => {
        if (e.target === e.currentTarget) closeModal();
    });
    document.addEventListener('keydown', e => {
        if (e.key === 'Escape') closeModal();
    });

    function makeChartCard(d, metric, cid, yLow, yHigh) {
        const card = document.createElement('div');
        card.className = 'chart-card';

        const t1 = document.createElement('div');
        t1.className = 'card-title';
        t1.textContent = d.disk + settingTag(d, RPV);
        t1.appendChild(document.createTextNode(' '));
        t1.appendChild(srcBadge(metric));
        const mdh = mdHeader(d, metric);
        if (mdh) mdh.badges.forEach(b => { t1.appendChild(document.createTextNode(' ')); t1.appendChild(b); });
        card.appendChild(t1);

        const t2 = document.createElement('div');
        t2.className = 'card-subtitle';
        const tech = d.technology ? d.technology.toUpperCase() : '';
        t2.textContent = [d.vendor, d.model, d.serial, tech, membersText(d)].filter(Boolean).join('  \u00b7  ');
        card.appendChild(t2);
        if (mdh && mdh.note) card.appendChild(mdh.note);

        const wrap = document.createElement('div');
        wrap.className = 'chart-wrap';
        const canvas = document.createElement('canvas');
        wrap.appendChild(canvas);
        card.appendChild(wrap);

        const warnEl = document.createElement('div');
        card.appendChild(warnEl);
        const why = d.skipped ? 'Skipped: ' + d.skipped : flushNone(d, metric);
        if (why) {
            // Traced, but no Flush command reached the device: say why
            // instead of drawing an empty chart.
            wrap.removeChild(canvas);
            wrap.className = 'chart-wrap none-note' + (d.skipped ? ' skipped' : '');
            wrap.textContent = why;
            return card;
        }
        charts[cid] = makeChart(canvas, d, metric, yLow, yHigh, warnEl);

        card.addEventListener('click', () => openModal(d, metric, yLow, yHigh));
        return card;
    }

    function renderAll() {
        destroyAllCharts();
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

        // One time axis for every chart shown.
        X_MAX = 0;
        data.forEach(d => selMetrics.forEach(m => {
            X_MAX = Math.max(X_MAX, seriesMaxT(d, m), memberMetric(d, m) ? memberMaxT(d, m) : 0);
        }));

        selMetrics.forEach(metric => {
            // ── Metric-level foldable section ──
            const section = document.createElement('div');
            section.className = 'metric-section';

            const secHdr = document.createElement('div');
            secHdr.className = 'sec-hdr';
            secHdr.innerHTML = '<span class="chev">▼</span>' + metric.label + ' ';
            secHdr.appendChild(srcBadge(metric));
            section.appendChild(secHdr);

            const secBody = document.createElement('div');
            secBody.className = 'sec-body';
            section.appendChild(secBody);
            main.appendChild(section);

            secHdr.addEventListener('click', () => toggleSection(secHdr, secBody));

            // Group data by phase, preserving encounter order. Metrics with a
            // "has" test (Flush) only show disks that have that data.
            const phaseOrder = [];
            const byPhase = {};
            const mdata = metric.has ? data.filter(metric.has) : data;
            if (mdata.length === 0) {
                const none = document.createElement('div');
                none.className = 'placeholder';
                none.textContent = 'No ' + metric.source + ' data for the selected disks and phases.';
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

                // Compute Y scale scoped to this (metric, phase) tuple.
                let yMax = 0, yMin = 0;
                phaseData.forEach(d => {
                    const ts = d[metric.tsKey];
                    if (metric.key === 'flush') {
                        const top = Math.max(metric.sumW(d), ...(ts || []).map(p => p.w || 0));
                        if (top > yMax) yMax = top;
                    } else if (ts && ts.length > 0) {
                        ts.forEach(p => {
                            if (p.r > yMax) yMax = p.r;
                            if (p.w > 0 && -p.w < yMin) yMin = -p.w;
                        });
                    } else {
                        const r = metric.sumR(d), w = metric.sumW(d);
                        if (r > yMax) yMax = r;
                        if (w > 0 && -w < yMin) yMin = -w;
                    }
                    if (memberMetric(d, metric)) {
                        const e = memberExtent(d, metric);
                        if (e.r > yMax) yMax = e.r;
                        if (e.w > 0 && -e.w < yMin) yMin = -e.w;
                    }
                });
                const yHigh = yMax > 0 ? yMax * 1.15 : (yMin < 0 ? 0 : 1);
                const yLow  = yMin < 0 ? yMin * 1.15 : 0;

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

                phaseData.forEach((d, i) => {
                    const cid = metric.key + ':' + pname + ':' + i;
                    const card = makeChartCard(d, metric, cid, yLow, yHigh);
                    grid.appendChild(card);
                });
            });
        });
    }

    document.getElementById('btn-show').addEventListener('click', renderAll);
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
