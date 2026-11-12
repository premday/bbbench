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
	"bbbench/internal/flushtrace"
	"bbbench/internal/iotrace"
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
		"const readPts  = (ts || []).filter(p => p.r > 0).map(p => ({x: Math.round(p.t + shift), y:  p.r}));",
		"const writePts = (ts || []).filter(p => p.w > 0).map(p => ({x: Math.round(p.t + shift), y: -p.w}));",
		"label: 'Read (fio)',  data: readPts,",
		"label: 'Write (fio)', data: writePts,",
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

// The per-phase graph mode: stacked (members) is the default, normal draws the
// members as plain lines, and the choice is kept per metric and phase.
func TestGraphsPageGraphMode(t *testing.T) {
	body := graphsPageBody(t)
	for _, want := range []string{
		"const GRAPH_MODES = [['normal', 'Normal'], ['stacked', 'Stacked']];",
		"if (metric.key !== 'lat' && phaseData.some(d => memberMetric(d, metric))) phaseHdr.appendChild(toggle);",
		"if (memberMetric(d, metric)) {",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("graphs page lacks %q", want)
		}
	}
	runPageJS(t, []string{"graphMode"}, `
const assert = require('assert');
const GRAPH_MODES = [['normal', 'Normal'], ['stacked', 'Stacked']];
const graphModeOf = {};
const M = {key: 'bw'};
assert.strictEqual(graphMode(M, 'randread'), 'stacked');
graphModeOf['bw:randread'] = 'normal';
assert.strictEqual(graphMode(M, 'randread'), 'normal');
assert.strictEqual(graphMode(M, 'seqwrite'), 'stacked');
assert.strictEqual(graphMode({key: 'iops'}, 'randread'), 'stacked');
console.log('OK');
`)
}

// memberGridJS is the node script TestGraphsPageMemberGrid runs over the page's
// own member chart functions.
const memberGridJS = `
const HIDDEN = {}, AXIS_HEADROOM = 1.15, MEMBER_PALETTE = [[1,2,3],[4,5,6]];
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
    // The phase extent is the drawn top (of the member stack and the
    // array's own line, its own stack) with the headroom.
    const close = (a, b, m) => { if (Math.abs(a - b) > 1e-9) fail(m + ' ' + a + ' != ' + b); };
    const arrR = Math.max(0, ...arr.filter(a => a.dir === 'r').flatMap(a => a.data.map(p => p.y || 0)));
    const arrW = Math.max(0, ...arr.filter(a => a.dir === 'w').flatMap(a => a.data.map(p => -(p.y || 0))));
    let e = phaseExtent([d], metric, 'stacked');
    close(e.yHigh, Math.max(topR, arrR) * AXIS_HEADROOM, 'yHigh');
    close(e.yLow, -Math.max(topW, arrW) * AXIS_HEADROOM, 'yLow');
    // Without the array line: the member stack top alone.
    setShown('iops', 'fio.r', false); setShown('iops', 'fio.w', false);
    e = phaseExtent([d], metric, 'stacked');
    close(e.yHigh, topR * AXIS_HEADROOM, 'stack yHigh');
    close(e.yLow, -topW * AXIS_HEADROOM, 'stack yLow');
    setShown('iops', 'fio.r', true); setShown('iops', 'fio.w', true);
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
		"memberGrid", "memberLabel", "hasDir", "rgba", "stackedMemberDatasets", "arrayDatasets",
		"iolatKind", "iolatPoints", "ftraceDatasets", "memberColors", "memberMetric", "memberLatencyDatasets", "ftraceOnGrid",
		"isShown", "setShown", "chartDatasets", "phaseExtent"} {
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
		"const readPts  = (ts || []).filter(p => p.r > 0).map(p => ({x: Math.round(p.t + shift), y:  p.r}));",
		"const writePts = (ts || []).filter(p => p.w > 0).map(p => ({x: Math.round(p.t + shift), y: -p.w}));",
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

func TestGenerateDiskPhaseData_IOTrace(t *testing.T) {
	sum := &iotrace.Summary{Text: "ftrace: x", Lost: 1}
	r := &BenchmarkResult{
		ID: "x", Drives: arrayDrives(),
		Phases: []PhaseResult{
			{PhaseNumber: 1, PhaseName: "p", Jobs: []JobResult{
				{Device: "md127", ReadStats: &IOStats{IOPS: 1, Bandwidth: 1}},
				{Device: "sdd", ReadStats: &IOStats{IOPS: 1, Bandwidth: 1}},
			},
				FlushTrace:   map[string]*flushtrace.Summary{"sdd": {Source: "block"}},
				IOTrace:      map[string]*iotrace.Summary{"md127": sum},
				IOTraceNotes: map[string]string{"sdd": "no ftrace latency: no tracefs"},
				IOTraceSeries: map[string]map[string][]IOLatPoint{"md127": {
					"md127": {{T: 1, Kind: "device", Dir: "r", N: 3, Mean: 10}},
					"sdb":   {{T: 1, Kind: "device", Dir: "r", N: 3, Mean: 20}},
					"sdc":   {{T: 1, Kind: "device", Dir: "w", N: 3, Mean: 30}},
				}}},
		},
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
		t.Fatal("missing entries")
	}
	if md.FlushSource != "" {
		t.Errorf("FlushSource without Flush = %q", md.FlushSource)
	}
	if len(md.TSiolat) != 1 || md.TSiolat[0].Mean != 10 || len(md.TSiolatMembers) != 2 || md.TSiolatMembers["sdb"][0].Mean != 20 || md.IOTrace != sum {
		t.Errorf("md127 = %+v", md)
	}
	if sdd.FlushSource != flushtrace.SourceLine(&flushtrace.Summary{Source: "block"}) || sdd.FlushSource == "" {
		t.Errorf("sdd FlushSource = %q", sdd.FlushSource)
	}
	if sdd.IOTraceNote != "no ftrace latency: no tracefs" || sdd.TSiolat != nil {
		t.Errorf("sdd = %+v", sdd)
	}
	// A phase without trace data has none of the keys.
	plain, _ := json.Marshal(DiskPhaseData{Disk: "sda"})
	for _, k := range []string{"ts_iolat", "io_trace", "io_trace_note", "flush_source"} {
		if strings.Contains(string(plain), `"`+k+`"`) {
			t.Errorf("plain entry has %s: %s", k, plain)
		}
	}
}

func TestGraphsPageLegendsNameSource(t *testing.T) {
	body := graphsPageBody(t)
	for _, want := range []string{"'Read (fio)'", "'Write (fio)'", "' (kernel stat)'", "' (fio)'"} {
		if !strings.Contains(body, want) {
			t.Errorf("graphs page lacks %q", want)
		}
	}
}

// Per-card tracepoint names come from the data (io_trace.text, flush_source),
// never from the page; only the metric-level help (ftraceRef) is static.
func TestGraphsPageNoNVMeAssumption(t *testing.T) {
	body := graphsPageBody(t)
	body = strings.Replace(body, jsFunction(t, body, "ftraceRef"), "", 1)
	for _, bad := range []string{"nvme_", "block_rq"} {
		if strings.Contains(body, bad) {
			t.Errorf("graphs page hard-codes %q", bad)
		}
	}
}

func TestGraphsPageSourceLine(t *testing.T) {
	runPageJS(t, []string{"iolatKind", "sourceLine", "lossNote", "addLossNote"}, `
