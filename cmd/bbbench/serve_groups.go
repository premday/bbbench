package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"bbbench/internal/rungroup"
)

// GroupStore holds the run groups known to the web server, loaded from the
// output directory and registered live while a group runs.
type GroupStore struct {
	mu     sync.RWMutex
	groups map[string]*rungroup.GroupFile
}

var globalGroupStore = &GroupStore{groups: make(map[string]*rungroup.GroupFile)}

// AddGroup adds or replaces a group. The copy is shallow: only Runs and each
// run's Drives get their own slices, since those are what the group loop
// changes in place. Everything else (Values, Plan, Originals, Restore.Failed,
// the labels' Before/After readings) is shared with the caller, which is safe
// only because the group loop never changes them in place after the first
// save: it assigns new values (a new Restore, new readings, a new Drives
// slice per record). Changing them in place would race the handlers.
func (s *GroupStore) AddGroup(g *rungroup.GroupFile) {
	if g == nil {
		return
	}
	cp := *g
	cp.Runs = make([]rungroup.GroupRun, len(g.Runs))
	for i, r := range g.Runs {
		r.Drives = append([]rungroup.GroupDrive(nil), r.Drives...)
		cp.Runs[i] = r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[cp.ID] = &cp
}

// GetGroup returns the group with the given id.
func (s *GroupStore) GetGroup(id string) (*rungroup.GroupFile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	return g, ok
}

// ListGroups returns all groups, newest first.
func (s *GroupStore) ListGroups() []*rungroup.GroupFile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*rungroup.GroupFile, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// Clear removes all groups.
func (s *GroupStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups = make(map[string]*rungroup.GroupFile)
}

// loadGroups loads the group files of outputDir into the store.
func loadGroups(outputDir string) error {
	groups, err := rungroup.LoadGroups(outputDir)
	if err != nil {
		return err
	}
	for _, g := range groups {
		globalGroupStore.AddGroup(g)
	}
	return nil
}

// groupFromRequest resolves ?id= to a group, answering 400 or 404 itself.
func groupFromRequest(w http.ResponseWriter, r *http.Request) (*rungroup.GroupFile, bool) {
	return groupByID(w, r.URL.Query().Get("id"))
}

// groupByID looks a group up, answering 400 (empty id) or 404 itself.
func groupByID(w http.ResponseWriter, id string) (*rungroup.GroupFile, bool) {
	if id == "" {
		http.Error(w, "Missing group ID", http.StatusBadRequest)
		return nil, false
	}
	g, ok := globalGroupStore.GetGroup(id)
	if !ok {
		http.Error(w, "Group not found", http.StatusNotFound)
		return nil, false
	}
	return g, true
}

// chartSource resolves the data the graphs and compare pages read:
// ?group=<id> (the member runs of a group together) or ?id=<run id>. It
// answers 400 or 404 itself. query is the page's own query string, for links
// between the two pages.
func chartSource(w http.ResponseWriter, r *http.Request) (dataURL, query string, ok bool) {
	q := r.URL.Query()
	if q.Has("group") {
		g, ok := groupByID(w, q.Get("group"))
		if !ok {
			return "", "", false
		}
		return "/api/groups/graphs?id=" + url.QueryEscape(g.ID), "group=" + url.QueryEscape(g.ID), true
	}
	id := q.Get("id")
	if id == "" {
		http.Error(w, "Missing result ID", http.StatusBadRequest)
		return "", "", false
	}
	if _, ok := globalResultsStore.GetResult(id); !ok {
		http.Error(w, "Result not found", http.StatusNotFound)
		return "", "", false
	}
	return "/api/results/graphs?id=" + url.QueryEscape(id), "id=" + url.QueryEscape(id), true
}

// jsString is s as a JavaScript string literal that is safe inside a
// <script> element (json.Marshal escapes <, > and &).
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		if logger != nil {
			logger.Error("encode json", "err", err)
		}
	}
}

