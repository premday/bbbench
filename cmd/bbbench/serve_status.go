package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"bbbench/internal/diskinfo"
)

// BenchmarkStatus represents the current state of benchmark execution.
type BenchmarkStatus struct {
	// Running indicates if benchmarks are currently executing
	Running bool `json:"running"`

	// StartTime is when the current benchmark run started
	StartTime time.Time `json:"start_time,omitempty"`

	// Mode is the execution mode (parallel-sync or sequential)
	Mode string `json:"mode,omitempty"`

	// Drives is the list of drives being benchmarked
	Drives []DriveStatus `json:"drives,omitempty"`

	// CurrentPhase is the current phase number (0-indexed)
	CurrentPhase int `json:"current_phase"`

	// TotalPhases is the total number of phases
	TotalPhases int `json:"total_phases"`

	// CompletedPhases is the number of completed phases
	CompletedPhases int `json:"completed_phases"`

	// LastUpdate is the last time the status was updated
	LastUpdate time.Time `json:"last_update"`

	// phaseConfigs describes each drive's phases (keyed by device name); it is
	// served by /api/status/phases, not with every status poll.
	phaseConfigs map[string][]PhaseConfig
}

// DriveStatus represents the status of a single drive.
type DriveStatus struct {
	Name         string `json:"name"`
	Device       string `json:"device"`
	Vendor       string `json:"vendor"`
	Model        string `json:"model"`
	Serial       string `json:"serial"`
	Capacity     uint64 `json:"capacity_gb"`
	Type         string `json:"type"` // "SSD" or "HDD"
	CurrentPhase int    `json:"current_phase"`
	PhaseName    string `json:"phase_name,omitempty"`
	// PhaseStart is when the running phase started (zero when none runs).
	PhaseStart   time.Time `json:"phase_start,omitzero"`
	Status       string    `json:"status"` // "running", "complete", "error", "pending"
	ErrorMessage string    `json:"error,omitempty"`
	// Skipped is why the drive sits out the current phase (Status "skipped").
	Skipped string `json:"skipped,omitempty"`
	// TempC is the last temperature read while a phase ran (nil before one has).
	TempC *float64 `json:"temp_c,omitempty"`
	// PhaseSeconds is how long each finished phase really took on this drive,
	// by phase index (0-based). The map is replaced, never mutated, on every
	// update, so snapshots can be encoded without the lock.
	PhaseSeconds map[int]float64 `json:"phase_seconds,omitempty"`
	// Config is how the disk is connected and configured (interface, link,
	// power, write cache, SMART); nil until it has been read.
	Config *diskinfo.Info `json:"config,omitempty"`
	// Warning is a caveat about the phase the drive is running.
	Warning       string `json:"warning,omitempty"`
	LastCompleted string `json:"last_completed,omitempty"`
}

// clearNotes drops the per-phase skip reason and warning (a new phase starts).
func (d *DriveStatus) clearNotes() { d.Skipped, d.Warning = "", "" }

// globalStatus is the singleton status tracker
var (
	globalStatus   = &BenchmarkStatus{LastUpdate: time.Now()}
	globalStatusMu sync.RWMutex
)

// UpdateStatus updates the global benchmark status.
func UpdateStatus(fn func(*BenchmarkStatus)) {
	globalStatusMu.Lock()
	defer globalStatusMu.Unlock()

	fn(globalStatus)
	globalStatus.LastUpdate = time.Now()
}

// BenchmarkStatusSnapshot is a mutex-free point-in-time copy of BenchmarkStatus,
// safe to return by value and encode as JSON.
type BenchmarkStatusSnapshot struct {
	Running         bool          `json:"running"`
	StartTime       time.Time     `json:"start_time,omitempty"`
	Mode            string        `json:"mode,omitempty"`
	Drives          []DriveStatus `json:"drives,omitempty"`
	CurrentPhase    int           `json:"current_phase"`
	TotalPhases     int           `json:"total_phases"`
	CompletedPhases int           `json:"completed_phases"`
	LastUpdate      time.Time     `json:"last_update"`
	// ServerTime lets the page measure elapsed time without trusting its own clock.
	ServerTime time.Time `json:"server_time"`
}