const assert = require('assert');
const M = k => ({key: k});
const nv = {io_trace: {text: 'ftrace: nvme_setup_cmd -> nvme_complete_rq', lost: 0}, flush_source: 'ftrace: flush src'};
// Fill's per-kind texts: each card names its own tracepoints.
const split = {io_trace: {text: 'ftrace: dev, blk · x', text_device: 'ftrace: dev · x', text_blocklayer: 'ftrace: blk · x', lost: 2}};
const hdd = {io_trace: {text: 'ftrace: block_rq_issue -> block_rq_complete, block_rq_insert -> block_rq_complete', lost: 3}};
const mem = {ts_members: {sdb: {}}, io_trace_note: 'no ftrace latency: no tracefs'};
assert.strictEqual(sourceLine({}, M('iops')), 'fio');
assert.strictEqual(sourceLine({}, M('bw')), 'fio');
assert.strictEqual(sourceLine(mem, M('bw')), 'fio · members: kernel stat (/sys/block/<dev>/stat)');
assert.strictEqual(sourceLine(nv, M('lat')), 'fio · ftrace: nvme_setup_cmd -> nvme_complete_rq');
assert.strictEqual(sourceLine(mem, M('lat')), 'fio · members: kernel stat (/sys/block/<dev>/stat) · no ftrace latency: no tracefs');
assert.strictEqual(sourceLine({}, M('lat')), 'fio');
assert.strictEqual(sourceLine(nv, M('flush')), 'ftrace: flush src');
assert.strictEqual(sourceLine({}, M('flush')), '');
assert.strictEqual(sourceLine(nv, M('iolat')), 'ftrace: nvme_setup_cmd -> nvme_complete_rq');
assert.strictEqual(sourceLine(mem, M('iolat')), 'no ftrace latency: no tracefs');
assert.strictEqual(sourceLine(split, M('iolat')), 'ftrace: dev · x');
assert.strictEqual(sourceLine(split, M('iolatblk')), 'ftrace: blk · x');
assert.strictEqual(sourceLine(split, M('lat')), 'fio · ftrace: dev, blk · x');
// a summary without the per-kind texts (older JSON) keeps the whole line
assert.strictEqual(sourceLine(nv, M('iolatblk')), 'ftrace: nvme_setup_cmd -> nvme_complete_rq');
assert.strictEqual(sourceLine(mem, M('iolatblk')), 'no ftrace latency: no tracefs');
assert.strictEqual(lossNote(split, M('iolatblk')), '⚠ 2 trace events lost: the ftrace series are incomplete');
const h = sourceLine(hdd, M('iolat'));
assert.ok(h.includes('block_rq_issue -> block_rq_complete') && !h.includes('nvme'), h);
assert.ok(!sourceLine(hdd, M('lat')).includes('nvme'));
assert.strictEqual(lossNote(hdd, M('lat')), '⚠ 3 trace events lost: the ftrace series are incomplete');
assert.strictEqual(lossNote(hdd, M('iolat')), '⚠ 3 trace events lost: the ftrace series are incomplete');
assert.strictEqual(lossNote(hdd, M('iops')), '');
assert.strictEqual(lossNote(hdd, M('flush')), '');
assert.strictEqual(lossNote(nv, M('lat')), '');
assert.strictEqual(lossNote({}, M('lat')), '');
const el = {className: 'x', textContent: 'a'};
addLossNote(el, hdd, M('lat'));
assert.ok(el.textContent.startsWith('a · ⚠ 3'), el.textContent);
console.log('OK');
`)
}

func TestGraphsPageRampFallsBackToTrace(t *testing.T) {
	runPageJS(t, []string{"rampOf"}, `
const assert = require('assert');
assert.strictEqual(rampOf({io_trace: {offset_ms: 2500}}), 2.5);
assert.strictEqual(rampOf({flush: {offset_ms: 1000}, io_trace: {offset_ms: 2500}}), 1);
assert.strictEqual(rampOf({ramp_sec: 4, io_trace: {offset_ms: 2500}}), 4);
assert.strictEqual(rampOf({}), 0);
console.log('OK');
`)
}

func TestGraphsPageMemberLabelsNameSource(t *testing.T) {
	runPageJS(t, []string{"memberLabel"}, `
const assert = require('assert');
const d = {ts_members: {sdb: {role: ''}, sdc: {role: 'spare'}}};
assert.strictEqual(memberLabel(d, 'sdb', 'r'), 'sdb read (kernel stat)');
assert.strictEqual(memberLabel(d, 'sdc', 'w'), 'sdc (spare) write (kernel stat)');
console.log('OK');
`)
}

func TestGraphsPageArrayLabelsNameSource(t *testing.T) {
	runPageJS(t, []string{"hasDir", "arrayDatasets"}, `
const assert = require('assert');
const g = {xs: [1], array: {1: {r: 5, w: 6}}};
const labels = arrayDatasets(g, false).map(x => x.label);
assert.deepStrictEqual(labels, ['Array read (fio)', 'Array write (fio)']);
console.log('OK');
`)
}

// A loss note shown in the modal must not survive into the next one.
func TestGraphsPageModalClearsWarn(t *testing.T) {
	runPageJS(t, []string{"openModal", "iolatKind", "lossNote", "addLossNote", "sourceLine", "iolatNodeNote"}, `
const assert = require('assert');
const els = {};
const el = id => els[id] || (els[id] = {id, textContent: '', className: '', innerHTML: '',
    appendChild() {}, classList: {add() {}, remove() {}}});
const wrap = {replaceChild() {}};
const document = {getElementById: el, querySelector: () => wrap,
    createElement: () => ({}), createTextNode: () => ({})};
let modalChart = null;
const membersText = () => '', settingTag = () => '', RPV = {}, mdHeader = () => null;
const seriesRow = () => ({});
let modalCtx = null;
const makeChart = (c, d, m, lo, hi, warnEl) => ({destroy() {}});
const lossy = {disk: 'sda', io_trace: {lost: 4}};
openModal(lossy, {key: 'lat', label: 'Latency', source: 'fio'}, 0, 1, 'normal');
assert.ok(el('modal-ts-warn').textContent.includes('4 trace events lost'));
openModal({disk: 'sda'}, {key: 'bw', label: 'BW', source: 'fio'}, 0, 1, 'normal');
assert.strictEqual(el('modal-ts-warn').textContent, '');
// a bio-based node's Block-layer modal adds the node's note to the source line
openModal({disk: 'md0', io_trace: {text_blocklayer: 'ftrace: md0: bio-based, no block layer',
    devices: [{name: 'md0', bio_based: true, block_layer_note: 'bio-based (no request queue)'}]}},
    {key: 'iolatblk', label: 'Block', source: 'ftrace'}, 0, 1, 'normal');
assert.strictEqual(el('modal-src').textContent, 'ftrace: md0: bio-based, no block layer \u00b7 md0: bio-based (no request queue)');
console.log('OK');
`)
}

// ftraceFuncs are the page functions of the ftrace latency overlay.
var ftraceFuncs = []string{"windowOf", "memberStep", "memberKey", "memberColors", "rgba",
	"iolatKind", "iolatHasKind", "iolatBlkHas", "iolatBlkNoneText",
	"iolatPoints", "iolatMean", "ftraceDatasets", "iolatNone", "siScale", "fmtScaled", "toBase",
	"rampOf", "fioShift", "memberMetric", "isShown", "chartDatasets", "phaseExtent"}

const ftraceTestPrelude = `
const assert = require('assert');
const MEMBER_PALETTE = [[1,2,3],[4,5,6],[7,8,9],[10,11,12]];
const LAT = {key: 'lat', unit: 'μs', tsKey: 'ts_lat'}, IOLAT = {key: 'iolat', unit: 'μs', tsKey: 'ts_iolat'};
const IOLATBLK = {key: 'iolatblk', unit: 'μs', tsKey: 'ts_iolat'};
const HIDDEN = {}, AXIS_HEADROOM = 1.15;
const pt = (t, kind, dir, n, mean, p99) => ({t, kind, dir, n, mean, p50: mean, p99});
function entry(src, scale) {
    const ev = src === 'nvme' ? 'nvme_setup_cmd -> nvme_complete_rq' : 'block_rq_issue -> block_rq_complete';
    return {
        log_avg_msec: 1000,
        io_trace: {text: 'ftrace: ' + ev, devices: [{name: 'dev', source: src,
            stats: {device: {r: {count: 5, mean_ns: 100 * scale * 1000}, w: {count: 5, mean_ns: 200 * scale * 1000}}}}]},
        ts_iolat: [
            pt(0, 'device', 'r', 4, 100 * scale, 150 * scale), pt(0, 'device', 'w', 4, 200 * scale, 250 * scale),
            pt(0, 'blocklayer', 'r', 4, 110 * scale, 160 * scale), pt(0, 'blocklayer', 'w', 4, 210 * scale, 260 * scale),
            pt(1, 'device', 'r', 0, 0, 0), pt(1, 'device', 'w', 3, 300 * scale, 350 * scale)],
        ts_iolat_members: {sdb: [pt(0, 'device', 'r', 2, 50 * scale, 60 * scale), pt(0, 'device', 'w', 2, 60 * scale, 70 * scale),
            pt(0, 'blocklayer', 'r', 2, 55 * scale, 65 * scale), pt(0, 'blocklayer', 'w', 2, 65 * scale, 75 * scale)]},
    };
}
`

func TestGraphsPageIOLatDatasets(t *testing.T) {
	runPageJS(t, ftraceFuncs, ftraceTestPrelude+`
