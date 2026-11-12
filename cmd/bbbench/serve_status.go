package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"bbbench/internal/diskinfo"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
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

	// Group is the run group being executed (nil for a single run). It is
	// replaced, never mutated, on every update.
	Group *GroupStatus `json:"group,omitempty"`

	// phaseConfigs describes each drive's phases (keyed by device name); it is
	// served by /api/status/phases, not with every status poll.
	phaseConfigs map[string][]PhaseConfig
}

// Group steps, in the order a run goes through them.
const (
	stepApplying        = "applying"
	stepVerifyingBefore = "verifying-before"
	stepRunning         = "running"
	stepVerifyingAfter  = "verifying-after"
	stepRestoring       = "restoring"
	stepDone            = "done"
	// Layout groups: before stepVerifyingBefore, then after stepVerifyingAfter.
	stepBuilding    = "building"
	stepResyncing   = "resyncing"
	stepTearingDown = "tearing-down"
)

// GroupStatus is where a run group is: run Run (1-based) of Runs, at Step.
type GroupStatus struct {
	ID       string            `json:"id"`
	Setting  string            `json:"setting"`
	Strategy rungroup.Strategy `json:"strategy"`
	Run      int               `json:"run"`
	Runs     int               `json:"runs"`
	Step     string            `json:"step"`
	// Progress is the resync progress in percent while Step is resyncing.
	Progress float64 `json:"progress,omitempty"`
	// Variant is the label of the current layout (layout groups only).
	Variant string `json:"variant,omitempty"`
}

// DriveSetting is a drive's setting value in the current run of a group and
// its verification: Before and After are nil until read, Valid is nil until
// the run has been verified after.
type DriveSetting struct {
	Value  string            `json:"value"`
	Before *settings.Reading `json:"before,omitempty"`
	After  *settings.Reading `json:"after,omitempty"`
	Valid  *bool             `json:"valid,omitempty"`
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
	// Setting is the run group setting of the drive (nil for a single run).
	// It is replaced, never mutated, so snapshots can share it.
	Setting *DriveSetting `json:"setting,omitempty"`
	// Kind is "" for a drive, "array" for a composite target with Members.
	Kind    string         `json:"kind,omitempty"`
	Members []MemberStatus `json:"members,omitempty"`
}

// MemberStatus is a member disk of an array target.
type MemberStatus struct {
	rungroup.Member
	Config *diskinfo.Info `json:"config,omitempty"`
}