// handleGroupsList serves /api/groups.
func handleGroupsList(w http.ResponseWriter, r *http.Request) {
	groups := globalGroupStore.ListGroups()
	out := make([]map[string]any, len(groups))
	for i, g := range groups {
		counts := map[string]int{}
		for _, run := range g.Runs {
			counts[string(run.Status)]++
		}
		out[i] = map[string]any{
			"id":            g.ID,
			"created":       g.Created,
			"setting":       g.Setting,
			"strategy":      g.Strategy,
			"values":        g.Values,
			"runs":          len(g.Runs),
			"status_counts": counts,
		}
	}
	writeJSON(w, out)
}

// runKernel is the kernel's view of the setting per device in a run, as read
// back after the run (before it when there is no later reading).
func runKernel(run rungroup.GroupRun) map[string]string {
	k := map[string]string{}
	for _, d := range run.Drives {
		switch {
		case d.Label.After != nil && d.Label.After.Kernel != "":
			k[d.Device] = d.Label.After.Kernel
		case d.Label.Before != nil && d.Label.Before.Kernel != "":
			k[d.Device] = d.Label.Before.Kernel
		}
	}
	return k
}

// runFlushNote is true when any phase of the member run carries a warning
// (for example write through: no Flush reaches the drive), so missing Flush
// data on the page can be explained.
func runFlushNote(res *BenchmarkResult) bool {
	if res == nil {
		return false
	}
	for _, p := range res.Phases {
		if len(p.Warnings) > 0 {
			return true
		}
	}
	return false
}

// groupEntries is the union of generateDiskPhaseData over the member runs;
// runs that did not run contribute nothing. Labelled entries carry the
// run's round (1-based). The entries of an interrupted run are kept but
// invalid, whatever its stored labels say (the setting was not verified
// after it), so comparisons leave them out.
func groupEntries(g *rungroup.GroupFile) []DiskPhaseData {
	out := []DiskPhaseData{}
	for _, run := range g.Runs {
		if run.Status == rungroup.RunNotRun {
			continue
		}
		res, ok := globalResultsStore.GetResult(run.RunID)
		if !ok {
			continue
		}
		for _, e := range generateDiskPhaseData(res) {
			if e.SettingValue != "" {
				e.Round = run.Round + 1
				if run.Status == rungroup.RunInterrupted {
					e.Invalid = true
				}
			}
			out = append(out, e)
		}
	}
	return out
}