for (const [src, scale, prefix] of [['nvme', 1, 'μ'], ['block', 1e6, '']]) {
    const d = entry(src, scale);
    const lat = ftraceDatasets(d, LAT);
    assert.deepStrictEqual(lat.map(x => x.label), ['Read device (ftrace)', 'Read block layer (ftrace)',
        'Write device (ftrace)', 'Write block layer (ftrace)']);
    assert.deepStrictEqual(lat.map(x => x.borderDash), [[6, 3], [2, 2], [6, 3], [2, 2]]);
    assert.ok(lat.every(x => x.fill === false));
    assert.ok(lat[0].borderColor.startsWith('rgba(37,99,235'));
    assert.ok(lat[2].borderColor.startsWith('rgba(220,38,38'));
    // the n: 0 read window is skipped; writes are negative
    assert.deepStrictEqual(lat[0].data, [{x: 0, y: 100 * scale}]);
    assert.deepStrictEqual(lat[2].data, [{x: 0, y: -200 * scale}, {x: 1, y: -300 * scale}]);
    const sc = siScale(LAT, 300 * scale);
    assert.strictEqual(sc.prefix, prefix === 'μ' ? 'μ' : '');
    const based = toBase(ftraceDatasets(d, LAT), sc);
    assert.strictEqual(fmtScaled(based[2].data[1].y, sc, true), prefix === 'μ' ? '300.00μ' : '300.00');
    // Device latency: the device kind only, the members' device means.
    const io = ftraceDatasets(d, IOLAT);
    assert.deepStrictEqual(io.map(x => x.label), [
        'Read device mean (ftrace)', 'Read device p99 (ftrace)',
        'Write device mean (ftrace)', 'Write device p99 (ftrace)',
        'sdb read device mean (ftrace)', 'sdb write device mean (ftrace)']);
    assert.deepStrictEqual(io[1].borderDash, [8, 3, 2, 3]);
    assert.ok(io[1].borderColor.endsWith('0.6)'));
    assert.strictEqual(io[1].data[0].y, 150 * scale);
    assert.strictEqual(io[5].data[0].y, -60 * scale);
    // Block-layer latency: the block-layer kind only, the members' block-layer means.
    const blk = ftraceDatasets(d, IOLATBLK);
    assert.deepStrictEqual(blk.map(x => x.label), [
        'Read block layer mean (ftrace)', 'Read block layer p99 (ftrace)',
        'Write block layer mean (ftrace)', 'Write block layer p99 (ftrace)',
        'sdb read block layer mean (ftrace)', 'sdb write block layer mean (ftrace)']);
    assert.deepStrictEqual(blk.map(x => x.skey), ['ftrace.blocklayer.mean.r', 'ftrace.blocklayer.p99.r',
        'ftrace.blocklayer.mean.w', 'ftrace.blocklayer.p99.w', 'ftrace.member:sdb', 'ftrace.member:sdb']);
    assert.deepStrictEqual(blk.map(x => x.borderDash), [[2, 2], [8, 3, 2, 3], [2, 2], [8, 3, 2, 3], [], [4, 2]]);
    assert.deepStrictEqual(blk[0].data, [{x: 0, y: 110 * scale}]);
    assert.deepStrictEqual(blk[3].data, [{x: 0, y: -260 * scale}]);
    assert.strictEqual(blk[4].data[0].y, 55 * scale);
    assert.strictEqual(blk[5].data[0].y, -65 * scale);
    const bsc = siScale(IOLATBLK, 260 * scale);
    assert.strictEqual(fmtScaled(toBase(blk, bsc)[1].data[0].y, bsc, true), prefix === 'μ' ? '160.00μ' : '160.00');
}
// a kind and direction without points yields no dataset
const e = entry('nvme', 1);
e.ts_iolat = e.ts_iolat.filter(p => p.kind === 'device' && p.dir === 'r');
assert.deepStrictEqual(ftraceDatasets(e, LAT).map(x => x.label), ['Read device (ftrace)']);
console.log('OK');
`)
}

func TestGraphsPageIOLatNone(t *testing.T) {
	runPageJS(t, ftraceFuncs, ftraceTestPrelude+`
assert.strictEqual(iolatNone({io_trace_note: 'no ftrace latency: no tracefs'}, IOLAT), 'no ftrace latency: no tracefs');
assert.strictEqual(iolatNone({}, IOLAT), '');
assert.strictEqual(iolatNone({io_trace: {text: 'x', devices: [{name: 'md0', stats: {}}]}}, IOLAT),
    'ftrace saw no read or write requests on md0');
assert.strictEqual(iolatNone({io_trace: {text: 'x', devices: [{name: 'md0'}]}}, IOLAT),
    'ftrace saw no read or write requests on md0');
assert.strictEqual(iolatNone(entry('nvme', 1), IOLAT), '');
assert.strictEqual(iolatNone({io_trace_note: 'n'}, LAT), '');
// a bio-based md0 target with traced members still shows the members
const md0 = {io_trace: {text: 'x', devices: [{name: 'md0', note: 'no request events seen', stats: {}}]},
    ts_iolat_members: {sdb: [pt(0, 'device', 'r', 2, 5, 6)]}};
assert.strictEqual(iolatNone(md0, IOLAT), '');
// the target's own note wins over the generic text
md0.ts_iolat_members = {};
assert.strictEqual(iolatNone(md0, IOLAT), 'no request events seen');
assert.strictEqual(iolatNone({io_trace: {text: 'x', devices: []}}, IOLAT), 'no traced device');
assert.strictEqual(iolatNone({io_trace: {text: 'x'}}, IOLAT), 'no traced device');
// an entry without io_trace has no ftrace dataset on lat
assert.deepStrictEqual(ftraceDatasets({log_avg_msec: 1000, ts_lat: [{t: 1, r: 5, w: 0}]}, LAT), []);
assert.deepStrictEqual(ftraceDatasets({}, IOLAT), []);
assert.deepStrictEqual(ftraceDatasets({}, IOLATBLK), []);
console.log('OK');
`)
}

// TestGraphsPageIOLatBlkNone: a Block-layer latency card without block-layer
// series says why, from the data (the device's block_layer_note, its note,
// the reason ftrace was off), instead of an empty chart or zeros.
func TestGraphsPageIOLatBlkNone(t *testing.T) {
	runPageJS(t, ftraceFuncs, ftraceTestPrelude+`
// A kernel without the block-layer start tracepoint: device series only.
const noIns = {io_trace: {text: 'x', devices: [{name: 'sdc', source: 'block', block_layer: false,
        block_layer_note: 'the kernel has no INSERT tracepoint', stats: {device: {r: {count: 3, mean_ns: 1e5}}}}]},
    ts_iolat: [pt(0, 'device', 'r', 3, 100, 150)]};
assert.strictEqual(iolatNone(noIns, IOLATBLK), 'no block-layer series: the kernel has no INSERT tracepoint');
assert.strictEqual(iolatNone(noIns, IOLAT), '');
assert.deepStrictEqual(ftraceDatasets(noIns, IOLATBLK), []);
assert.strictEqual(iolatMean(noIns, 'r', 'blocklayer'), 0);
// A multipath head: traced, block layer on, but no block-layer requests seen.
const mpath = {io_trace: {text: 'x', devices: [{name: 'nvme0n1', source: 'nvme', block_layer: true,
        block_layer_note: 'ftrace saw no block-layer requests on nvme0n1 (multipath)', stats: {device: {r: {count: 3}}}}]},
    ts_iolat: [pt(0, 'device', 'r', 3, 100, 150)]};
assert.strictEqual(iolatNone(mpath, IOLATBLK), 'no block-layer series: ftrace saw no block-layer requests on nvme0n1 (multipath)');
// A bio-based target: its members' block-layer series are drawn; without them it says why.
const md0 = {io_trace: {text: 'x', devices: [{name: 'md0', source: 'block', block_layer: true,
        block_layer_note: 'ftrace saw no block-layer requests on md0', stats: {}}]},
    ts_iolat_members: {sdb: [pt(0, 'device', 'r', 2, 5, 6), pt(0, 'blocklayer', 'r', 2, 7, 8)]}};
assert.strictEqual(iolatNone(md0, IOLATBLK), '');
assert.deepStrictEqual(ftraceDatasets(md0, IOLATBLK).map(x => x.label), ['sdb read block layer mean (ftrace)']);
md0.ts_iolat_members.sdb = md0.ts_iolat_members.sdb.filter(p => p.kind === 'device');
assert.strictEqual(iolatNone(md0, IOLATBLK), 'no block-layer series: ftrace saw no block-layer requests on md0');
assert.strictEqual(iolatNone(md0, IOLAT), '');
// An older summary without block_layer_note: the note, else a generic text.
assert.strictEqual(iolatNone({io_trace: {text: 'x', devices: [{name: 'sdz', note: 'no such device'}]}}, IOLATBLK),
    'no block-layer series: no such device');
assert.strictEqual(iolatNone({io_trace: {text: 'x', devices: [{name: 'sdq', stats: {device: {r: {count: 1}}}}]}}, IOLATBLK),
    'no block-layer series: ftrace saw no block-layer requests on sdq');