// newDriveStatuses is the pending status of each drive at the start of a
// run. A drive's Setting is kept from prev (the run group sets it before the
// run starts).
func newDriveStatuses(drives []DriveInfo, prev []DriveStatus) []DriveStatus {
	out := make([]DriveStatus, len(drives))
	for i, drive := range drives {
		driveType := "SSD"
		if drive.Device.Rotational {
			driveType = "HDD"
		}
		out[i] = DriveStatus{
			Name:     drive.Device.Name,
			Vendor:   drive.Device.Vendor,
			Model:    drive.Device.Model,
			Serial:   drive.Device.Serial,
			Capacity: drive.Device.CapacityBytes / (1024 * 1024 * 1024),
			Type:     driveType,
			Status:   "pending",
		}
		if drive.IsArray() {
			out[i].Kind = string(settings.TargetArray)
			for _, m := range drive.Members {
				out[i].Members = append(out[i].Members, MemberStatus{Member: m})
			}
		}
		for _, p := range prev {
			if p.Name == drive.Device.Name {
				out[i].Setting = p.Setting
				break
			}
		}
	}
	return out
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
	// Group is a copy of the run group state (nil for a single run).
	Group *GroupStatus `json:"group,omitempty"`
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
	if globalStatus.Group != nil {
		g := *globalStatus.Group
		snap.Group = &g
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
		s.Group = nil
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
        .group-banner { background: #eef4ff; border: 1px solid #c5d6f5; border-radius: 6px; padding: 10px 14px; margin-bottom: 14px; color: #223; }
        .group-banner .dim { color: #667; }
        .sbadge { display: inline-block; padding: 1px 8px; border-radius: 10px; font-size: 12px; font-weight: 600; background: #eee; color: #555; }
        .sbadge.ok { background: #e3f4e5; color: #2e7d32; }
        .sbadge.failed, .sbadge.invalid { background: #fde3e1; color: #c62828; }
        .sbadge.restoring { background: #fff1d6; color: #b26a00; }
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
        <div id="group-banner"></div>
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

        // setHTML replaces an element's content only when it changed. The
        // page polls every 2 seconds; rebuilding identical markup would still
        // collapse what the user expanded, reset scroll positions and drop a
        // text selection or a hovered tooltip.
        function setHTML(el, html) {
            if (el._html === html) return;
            el._html = html;
            el.innerHTML = html;
        }
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
        function configHTML(c, scope) {
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
            // Where the disk sits and what it hangs off.
            const sl = c.slot;
            const slot = sl ? esc(sl.name) + ' <span class="dim">(' + esc([sl.source, sl.detail].filter(Boolean).join(', ')) + ')</span>' : '';
            const ct = c.controller;
            let ctrl = '';
            if (ct) {
                ctrl = esc(ct.kind) + (ct.name ? ' &middot; ' + esc(ct.name) : '');
                const bits = [ct.driver, ct.pci, ct.ids, ct.firmware ? 'fw ' + ct.firmware : '', ct.bios ? 'BIOS ' + ct.bios : '',
                              ct.sas_address ? 'SAS ' + ct.sas_address : ''].filter(Boolean);
                if (bits.length) ctrl += '<br><span class="dim">' + esc(bits.join(' \u00b7 ')) + '</span>';
                if (ct.mode === 'RAID virtual drive') {
                    ctrl += '<br>' + sp('warn', 'RAID virtual drive: the physical disks behind it are hidden');
                } else if (ct.mode) {
                    ctrl += ' <span class="dim">(' + esc(ct.mode) + ')</span>';
                }
            }
            const q = c.queue;
            return '<div class="cfg">' + row('Interface', esc(c.interface || 'unknown')) + row('Slot', slot) + row('Controller', ctrl) + row('Link', link) +
                row('Power', power) + row('Write cache', wc) + row('SMART', smart) +
                row('Queue', queueHTML(q)) + row('md', q ? mdHTML(q.md) : '') + row('dm_mod', q ? dmModHTML(q.dm_mod, scope) : '') + '</div>';
        }

        // The request queue read from sysfs: blk-mq or bio-based, hardware
        // queues, active scheduler, then the tunables. '' when unknown.
        function queueMode(q) {
            const n = q.nr_hw_queues || 0;
            let s = q.mq ? 'blk-mq: yes, ' + n + ' hardware queue' + (n === 1 ? '' : 's') : 'blk-mq: no (bio-based)';
            if (q.scheduler) s += ', scheduler ' + q.scheduler;
            return s;
        }
        function queueHTML(q) {
            if (!q) return '';
            const bits = [
                q.schedulers && q.schedulers.length > 1 ? 'schedulers ' + q.schedulers.join(' ') : '',
                q.nr_requests ? 'nr_requests ' + q.nr_requests : '',
                q.rotational ? 'rotational ' + q.rotational : '',
                q.read_ahead_kb ? 'read_ahead_kb ' + q.read_ahead_kb : '',
                q.max_sectors_kb ? 'max_sectors_kb ' + q.max_sectors_kb : '',
                q.wbt_lat_usec ? 'wbt_lat_usec ' + q.wbt_lat_usec : '',
                q.rq_affinity ? 'rq_affinity ' + q.rq_affinity : '',
                q.nomerges ? 'nomerges ' + q.nomerges : '',
                q.io_poll ? 'io_poll ' + q.io_poll : '',
                q.device_queue_depth ? 'queue_depth ' + q.device_queue_depth : '',
                q.nvme_queue_count ? 'nvme queue_count ' + q.nvme_queue_count : '',
                q.nvme_sqsize ? 'nvme sqsize ' + q.nvme_sqsize : ''
            ].filter(Boolean);
            return esc(queueMode(q)) + (bits.length ? '<br><span class="dim">' + esc(bits.join(' \u00b7 ')) + '</span>' : '');
        }
        // An md array's own configuration (an md node, or a dm-raid node
        // that shows its md state).
        function mdHTML(m) {
            if (!m) return '';
            return esc([m.level, m.raid_disks ? m.raid_disks + ' disks' : '', m.chunk_size ? 'chunk ' + m.chunk_size : '',
                m.stripe_cache_size ? 'stripe cache ' + m.stripe_cache_size : '', m.sync_action ? 'sync ' + m.sync_action : '',
                m.bitmap ? 'bitmap ' + m.bitmap : ''].filter(Boolean).join(' \u00b7 '));
        }
        // The device-mapper core module's queue parameters; one this kernel
        // does not have is said so, never hidden. Every parameter is in the
        // collapsed list.
        function dmModHTML(d, scope) {
            if (!d) return '';
            if (!d.loaded) return '<span class="dim">not loaded</span>';
            const v = k => d[k] ? esc(d[k]) : '<span class="dim">n/a (not on this kernel)</span>';
            let h = ['use_blk_mq', 'dm_mq_nr_hw_queues', 'dm_mq_queue_depth'].map(k => k + ' ' + v(k)).join(' &middot; ');
            const ks = Object.keys(d.params || {}).sort();
            if (ks.length) {
                const key = 'dmmod:' + (scope || '');
                h += '<details data-key="' + esc(key) + '"' + (openKeys.has(key) ? ' open' : '') + '><summary class="dim">all dm_mod parameters (' + ks.length + ')</summary>' +
                    ks.map(k => esc(k) + ' = ' + esc(d.params[k])).join('<br>') + '</details>';
            }
            return h;
        }

        // The member disks of an array target, one row each: model, serial,
        // firmware, role, then (once the member's configuration has been read)
        // interface, physical slot with who reported it, link speed and SMART
        // health. '' for a drive.
        function membersHTML(drive) {
            const ms = drive.members || [];
            if (ms.length === 0) return '';
            const rows = ms.map(m => {
                const c = m.config || {};
                const bits = [esc(m.model || ''), m.serial ? 'S/N ' + esc(m.serial) : '', c.firmware ? 'fw ' + esc(c.firmware) : '', esc(m.role || '')];
                if (c.interface) bits.push(esc(c.interface));
                const sl = c.slot;
                if (sl && sl.name) bits.push('slot ' + esc(sl.name) + (sl.source ? ' <span class="dim">(' + esc(sl.source) + ')</span>' : ''));
                if (c.link_speed) {
                    bits.push('link ' + esc(c.link_speed) + (c.link_max ? ' <span class="dim">(max ' + esc(c.link_max) + ')</span>' : '') +
                        (c.link_below_max ? ' <span class="warn">below max</span>' : ''));
                }
                if (c.queue) bits.push('queue ' + esc(queueMode(c.queue)));
                const st = c.smart && c.smart.status;
                if (st) bits.push('SMART <span class="' + (st === 'PASSED' ? 'ok' : 'bad') + '">' + esc(st) + '</span>');
                return '<div class="row"><span class="k">' + esc(m.name) + '</span><span class="v">' +
                    bits.filter(Boolean).join(' &middot; ') + '</span></div>';
            }).join('');
            return '<div class="cfg"><div class="dim">Members</div>' + rows + '</div>';
        }

        // ---- Run groups: banner, per-drive setting badges, whole-group ETA ----

        function badgeHTML(cls, text, tip) {
            return '<span class="sbadge ' + cls + '"' + (tip ? ' title="' + esc(tip) + '"' : '') + '>' + esc(text) + '</span>';
        }

        // What one reading says about the expected value: pending until it is
        // read, ok when it matches, failed otherwise; the reading (and what
        // the kernel believes) is in the tooltip.
        function readingBadge(label, reading, want) {
            if (!reading) return badgeHTML('pending', label + ' pending', label + ': not read yet');
            const tip = label + ': read ' + reading.value + ', expected ' + want +
                (reading.kernel ? ', kernel ' + reading.kernel : '');
            const ok = reading.value === want;
            return badgeHTML(ok ? 'ok' : 'failed', label + ' ' + (ok ? 'ok' : 'failed'), tip);
        }

        // A drive card's setting line: the value and the before/after badges,
        // invalid when the run did not verify, restoring while the group
        // restores. Empty outside a run group. The restore outcome is not
        // known here (the group page shows it).
        function settingBadgesHTML(drive, group) {
            const st = drive.setting;
            if (!st) return '';
            let h = '<div class="drive-detail setting">Setting: <b>' + esc(st.value) + '</b> ' +
                readingBadge('before', st.before, st.value) + ' ' + readingBadge('after', st.after, st.value);
            if (st.valid === false) h += ' ' + badgeHTML('invalid', 'invalid', 'this drive did not hold the value for the run');
            if (group && group.step === 'restoring') h += ' ' + badgeHTML('restoring', 'restoring', 'the original setting is being restored');
            if (group && group.step === 'tearing-down') h += ' ' + badgeHTML('restoring', 'tearing down', 'the layout is being torn down');
            return h + '</div>';
        }

        // Expected length of one full run on a drive, in seconds; null when a
        // phase has no known length.
        function runLengthOf(phases) {
            if (!phases || phases.length === 0) return null;
            let sec = 0;
            for (const p of phases) {
                if (!p.deterministic) return null;
                sec += p.duration_sec;
            }
            return sec;
        }

        // ETA of the whole group: what is left of the current run (all of it
        // before the run starts) plus the runs still to do at the expected
        // length of one full run. Drives run side by side, so the longest
        // drive counts.
        function groupEtaText(group, drives, phasesMap) {
            const notStarted = group.step === 'applying' || group.step === 'verifying-before' ||
                group.step === 'building' || group.step === 'resyncing';
            const after = group.step === 'verifying-after' || group.step === 'restoring' || group.step === 'done' ||
                group.step === 'tearing-down';
            const runsLeft = Math.max(0, group.runs - group.run);
            let sec = 0, unknownPhases = 0, anyLeft = runsLeft > 0;
            drives.forEach(d => {
                const phases = phasesMap[d.name] || [];
                const full = runLengthOf(phases);
                // The runs still to do repeat the unknown-length phases too.
                if (full == null) unknownPhases += runsLeft * phases.filter(p => !p.deterministic).length;
                if (full != null) {
                    sec = Math.max(sec, runsLeft * full);
                }
                if (after) return;
                if (notStarted) {
                    if (full != null) sec = Math.max(sec, runsLeft * full + full);
                    else unknownPhases += phases.filter(p => !p.deterministic).length;
                    anyLeft = anyLeft || phases.length > 0;
                    return;
                }
                const e = etaOf(d, phases);
                unknownPhases += e.unknown;
                if (e.left > 0) anyLeft = true;
                if (full != null) sec = Math.max(sec, runsLeft * full + e.sec);
                else sec = Math.max(sec, e.sec);
            });
            if (!anyLeft) return 'all done';
            if (unknownPhases > 0) {
                return (sec > 0 ? 'ETA ~' + fmtDur(sec) + ' + ' : 'ETA unknown: ') + unknownPhases + ' phase' +
                    (unknownPhases > 1 ? 's' : '') + ' of unknown length';
            }
            if (sec <= 0) return 'ETA unknown';
            return 'ETA ~' + fmtDur(sec) + ' (about ' +
                new Date(Date.now() + sec * 1000).toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'}) + ')';
        }

        // What the group is doing: the layout steps in words (with the resync
        // progress), the other steps by name.
        function groupStepText(group) {
            switch (group.step) {
            case 'building': return 'building the layout';
            case 'resyncing': return 'resyncing ' + Math.round(group.progress || 0) + '%';
            case 'tearing-down': return 'tearing down';
            }
            return group.step;
        }

        // The banner above the drives; empty (so the page is as before)
        // outside a run group. A layout group names the run's layout; its ETA
        // leaves the resyncs out (their length is not known).
        function groupBannerHTML(group, drives, phasesMap) {
            if (!group) return '';
            let eta = groupEtaText(group, drives || [], phasesMap || {});
            if (group.variant && eta !== 'all done') eta += ' (resync time not included)';
            return '<div class="group-banner">Setting test: <b>' + esc(group.setting) + '</b>, run ' +
                esc(group.run) + ' of ' + esc(group.runs) +
                (group.variant ? ' (layout <b>' + esc(group.variant) + '</b>)' : '') + ', ' + esc(groupStepText(group)) +
                ' <span class="dim">&middot; strategy: ' + esc(group.strategy) + ' &middot; ' +
                esc(eta) + '</span></div>';
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
            setHTML(document.getElementById('group-banner'), groupBannerHTML(status.group, status.drives, phasesByDrive));
            const lastUpdate = document.getElementById('last-update');

            // Update status badge
            if (status.running) {
                setHTML(statusInfo, '<span class="status-badge status-running">Running</span>');
                progressContainer.style.display = 'block';

                // Update progress
                const progress = status.total_phases > 0
                    ? Math.round((status.completed_phases / status.total_phases) * 100)
                    : 0;
                progressFill.style.width = progress + '%';
                progressText.textContent = progress + '%';

                // Show benchmark details
                setHTML(benchmarkDetails,
                    '<div class="drive-detail">Mode: ' + esc(status.mode) + '</div>' +
                    '<div class="drive-detail">Phase: ' + (status.current_phase + 1) + ' / ' + status.total_phases + '</div>' +
                    '<div class="drive-detail">Completed: ' + status.completed_phases + ' / ' + status.total_phases + '</div>');
            } else {
                setHTML(statusInfo, '<span class="status-badge status-idle">Idle</span>');
                progressContainer.style.display = 'none';
                setHTML(benchmarkDetails, '');
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
                        '<div class="drive-name">' + esc(drive.name) + ' (' + esc(drive.type || 'Unknown') + ')' +
                        (drive.kind === 'array' ? ' &middot; array' : '') + '</div>' +
                        '<div class="drive-detail">' + esc(drive.vendor) + ' ' + esc(drive.model) + '</div>';
                    if (drive.serial) {
                        html += '<div class="drive-detail">S/N: ' + esc(drive.serial) + '</div>';
                    }
                    if (drive.capacity_gb) {
                        html += '<div class="drive-detail">Capacity: ' + drive.capacity_gb + ' GB</div>';
                    }
                    html += settingBadgesHTML(drive, status.group);
                    // An array shows its own node (queue, md/dm state) once
                    // read, then its members' configuration.
                    html += drive.kind === 'array' ? (drive.config ? configHTML(drive.config, drive.name) : '') + membersHTML(drive) : configHTML(drive.config, drive.name);
                    html += '<div class="drive-detail" style="color: ' + statusColor + '; font-weight: 600;">Status: ' + statusText + '</div>';
                    if (drive.phase_name && drive.status === 'running') {
                        html += '<div class="drive-detail">Phase: ' + esc(drive.phase_name) + '</div>';
                    }
                    if (drive.warning) {
                        html += '<div class="drive-detail" style="color: #b26a00; white-space: pre-line; text-align: left;">&#9888; ' + esc(drive.warning) + '</div>';
                    }
                    if (drive.skipped) {
                        html += '<div class="drive-detail" style="color: #b26a00;">' + esc(drive.skipped) + '</div>';
                    }
                    if (drive.error) {
                        html += '<div class="drive-detail" style="color: #f44336;">Error: ' + esc(drive.error) + '</div>';
                    }
                    html += phasesHTML(drive);
                    html += '</div>';
                });
                html += '</div>';
                setHTML(drivesList, html);
                loadPhases(sortedDrives);
            } else {
                setHTML(drivesList, '<div class="no-data">No active benchmarks</div>');
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