// Stat is mean, min and max of one metric.
type Stat struct {
	Mean float64 `json:"mean"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

// MatrixRow aggregates the valid results of one phase under one value.
type MatrixRow struct {
	PhaseNum  int    `json:"phase_num"`
	PhaseName string `json:"phase_name"`
	Value     string `json:"value"`
	N         int    `json:"n"`
	// Incomplete: the value has no valid result for the phase.
	Incomplete bool `json:"incomplete"`
	IOPS       Stat `json:"iops"` // read + write
	BWMbps     Stat `json:"bw_mbps"`
}

// groupMatrix aggregates entries per (phase, value) over the valid ones
// (Invalid entries and unlabelled ones with no value do not count). Every
// phase seen gets a row for each of values, marked incomplete when the value
// has no valid result there (also when all its runs were not run). Rows come
// by phase, then in the order of values (other values after them, in order of
// first appearance).
func groupMatrix(entries []DiskPhaseData, values []string) []MatrixRow {
	type key struct {
		phase int
		name  string
		value string
	}
	idx := map[key]int{}
	var rows []MatrixRow
	sums := map[int][2]float64{}
	for _, e := range entries {
		if e.SettingValue == "" || e.Skipped != "" {
			continue
		}
		k := key{e.PhaseNum, e.PhaseName, e.SettingValue}
		i, ok := idx[k]
		if !ok {
			i = len(rows)
			idx[k] = i
			rows = append(rows, MatrixRow{PhaseNum: e.PhaseNum, PhaseName: e.PhaseName, Value: e.SettingValue, Incomplete: true})
		}
		if e.Invalid {
			continue
		}
		iops := e.ReadIOPS + e.WriteIOPS
		bw := e.ReadBWMbps + e.WriteBWMbps
		r := &rows[i]
		s := sums[i]
		if r.N == 0 {
			r.IOPS.Min, r.IOPS.Max, r.BWMbps.Min, r.BWMbps.Max = iops, iops, bw, bw
		}
		r.N++
		r.Incomplete = false
		s[0] += iops
		s[1] += bw
		sums[i] = s
		r.IOPS.Min, r.IOPS.Max = min(r.IOPS.Min, iops), max(r.IOPS.Max, iops)
		r.BWMbps.Min, r.BWMbps.Max = min(r.BWMbps.Min, bw), max(r.BWMbps.Max, bw)
	}
	type phaseKey struct {
		num  int
		name string
	}
	var phases []phaseKey
	seen := map[phaseKey]bool{}
	for _, r := range rows {
		if p := (phaseKey{r.PhaseNum, r.PhaseName}); !seen[p] {
			seen[p] = true
			phases = append(phases, p)
		}
	}
	for _, p := range phases {
		for _, v := range values {
			k := key{p.num, p.name, v}
			if _, ok := idx[k]; !ok {
				idx[k] = len(rows)
				rows = append(rows, MatrixRow{PhaseNum: p.num, PhaseName: p.name, Value: v, Incomplete: true})
			}
		}
	}
	for i := range rows {
		if n := float64(rows[i].N); n > 0 {
			rows[i].IOPS.Mean = sums[i][0] / n
			rows[i].BWMbps.Mean = sums[i][1] / n
		}
	}
	order := map[string]int{}
	for i, v := range values {
		order[v] = i
	}
	rank := func(v string) int {
		if i, ok := order[v]; ok {
			return i
		}
		return len(values)
	}
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].PhaseNum != rows[b].PhaseNum {
			return rows[a].PhaseNum < rows[b].PhaseNum
		}
		return rank(rows[a].Value) < rank(rows[b].Value)
	})
	return rows
}

// handleGroupDetail serves /api/groups/detail: the group file, each run's
// headline and kernel reading, and the metric matrix.
func handleGroupDetail(w http.ResponseWriter, r *http.Request) {
	g, ok := groupFromRequest(w, r)
	if !ok {
		return
	}
	type runHeadline struct {
		ID        string `json:"id"`
		Timestamp any    `json:"timestamp"`
		Mode      string `json:"mode"`
		Drives    int    `json:"drives"`
		Phases    int    `json:"phases"`
	}
	type runOut struct {
		rungroup.GroupRun
		Kernel    map[string]string `json:"kernel"`
		FlushNote bool              `json:"flush_note"`
		Summary   *runHeadline      `json:"summary"`
	}
	runs := make([]runOut, len(g.Runs))
	for i, run := range g.Runs {
		ro := runOut{GroupRun: run, Kernel: runKernel(run)}
		if res, ok := globalResultsStore.GetResult(run.RunID); ok {
			ro.FlushNote = runFlushNote(res)
			ro.Summary = &runHeadline{ID: res.ID, Timestamp: res.Timestamp, Mode: res.Mode,
				Drives: len(res.Drives), Phases: len(res.Phases)}
		}
		runs[i] = ro
	}
	writeJSON(w, map[string]any{
		"id": g.ID, "created": g.Created, "host": g.Host, "plan": g.Plan,
		"setting": g.Setting, "strategy": g.Strategy, "values": g.Values,
		"originals": g.Originals, "restore": g.Restore,
		"runs": runs, "matrix": groupMatrix(groupEntries(g), g.Values),
	})
}

// handleGroupGraphs serves /api/groups/graphs: the data of the member runs
// together.
func handleGroupGraphs(w http.ResponseWriter, r *http.Request) {
	g, ok := groupFromRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, groupEntries(g))
}

// handleGroupPage serves /group?id=.
func handleGroupPage(w http.ResponseWriter, r *http.Request) {
	g, ok := groupFromRequest(w, r)
	if !ok {
		return
	}
	html := groupPageHTML
	html = strings.ReplaceAll(html, "__NAVBAR_CSS__", navbarCSS)
	html = strings.ReplaceAll(html, "__NAVBAR_HTML__", navbarHTML("/results"))
	html = strings.ReplaceAll(html, "__GROUP_ID__", escapeHTML(g.ID))
	html = strings.ReplaceAll(html, "__GROUP_Q__", escapeHTML(url.QueryEscape(g.ID)))
	html = strings.ReplaceAll(html, "__DETAIL_URL__", escapeHTML("/api/groups/detail?id="+url.QueryEscape(g.ID)))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

const groupPageHTML = `<!DOCTYPE html>
<html>
<head>
    <title>bbbench - Run group</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Arial, sans-serif; background: #f1f5f9; color: #1e293b; }
        __NAVBAR_CSS__
        .wrap { max-width: 1200px; margin: 0 auto; padding: 20px; display: flex; flex-direction: column; gap: 16px; }
        .card { background: #fff; border: 1px solid #e2e8f0; border-radius: 8px; padding: 16px; }
        h1 { font-size: 20px; } h2 { font-size: 15px; margin-bottom: 10px; }
        .meta { display: grid; grid-template-columns: max-content 1fr; gap: 4px 14px; font-size: 13px; }
        .meta dt { color: #64748b; }
        table { border-collapse: collapse; width: 100%; font-size: 12px; }
        th, td { text-align: left; padding: 5px 8px; border-bottom: 1px solid #f1f5f9; vertical-align: top; }
        th { color: #64748b; font-size: 11px; text-transform: uppercase; }
        .ok { color: #15803d; font-weight: 600; } .bad { color: #b91c1c; font-weight: 600; } .dim { color: #94a3b8; }
        .note { color: #b45309; font-size: 11px; margin-top: 3px; }
        .reading { font-family: monospace; }
        .incomplete td { color: #b45309; }
        .links { margin: 6px 0 10px; font-size: 13px; display: flex; gap: 12px; }
        .links a { color: #2563eb; text-decoration: none; }
    </style>
</head>
<body>
__NAVBAR_HTML__
<div class="wrap" id="app" data-detail="__DETAIL_URL__">
    <div class="card"><h1>Run group __GROUP_ID__</h1>
        <div class="links"><a href="/graphs?group=__GROUP_Q__">Graphs</a> <a href="/compare?group=__GROUP_Q__">Compare</a></div>
        <dl class="meta" id="meta"></dl></div>
    <div class="card"><h2>Runs and drives</h2><div id="runs">Loading...</div></div>
    <div class="card"><h2>Restore</h2><div id="restore"></div></div>
    <div class="card"><h2>Metrics per phase and value</h2><div id="matrix"></div></div>
</div>
<script>
function esc(s) {
    return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
        .replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
function fmtNum(x) {
    if (x == null || isNaN(x)) return '-';
    return Math.abs(x) >= 100 ? Math.round(x).toLocaleString() : (Math.round(x * 10) / 10).toString();
}
function fmtReading(r) {
    if (!r) return '<span class="dim">-</span>';
    var s = '<span class="reading">' + esc(r.value) + '</span>';
    if (r.kernel) s += ' <span class="dim">(kernel: ' + esc(r.kernel) + ')</span>';
    return s;
}
function fmtMeta(g) {
    var rows = [['Setting', g.setting], ['Values', (g.values || []).join(', ')],
        ['Strategy', g.strategy], ['Host', g.host], ['Plan', (g.plan || []).join(', ')],
        ['Created', g.created]];
    return rows.map(function (r) { return '<dt>' + esc(r[0]) + '</dt><dd>' + esc(r[1]) + '</dd>'; }).join('');
}
function runValid(run, d) { return run.status !== 'interrupted' && d.label && d.label.valid; }
function renderRuns(g) {
    var rows = [];
    (g.runs || []).forEach(function (run, i) {
        var drives = run.drives || [];
        if (!drives.length) {
            rows.push('<tr><td>' + (i + 1) + '</td><td colspan="6" class="dim">' + esc(run.status) + '</td></tr>');
            return;
        }
        drives.forEach(function (d, j) {
            var l = d.label || {};
            var link = (run.summary && j === 0)
                ? '<a href="/result?id=' + encodeURIComponent(run.run_id) + '">' + esc(run.run_id) + '</a>' : (j === 0 ? esc(run.run_id) : '');
            var valid = runValid(run, d) ? '<span class="ok">valid</span>' : '<span class="bad">invalid</span>';
            var kernel = (run.kernel && run.kernel[d.device]) || '';
            var note = '';
            if (l.note) note += '<div class="note">' + esc(l.note) + '</div>';
            if (run.flush_note) note += '<div class="note">This run had a write-through phase: the drive received no Flush, so Flush data is missing.</div>';
            rows.push('<tr><td>' + (j === 0 ? (i + 1) + ' (round ' + (run.round + 1) + ', ' + esc(run.status) + ')' : '') + '</td>' +
                '<td>' + link + '</td><td>' + esc(d.device) + '<div class="dim">' + esc(d.model) + ' ' + esc(d.serial) + '</div></td>' +
                '<td>' + esc(d.value) + '</td><td>' + fmtReading(l.before) + '</td><td>' + fmtReading(l.after) + note + '</td>' +
                '<td>' + valid + (kernel ? '<div class="dim">write_cache: ' + esc(kernel) + '</div>' : '') + '</td></tr>');
        });
    });
    return '<table><tr><th>Run</th><th>ID</th><th>Drive</th><th>Value</th><th>Before</th><th>After</th><th>Result</th></tr>' + rows.join('') + '</table>';
}
function renderRestore(g) {
    var r = g.restore || {};
    if (r.status === 'done') return '<span class="ok">Original values restored.</span>';
    if (r.status === 'failed') return '<span class="bad">Restore failed on: ' + esc((r.failed || []).join(', ')) + '</span>';
    return '<span class="dim">' + esc(r.status || 'pending') + '</span>';
}
function renderMatrix(rows) {
    if (!rows || !rows.length) return '<span class="dim">No results yet.</span>';
    var body = rows.map(function (m) {
        if (m.incomplete) {
            return '<tr class="incomplete"><td>' + esc(m.phase_name) + '</td><td>' + esc(m.value) +
                '</td><td colspan="3">incomplete: no valid result (n=0)</td></tr>';
        }
        return '<tr><td>' + esc(m.phase_name) + '</td><td>' + esc(m.value) + '</td>' +
            '<td>' + fmtNum(m.iops.mean) + ' (' + fmtNum(m.iops.min) + ' - ' + fmtNum(m.iops.max) + ')</td>' +
            '<td>' + fmtNum(m.bw_mbps.mean) + ' (' + fmtNum(m.bw_mbps.min) + ' - ' + fmtNum(m.bw_mbps.max) + ')</td>' +
            '<td>' + m.n + '</td></tr>';
    }).join('');
    return '<table><tr><th>Phase</th><th>Value</th><th>IOPS mean (min - max)</th><th>MB/s mean (min - max)</th><th>n</th></tr>' + body + '</table>';
}
function render(g) {
    document.getElementById('meta').innerHTML = fmtMeta(g);
    document.getElementById('runs').innerHTML = renderRuns(g);
    document.getElementById('restore').innerHTML = renderRestore(g);
    document.getElementById('matrix').innerHTML = renderMatrix(g.matrix);
}
function load() {
    fetch(document.getElementById('app').dataset.detail)
        .then(function (r) { return r.json(); })
        .then(function (g) {
            render(g);
            var running = (g.runs || []).some(function (r) { return r.status === 'running' || r.status === 'planned'; });
            if (running) setTimeout(load, 5000);
        })
        .catch(function (e) { document.getElementById('runs').textContent = 'Error loading group: ' + e; });
}
load();
</script>
</body>
</html>`