// Off or untraced: the same reasons as the Device latency card.
assert.strictEqual(iolatNone({io_trace_note: 'no ftrace latency: no tracefs'}, IOLATBLK), 'no ftrace latency: no tracefs');
assert.strictEqual(iolatNone({io_trace: {text: 'x', devices: []}}, IOLATBLK), 'no traced device');
assert.strictEqual(iolatNone(entry('block', 1e6), IOLATBLK), '');
// has: an entry with block-layer data, or any traced one when another shown
// entry has some (its card says why); none at all: no card.
const good = entry('nvme', 1);
assert.strictEqual(iolatHasKind(good, 'blocklayer'), true);
assert.strictEqual(iolatHasKind(noIns, 'blocklayer'), false);
assert.strictEqual(iolatHasKind(noIns, 'device'), true);
assert.strictEqual(iolatBlkHas(good, [good]), true);
assert.strictEqual(iolatBlkHas(noIns, [good, noIns]), true);
assert.strictEqual(iolatBlkHas(noIns, [noIns, mpath]), false);
assert.strictEqual(iolatBlkHas({}, [good, {}]), false);
assert.strictEqual(iolatBlkHas({io_trace_note: 'no ftrace latency: no tracefs'}, [good]), true);
// The section placeholder names the reasons, once each.
assert.strictEqual(iolatBlkNoneText([noIns, noIns, mpath, {}]),
    'No block-layer data for the selected disks and phases: the kernel has no INSERT tracepoint; ' +
    'ftrace saw no block-layer requests on nvme0n1 (multipath)');
assert.strictEqual(iolatBlkNoneText([{}]), 'No block-layer data for the selected disks and phases.');
console.log('OK');
`)
}

func TestGraphsPageIOLatExtent(t *testing.T) {
	runPageJS(t, ftraceFuncs, ftraceTestPrelude+`
const d = entry('nvme', 1);
d.ts_iolat.push(pt(2, 'device', 'r', 1, 90, 9000));
const close = (a, b) => assert.ok(Math.abs(a - b) < 1e-9, a + ' != ' + b);
const lat = phaseExtent([d], LAT, 'normal'), io = phaseExtent([d], IOLAT, 'normal');
close(lat.yHigh, 110 * 1.15);
close(lat.yLow, -300 * 1.15);
close(io.yHigh, 9000 * 1.15);
close(io.yLow, -350 * 1.15);
// The Block-layer card's range comes from its own series: block-layer p99s and the member means.
const blk = phaseExtent([d], IOLATBLK, 'normal');
close(blk.yHigh, 160 * 1.15);
close(blk.yLow, -260 * 1.15);
// Summary means only (no series): bars of that kind.
const bars = phaseExtent([{io_trace: {devices: [{name: 'sda', stats: {device: {r: {mean_ns: 9e5}}, blocklayer: {r: {mean_ns: 4e5}, w: {mean_ns: 5e5}}}}]}}],
    Object.assign({sumR: d => iolatMean(d, 'r', 'blocklayer'), sumW: d => iolatMean(d, 'w', 'blocklayer')}, IOLATBLK), 'normal');
close(bars.yHigh, 400 * 1.15);
close(bars.yLow, -500 * 1.15);
assert.deepStrictEqual(phaseExtent([{}], Object.assign({sumR: () => 0, sumW: () => 0}, LAT), 'normal'), {yLow: 0, yHigh: 1});
console.log('OK');
`)
}

func TestGraphsPageIOLatMetric(t *testing.T) {
	body := graphsPageBody(t)
	for _, want := range []string{"key: 'iolat'", "Device latency (ftrace)", "['iolat', 'Device latency (ftrace)']",
		"key: 'iolatblk', label: 'Block-layer latency (ftrace)'", "['iolatblk', 'Block-layer latency (ftrace)']",
		"has:  (d, all) => iolatBlkHas(d, all)", "metric.has(d, data)", "metric.none ? metric.none(data)",
		"metric.srcTitle || SRC_TITLE[metric.source]", "function makeIOLatChart("} {
		if !strings.Contains(body, want) {
			t.Errorf("graphs page lacks %q", want)
		}
	}
	shared := chartSharedJS
	if !strings.Contains(shared, "metric.key === 'flush' || metric.key === 'iolat' || metric.key === 'iolatblk' ? 0") {
		t.Error("seriesMaxT must not shift flush, iolat and iolatblk points")
	}
}

// TestGraphsPageIOLatCharts: the Latency chart appends the ftrace lines in
// the phase's base unit (HDD seconds included), the Device latency chart
// draws them alone, with bars of the summary means when there is no series.
func TestGraphsPageIOLatCharts(t *testing.T) {
	runPageJS(t, chartFuncs, chartStubJS+`
const LAT = {key: 'lat', unit: 'μs', tsKey: 'ts_lat', sumR: d => 0, sumW: d => 0};
const IOLAT = {key: 'iolat', unit: 'μs', tsKey: 'ts_iolat', sumR: d => iolatMeanStub(d, 'r'), sumW: d => iolatMeanStub(d, 'w')};
const IOLATBLK = {key: 'iolatblk', unit: 'μs', tsKey: 'ts_iolat', sumR: d => iolatMeanStub(d, 'r'), sumW: d => iolatMeanStub(d, 'w')};
const iolatMeanStub = (d, dir) => d.mean ? d.mean[dir] : 0;
const pt = (t, kind, dir, n, mean, p99) => ({t, kind, dir, n, mean, p50: mean, p99});
const label = (k, i) => last.options.plugins.tooltip.callbacks.label({dataset: last.data.datasets[k], raw: last.data.datasets[k].data[i]});
// An HDD with second-scale latencies: s, two decimals in the tooltip.
const d = {log_avg_msec: 1000, ts_lat: [{t: 1, r: 2e6, w: 0}],
    ts_iolat: [pt(0, 'device', 'r', 3, 2.5e6, 4e6), pt(0, 'blocklayer', 'r', 3, 2.6e6, 4.1e6)]};
makeChart({}, d, LAT, 0, 4.6e6);
assert.deepStrictEqual(last.data.datasets.map(x => x.label), ['Read (fio)', 'Read device (ftrace)', 'Read block layer (ftrace)']);
assert.strictEqual(yTitle(), 's');
assert.strictEqual(last.data.datasets[1].data[0].y, 2.5);
assert.strictEqual(label(1, 0), 'Read device (ftrace): 2.50 s');
makeChart({}, d, IOLAT, 0, 4.6e6);
assert.deepStrictEqual(last.data.datasets.map(x => x.label), ['Read device mean (ftrace)', 'Read device p99 (ftrace)']);
assert.strictEqual(label(1, 0), 'Read device p99 (ftrace): 4.00 s');
// The HDD's block layer on its own card, in seconds, two decimals.
makeChart({}, d, IOLATBLK, 0, 4.1e6 * 1.15);
assert.deepStrictEqual(last.data.datasets.map(x => x.label), ['Read block layer mean (ftrace)', 'Read block layer p99 (ftrace)']);
assert.strictEqual(yTitle(), 's');
assert.strictEqual(last.data.datasets[0].data[0].y, 2.6);
assert.strictEqual(label(0, 0), 'Read block layer mean (ftrace): 2.60 s');
assert.strictEqual(label(1, 0), 'Read block layer p99 (ftrace): 4.10 s');
assert.strictEqual(last.options.plugins.rampZone.sec, 0);
// No series: bars of the summary means.
makeChart({}, {mean: {r: 100, w: 200}}, IOLAT, -230, 115);
assert.deepStrictEqual(last.data.labels, ['Read device (ftrace)', 'Write device (ftrace)']);
makeChart({}, {mean: {r: 100, w: 200}}, IOLATBLK, -230, 115);
assert.deepStrictEqual(last.data.labels, ['Read block layer (ftrace)', 'Write block layer (ftrace)']);
console.log('OK');
`)
}

// seriesFuncs are the page functions that build a card's datasets and the
// series visibility around them.
var seriesFuncs = []string{"rampOf", "windowOf", "fioShift", "memberMetric", "memberNames", "memberColors", "rgba",
	"memberStep", "memberKey", "memberGrid", "memberLabel", "hasDir", "stackedMemberDatasets", "memberLatencyDatasets",
	"arrayDatasets", "iolatKind", "iolatPoints", "iolatMean", "ftraceDatasets", "ftraceOnGrid",
	"isShown", "setShown", "chartDatasets", "cardSeries", "phaseExtent"}

const seriesTestPrelude = `
const assert = require('assert');
const MEMBER_PALETTE = [[1,2,3],[4,5,6],[7,8,9],[10,11,12]];
const AXIS_HEADROOM = 1.15;
const HIDDEN = {};
const metric = (key, tsKey, sumR, sumW) => ({key, tsKey, sumR: sumR || (d => 0), sumW: sumW || (d => 0)});
const LAT = metric('lat', 'ts_lat'), IOPS = metric('iops', 'ts_iops'), BW = metric('bw', 'ts_bw');
const IOLAT = metric('iolat', 'ts_iolat', d => iolatMean(d, 'r'), d => iolatMean(d, 'w'));
const IOLATBLK = metric('iolatblk', 'ts_iolat', d => iolatMean(d, 'r', 'blocklayer'), d => iolatMean(d, 'w', 'blocklayer'));
const FLUSH = metric('flush', 'ts_flush', d => 0, d => d.flush ? d.flush.mean_ns / 1000 : 0);
const pt = (t, kind, dir, n, mean, p99) => ({t, kind, dir, n, mean, p50: mean, p99});
const keys = cd => cd.datasets.map(x => x.skey);
// A drive: fio latency of 500/600 us, ftrace means of at most 120/310 us.
const drive = () => ({log_avg_msec: 1000, ramp_sec: 0,
    ts_lat: [{t: 0, r: 500, w: 600}, {t: 1, r: 400, w: 300}],
    io_trace: {text: 'ftrace: x', devices: [{name: 'sda', stats: {device: {r: {mean_ns: 1e5}, w: {mean_ns: 2e5}}}}]},
    ts_iolat: [pt(0, 'device', 'r', 4, 100, 150), pt(0, 'blocklayer', 'r', 4, 120, 170),
        pt(0, 'device', 'w', 4, 200, 250), pt(1, 'blocklayer', 'w', 4, 310, 400)],
    ts_iolat_members: {sdb: [pt(0, 'device', 'r', 2, 50, 60), pt(0, 'device', 'w', 2, 60, 70),
        pt(0, 'blocklayer', 'r', 2, 52, 62)]}});
