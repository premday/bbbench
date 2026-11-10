package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

func makeTestResult() *BenchmarkResult {
	return &BenchmarkResult{
		ID:        "test-123",
		Timestamp: time.Now(),
		Drives: []DriveInfo{
			{Device: blockdev.Device{Name: "sda", Vendor: "ACME", Model: "Disk1000"}},
			{Device: blockdev.Device{Name: "sdb", Vendor: "ACME", Model: "Disk1000"}},
		},
		Phases: []PhaseResult{
			{
				PhaseNumber: 1,
				PhaseName:   "seq-read",
				Jobs: []JobResult{
					{Device: "sda", ReadStats: &IOStats{IOPS: 50000, Bandwidth: 204800, AvgLatNS: 10000}},
					{Device: "sdb", ReadStats: &IOStats{IOPS: 48000, Bandwidth: 196608, AvgLatNS: 12000}},
				},
			},
			{
				PhaseNumber: 2,
				PhaseName:   "seq-write",
				Jobs: []JobResult{
					{Device: "sda", WriteStats: &IOStats{IOPS: 40000, Bandwidth: 163840, AvgLatNS: 15000}},
					{Device: "sdb", WriteStats: &IOStats{IOPS: 38000, Bandwidth: 155648, AvgLatNS: 18000}},
				},
			},
		},
	}
}

func TestGenerateDiskGraphs(t *testing.T) {
	result := makeTestResult()
	graphs := generateDiskGraphs(result)

	if len(graphs) != 2 {
		t.Fatalf("graphs length = %d, want 2 (one per disk)", len(graphs))
	}

	sda := graphs[0]
	if sda.Disk != "sda" {
		t.Errorf("disk = %q, want sda", sda.Disk)
	}
	if len(sda.Phases) != 2 {
		t.Fatalf("phases = %d, want 2", len(sda.Phases))
	}

	p0 := sda.Phases[0]
	if !p0.HasRead {
		t.Error("phase 0 should have read data")
	}
	if p0.ReadIOPS != 50000 {
		t.Errorf("ReadIOPS = %f, want 50000", p0.ReadIOPS)
	}
	if p0.HasWrite {
		t.Error("phase 0 should not have write data")
	}

	p1 := sda.Phases[1]
	if p1.HasRead {
		t.Error("phase 1 should not have read data")
	}
	if !p1.HasWrite {
		t.Error("phase 1 should have write data")
	}
	if p1.WriteIOPS != 40000 {
		t.Errorf("WriteIOPS = %f, want 40000", p1.WriteIOPS)
	}
}

func TestGenerateDiskGraphs_NoPhases(t *testing.T) {
	result := &BenchmarkResult{Phases: []PhaseResult{}}
	graphs := generateDiskGraphs(result)
	if len(graphs) != 0 {
		t.Errorf("expected 0 graphs for empty result, got %d", len(graphs))
	}
}

func TestGenerateDiskGraphs_NoDrives(t *testing.T) {
	result := &BenchmarkResult{
		Phases: []PhaseResult{
			{PhaseNumber: 1, Jobs: []JobResult{
				{Device: "sda", ReadStats: &IOStats{IOPS: 1000}},
			}},
		},
	}
	// No drives listed → disk order comes from drives slice, so no graphs
	graphs := generateDiskGraphs(result)
	if len(graphs) != 0 {
		t.Errorf("expected 0 graphs when drives list is empty, got %d", len(graphs))
	}
}

func TestHandleResultGraphs(t *testing.T) {
	result := makeTestResult()
	globalResultsStore.AddResult(result)
	defer globalResultsStore.Clear()

	req := httptest.NewRequest("GET", "/api/results/graphs?id=test-123", nil)
	w := httptest.NewRecorder()
	handleResultGraphs(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var graphs []DiskGraphData
	if err := json.NewDecoder(resp.Body).Decode(&graphs); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(graphs) == 0 {
		t.Error("expected at least one disk graph")
	}
}

func TestHandleResultGraphs_MissingID(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/results/graphs", nil)
	w := httptest.NewRecorder()
	handleResultGraphs(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Result().StatusCode)
	}
}