// GetStatus returns a point-in-time snapshot of the current status.
func GetStatus() BenchmarkStatusSnapshot {
	globalStatusMu.RLock()
	defer globalStatusMu.RUnlock()

	snap := BenchmarkStatusSnapshot{
		Running:         globalStatus.Running,
		StartTime:       globalStatus.StartTime,
		Mode:            globalStatus.Mode,
		CurrentPhase:    globalStatus.CurrentPhase,
		TotalPhases:     globalStatus.TotalPhases,
		CompletedPhases: globalStatus.CompletedPhases,
		LastUpdate:      globalStatus.LastUpdate,
		ServerTime:      time.Now(),
	}
	if globalStatus.Drives != nil {
		snap.Drives = make([]DriveStatus, len(globalStatus.Drives))
		copy(snap.Drives, globalStatus.Drives)
	}
	return snap
}

// ResetStatus resets the global status to initial state.
func ResetStatus() {
	UpdateStatus(func(s *BenchmarkStatus) {
		s.Running = false
		s.StartTime = time.Time{}
		s.Mode = ""
		s.Drives = nil
		s.CurrentPhase = 0
		s.TotalPhases = 0
		s.CompletedPhases = 0
		s.phaseConfigs = nil
	})
}

// handleStatus serves the current benchmark status as JSON.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	status := GetStatus()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	if err := json.NewEncoder(w).Encode(status); err != nil {
		logger.Error("encode status", "err", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// handleStatusPage serves the auto-refreshing HTML status page.
func handleStatusPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	html := `<!DOCTYPE html>
<html>
<head>
    <title>bbbench - Status</title>
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
        .status-card {
            background: #fff;
            padding: 20px;
            border-radius: 8px;
            margin-bottom: 20px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        .status-badge {
            display: inline-block;
            padding: 4px 12px;
            border-radius: 4px;
            font-size: 14px;
            font-weight: 500;
        }
        .status-idle {
            background: #e0e0e0;
            color: #666;
        }
        .status-running {
            background: #4caf50;
            color: white;
            animation: pulse 2s infinite;
        }
        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50% { opacity: 0.7; }
        }
        .progress-bar {
            width: 100%;
            height: 24px;
            background: #e0e0e0;
            border-radius: 4px;
            overflow: hidden;
            margin: 10px 0;
        }
        .progress-fill {
            height: 100%;
            background: linear-gradient(90deg, #4caf50, #45a049);
            transition: width 0.3s ease;
            display: flex;
            align-items: center;
            justify-content: center;
            color: white;
            font-size: 12px;
            font-weight: 500;
        }
        .drives-grid {
            display: grid;
            grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
            gap: 15px;
            margin-top: 20px;
        }
        .drive-card {
            background: #f9f9f9;
            padding: 15px;
            border-radius: 6px;
            border-left: 4px solid #2196f3;
        }
        .drive-name {
            font-weight: 600;
            font-size: 16px;
            color: #333;
        }
        .drive-detail {
            font-size: 14px;
            color: #666;
            margin-top: 5px;
        }
        .cfg { text-align: left; margin-top: 8px; font-size: 13px; color: #444; }
        .cfg .row { display: flex; gap: 8px; margin-top: 2px; }
        .cfg .k { flex: 0 0 82px; color: #888; }
        .cfg .v { flex: 1; min-width: 0; overflow-wrap: anywhere; }
        .cfg .ok { color: #2e7d32; font-weight: 600; }
        .cfg .bad { color: #c62828; font-weight: 600; }
        .cfg .warn { color: #b26a00; }
        .cfg .dim { color: #888; }
        .phases { text-align: left; margin-top: 10px; border-top: 1px solid #e5e5e5; padding-top: 8px; }
        .phases > summary { cursor: pointer; font-size: 13px; color: #555; font-weight: 600; }
        .phase { margin: 6px 0 0 4px; font-size: 13px; }
        .phase > summary { cursor: pointer; color: #333; }
        .phase.done .pname { text-decoration: line-through; color: #999; }
        .plen { color: #888; font-size: 12px; margin-left: 6px; }
        .plen.unk { color: #b26a00; }
        .plen.real { color: #2e7d32; font-weight: 600; }
        .phase.current > summary { color: #2e7d32; font-weight: 600; }
        .phase-sum { color: #888; font-family: monospace; font-size: 12px; display: block; margin-left: 14px; }
        .phase pre { background: #263238; color: #eceff1; padding: 8px 10px; border-radius: 4px;
            font-size: 12px; overflow-x: auto; margin: 6px 0 4px 14px; }
        .phase pre .sec { color: #82aaff; font-weight: 600; }
        .phase pre .key { color: #c3e88d; }
        .phase pre .eq { color: #78909c; }
        .phase pre .num { color: #f78c6c; }
        .phase pre .val { color: #ffcb6b; }
        .phase pre .flag { color: #c792ea; }
        .last-update {
            text-align: right;
            color: #999;
            font-size: 12px;
            margin-top: 10px;
        }
        .no-data {
            text-align: center;
            padding: 40px;
            color: #999;
        }
        __NAVBAR_CSS__
    </style>
</head>
<body>
__NAVBAR_HTML__
    <div class="page-content">
    <h2 style="margin: 0 0 16px; color: #333; font-size: 20px;">Benchmark Status</h2>
    <div class="status-card">
        <h2>Current Status</h2>
        <div id="status-info">
            <span class="status-badge status-idle">Idle</span>
        </div>
        <div id="progress-container" style="display: none;">
            <div class="progress-bar">
                <div class="progress-fill" id="progress-fill" style="width: 0%">
                    <span id="progress-text">0%</span>
                </div>
            </div>
        </div>
        <div id="benchmark-details"></div>
    </div>

    <div class="status-card">
        <h2>Drives</h2>
        <div id="drives-list" class="no-data">No active benchmarks</div>
    </div>

    <div class="last-update" id="last-update">Last updated: Never</div>

    <script>
        function esc(s) {
            return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
        }

        // Phase descriptions are static for a run: fetched when the drives
        // change, not on every poll. openKeys remembers which <details> the
        // user expanded, since the lists are re-rendered every 2 seconds.
        let phasesByDrive = {};
        let phasesFor = '';
        const openKeys = new Set();

        document.addEventListener('toggle', e => {
            const key = e.target && e.target.dataset && e.target.dataset.key;
            if (!key) return;
            if (e.target.open) { openKeys.add(key); } else { openKeys.delete(key); }
        }, true);

        function loadPhases(drives) {
            const sig = drives.map(d => d.name).join(',');
            if (sig === phasesFor) return;
            phasesFor = sig;
            fetch('/api/status/phases')
                .then(r => r.json())
                .then(data => { phasesByDrive = data || {}; updateStatus(); })
                .catch(() => { phasesFor = ''; });
        }

        // Colors an fio job file: [section], key=value, bare flags.
        function highlight(config) {
            return config.split('\n').map(line => {
                const t = line.trim();
                if (t.charAt(0) === '[') return '<span class="sec">' + esc(line) + '</span>';
                const i = line.indexOf('=');
                if (i < 0) return t ? '<span class="flag">' + esc(line) + '</span>' : '';
                const val = line.slice(i + 1);
                const cls = /^[0-9][0-9a-zA-Z.%]*$/.test(val.trim()) ? 'num' : 'val';
                return '<span class="key">' + esc(line.slice(0, i)) + '</span><span class="eq">=</span>' +
                    '<span class="' + cls + '">' + esc(val) + '</span>';
            }).join('\n');
        }

        let clockSkew = 0; // server clock minus browser clock, in ms

        function fmtDur(sec) {
            sec = Math.max(0, Math.round(sec));
            if (sec < 60) return sec + 's';
            const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60);
            if (h === 0) return m + 'm' + (sec % 60 ? ' ' + (sec % 60) + 's' : '');
            return h + 'h ' + m + 'm';
        }

        // A drive's phase i (0-based) is done when the drive is complete, or
        // when the drive has moved on to a later phase (current_phase is 1-based).
        function phaseDone(drive, i) {
            if (drive.status === 'complete') return true;
            if (drive.status === 'running' || drive.status === 'error' || drive.status === 'skipped') return i < drive.current_phase - 1;
            return false;
        }

        function phaseRunning(drive, i) {
            return drive.status === 'running' && i === drive.current_phase - 1;
        }

        // The real time a phase took on this drive, once it has finished there
        // (a drive can finish before the others have finished the phase).
        function realOf(drive, i) {
            const r = drive.phase_seconds && drive.phase_seconds[i];
            return r == null ? null : r;
        }

        // How the finished phases really ran against their expected length
        // (fio's setup and ramp-up, the drive being slower, ...): the expected
        // length of what remains is scaled by it, so the ETA adjusts as phases
        // finish. 1 until a phase with a known length has finished.
        function paceOf(drive, phases) {
            let actual = 0, expected = 0;
            phases.forEach(p => {
                const r = realOf(drive, p.index);
                if (r != null && p.deterministic && p.duration_sec > 0) { actual += r; expected += p.duration_sec; }
            });
            return expected > 0 ? Math.min(4, Math.max(0.25, actual / expected)) : 1;
        }

        // Total real time of the finished phases.
        function spentOf(drive, phases) {
            return phases.reduce((t, p) => t + (realOf(drive, p.index) || 0), 0);
        }

        // Remaining time of the phases still to run: known lengths are summed,
        // scaled by the pace so far (the running phase minus its elapsed time);
        // phases without a known length are counted apart.
        function etaOf(drive, phases) {
            const pace = paceOf(drive, phases);
            let sec = 0, unknown = 0, left = 0;
            phases.forEach(p => {
                if (phaseDone(drive, p.index) || realOf(drive, p.index) != null) return;
                left++;
                if (!p.deterministic) { unknown++; return; }
                let d = p.duration_sec * pace;
                if (phaseRunning(drive, p.index) && drive.phase_start) {
                    d -= (Date.now() + clockSkew - Date.parse(drive.phase_start)) / 1000;
                }
                sec += Math.max(0, d);
            });
            return { sec: sec, unknown: unknown, left: left, pace: pace };
        }

        function etaText(e) {
            if (e.left === 0) return 'all done';
            let t = e.sec > 0 ? 'ETA ~' + fmtDur(e.sec) + ' (about ' +
                new Date(Date.now() + e.sec * 1000).toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'}) + ')' : '';
            if (!t && e.unknown === 0) return 'past its expected length, ETA unknown';
            if (e.unknown > 0) {
                t += (t ? ' + ' : 'ETA unknown: ') + e.unknown + ' phase' + (e.unknown > 1 ? 's' : '') + ' of unknown length';
            }
            return t;
        }

        // What a phase row says about its time: the real time when finished
        // (with the expected one beside it), elapsed against expected while
        // running, the expected length before.
        function phaseTimeHTML(drive, p) {
            const real = realOf(drive, p.index);
            const exp = p.deterministic ? fmtDur(p.duration_sec) : null;
            if (real != null) {
                return '<span class="plen real">took ' + fmtDur(real) + '</span>' +
                    (exp ? ' <span class="plen">expected ~' + exp + '</span>' : '');
            }
            if (phaseRunning(drive, p.index)) {
                const el = drive.phase_start ? (Date.now() + clockSkew - Date.parse(drive.phase_start)) / 1000 : 0;
                const base = fmtDur(el) + ' elapsed';
                if (!exp) return '<span class="plen unk">' + base + ', length unknown</span>';
                return '<span class="plen">' + base + ' of ~' + exp + (el > p.duration_sec ? ' (over)' : '') + '</span>';
            }
            return '<span class="plen' + (p.deterministic ? '' : ' unk') + '">' + (exp ? '~' + exp : 'length unknown') + '</span>';
        }

        // The disk configuration read at the start of the run: interface, link,
        // power, write cache and SMART health. Anything unreadable is left out.
        function configHTML(c) {
            if (!c) return '<div class="cfg"><span class="dim">Reading configuration&hellip;</span></div>';
            const row = (k, v) => v ? '<div class="row"><span class="k">' + k + '</span><span class="v">' + v + '</span></div>' : '';
            const sp = (cls, t) => '<span class="' + cls + '">' + esc(t) + '</span>';
            let link = c.link_speed ? esc(c.link_speed) : '';
            if (c.link_max) link += ' <span class="dim">(max ' + esc(c.link_max) + ')</span>';
            if (c.link_below_max) link += ' ' + sp('warn', 'below max');
            const p = c.power || {};
            const power = [
                p.nvme_state ? 'state ' + p.nvme_state : '',
                p.apst ? 'APST ' + p.apst : '',
                p.runtime_pm ? 'runtime PM ' + p.runtime_pm : '',
                p.aspm ? 'ASPM ' + p.aspm : '',
                p.link_policy ? 'link policy ' + p.link_policy : ''
            ].filter(Boolean).map(esc).join('<br>');
            const w = c.write_cache || {};
            let wc = '';
            if (w.kernel) wc += 'kernel ' + (w.kernel === 'write through' ? sp('warn', w.kernel) : esc(w.kernel));
            if (w.drive) wc += (wc ? ' &middot; ' : '') + 'drive cache ' + esc(w.drive);
            const s = c.smart || {};
            let smart = '';
            if (s.status) {
                smart = sp(s.status === 'PASSED' ? 'ok' : 'bad', s.status);
                const bits = [];
                if (s.temp_c != null) bits.push(s.temp_c + ' \u00b0C');
                if (s.percent_used != null) bits.push('wear ' + s.percent_used + '%');
                if (s.power_on_hours != null) bits.push(s.power_on_hours + ' h');
                if (s.media_errors) bits.push(s.media_errors + ' media errors');
                if (s.reallocated) bits.push(s.reallocated + ' reallocated');
                if (s.pending) bits.push(s.pending + ' pending');
                if (bits.length) smart += ' <span class="dim">' + esc(bits.join(' \u00b7 ')) + '</span>';
                if (s.source) smart += ' <span class="dim">(' + esc(s.source) + ')</span>';
            } else if (s.detail) {
                smart = sp('dim', 'unknown: ' + s.detail);
            }
            return '<div class="cfg">' + row('Interface', esc(c.interface || 'unknown')) + row('Link', link) +
                row('Power', power) + row('Write cache', wc) + row('SMART', smart) + '</div>';
        }

        function phasesHTML(drive) {
            const phases = phasesByDrive[drive.name];
            if (!phases || phases.length === 0) return '';
            const k = 'p:' + drive.name;
            const done = phases.filter(p => phaseDone(drive, p.index)).length;
            const spent = spentOf(drive, phases);
            let h = '<details class="phases" data-key="' + esc(k) + '"' + (openKeys.has(k) ? ' open' : '') + '>' +
                '<summary>Phases (' + phases.length + ') &middot; ' + done + ' done' +
                (spent > 0 ? ' in ' + esc(fmtDur(spent)) : '') + ' &middot; ' +
                esc(etaText(etaOf(drive, phases))) + '</summary>';
            phases.forEach(p => {
                const key = 'c:' + drive.name + ':' + p.index;
                const isDone = phaseDone(drive, p.index);
                const cur = phaseRunning(drive, p.index);
                h += '<details class="phase' + (cur ? ' current' : '') + (isDone ? ' done' : '') +
                    '" data-key="' + esc(key) + '"' + (openKeys.has(key) ? ' open' : '') + '>' +
                    '<summary><span class="pname">' + (p.index + 1) + '. ' + esc(p.name) + '</span>' +
                    (cur ? ' (running)' : '') + ' ' + phaseTimeHTML(drive, p) + '</summary>' +
                    '<span class="phase-sum">' + esc(p.summary) + '</span>' +
                    (p.open ? '<span class="phase-sum">' + esc(p.open) + '</span>' : '') +
                    '<pre>' + highlight(p.config) + '</pre></details>';
            });
            return h + '</details>';
        }

        function updateStatus() {
            fetch('/api/status')
                .then(response => response.json())
                .then(data => {
                    updateStatusDisplay(data);
                })
                .catch(error => {
                    console.error('Error fetching status:', error);
                });
        }

        function updateStatusDisplay(status) {
            if (status.server_time) clockSkew = Date.parse(status.server_time) - Date.now();
            const statusInfo = document.getElementById('status-info');
            const progressContainer = document.getElementById('progress-container');
            const progressFill = document.getElementById('progress-fill');
            const progressText = document.getElementById('progress-text');
            const benchmarkDetails = document.getElementById('benchmark-details');
            const drivesList = document.getElementById('drives-list');
            const lastUpdate = document.getElementById('last-update');

            // Update status badge
            if (status.running) {
                statusInfo.innerHTML = '<span class="status-badge status-running">Running</span>';
                progressContainer.style.display = 'block';

                // Update progress
                const progress = status.total_phases > 0
                    ? Math.round((status.completed_phases / status.total_phases) * 100)
                    : 0;
                progressFill.style.width = progress + '%';
                progressText.textContent = progress + '%';

                // Show benchmark details
                benchmarkDetails.innerHTML =
                    '<div class="drive-detail">Mode: ' + status.mode + '</div>' +
                    '<div class="drive-detail">Phase: ' + (status.current_phase + 1) + ' / ' + status.total_phases + '</div>' +
                    '<div class="drive-detail">Completed: ' + status.completed_phases + ' / ' + status.total_phases + '</div>';
            } else {
                statusInfo.innerHTML = '<span class="status-badge status-idle">Idle</span>';
                progressContainer.style.display = 'none';
                benchmarkDetails.innerHTML = '';
            }

            // Update drives list
            if (status.drives && status.drives.length > 0) {
                // Sort drives alphabetically by name
                const sortedDrives = status.drives.slice().sort((a, b) => a.name.localeCompare(b.name));

                let html = '<div class="drives-grid">';
                sortedDrives.forEach(drive => {
                    // Determine status display
                    let statusText = drive.status || 'pending';
                    let statusColor = '#666';
                    if (statusText === 'running') {
                        statusColor = '#4caf50';
                        statusText = 'Running';
                    } else if (statusText === 'complete') {
                        statusColor = '#2196f3';
                        statusText = 'Complete';
                    } else if (statusText === 'skipped') {
                        statusColor = '#b26a00';
                        statusText = 'Skipped this phase';
                    } else if (statusText === 'error') {
                        statusColor = '#f44336';
                        statusText = 'Error';
                    } else {
                        statusText = 'Pending';
                    }

                    html += '<div class="drive-card">' +
                        '<div class="drive-name">' + drive.name + ' (' + (drive.type || 'Unknown') + ')</div>' +
                        '<div class="drive-detail">' + drive.vendor + ' ' + drive.model + '</div>';
                    if (drive.serial) {
                        html += '<div class="drive-detail">S/N: ' + drive.serial + '</div>';
                    }
                    if (drive.capacity_gb) {
                        html += '<div class="drive-detail">Capacity: ' + drive.capacity_gb + ' GB</div>';
                    }
                    html += configHTML(drive.config);
                    if (drive.temp_c != null) {
                        html += '<div class="drive-detail">Temperature: ' + drive.temp_c.toFixed(1) + ' &deg;C <span class="dim" title="last reading while a phase ran">(last sampled)</span></div>';
                    }
                    html += '<div class="drive-detail" style="color: ' + statusColor + '; font-weight: 600;">Status: ' + statusText + '</div>';
                    if (drive.phase_name && drive.status === 'running') {
                        html += '<div class="drive-detail">Phase: ' + drive.phase_name + '</div>';
                    }
                    if (drive.warning) {
                        html += '<div class="drive-detail" style="color: #b26a00; white-space: pre-line; text-align: left;">&#9888; ' + esc(drive.warning) + '</div>';
                    }
                    if (drive.skipped) {
                        html += '<div class="drive-detail" style="color: #b26a00;">' + esc(drive.skipped) + '</div>';
                    }
                    if (drive.error) {
                        html += '<div class="drive-detail" style="color: #f44336;">Error: ' + drive.error + '</div>';
                    }
                    html += phasesHTML(drive);
                    html += '</div>';
                });
                html += '</div>';
                drivesList.innerHTML = html;
                loadPhases(sortedDrives);
            } else {
                drivesList.innerHTML = '<div class="no-data">No active benchmarks</div>';
            }

            // Update last update time
            if (status.last_update) {
                const date = new Date(status.last_update);
                lastUpdate.textContent = 'Last updated: ' + date.toLocaleTimeString();
            }
        }

        // Update every 2 seconds
        updateStatus();
        setInterval(updateStatus, 2000);
    </script>
    </div>
</body>
</html>`
	html = strings.ReplaceAll(html, "__NAVBAR_CSS__", navbarCSS)
	html = strings.ReplaceAll(html, "__NAVBAR_HTML__", navbarHTML("/status"))

	w.Write([]byte(html))
}