// An array: three members whose stack (read 600, write 300) is above the
// array's own fio numbers.
const array = () => ({log_avg_msec: 1000, ramp_sec: 0,
    ts_iops: [{t: 0, r: 500, w: 200}, {t: 1, r: 400, w: 100}],
    ts_members: {
        sdb: {role: 'data', iops: [{t: 0, r: 300, w: 100}, {t: 1, r: 200, w: 50}]},
        sdc: {role: 'data', iops: [{t: 0, r: 200, w: 120}, {t: 1, r: 100, w: 50}]},
        sdd: {role: 'spare', iops: [{t: 0, r: 100, w: 80}, {t: 1, r: 100, w: 0}]}}});
`

// TestGraphsPageSeriesKeys: every dataset of a card carries its series key,
// and cardSeries lists each key once.
func TestGraphsPageSeriesKeys(t *testing.T) {
	runPageJS(t, seriesFuncs, seriesTestPrelude+`
const d = drive();
assert.deepStrictEqual(keys(chartDatasets(d, LAT, 'normal')), ['fio.r', 'fio.w',
    'ftrace.device.mean.r', 'ftrace.blocklayer.mean.r', 'ftrace.device.mean.w', 'ftrace.blocklayer.mean.w']);
assert.deepStrictEqual(keys(chartDatasets(d, IOLAT, 'normal')), [
    'ftrace.device.mean.r', 'ftrace.device.p99.r', 'ftrace.device.mean.w', 'ftrace.device.p99.w',
    'ftrace.member:sdb', 'ftrace.member:sdb']);
assert.deepStrictEqual(cardSeries(d, IOLAT, 'normal').map(s => s.skey).slice(-1), ['ftrace.member:sdb']);
assert.strictEqual(cardSeries(d, IOLAT, 'normal').slice(-1)[0].label, 'sdb device mean (ftrace)');
assert.deepStrictEqual(keys(chartDatasets(d, IOLATBLK, 'normal')), [
    'ftrace.blocklayer.mean.r', 'ftrace.blocklayer.p99.r', 'ftrace.blocklayer.mean.w', 'ftrace.blocklayer.p99.w',
    'ftrace.member:sdb']);
assert.deepStrictEqual(cardSeries(d, IOLATBLK, 'normal').map(s => s.label), ['Read block layer mean (ftrace)',
    'Read block layer p99 (ftrace)', 'Write block layer mean (ftrace)', 'Write block layer p99 (ftrace)', 'sdb block layer mean (ftrace)']);
const a = array();
const st = chartDatasets(a, IOPS, 'stacked');
assert.strictEqual(st.stacked, true);
assert.deepStrictEqual([...new Set(keys(st))], ['member:sdb', 'member:sdc', 'member:sdd', 'fio.r', 'fio.w']);
const cs = cardSeries(a, IOPS, 'stacked');
assert.deepStrictEqual(cs.map(s => s.skey), ['member:sdb', 'member:sdc', 'member:sdd', 'fio.r', 'fio.w']);
assert.deepStrictEqual(cs.map(s => s.label), ['sdb (data) (kernel stat)', 'sdc (data) (kernel stat)',
    'sdd (spare) (kernel stat)', 'Array read (fio)', 'Array write (fio)']);
assert.ok(cs.every(s => typeof s.color === 'string' && s.color.startsWith('rgba(')));
// Normal mode of the array: every member and the array, unstacked.
assert.deepStrictEqual([...new Set(keys(chartDatasets(a, IOPS, 'normal')))], ['member:sdb', 'member:sdc', 'member:sdd', 'fio.r', 'fio.w']);
// Bars (no series) and Flush.
const bars = chartDatasets({}, metric('bw', 'ts_bw', d => 5, d => 6), 'normal');
assert.strictEqual(bars.bars, true);
assert.deepStrictEqual(keys(bars), ['fio.r', 'fio.w']);
assert.deepStrictEqual(keys(chartDatasets({ts_flush: [{t: 0, w: 5}]}, FLUSH)), ['flush']);
assert.deepStrictEqual(keys(chartDatasets({flush: {mean_ns: 5000}}, FLUSH)), ['flush']);
assert.deepStrictEqual(keys(chartDatasets({io_trace: d.io_trace}, IOLAT)), ['ftrace.device.mean.r', 'ftrace.device.mean.w']);
assert.deepStrictEqual(keys(chartDatasets({io_trace: d.io_trace}, IOLATBLK)), []);
const blkSum = {io_trace: {devices: [{name: 'sda', stats: {blocklayer: {r: {mean_ns: 3e5}}}}]}};
assert.deepStrictEqual(keys(chartDatasets(blkSum, IOLATBLK)), ['ftrace.blocklayer.mean.r']);
assert.deepStrictEqual(chartDatasets(blkSum, IOLATBLK).datasets[0].data, [300]);
console.log('OK');
`)
}

// TestGraphsPageSeriesToggleScale: the phase's y range and SI prefix come
// from the shown series only.
func TestGraphsPageSeriesToggleScale(t *testing.T) {
	runPageJS(t, seriesFuncs, seriesTestPrelude+`
const close = (a, b) => assert.ok(Math.abs(a - b) < 1e-9, a + ' != ' + b);
const p = [drive()];
let e = phaseExtent(p, LAT, 'normal');
close(e.yHigh, 500 * 1.15); close(e.yLow, -600 * 1.15);
setShown('lat', 'fio.r', false);
setShown('lat', 'fio.w', false);
e = phaseExtent(p, LAT, 'normal');
close(e.yHigh, 120 * 1.15); close(e.yLow, -310 * 1.15);
assert.ok(!keys(chartDatasets(p[0], LAT, 'normal')).some(k => k.startsWith('fio.')));
// cardSeries still offers the hidden ones.
assert.deepStrictEqual(cardSeries(p[0], LAT, 'normal').map(s => s.skey).slice(0, 2), ['fio.r', 'fio.w']);
// Only the block layer reads left: the extent follows them.
['ftrace.device.mean.r', 'ftrace.device.mean.w', 'ftrace.blocklayer.mean.w'].forEach(k => setShown('lat', k, false));
e = phaseExtent(p, LAT, 'normal');
close(e.yHigh, 120 * 1.15); assert.strictEqual(e.yLow, 0);
// Nothing shown.
setShown('lat', 'ftrace.blocklayer.mean.r', false);
assert.deepStrictEqual(phaseExtent(p, LAT, 'normal'), {yLow: 0, yHigh: 1});
// The Block-layer card has its own hidden set and range: the Latency
// toggles above do not touch it, hiding its p99s and member leaves the means.
e = phaseExtent(p, IOLATBLK, 'normal');
close(e.yHigh, 170 * 1.15); close(e.yLow, -400 * 1.15);
['ftrace.blocklayer.p99.r', 'ftrace.blocklayer.p99.w', 'ftrace.member:sdb'].forEach(k => setShown('iolatblk', k, false));
e = phaseExtent(p, IOLATBLK, 'normal');
close(e.yHigh, 120 * 1.15); close(e.yLow, -310 * 1.15);
assert.strictEqual(phaseExtent(p, IOLAT, 'normal').yHigh, 150 * 1.15);
assert.ok(isShown('iolat', 'ftrace.member:sdb'));
// Bars follow the toggles too.
const B = metric('bw', 'ts_bw', d => 5, d => 6);
close(phaseExtent([{}], B, 'normal').yLow, -6 * 1.15);
setShown('bw', 'fio.w', false);
assert.strictEqual(phaseExtent([{}], B, 'normal').yLow, 0);
console.log('OK');
`)
}

// TestGraphsPageSeriesToggleStack: a hidden member is not built, the stack
// totals and the extent drop by its values, and the first shown member of
// each direction fills from the origin.
func TestGraphsPageSeriesToggleStack(t *testing.T) {
	runPageJS(t, seriesFuncs, seriesTestPrelude+`