func TestHandleResultGraphs_NotFound(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/results/graphs?id=nonexistent", nil)
	w := httptest.NewRecorder()
	handleResultGraphs(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Result().StatusCode)
	}
}

func TestHandleResultsList(t *testing.T) {
	globalResultsStore.Clear()

	now := time.Now()
	results := []*BenchmarkResult{
		{
			ID: "1", Timestamp: now, Mode: "parallel-sync",
			Drives: []DriveInfo{{Device: blockdev.Device{Name: "sda"}}},
			Phases: []PhaseResult{{}, {}},
		},
		{
			ID: "2", Timestamp: now.Add(-1 * time.Hour), Mode: "sequential",
			Drives: []DriveInfo{{Device: blockdev.Device{Name: "nvme0n1"}}},
			Phases: []PhaseResult{{}},
		},
	}
	for _, r := range results {
		globalResultsStore.AddResult(r)
	}
	defer globalResultsStore.Clear()

	req := httptest.NewRequest("GET", "/api/results", nil)
	w := httptest.NewRecorder()
	handleResultsList(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var summaries []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&summaries); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(summaries) != 2 {
		t.Errorf("summaries length = %d, want 2", len(summaries))
	}
	if summaries[0]["id"] != "1" {
		t.Errorf("first result ID = %v, want 1", summaries[0]["id"])
	}
}

func TestHandleGraphsPage(t *testing.T) {
	result := &BenchmarkResult{
		ID:        "test-456",
		Timestamp: time.Now(),
		Phases:    []PhaseResult{{}},
	}
	globalResultsStore.AddResult(result)
	defer globalResultsStore.Clear()

	req := httptest.NewRequest("GET", "/graphs?id=test-456", nil)
	w := httptest.NewRecorder()
	handleGraphsPage(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	body := w.Body.String()
	if !contains(body, "chart.js") {
		t.Error("page should include Chart.js")
	}
	if !contains(body, "test-456") {
		t.Error("page should include result ID")
	}
}

func TestHandleGraphsPage_MissingID(t *testing.T) {
	req := httptest.NewRequest("GET", "/graphs", nil)
	w := httptest.NewRecorder()
	handleGraphsPage(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Result().StatusCode)
	}
}

func TestGenerateDiskPhaseData_Labels(t *testing.T) {
	r := makeTestResult()
	r.Labels = map[string]rungroup.Label{
		"sdb": {Setting: "write-cache", Value: "write through", Valid: false},
	}
	var sda, sdb int
	for _, d := range generateDiskPhaseData(r) {
		switch d.Disk {
		case "sdb":
			sdb++
			if d.Setting != "write-cache" || d.SettingValue != "write through" || d.RunID != "test-123" || !d.Invalid {
				t.Errorf("sdb entry = %+v", d)
			}
		case "sda":
			sda++
			if d.Setting != "" || d.SettingValue != "" || d.RunID != "" || d.Invalid {
				t.Errorf("unlabelled sda carries label data: %+v", d)
			}
		}
	}
	if sda == 0 || sdb == 0 {
		t.Fatalf("entries: sda=%d sdb=%d", sda, sdb)
	}
	r.Labels["sdb"] = rungroup.Label{Setting: "write-cache", Value: "write back", Valid: true}
	for _, d := range generateDiskPhaseData(r) {
		if d.Disk == "sdb" && d.Invalid {
			t.Errorf("valid label marked invalid: %+v", d)
		}
	}
}

func arrayDrives() []DriveInfo {
	return []DriveInfo{
		{Device: blockdev.Device{Name: "md127", Model: "raid1", Serial: "0a:0b"}, UUID: "0a:0b", Members: []rungroup.Member{
			{Name: "sdb", Serial: "S1", Model: "M", Role: settings.RoleData},
			{Name: "sdc", Serial: "S2", Model: "M", Role: settings.RoleData},
		}},
		{Device: blockdev.Device{Name: "sdd", Model: "M", Serial: "S3"}},
	}
}

