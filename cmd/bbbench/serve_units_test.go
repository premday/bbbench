package main

import (
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// comparePageBody renders the compare page for a stored result.
func comparePageBody(t *testing.T) string {
	t.Helper()
	globalResultsStore.AddResult(&BenchmarkResult{ID: "units-page", Timestamp: time.Now(), Phases: []PhaseResult{{}}})
	defer globalResultsStore.Clear()
	w := httptest.NewRecorder()
	handleComparePage(w, httptest.NewRequest("GET", "/compare?id=units-page", nil))
	return w.Body.String()
}

// runJS runs the named functions of body plus script in node and expects it
// to print OK.
func runJS(t *testing.T, body string, funcs []string, script string) {
	t.Helper()
	node := requireNode(t)
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

// TestChartUnitScale: the shared scaler picks the largest unit whose divisor
// is at most the maximum, and fmtScaled prints the scaled value with a
// precision that depends on its size, trailing zeros trimmed.
func TestChartUnitScale(t *testing.T) {
	runPageJS(t, []string{"unitScale", "fmtScaled"}, `
const assert = require('assert');
const u = (unit, max) => { const s = unitScale(unit, max); return s.unit + '/' + s.div; };
assert.strictEqual(u('MB/s', 0), 'MB/s/1');
assert.strictEqual(u('MB/s', NaN), 'MB/s/1');
assert.strictEqual(u('MB/s', 999), 'MB/s/1');
assert.strictEqual(u('MB/s', 1000), 'GB/s/1000');
assert.strictEqual(u('MB/s', 2048), 'GB/s/1000');
assert.strictEqual(u('MB/s', 2e6), 'TB/s/1000000');
assert.strictEqual(u('IOPS', 999), 'IOPS/1');
assert.strictEqual(u('IOPS', 1e3), 'kIOPS/1000');
assert.strictEqual(u('IOPS', 1.2e6), 'MIOPS/1000000');
assert.strictEqual(u('μs', 800), 'μs/1');
assert.strictEqual(u('μs', 1500), 'ms/1000');
assert.strictEqual(u('μs', 2e6), 's/1000000');
assert.strictEqual(u('furlongs', 5000), 'furlongs/1');
const G = {div: 1000, unit: 'GB/s'}, B = {div: 1, unit: 'MB/s'};
const f = [[0, B, '0'], [950, B, '950'], [2000, G, '2'], [1500, G, '1.5'], [2300, G, '2.3'],
    [1234, G, '1.23'], [12345, G, '12.3'], [123456, G, '123'], [250, G, '0.25'], [600, G, '0.6'],
    [-1500, G, '1.5'], [12.5, B, '12.5'], [0.333, B, '0.33']];
f.forEach(([v, sc, want]) => assert.strictEqual(fmtScaled(v, sc), want, v + ' ' + sc.unit));
console.log('OK');
`)
}

// chartStubJS replaces Chart.js: it keeps the last config.
const chartStubJS = `
const assert = require('assert');
let last = null;
function Chart(canvas, cfg) { last = cfg; this.cfg = cfg; }
// rampPlugin comes with fioShift's source.
const mdPlugin = {id: 'mdTrack'};
const MEMBER_PALETTE = [[22, 163, 74], [217, 119, 6], [124, 58, 237]];
let X_MAX = 0;
const AXIS_HEADROOM = 1.15;
const tick = v => last.options.scales.y.ticks.callback(v);
const yTitle = () => last.options.scales.y.title && last.options.scales.y.title.display && last.options.scales.y.title.text;
`

var chartFuncs = []string{"rampOf", "windowOf", "fioShift", "unitScale", "fmtScaled", "phaseScale", "tsWarn", "memberMetric",
	"memberNames", "memberColors", "memberStep", "memberKey", "memberGrid", "memberLabel", "hasDir", "rgba",
	"stackedMemberDatasets", "memberLatencyDatasets", "arrayDatasets", "makeMembersChart",
	"mdMetric", "hasMD", "mdBusy", "mdActions", "mdBands", "mdIndex", "mdTipLines", "mdMarkers", "mdOptions",
	"chartPlugins", "yAxisTitle", "makeFlushChart", "makeChart"}

// TestGraphsPageChartPhaseScale: a card's ticks, tooltips and y axis title
// use the unit chosen from the phase's scale (yLow/yHigh), not the metric's
// base unit.
func TestGraphsPageChartPhaseScale(t *testing.T) {
	runPageJS(t, chartFuncs, chartStubJS+`
const BW = {key: 'bw', unit: 'MB/s', tsKey: 'ts_bw', sumR: d => d.r || 0, sumW: d => d.w || 0};
const IOPS = {key: 'iops', unit: 'IOPS', tsKey: 'ts_iops', sumR: d => 0, sumW: d => 0};
const LAT = {key: 'lat', unit: 'μs', tsKey: 'ts_lat', sumR: d => 0, sumW: d => 0};
const FLUSH = {key: 'flush', unit: 'μs', tsKey: 'ts_flush', sumR: d => 0, sumW: d => 2500};
const ts = [{t: 1, r: 2000, w: 1500}, {t: 2, r: 2300, w: 0}];
// Bandwidth phase whose scale reaches 2300 MB/s: GB/s.
makeChart({}, {ts_bw: ts}, BW, -1725, 2300);
assert.strictEqual(tick(2000), '2');
assert.strictEqual(tick(1500), '1.5');
assert.strictEqual(tick(-1500), '1.5');
assert.strictEqual(yTitle(), 'GB/s');
const lbl = last.options.plugins.tooltip.callbacks.label;
assert.strictEqual(lbl({raw: {x: 1, y: 2300}}), 'Read: 2.3 GB/s');
assert.strictEqual(lbl({raw: {x: 1, y: -1500}}), 'Write: 1.5 GB/s');
// The same drive in a phase with a small scale keeps MB/s.
makeChart({}, {ts_bw: [{t: 1, r: 400, w: 0}]}, BW, 0, 460);
assert.strictEqual(yTitle(), 'MB/s');
assert.strictEqual(tick(400), '400');
// A 900 MB/s peak has 15% headroom on the axis (1035): the unit follows the
// data, not the axis, so it stays MB/s.
makeChart({}, {ts_bw: [{t: 1, r: 900, w: 0}]}, BW, 0, 1035);
assert.strictEqual(yTitle(), 'MB/s');
assert.strictEqual(tick(900), '900');
// IOPS of 1.2 M, latency of 2500 us.
makeChart({}, {ts_iops: [{t: 1, r: 1.2e6, w: 0}]}, IOPS, 0, 1.38e6);
assert.strictEqual(yTitle(), 'MIOPS');
assert.strictEqual(last.options.plugins.tooltip.callbacks.label({raw: {x: 1, y: 1.2e6}}), 'Read: 1.2 MIOPS');
makeChart({}, {ts_lat: [{t: 1, r: 2500, w: 0}]}, LAT, 0, 2875);
assert.strictEqual(yTitle(), 'ms');
assert.strictEqual(last.options.plugins.tooltip.callbacks.label({raw: {x: 1, y: 2500}}), 'Read: 2.5 ms');
// Bar fallback (no series).
makeChart({}, {r: 2300}, BW, 0, 2645);
assert.strictEqual(yTitle(), 'GB/s');
assert.strictEqual(last.options.plugins.tooltip.callbacks.label({raw: 2300}), '2.3 GB/s');
// Flush latency: the series and the bar.
makeChart({}, {ts_flush: [{t: 0, w: 2500}], flush: {avg_msec: 1000}}, FLUSH, 0, 2875);
assert.strictEqual(yTitle(), 'ms');
assert.ok(last.options.plugins.tooltip.callbacks.label({raw: {x: 0, y: 2500}}).startsWith('Flush latency: 2.5 ms'));
makeChart({}, {}, FLUSH, 0, 2875);
assert.strictEqual(yTitle(), 'ms');
assert.strictEqual(tick(2500), '2.5');
// Members: member lines, stack totals and the array line.
const d = {log_avg_msec: 1000, ts_bw: [{t: 1, r: 2300, w: 0}],
    ts_members: {sdb: {bw: [{t: 0, r: 1200, w: 0}]}, sdc: {bw: [{t: 0, r: 1100, w: 0}]}}};
makeChart({}, d, BW, 0, 2645);
assert.strictEqual(yTitle(), 'GB/s');
assert.strictEqual(tick(1000), '1');
const cb = last.options.plugins.tooltip.callbacks;
assert.strictEqual(cb.label({dataset: {label: 'sdb read'}, raw: {x: 0, y: 1200}}), 'sdb read: 1.2 GB/s');
const lines = [].concat(cb.afterBody([{chart: {isDatasetVisible: () => false}, dataIndex: 0, raw: {x: 0}}]));
assert.ok(lines.some(l => l === 'Stack total read: 2.3 GB/s'), JSON.stringify(lines));
console.log('OK');
`)
}

// TestChartPagesDropFixedUnit: no tick or tooltip on the graphs and compare
// pages prints the metric's fixed base unit.
func TestChartPagesDropFixedUnit(t *testing.T) {
	pages := map[string]string{"graphs": graphsPageBody(t), "compare": comparePageBody(t)}
	for name, body := range pages {
		for _, bad := range []string{"' ' + metric.unit", "callback: fmtTick", "fmtTick(", "'Bandwidth MB/s'",
			"'Bandwidth (MB/s)'", "'Latency \\u03bcs'", "'Latency (\\u03bcs)'"} {
			if strings.Contains(body, bad) && !(bad == "fmtTick(" && strings.Count(body, bad) == 1) {
				t.Errorf("%s page still has %q", name, bad)
			}
		}
		if !strings.Contains(body, "unitScale(metric.unit,") && !strings.Contains(body, "phaseScale(metric,") {
			t.Errorf("%s page does not scale the unit", name)
		}
	}
	if !strings.Contains(jsFunction(t, pages["graphs"], "yAxisTitle"), "text: sc.unit") ||
		strings.Count(pages["graphs"], "title: yAxisTitle(sc)") != 5 {
		t.Error("graphs page cards have no y axis unit title")
	}
	if !strings.Contains(pages["compare"], "metric.label + ' (' + sc.unit + ')'") {
		t.Error("compare page axis title does not use the scaled unit")
	}
}

// TestComparePageScale: the compare chart takes its unit from the largest
// value it plots.
func TestComparePageScale(t *testing.T) {
	runJS(t, comparePageBody(t), []string{"unitScale", "fmtScaled", "chartScale"}, `
const assert = require('assert');
const BW = {key: 'bw', unit: 'MB/s'};
assert.strictEqual(chartScale(BW, [{data: [{x: 0, y: 900}]}, {data: [{x: 0, y: 2300}]}]).unit, 'GB/s');
assert.strictEqual(chartScale(BW, [{data: [400, 900]}]).unit, 'MB/s');
assert.strictEqual(chartScale(BW, [{data: [1200, 900]}]).unit, 'GB/s');
assert.strictEqual(chartScale(BW, []).unit, 'MB/s');
console.log('OK');
`)
}