const close = (a, b) => assert.ok(Math.abs(a - b) < 1e-9, a + ' != ' + b);
const a = array();
let cd = chartDatasets(a, IOPS, 'stacked');
assert.deepStrictEqual(cd.grid.totals[0], {r: 600, w: 300});
let e = phaseExtent([a], IOPS, 'stacked');
close(e.yHigh, 600 * 1.15); close(e.yLow, -300 * 1.15);
setShown('iops', 'member:sdb', false);
cd = chartDatasets(a, IOPS, 'stacked');
assert.ok(!cd.datasets.some(x => x.skey === 'member:sdb'));
assert.deepStrictEqual(cd.grid.totals[0], {r: 300, w: 200});
assert.deepStrictEqual(cd.grid.totals[1], {r: 200, w: 50});
['r', 'w'].forEach(dir => {
    const m = cd.datasets.filter(x => x.member && x.dir === dir);
    assert.deepStrictEqual(m.map(x => x.member), ['sdc', 'sdd']);
    assert.deepStrictEqual(m.map(x => x.fill), ['origin', '-1']);
});
// The array's own line (500 / 200) is now the top.
e = phaseExtent([a], IOPS, 'stacked');
close(e.yHigh, 500 * 1.15); close(e.yLow, -200 * 1.15);
// Hiding the array too leaves the stack of sdc and sdd.
setShown('iops', 'fio.r', false);
setShown('iops', 'fio.w', false);
e = phaseExtent([a], IOPS, 'stacked');
close(e.yHigh, 300 * 1.15); close(e.yLow, -200 * 1.15);
// Member colours do not move when one is hidden.
const sdc = chartDatasets(a, IOPS, 'stacked').datasets.find(x => x.member === 'sdc');
assert.strictEqual(sdc.borderColor, cardSeries(a, IOPS, 'stacked')[1].color);
assert.strictEqual(sdc.borderColor, chartDatasets(a, IOPS, 'stacked', true).datasets.find(x => x.member === 'sdc').borderColor);
console.log('OK');
`)
}

// TestGraphsPageToggleSurvivesRefill: the choice is page memory: building the
// card again keeps it hidden, another metric is not affected.
func TestGraphsPageToggleSurvivesRefill(t *testing.T) {
	runPageJS(t, seriesFuncs, seriesTestPrelude+`
const d = drive();
setShown('lat', 'fio.r', false);
assert.strictEqual(isShown('lat', 'fio.r'), false);
assert.strictEqual(isShown('lat', 'fio.w'), true);
for (let i = 0; i < 2; i++) assert.ok(!keys(chartDatasets(d, LAT, 'normal')).includes('fio.r'));
const bw = {ts_bw: [{t: 0, r: 1, w: 1}]};
assert.deepStrictEqual(keys(chartDatasets(bw, BW, 'normal')), ['fio.r', 'fio.w']);
setShown('lat', 'fio.r', true);
assert.ok(keys(chartDatasets(d, LAT, 'normal')).includes('fio.r'));
console.log('OK');
`)
}

// TestGraphsPageSeriesRowStopsPropagation: the row's clicks never reach the
// card (which opens the modal); a change records the choice and redraws
// every phase of the metric and the open modal of that metric.
func TestGraphsPageSeriesRowStopsPropagation(t *testing.T) {
	body := graphsPageBody(t)
	row := jsFunction(t, body, "seriesRow")
	for _, want := range []string{"stopPropagation()", "setShown(metric.key", "applyShown(metric)", "cardSeries(d, metric, mode)"} {
		if !strings.Contains(row, want) {
			t.Errorf("seriesRow lacks %q:\n%s", want, row)
		}
	}
	for fn, want := range map[string]string{"makeChartCard": "seriesRow(d, metric, mode, cid)", "openModal": "seriesRow(d, metric, mode, 'modal:' + metric.key)",
		"renderAll": "REFILL = {};", "applyShown": "REFILL[metric.key]"} {
		if !strings.Contains(jsFunction(t, body, fn), want) {
			t.Errorf("%s lacks %q", fn, want)
		}
	}
	if strings.Contains(body, "legend: { display: true, position: 'top'") {
		t.Error("a chart still shows Chart.js's own legend next to the series row")
	}
	if !strings.Contains(body, `id="modal-series"`) {
		t.Error("the modal has no series row")
	}
	runPageJS(t, []string{"seriesRow", "applyShown", "isShown", "setShown"}, `
const assert = require('assert');
const HIDDEN = {};
let REFILL = {}, modalCtx = null, opened = [], ROW_BOXES = {}, focused = null;
const made = [];
const document = {createElement: tag => { const e = {tag, children: [], style: {}, dataset: {}, className: '', handlers: {},
    appendChild(c) { this.children.push(c); }, addEventListener(k, f) { this.handlers[k] = f; }, focus() { focused = this; }}; made.push(e); return e; },
    createTextNode: t => ({textContent: t})};
const cardSeries = () => [{skey: 'fio.r', label: 'Read (fio)', color: 'red'}, {skey: 'ftrace.device.mean.r', label: 'Read device (ftrace)', color: 'blue', dash: [6, 3]}];
const phaseExtent = () => ({yLow: -2, yHigh: 3});
const openModal = (...a) => opened.push(a);
const M = {key: 'lat'};
let fills = 0;
REFILL.lat = [() => fills++, () => fills++];
REFILL.iops = [() => { throw new Error('iops refilled'); }];
const row = seriesRow({disk: 'sda'}, M, 'normal', 'lat:p:0');
assert.strictEqual(row.className, 'series-row');
let stopped = 0;
row.handlers.click({stopPropagation: () => stopped++});
assert.strictEqual(stopped, 1);
const boxes = made.filter(e => e.tag === 'input');
assert.strictEqual(boxes.length, 2);
assert.ok(boxes.every(b => b.type === 'checkbox' && b.checked === true));
boxes[0].checked = false;
boxes[0].handlers.change();
assert.strictEqual(isShown('lat', 'fio.r'), false);
assert.strictEqual(fills, 2);
assert.strictEqual(opened.length, 0);
assert.strictEqual(boxes[0].dataset.skey, 'fio.r');
// The redraw rebuilds the row: focus moves to the new checkbox of the same
// card and series, so the keyboard keeps its place.
REFILL.lat = [() => seriesRow({disk: 'sda'}, M, 'normal', 'lat:p:0'), () => seriesRow({disk: 'sdb'}, M, 'normal', 'lat:p:1')];
made.length = 0;
boxes[0].checked = true;
boxes[0].handlers.change();
const rebuilt = made.filter(e => e.tag === 'input');
assert.strictEqual(focused, rebuilt[0], 'focus not on the rebuilt checkbox of the same card');
REFILL.lat = [() => fills++, () => fills++];
// With the modal on this metric, it is opened again on the phase's new scale.
modalCtx = {d: {disk: 'sda'}, metric: M, pname: 'p', mode: 'normal', phaseData: []};
boxes[0].checked = false;
boxes[0].handlers.change();
assert.strictEqual(isShown('lat', 'fio.r'), false);
setShown('lat', 'fio.r', true);
assert.strictEqual(opened.length, 1);
assert.deepStrictEqual(opened[0].slice(2, 5), [-2, 3, 'normal']);
// A row built while a series is hidden shows it unchecked.
setShown('lat', 'ftrace.device.mean.r', false);
made.length = 0;
seriesRow({disk: 'sda'}, M, 'normal', 'lat:p:0');
assert.deepStrictEqual(made.filter(e => e.tag === 'input').map(b => b.checked), [true, false]);
console.log('OK');
`)
}

func TestGraphsPageSrcBadgeTracepoints(t *testing.T) {
	runPageJS(t, []string{"ftraceRef", "ftraceEvents", "srcTitleOf", "srcBadge"}, `