func TestGenerateDiskPhaseData_ArrayMembers(t *testing.T) {
	r := &BenchmarkResult{
		ID: "x", Drives: arrayDrives(),
		Phases: []PhaseResult{{PhaseNumber: 1, PhaseName: "p", Jobs: []JobResult{
			{Device: "md127", ReadStats: &IOStats{IOPS: 1, Bandwidth: 1}},
			{Device: "sdd", ReadStats: &IOStats{IOPS: 1, Bandwidth: 1}},
		}}},
	}
	var md, sdd *DiskPhaseData
	for _, d := range generateDiskPhaseData(r) {
		d := d
		switch d.Disk {
		case "md127":
			md = &d
		case "sdd":
			sdd = &d
		}
	}
	if md == nil || sdd == nil {
		t.Fatalf("entries: md=%v sdd=%v", md, sdd)
	}
	if md.Kind != "array" || len(md.Members) != 2 || md.Members[0].Role != settings.RoleData || md.Model != "raid1" || md.Serial != "0a:0b" {
		t.Errorf("md127 = %+v", md)
	}
	if sdd.Kind != "" || sdd.Members != nil {
		t.Errorf("sdd = %+v", sdd)
	}
}

// graphsPageBody renders the graphs page for a stored result.
func graphsPageBody(t *testing.T) string {
	t.Helper()
	globalResultsStore.AddResult(&BenchmarkResult{ID: "members-page", Timestamp: time.Now(), Phases: []PhaseResult{{}}})
	defer globalResultsStore.Clear()
	w := httptest.NewRecorder()
	handleGraphsPage(w, httptest.NewRequest("GET", "/graphs?id=members-page", nil))
	return w.Body.String()
}

// jsFunction returns the source of the page function name, up to the next
// function or comment at the same indentation.
func jsFunction(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, "function "+name+"(")
	if start < 0 {
		t.Fatalf("page has no function %s", name)
	}
	rest := body[start:]
	end := len(rest)
	for _, next := range []string{"\n    function ", "\n    //"} {
		if i := strings.Index(rest, next); i >= 0 && i < end {
			end = i
		}
	}
	return rest[:end]
}

func TestGraphsPageStacksMembers(t *testing.T) {
	body := graphsPageBody(t)
	// Read and write stack separately: Chart.js adds a 0 to every earlier
	// value of its stack, whatever their sign.
	for _, want := range []string{"'members'", "'members-write'", "stack = 'array'", "y: { stacked: stacked,",
		"mode: 'index', intersect: false", "Stack total", "'Array '", "ts_members", "fill: first ? 'origin' : '-1'"} {
		if !strings.Contains(body, want) {
			t.Errorf("graphs page lacks %q", want)
		}
	}
	// The array line is not filled, so it is drawn over the stack, not into it.
	arr := jsFunction(t, body, "arrayDatasets")
	if !strings.Contains(arr, "fill: false") {
		t.Errorf("array datasets are filled:\n%s", arr)
	}
}

func TestGraphsPageLatencyNotStacked(t *testing.T) {
	body := graphsPageBody(t)
	lat := jsFunction(t, body, "memberLatencyDatasets")
	if strings.Contains(lat, "stack") {
		t.Errorf("member latency datasets are stacked:\n%s", lat)
	}
	if !strings.Contains(lat, "fill: false") {
		t.Errorf("member latency lines are filled:\n%s", lat)
	}
	// The scale stacks only for IOPS and bandwidth.
	if !strings.Contains(body, "metric.key !== 'lat'") {
		t.Error("page does not keep latency off the stacked scale")
	}
}

