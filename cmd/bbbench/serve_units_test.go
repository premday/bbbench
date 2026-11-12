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

// TestChartUnitScale: siScale converts a metric to its base unit (bytes/s
// from MiB/s, IOPS, seconds from us) and picks the SI prefix that puts the
// maximum in [1, 1000); fmtScaled prints a base-unit value as the number with
// the prefix letter, no space, trailing zeros trimmed.
func TestChartUnitScale(t *testing.T) {
	runPageJS(t, []string{"siScale", "fmtScaled"}, `
const assert = require('assert');
const BW = {unit: 'MB/s'}, IOPS = {unit: 'IOPS'}, LAT = {unit: '\u03bcs'};
const u = (m, max) => { const s = siScale(m, max); return s.prefix + '|' + s.unit; };
// The formatted maximum: the data value converted to base units.
const f = (m, max) => { const s = siScale(m, max); return fmtScaled(max * s.mul, s); };
assert.strictEqual(u(BW, 0), '|bytes/s');
assert.strictEqual(u(BW, NaN), '|bytes/s');
assert.strictEqual(siScale(BW, 1).mul, 1048576);
assert.strictEqual(f(BW, 900), '944M');
assert.strictEqual(f(BW, 2300), '2.41G');
assert.strictEqual(f(BW, 2e6), '2.1T');
assert.strictEqual(u(BW, 1e-6), '|bytes/s');
assert.strictEqual(f(IOPS, 999), '999');
assert.strictEqual(f(IOPS, 1000), '1k');
assert.strictEqual(f(IOPS, 1.2e6), '1.2M');
assert.strictEqual(u(IOPS, 0.5), '|IOPS');
assert.strictEqual(f(IOPS, 0.5), '0.5');
assert.strictEqual(u(LAT, 0), '|s');
assert.strictEqual(f(LAT, 0.5), '500n');
assert.strictEqual(f(LAT, 800), '800\u03bc');
assert.strictEqual(f(LAT, 1000), '1m');
assert.strictEqual(f(LAT, 2500), '2.5m');
assert.strictEqual(f(LAT, 2e6), '2');
assert.strictEqual(f(LAT, 5e9), '5000');
assert.strictEqual(f(LAT, 1e-5), '0.01n');
assert.strictEqual(u({unit: 'furlongs'}, 5000), 'k|furlongs');
// Precision: 0 decimals from 100, 1 from 10, 2 from 1, 2 significant digits below.
const G = {mul: 1, div: 1e9, prefix: 'G', unit: 'bytes/s'}, N = {mul: 1, div: 1, prefix: '', unit: 'IOPS'};
[[0, G, '0'], [950, N, '950'], [2e9, G, '2G'], [1.5e9, G, '1.5G'], [-1.5e9, G, '1.5G'], [1.234e9, G, '1.23G'],
 [12.34e9, G, '12.3G'], [123.4e9, G, '123G'], [0.25e9, G, '0.25G'], [12.5, N, '12.5'], [0.333, N, '0.33']]
    .forEach(([v, sc, want]) => assert.strictEqual(fmtScaled(v, sc), want, String(v)));
[[0, G, '0.00'], [950, N, '950.00'], [2e9, G, '2.00G'], [1.5e9, G, '1.50G'], [123.4e9, G, '123.40G'], [0.0033, N, '0.0033'], [-1.5e9, G, '1.50G']]
    .forEach(([v, sc, want]) => assert.strictEqual(fmtScaled(v, sc, true), want, 'fixed ' + v));
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

var chartFuncs = []string{"rampOf", "windowOf", "fioShift", "siScale", "fmtScaled", "toBase", "phaseScale", "tsWarn", "memberMetric",
	"memberNames", "memberColors", "memberStep", "memberKey", "memberGrid", "memberLabel", "hasDir", "rgba",
	"stackedMemberDatasets", "memberLatencyDatasets", "arrayDatasets", "makeMembersChart",
	"mdMetric", "hasMD", "mdBusy", "mdActions", "mdBands", "mdIndex", "mdTipLines", "mdMarkers", "mdOptions",
	"chartPlugins", "yAxisTitle", "makeFlushChart", "makeChart"}

// TestGraphsPageChartPhaseScale: a card plots its values in the base unit,
// its ticks and tooltips carry the SI prefix chosen from the phase's data
// maximum (yLow/yHigh without the axis headroom), and its y axis is titled
// with the base unit.
func TestGraphsPageChartPhaseScale(t *testing.T) {
	runPageJS(t, chartFuncs, chartStubJS+`
const BW = {key: 'bw', unit: 'MB/s', tsKey: 'ts_bw', sumR: d => d.r || 0, sumW: d => d.w || 0};
const IOPS = {key: 'iops', unit: 'IOPS', tsKey: 'ts_iops', sumR: d => 0, sumW: d => 0};
const LAT = {key: 'lat', unit: '\u03bcs', tsKey: 'ts_lat', sumR: d => 0, sumW: d => 0};
const FLUSH = {key: 'flush', unit: '\u03bcs', tsKey: 'ts_flush', sumR: d => 0, sumW: d => 2500};
const MiB = 1048576;
const pt = (k, i) => last.data.datasets[k].data[i];
const label = (k, i) => last.options.plugins.tooltip.callbacks.label({dataset: last.data.datasets[k], raw: pt(k, i)});
// A bandwidth phase of 2300 MiB/s reads and 1500 MiB/s writes: G.
makeChart({}, {ts_bw: [{t: 1, r: 2000, w: 1500}, {t: 2, r: 2300, w: 0}]}, BW, -1725, 2645);
assert.strictEqual(yTitle(), 'bytes/s');
assert.strictEqual(last.options.scales.y.max, 2645 * MiB);
assert.strictEqual(last.options.scales.y.min, -1725 * MiB);
assert.strictEqual(pt(0, 1).y, 2300 * MiB);
assert.strictEqual(tick(2e9), '2G');
assert.strictEqual(tick(1.5e9), '1.5G');
assert.strictEqual(tick(-1.5e9), '1.5G');
assert.strictEqual(label(0, 1), 'Read (fio): 2.41G bytes/s');
assert.strictEqual(label(1, 0), 'Write (fio): 1.57G bytes/s');
// A 900 MiB/s peak with 15% headroom on the axis (1035) stays M.
makeChart({}, {ts_bw: [{t: 1, r: 900, w: 0}]}, BW, 0, 1035);
assert.strictEqual(tick(9e8), '900M');
assert.strictEqual(label(0, 0), 'Read (fio): 943.72M bytes/s');
// IOPS of 1.2 M, latency of 2500 us.
makeChart({}, {ts_iops: [{t: 1, r: 1.2e6, w: 0}]}, IOPS, 0, 1.38e6);
assert.strictEqual(yTitle(), 'IOPS');
assert.strictEqual(tick(1e6), '1M');
assert.strictEqual(label(0, 0), 'Read (fio): 1.20M IOPS');
makeChart({}, {ts_lat: [{t: 1, r: 2500, w: 0}]}, LAT, 0, 2875);
assert.strictEqual(yTitle(), 's');
assert.strictEqual(tick(0.002), '2m');
assert.strictEqual(label(0, 0), 'Read (fio): 2.50m s');
// Bar fallback (no series).
makeChart({}, {r: 2300}, BW, 0, 2645);
assert.strictEqual(yTitle(), 'bytes/s');
assert.strictEqual(last.options.scales.y.max, 2645 * MiB);
assert.strictEqual(last.options.plugins.tooltip.callbacks.label({raw: last.data.datasets[0].data[0]}), '2.41G bytes/s');
// Flush latency: the series and the bar.
makeChart({}, {ts_flush: [{t: 0, w: 2500}], flush: {avg_msec: 1000}}, FLUSH, 0, 2875);
assert.strictEqual(yTitle(), 's');
assert.ok(label(0, 0).startsWith('Flush latency: 2.50m s'), label(0, 0));
makeChart({}, {}, FLUSH, 0, 2875);
assert.strictEqual(yTitle(), 's');
assert.strictEqual(tick(0.0025), '2.5m');
assert.strictEqual(last.options.plugins.tooltip.callbacks.label({raw: last.data.datasets[0].data[0]}), '2.50m s');
// Members: member lines, stack totals and the array line.
const d = {log_avg_msec: 1000, ts_bw: [{t: 1, r: 2300, w: 0}],
    ts_members: {sdb: {bw: [{t: 0, r: 1200, w: 0}]}, sdc: {bw: [{t: 0, r: 1100, w: 0}]}}};
makeChart({}, d, BW, 0, 2645);
assert.strictEqual(yTitle(), 'bytes/s');
assert.strictEqual(tick(1e9), '1G');
const cb = last.options.plugins.tooltip.callbacks;
const sdb = last.data.datasets.find(x => x.member === 'sdb' && x.dir === 'r');
assert.strictEqual(cb.label({dataset: sdb, raw: sdb.data.find(p => p.y !== null)}), sdb.label + ': 1.26G bytes/s');
const arr = last.data.datasets.find(x => x.array && x.dir === 'r');
assert.strictEqual(cb.label({dataset: arr, raw: arr.data.find(p => p.y !== null)}), arr.label + ': 2.41G bytes/s');
const lines = [].concat(cb.afterBody([{chart: {isDatasetVisible: () => false}, dataIndex: 0, raw: {x: 0}}]));
assert.ok(lines.some(l => l === 'Stack total read: 2.41G bytes/s'), JSON.stringify(lines));
// Member latency in seconds.
const dl = {log_avg_msec: 1000, ts_lat: [{t: 1, r: 2500, w: 0}],
    ts_members: {sdb: {lat: [{t: 0, r: 800, w: 0}]}}};
makeChart({}, dl, LAT, 0, 2875);
assert.strictEqual(yTitle(), 's');
const ml = last.data.datasets.find(x => x.member === 'sdb');
assert.strictEqual(last.options.plugins.tooltip.callbacks.label({dataset: ml, raw: ml.data.find(p => p.y !== null)}), ml.label + ': 0.80m s');
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
		if !strings.Contains(body, "siScale(metric,") || strings.Contains(body, "unitScale(") ||
			!strings.Contains(body, "toBase(") {
			t.Errorf("%s page does not scale the unit", name)
		}
	}
	if !strings.Contains(jsFunction(t, pages["graphs"], "yAxisTitle"), "text: sc.unit") ||
		strings.Count(pages["graphs"], "title: yAxisTitle(sc)") != 5 {
		t.Error("graphs page cards have no y axis unit title")
	}
	if strings.Count(pages["compare"], "text: axisTitle(metric, sc)") != 2 {
		t.Error("compare page axis title does not use the scaled unit")
	}
}