const assert = require('assert');
const SRC_TITLE = {fio: 'Measured by fio'};
const document = {createElement: () => ({})};
const M = (key, source, srcTitle) => ({key, source, srcTitle});
const flush = M('flush', 'ftrace', 'Measured from kernel ftrace events (device Flush commands), not by fio');
const iolat = M('iolat', 'ftrace', 'Measured from kernel ftrace events (dev)');
const blk = M('iolatblk', 'ftrace', 'Measured from kernel ftrace events (blk)');
const lat = M('lat', 'fio');
const iops = M('iops', 'fio');
const nv = {flush: {events: ['nvme_setup_cmd -> nvme_complete_rq']},
  io_trace: {devices: [{block_layer: true, events: ['nvme_setup_cmd -> nvme_complete_rq', 'block_rq_insert -> block_rq_complete']}]}};
const hdd = {flush: {events: ['block_rq_issue -> block_rq_complete']},
  io_trace: {devices: [{events: ['block_rq_issue -> block_rq_complete']}]}};
// static reference: every ftrace metric names its tracepoints, NVMe vs the rest
for (const m of [flush, iolat, blk, lat]) {
  const r = ftraceRef(m);
  assert.ok(r.includes('->') && r.includes('_complete'), m.key);
}
assert.ok(ftraceRef(flush).includes('nvme_setup_cmd -> nvme_complete_rq') && ftraceRef(flush).includes('block_rq_issue -> block_rq_complete') && ftraceRef(flush).includes('rwbs F'));
assert.ok(ftraceRef(iolat).includes('NVMe') && ftraceRef(iolat).includes('any other device'));
assert.ok(ftraceRef(blk).includes('block_rq_insert -> block_rq_complete') && ftraceRef(blk).includes('block_rq_merge'));
assert.ok(ftraceRef(lat).includes(ftraceRef(iolat)) && ftraceRef(lat).includes(ftraceRef(blk)));
assert.strictEqual(ftraceRef(iops), '');
// monitored events: union, per kind, in first-seen order
assert.deepStrictEqual(ftraceEvents([nv, hdd], flush), ['nvme_setup_cmd -> nvme_complete_rq', 'block_rq_issue -> block_rq_complete']);
assert.deepStrictEqual(ftraceEvents([nv, hdd], iolat), ['nvme_setup_cmd -> nvme_complete_rq', 'block_rq_issue -> block_rq_complete']);
assert.deepStrictEqual(ftraceEvents([nv, hdd], blk), ['block_rq_insert -> block_rq_complete']);
assert.deepStrictEqual(ftraceEvents([hdd], blk), []);
assert.deepStrictEqual(ftraceEvents([nv], lat), ['nvme_setup_cmd -> nvme_complete_rq', 'block_rq_insert -> block_rq_complete']);
assert.deepStrictEqual(ftraceEvents([nv], iops), []);
assert.deepStrictEqual(ftraceEvents(undefined, flush), []);
// the badge title: the source, the static reference, then what the data monitored
let t = srcBadge(flush, [hdd]).title;
assert.ok(t.startsWith('Measured from kernel ftrace events (device Flush commands)'), t);
assert.ok(t.includes('\nKernel tracepoints: NVMe: nvme_setup_cmd -> nvme_complete_rq'), t);
assert.ok(t.endsWith('\nMonitored in the shown runs: block_rq_issue -> block_rq_complete'), t);
assert.ok(!srcBadge(flush, []).title.includes('Monitored'), 'no data, no monitored line');
assert.ok(!srcBadge(flush).title.includes('Monitored'));
t = srcBadge(blk, [nv]).title;
assert.ok(t.includes('block_rq_insert -> block_rq_complete') && t.endsWith('Monitored in the shown runs: block_rq_insert -> block_rq_complete'), t);
t = srcBadge(lat, [nv]).title;
assert.ok(t.startsWith('Measured by fio\nDashed ftrace lines, kernel tracepoints: device: '), t);
assert.strictEqual(srcBadge(iops, [nv]).title, 'Measured by fio');
console.log('OK');
`)
}

// Normal mode of an array phase: every member and the array are drawn, as
// plain unfilled lines at their own values; the extent is the largest single
// value, not a sum, and hidden series leave it.
func TestGraphsPageNormalModeDrawsMembersAndArray(t *testing.T) {
	node := requireNode(t)
	body := graphsPageBody(t)
	var js strings.Builder
	for _, f := range []string{"rampOf", "windowOf", "fioShift", "memberNames", "memberStep", "memberKey",
		"memberGrid", "memberLabel", "hasDir", "rgba", "stackedMemberDatasets", "arrayDatasets",
		"iolatKind", "iolatPoints", "ftraceDatasets", "memberColors", "memberMetric", "memberLatencyDatasets", "ftraceOnGrid",
		"isShown", "setShown", "chartDatasets", "phaseExtent"} {
		js.WriteString(jsFunction(t, body, f))
		js.WriteString("\n")
	}
	js.WriteString(`
const assert = require('assert');
const HIDDEN = {}, AXIS_HEADROOM = 1.15, MEMBER_PALETTE = [[1,2,3],[4,5,6]];
const pts = (v, w) => [1, 2, 3].map(t => ({t, r: v, w: w}));
const d = { ramp_sec: 0, log_avg_msec: 1000,
    ts_iops: pts(500, 100),
    ts_members: { sdb: {iops: pts(300, 40), lat: pts(2, 3)}, sdc: {iops: pts(200, 60), lat: pts(4, 5)} } };
const IOPS = {key: 'iops', tsKey: 'ts_iops'}, LAT = {key: 'lat', tsKey: 'ts_lat'};
const labels = cd => cd.datasets.map(x => x.label);
let cd = chartDatasets(d, IOPS, 'normal');
assert.strictEqual(cd.stacked, false);
assert.ok(!cd.bars);
assert.strictEqual(cd.datasets.filter(x => x.member === 'sdb').length, 2);
assert.strictEqual(cd.datasets.filter(x => x.member === 'sdc').length, 2);
assert.deepStrictEqual(cd.datasets.filter(x => x.array).map(x => x.label), ['Array read (fio)', 'Array write (fio)']);
cd.datasets.forEach(x => { assert.ok(!x.stack, x.label + ' stacked'); assert.ok(!x.fill, x.label + ' filled'); });
// Each member at its own value, writes mirrored; zeros kept.
assert.deepStrictEqual(cd.datasets.find(x => x.member === "sdb" && x.dir === "r").data.filter(p => p.y !== null).map(p => p.y), [300, 300, 300]);
assert.deepStrictEqual(cd.datasets.find(x => x.member === "sdc" && x.dir === "w").data.filter(p => p.y !== null).map(p => p.y), [-60, -60, -60]);
// Same series keys and colours as Stacked.
const st = chartDatasets(d, IOPS, 'stacked');
assert.deepStrictEqual(cd.datasets.map(x => x.skey), st.datasets.map(x => x.skey));
assert.deepStrictEqual(cd.datasets.map(x => x.borderColor), st.datasets.map(x => x.borderColor));
assert.strictEqual(st.stacked, true);
// Extent: the largest single value (500 array), not 500 = 300 + 200 sum.
let e = phaseExtent([d], IOPS, 'normal');
assert.strictEqual(e.yHigh, 500 * AXIS_HEADROOM);
assert.strictEqual(e.yLow, -100 * AXIS_HEADROOM);
// Hidden array lines leave the extent: members alone, max 300 (not 500).
setShown('iops', 'fio.r', false); setShown('iops', 'fio.w', false);
e = phaseExtent([d], IOPS, 'normal');
assert.strictEqual(e.yHigh, 300 * AXIS_HEADROOM);
assert.strictEqual(e.yLow, -60 * AXIS_HEADROOM);
// A hidden member is not built; the mode switch keeps the state.
setShown('iops', 'member:sdb', false);
assert.ok(!chartDatasets(d, IOPS, 'normal').datasets.some(x => x.member === 'sdb'));
assert.strictEqual(phaseExtent([d], IOPS, 'normal').yHigh, 200 * AXIS_HEADROOM);
assert.ok(!chartDatasets(d, IOPS, 'stacked').datasets.some(x => x.member === 'sdb'));
// Latency: Normal and Stacked are the same lines, never stacked.
setShown('iops', 'member:sdb', true);
const ln = chartDatasets(d, LAT, 'normal'), ls = chartDatasets(d, LAT, 'stacked');
assert.strictEqual(ln.stacked, false);
assert.deepStrictEqual(ln.datasets.map(x => x.skey), ls.datasets.map(x => x.skey));
console.log('OK');
`)
	cmd := exec.Command(node)
	cmd.Stdin = strings.NewReader(js.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The members chart builds the tooltip's Stack total lines only when stacked.
func TestGraphsPageNormalModeNoStackTotals(t *testing.T) {
	body := graphsPageBody(t)
	mc := jsFunction(t, body, "makeMembersChart")
	if !strings.Contains(mc, "const totals = stacked ? g.totals : {};") {
		t.Errorf("Stack total lines are not limited to stacked mode:\n%s", mc)
	}
}

// bioNodeEntry is an md0 target whose node is bio-based: its own series are
// the bios (kind bio), its members' the device and block-layer requests.
const bioNodeJS = `
const md0 = (scale) => ({log_avg_msec: 1000, kind: 'array',
    io_trace: {text: 'ftrace: md0: block_bio_queue -> block_bio_complete', devices: [
        {name: 'md0', source: 'bio', bio_based: true, block_layer: false, events: ['block_bio_queue -> block_bio_complete'],
         block_layer_note: 'bio-based (no request queue): no block-layer series; bio-level latency (block_bio_queue -> block_bio_complete) on the Device latency card',
         stats: {bio: {r: {count: 4, mean_ns: 300 * scale * 1000}, w: {count: 4, mean_ns: 900 * scale * 1000}}}},
        {name: 'sdb', source: 'block', block_layer: true, events: ['block_rq_issue -> block_rq_complete', 'block_rq_insert -> block_rq_complete'],
         stats: {device: {r: {count: 2}}}}]},
    ts_iolat: [pt(0, 'bio', 'r', 4, 300 * scale, 500 * scale), pt(0, 'bio', 'w', 4, 900 * scale, 1500 * scale),
        pt(1, 'bio', 'w', 2, 800 * scale, 1000 * scale)],
    ts_iolat_members: {sdb: [pt(0, 'device', 'r', 2, 50 * scale, 60 * scale), pt(0, 'blocklayer', 'r', 2, 55 * scale, 65 * scale)]}});