func TestGraphsPageNoMembersUnchanged(t *testing.T) {
	body := graphsPageBody(t)
	// The plain Read/Write series of a drive, as before.
	for _, want := range []string{
		"const readPts  = ts.filter(p => p.r > 0).map(p => ({x: Math.round(p.t + shift), y:  p.r}));",
		"const writePts = ts.filter(p => p.w > 0).map(p => ({x: Math.round(p.t + shift), y: -p.w}));",
		"label: 'Read',  data: readPts,",
		"label: 'Write', data: writePts,",
		"if (memberMetric(d, metric)) return makeMembersChart(",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("graphs page lacks %q", want)
		}
	}
	// Only phases with members take the member path.
	mm := jsFunction(t, body, "memberMetric")
	if !strings.Contains(mm, "d.ts_members") {
		t.Errorf("memberMetric does not test ts_members:\n%s", mm)
	}
}

// memberGridJS is the node script TestGraphsPageMemberGrid runs over the page's
// own member chart functions.
const memberGridJS = `
function series(n, t0, step, jitter, f) {
    const out = [];
    for (let k = 0; k < n; k++) out.push({t: t0 + k * step + jitter(k), r: f(k), w: f(k) / 2});
    return out;
}
function check(name, d) {
    const metric = {key: 'iops', tsKey: 'ts_iops'};
    const g = memberGrid(d, metric);
    const bands = stackedMemberDatasets(d, g, {sdb: [0, 0, 0], sdc: [0, 0, 0]});
    const reads = bands.filter(b => b.dir === 'r'), writes = bands.filter(b => b.dir === 'w');
    const fail = m => { throw new Error(name + ': ' + m); };
    // No window merged: every member and the array keep each of their points.
    Object.keys(d.ts_members).forEach(n => {
        const drawn = reads.find(b => b.member === n).data.filter(p => p.y !== null).length;
        if (drawn !== d.ts_members[n].iops.length) fail(n + ' draws ' + drawn + ' of ' + d.ts_members[n].iops.length + ' windows');
    });
    const arr = arrayDatasets(g, true);
    const arrDrawn = arr.find(a => a.dir === 'r').data.filter(p => p.y !== null).length;
    if (arrDrawn !== d.ts_iops.length) fail('array draws ' + arrDrawn + ' of ' + d.ts_iops.length);
    // Stack total and Y scale = the drawn stack top (Chart.js draws a band
    // at the sum of the bands below it).
    let topR = 0, topW = 0;
    g.xs.forEach((x, i) => {
        const r = reads.reduce((s, b) => s + (b.data[i].y || 0), 0);
        const w = -writes.reduce((s, b) => s + (b.data[i].y || 0), 0);
        const t = g.totals[x] || {r: 0, w: 0};
        if (Math.abs(t.r - r) > 1e-9 || Math.abs(t.w - w) > 1e-9) fail('total at ' + x + ' = ' + JSON.stringify(t) + ', drawn ' + r + '/' + w);
        topR = Math.max(topR, r); topW = Math.max(topW, w);
    });
    const e = memberExtent(d, metric);
    if (e.r !== topR || e.w !== topW) fail('extent ' + JSON.stringify(e) + ', drawn top ' + topR + '/' + topW);
    return g;
}
const jit = k => [0.001, 0.003, 0.002, 0.004][k % 4];
// 0.5 s windows: two windows a second, each its own x.
let g = check('500ms', { ramp_sec: 2, log_avg_msec: 500,
    ts_iops: series(10, 0.5, 0.5, k => 0, k => 200 + k),
    ts_members: { sdb: {iops: series(10, 2, 0.5, jit, k => 100 + k)}, sdc: {iops: series(10, 2, 0.5, jit, k => 50 + 3 * k)} } });
if (g.xs.length !== 10 || g.xs[1] !== 2.5) throw new Error('500ms grid ' + JSON.stringify(g.xs));
// fio offset from the members by a quarter window.
check('offset', { ramp_sec: 2.25, log_avg_msec: 500,
    ts_iops: series(10, 0.5, 0.5, k => 0, k => 200 + k),
    ts_members: { sdb: {iops: series(10, 2, 0.5, jit, k => 100 + k)}, sdc: {iops: series(10, 2, 0.5, jit, k => 50 + 3 * k)} } });
// 1 s windows: x stays the whole second, as Math.round(t) gave.
g = check('1s', { ramp_sec: 2, log_avg_msec: 1000,
    ts_iops: series(10, 1, 1, k => 0, k => 200 + k),
    ts_members: { sdb: {iops: series(10, 2, 1, jit, k => 100 + k)}, sdc: {iops: series(10, 2, 1, jit, k => 50 + k)} } });
if (g.xs.join(',') !== '2,3,4,5,6,7,8,9,10,11') throw new Error('1s grid ' + g.xs.join(','));
console.log('OK');
`