// TestComparePageScale: the compare chart takes its SI prefix from the
// largest value it plots.
func TestComparePageScale(t *testing.T) {
	runJS(t, comparePageBody(t), []string{"siScale", "fmtScaled", "toBase", "chartScale", "axisTitle"}, `
const assert = require('assert');
const BW = {key: 'bw', unit: 'MB/s'};
const sc = chartScale(BW, [{data: [{x: 0, y: 900}]}, {data: [{x: 0, y: 2300}]}]);
assert.strictEqual(sc.prefix + '|' + sc.unit, 'G|bytes/s');
assert.strictEqual(chartScale(BW, [{data: [400, 900]}]).prefix, 'M');
assert.strictEqual(chartScale(BW, [{data: [1200, 900]}]).prefix, 'G');
assert.strictEqual(chartScale(BW, []).prefix, '');
// toBase converts points and bar numbers in place, keeps nulls.
const ds = [{data: [{x: 0, y: 2}, {x: 1, y: null}]}, {data: [3]}];
toBase(ds, {mul: 10});
assert.deepStrictEqual(ds, [{data: [{x: 0, y: 20}, {x: 1, y: null}]}, {data: [30]}]);
// The axis title does not repeat itself.
assert.strictEqual(axisTitle({label: 'IOPS'}, {unit: 'IOPS'}), 'IOPS');
assert.strictEqual(axisTitle({label: 'Bandwidth'}, {unit: 'bytes/s'}), 'Bandwidth (bytes/s)');
console.log('OK');
`)
}