`

// TestGraphsPageBioNodeSeries: the Device latency card of a bio-based array
// target draws the node's bio mean and p99 in the array's fio colours next to
// its members' device lines, the Latency card overlays the bio mean, the
// Block-layer card draws the members only (the node says it has none), and
// toggles and the y extent follow the new series.
func TestGraphsPageBioNodeSeries(t *testing.T) {
	runPageJS(t, append(append([]string{}, ftraceFuncs...), "iolatNodeNote", "setShown"), ftraceTestPrelude+bioNodeJS+`
const d = md0(1);
const io = ftraceDatasets(d, IOLAT);
assert.deepStrictEqual(io.map(x => x.label), [
    'Array read bio mean (ftrace)', 'Array read bio p99 (ftrace)',
    'Array write bio mean (ftrace)', 'Array write bio p99 (ftrace)',
    'sdb read device mean (ftrace)']);
assert.deepStrictEqual(io.map(x => x.skey), ['ftrace.bio.mean.r', 'ftrace.bio.p99.r', 'ftrace.bio.mean.w', 'ftrace.bio.p99.w', 'ftrace.member:sdb']);
assert.deepStrictEqual(io.slice(0, 4).map(x => x.borderDash), [[6, 3], [8, 3, 2, 3], [6, 3], [8, 3, 2, 3]]);
assert.ok(io[0].borderColor.startsWith('rgba(37,99,235') && io[2].borderColor.startsWith('rgba(220,38,38'));
assert.ok(io.slice(0, 4).every(x => x.borderWidth === 2 && x.fill === false));
assert.deepStrictEqual(io[2].data, [{x: 0, y: -900}, {x: 1, y: -800}]);
assert.strictEqual(io[1].data[0].y, 500);
// the Latency card: the node's bio mean as a dashed line
const lat = ftraceDatasets(d, LAT);
assert.deepStrictEqual(lat.map(x => x.label), ['Array read bio mean (ftrace)', 'Array write bio mean (ftrace)']);
assert.deepStrictEqual(lat[0].borderDash, [6, 3]);
// the Block-layer card: members only, and the node's note
assert.deepStrictEqual(ftraceDatasets(d, IOLATBLK).map(x => x.label), ['sdb read block layer mean (ftrace)']);
assert.strictEqual(iolatNone(d, IOLATBLK), '');
assert.strictEqual(iolatNodeNote(d, IOLATBLK), 'md0: ' + d.io_trace.devices[0].block_layer_note);
assert.strictEqual(iolatNodeNote(d, IOLAT), '');
assert.strictEqual(iolatNodeNote(entry('nvme', 1), IOLATBLK), '');
// without members the node's bio series still fills the Device card
const alone = md0(1);
delete alone.ts_iolat_members;
assert.strictEqual(iolatNone(alone, IOLAT), '');
assert.strictEqual(iolatNone(alone, IOLATBLK), 'no block-layer series: ' + alone.io_trace.devices[0].block_layer_note);
// a request-based drive never shows a bio line
assert.ok(!ftraceDatasets(entry('block', 1), IOLAT).some(x => x.skey.indexOf('bio') !== -1));
// y extent: the bio p99 counts, and hiding it recomputes the range
const close = (a, b) => assert.ok(Math.abs(a - b) < 1e-9, a + ' != ' + b);
let e = phaseExtent([d], IOLAT, 'normal');
close(e.yHigh, 500 * 1.15);
close(e.yLow, -1500 * 1.15);
setShown('iolat', 'ftrace.bio.p99.w', false);
e = phaseExtent([d], IOLAT, 'normal');
close(e.yLow, -900 * 1.15);
assert.ok(!chartDatasets(d, IOLAT, 'normal').datasets.some(x => x.skey === 'ftrace.bio.p99.w'));
setShown('iolat', 'ftrace.bio.p99.w', true);
// HDD-scale latencies (seconds) keep their SI scale
const slow = md0(1e4);
const sc = siScale(IOLAT, 1.5e7);
assert.strictEqual(sc.prefix, '');
assert.strictEqual(fmtScaled(toBase(ftraceDatasets(slow, IOLAT), sc)[1].data[0].y, sc, true), '5.00');
console.log('OK');
`)
}

// TestGraphsPageBioNodeCharts: the Device latency chart of a bio-based node
// draws its bio lines in seconds for an HDD array, with two decimals.
func TestGraphsPageBioNodeCharts(t *testing.T) {
	runPageJS(t, chartFuncs, chartStubJS+`
const IOLAT = {key: 'iolat', unit: 'μs', tsKey: 'ts_iolat', sumR: d => 0, sumW: d => 0};
const pt = (t, kind, dir, n, mean, p99) => ({t, kind, dir, n, mean, p50: mean, p99});
const d = {log_avg_msec: 1000, ts_iolat: [pt(0, 'bio', 'r', 3, 2.5e6, 4e6)]};
makeChart({}, d, IOLAT, 0, 4.6e6);
assert.deepStrictEqual(last.data.datasets.map(x => x.label), ['Array read bio mean (ftrace)', 'Array read bio p99 (ftrace)']);
assert.strictEqual(yTitle(), 's');
const label = (k, i) => last.options.plugins.tooltip.callbacks.label({dataset: last.data.datasets[k], raw: last.data.datasets[k].data[i]});
assert.strictEqual(label(1, 0), 'Array read bio p99 (ftrace): 4.00 s');
console.log('OK');
`)
}

// The tracepoint reference of the badges names the bio pair of a bio-based
// node for the Device latency, Latency and Flush metrics.
func TestGraphsPageSrcBadgeBioNode(t *testing.T) {
	runPageJS(t, []string{"ftraceRef", "ftraceEvents"}, `
const assert = require('assert');
for (const key of ['iolat', 'lat', 'flush']) {
    const r = ftraceRef({key});
    assert.ok(r.includes('bio-based md/dm node') && r.includes('block_bio_queue -> block_bio_complete'), key + ': ' + r);
}
assert.ok(ftraceRef({key: 'iolatblk'}).includes('none on a bio-based md/dm node'));
const md = {io_trace: {devices: [{block_layer: false, events: ['block_bio_queue -> block_bio_complete']},
    {block_layer: true, events: ['block_rq_issue -> block_rq_complete', 'block_rq_insert -> block_rq_complete']}]},
    flush: {events: ['block_bio_queue -> block_bio_complete']}};
assert.deepStrictEqual(ftraceEvents([md], {key: 'iolat'}), ['block_bio_queue -> block_bio_complete', 'block_rq_issue -> block_rq_complete']);
assert.deepStrictEqual(ftraceEvents([md], {key: 'iolatblk'}), ['block_rq_insert -> block_rq_complete']);
assert.deepStrictEqual(ftraceEvents([md], {key: 'flush'}), ['block_bio_queue -> block_bio_complete']);
console.log('OK');
`)
}