// TestGraphsPageMemberGrid runs the page's member chart functions in node:
// sub-second windows are neither merged nor double counted, the stack totals
// and the Y extent equal the drawn stack top, 1 s windows keep whole-second x.
func TestGraphsPageMemberGrid(t *testing.T) {
	node := requireNode(t)
	body := graphsPageBody(t)
	var js strings.Builder
	for _, f := range []string{"rampOf", "windowOf", "fioShift", "memberNames", "memberStep", "memberKey",
		"memberGrid", "memberExtent", "memberLabel", "hasDir", "rgba", "stackedMemberDatasets", "arrayDatasets"} {
		js.WriteString(jsFunction(t, body, f))
		js.WriteString("\n")
	}
	js.WriteString(memberGridJS)
	cmd := exec.Command(node)
	cmd.Stdin = strings.NewReader(js.String())
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// runPageJS runs the named page functions plus script in node and expects it
// to print OK.
func runPageJS(t *testing.T, funcs []string, script string) {
	t.Helper()
	node := requireNode(t)
	body := graphsPageBody(t)
	var js strings.Builder
	for _, f := range funcs {
		js.WriteString(jsFunction(t, body, f))
		js.WriteString("\n")
	}
	js.WriteString(script)
	cmd := exec.Command(node)
	cmd.Stdin = strings.NewReader(js.String())
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// mdFuncs are the page functions of the md track and what they use.
var mdFuncs = []string{"windowOf", "memberStep", "memberKey", "mdMetric", "hasMD", "mdBusy",
	"mdActions", "mdBands", "mdBadges", "mdBusyNote", "mdIndex", "mdTipLines", "mdMarkers",
	"mdLabelLayout", "chartPlugins"}

const mdTestPrelude = `
const assert = require('assert');
const rampPlugin = {id: 'rampZone'}, mdPlugin = {id: 'mdTrack'};
const IOPS = {key: 'iops', tsKey: 'ts_iops'}, FLUSH = {key: 'flush', tsKey: 'ts_flush'};
function md(t, action, pct, speed, degraded) { return {t, action, pct, speed, degraded, state: 'clean'}; }
`

// TestGraphsPageMDBands: contiguous non-idle windows of one action form one
// band from the first window's start to the last window's end, labelled with
// the action; idle windows and an unreadable action between two windows of
// the same action do not split it; another action starts a new band.
func TestGraphsPageMDBands(t *testing.T) {
	runPageJS(t, mdFuncs, mdTestPrelude+`
const d = {log_avg_msec: 1000, ts_md: [
    md(0, 'idle', 0, 0, 0), md(1, 'resync', 43, 10240, 0), md(2, 'resync', 50, 10240, 0),
    md(3, '', 0, 0, 0), md(4, 'resync', 61, 20480, 0), md(5, 'idle', 0, 0, 0),
    md(6, 'check', 1, 1024, 0), md(7, 'recover', 5, 2048, 1), md(8, 'idle', 0, 0, 1)]};
const b = mdBands(d);
assert.deepStrictEqual(b.map(x => [x.action, x.x0, x.x1]), [['resync', 1, 5], ['check', 6, 7], ['recover', 7, 8]]);
// 0.5 s windows: a band ends half a second after its last window starts.
const h = mdBands({log_avg_msec: 500, ts_md: [md(2, 'check', 1, 1, 0), md(2.5, 'check', 2, 1, 0)]});
assert.deepStrictEqual(h.map(x => [x.action, x.x0, x.x1]), [['check', 2, 3]]);
assert.deepStrictEqual(mdBands({ts_md: [md(0, 'idle', 0, 0, 0)]}), []);
assert.deepStrictEqual(mdBands({}), []);
// Events: one marker per window, its texts joined; sorted by time.
const m = mdMarkers({md_events: [{t: 4, text: 'b'}, {t: 1, text: 'sync_action idle -> resync'}, {t: 4, text: 'c'}]});
assert.deepStrictEqual(m, [{t: 1, text: 'sync_action idle -> resync'}, {t: 4, text: 'b; c'}]);
// Labels stagger over 3 rows and are cut before the next label of their row.
const l = mdLabelLayout([{x: 10, w: 50}, {x: 20, w: 50}, {x: 30, w: 50}, {x: 40, w: 50}], 200);
assert.deepStrictEqual(l.map(x => x.row), [0, 1, 2, 0]);
assert.strictEqual(l[0].maxW, 26);
assert.strictEqual(l[3].maxW, 156);
console.log('OK');
`)
}

// TestGraphsPageMDBadge: one badge per action with its progress at the first
// and last window of the activity; a degraded array gets a degraded badge;
// the busy note shows for any non-idle window or any event; the hover line
// names the hovered window's state.
func TestGraphsPageMDBadge(t *testing.T) {
	runPageJS(t, mdFuncs, mdTestPrelude+`
const d = {log_avg_msec: 1000, ts_md: [
    md(0, 'idle', 0, 0, 0), md(1, 'resync', 43.4, 10240, 0), md(2, 'resync', 50, 10240, 0),
    md(3, 'idle', 0, 0, 0), md(4, 'resync', 61.2, 20480, 0), md(5, 'check', 0, 0, 0)]};
assert.deepStrictEqual(mdBadges(d), [{text: 'resync 43% -> 61%', kind: 'sync'}, {text: 'check', kind: 'sync'}]);
// pct 0 (total unknown) is not the first value; a drop is a restart: new badge.
const r = {ts_md: [md(0, 'check', 0, 0, 0), md(1, 'check', 20, 1, 0), md(2, 'check', 30, 1, 0),
    md(3, 'idle', 0, 0, 0), md(4, 'check', 2, 1, 0), md(5, 'check', 0, 1, 0), md(6, 'check', 9, 1, 0)]};
assert.deepStrictEqual(mdBadges(r).map(b => b.text), ['check 20% -> 30%', 'check 2% -> 9%']);
assert.deepStrictEqual(mdBadges({ts_md: [md(0, 'resync', 50, 1, 0)]}).map(b => b.text), ['resync 50%']);
assert.strictEqual(mdBusyNote(d), true);
// Degraded only: no sync badge.
const g = {ts_md: [md(0, 'idle', 0, 0, 0), md(1, 'idle', 0, 0, 1)], md_events: [{t: 1, text: 'degraded 0 -> 1'}]};
assert.deepStrictEqual(mdBadges(g), [{text: 'degraded', kind: 'degraded'}]);
assert.deepStrictEqual(mdBadges({ts_md: [md(0, 'idle', 0, 0, 2)]}), [{text: 'degraded 2', kind: 'degraded'}]);
assert.strictEqual(mdBusyNote(g), true);
// Idle all along, no event: nothing.
const q = {ts_md: [md(0, 'idle', 0, 0, 0), md(1, 'idle', 0, 0, 0)]};
assert.deepStrictEqual(mdBadges(q), []);
assert.strictEqual(mdBusyNote(q), false);
assert.strictEqual(mdBusyNote({md_events: [{t: 0, text: 'sdb: in_sync -> faulty'}]}), true);
// Hover: the md point of the window, keyed like the member grid.
const items = x => [{raw: {x}}];
assert.deepStrictEqual(mdTipLines(d, items(1)), ['md: resync 43% 10.0 MiB/s, degraded 0']);
assert.deepStrictEqual(mdTipLines(d, items(0)), ['md: idle, degraded 0']);
assert.deepStrictEqual(mdTipLines(d, items(9)), []);
// Events of the window are spelt out in full.
const e = {log_avg_msec: 1000, ts_md: [md(1, 'idle', 0, 0, 1)], md_events: [{t: 1.002, text: 'degraded 0 -> 1'}]};
assert.deepStrictEqual(mdTipLines(e, items(1)), ['md: idle, degraded 1', 'md event: degraded 0 -> 1']);
// Sub-second windows keep their own key.
const s = {log_avg_msec: 500, ts_md: [md(2, 'check', 10, 512, 0), md(2.5, 'check', 11, 512, 0)]};
assert.deepStrictEqual(mdTipLines(s, items(2.5)), ['md: check 11% 0.5 MiB/s, degraded 0']);
assert.strictEqual(mdMetric(d, IOPS), true);
assert.strictEqual(mdMetric(d, FLUSH), false);
console.log('OK');
`)
}

// TestGraphsPageNoMDUnchanged: without md data the charts get no md plugin,
// no badge, no note and no tooltip line, and the drawing code is the old one.
func TestGraphsPageNoMDUnchanged(t *testing.T) {
	runPageJS(t, mdFuncs, mdTestPrelude+`
const d = {log_avg_msec: 1000, ts_iops: [{t: 1, r: 1, w: 0}]};
assert.strictEqual(hasMD(d), false);
assert.strictEqual(mdMetric(d, IOPS), false);
assert.deepStrictEqual(chartPlugins(d, IOPS), [rampPlugin]);
assert.deepStrictEqual(mdBadges(d), []);
assert.strictEqual(mdBusyNote(d), false);
assert.deepStrictEqual(mdTipLines(d, [{raw: {x: 1}}]), []);
assert.deepStrictEqual(mdTipLines({ts_md: [], md_events: []}, [{raw: {x: 1}}]), []);
const m = {ts_md: [md(0, 'resync', 1, 1, 0)]};
assert.deepStrictEqual(chartPlugins(m, IOPS), [rampPlugin, mdPlugin]);
assert.deepStrictEqual(chartPlugins(m, FLUSH), [rampPlugin]);
console.log('OK');
`)
	body := graphsPageBody(t)
	for _, want := range []string{
		"const readPts  = ts.filter(p => p.r > 0).map(p => ({x: Math.round(p.t + shift), y:  p.r}));",
		"const writePts = ts.filter(p => p.w > 0).map(p => ({x: Math.round(p.t + shift), y: -p.w}));",
		"type: 'line', plugins: [rampPlugin],", // the Flush chart keeps its plugin list
		"array busy: results include administrative work",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("graphs page lacks %q", want)
		}
	}
	// The md header is only built for a phase with md data.
	card := jsFunction(t, body, "makeChartCard")
	if !strings.Contains(card, "mdHeader(") {
		t.Errorf("chart card does not add the md header:\n%s", card)
	}
	hdr := jsFunction(t, body, "mdHeader")
	if !strings.Contains(hdr, "if (!mdMetric(d, metric)) return null;") {
		t.Errorf("mdHeader does not bail out without md data:\n%s", hdr)
	}
}

// TestGraphsPageMDPluginDraw runs the page's own mdPlugin against a fake
// canvas: the light full-height tint is drawn before the datasets, the opaque
// strip at the top of the chart area and the action label (on a white box)
// after them, so stacked fills cannot hide the activity.
func TestGraphsPageMDPluginDraw(t *testing.T) {
	body := graphsPageBody(t)
	start := strings.Index(body, "const MD_BAND")
	end := strings.Index(body[start:], "\n    };\n")
	if start < 0 || end < 0 {
		t.Fatal("page has no mdPlugin")
	}
	plugin := body[start:start+end] + "\n    };\n"
	runPageJS(t, []string{"mdLabelLayout", "mdFit"}, plugin+`
const assert = require('assert');
function fakeChart() {
    const calls = [];
    const ctx = {fillStyle: '', strokeStyle: '', font: '', textBaseline: '', lineWidth: 1,
        save() {}, restore() {}, setLineDash() {}, beginPath() {}, moveTo() {}, lineTo() {}, stroke() { calls.push({op: 'stroke'}); },
        measureText(s) { return {width: s.length * 5}; },
        fillRect(x, y, w, h) { calls.push({op: 'rect', x, y, w, h, style: this.fillStyle}); },
        fillText(s, x, y) { calls.push({op: 'text', s, x, y, style: this.fillStyle}); }};
    return {calls, chart: {ctx, chartArea: {left: 0, right: 300, top: 20, bottom: 120},
        scales: {x: {getPixelForValue: v => v * 10}}}};
}
const alpha = s => { const m = /rgba\(.*,\s*([\d.]+)\)/.exec(s); return m ? +m[1] : 1; };
const opts = {bands: [{action: 'resync', x0: 6, x1: 13}], markers: [{t: 6, text: 'sync_action idle -> resync'}]};
const before = fakeChart();
mdPlugin.beforeDatasetsDraw(before.chart, {}, opts);
const tint = before.calls.filter(c => c.op === 'rect');
assert.strictEqual(tint.length, 1);
assert.deepStrictEqual([tint[0].x, tint[0].y, tint[0].w, tint[0].h], [60, 20, 70, 100]);
assert.ok(alpha(tint[0].style) < 0.3, 'tint is light: ' + tint[0].style);
assert.ok(!before.calls.some(c => c.op === 'text' && c.s === 'resync'), 'label drawn under the data');
const after = fakeChart();
mdPlugin.afterDatasetsDraw(after.chart, {}, opts);
const strip = after.calls.find(c => c.op === 'rect' && c.x === 60 && c.w === 70);
assert.ok(strip, 'no strip over the band: ' + JSON.stringify(after.calls));
assert.ok(strip.y === 20 && strip.h > 0 && strip.h <= 6, 'strip not in the top 6 px: ' + JSON.stringify(strip));
assert.ok(alpha(strip.style) >= 0.85, 'strip not opaque: ' + strip.style);
const li = after.calls.findIndex(c => c.op === 'text' && c.s === 'resync');
assert.ok(li > 0, 'no action label after the data');
const box = after.calls[li - 1];
assert.ok(box.op === 'rect' && /255,\s*255,\s*255/.test(box.style), 'label without white box: ' + JSON.stringify(box));
// Event labels sit below the strip label.
const ev = after.calls.find(c => c.op === 'text' && c.s.indexOf('sync_action') === 0);
assert.ok(ev && ev.y >= after.calls[li].y + 11, 'event label overlaps the strip label');
// Markers alone still draw; nothing at all without options.
const m = fakeChart();
mdPlugin.afterDatasetsDraw(m.chart, {}, {bands: [], markers: opts.markers});
assert.ok(m.calls.some(c => c.op === 'stroke'));
const n = fakeChart();
mdPlugin.beforeDatasetsDraw(n.chart, {}, false);
mdPlugin.afterDatasetsDraw(n.chart, {}, false);
assert.strictEqual(n.calls.length, 0);
console.log('OK');
`)
}

// requireNode returns the node binary the page-script tests run in. Without it
// the test skips locally, but fails when CI=true so a CI run cannot pass
// without having run the JS tests.
func requireNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("node not installed (required when CI=true)")
		}
		t.Skip("node not installed")
	}
	return node
}
