package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bbbench/internal/blockdev"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// The graphs and compare pages for ?group= read the group's data; an unknown
// group is a 404 and a missing one a 400.
func TestGraphsPage_GroupParam(t *testing.T) {
	seedGroup(t)
	for _, p := range []struct {
		name string
		h    func() func(string) (int, string)
	}{
		{"graphs", func() func(string) (int, string) {
			return func(u string) (int, string) { w := get(handleGraphsPage, u); return w.Code, w.Body.String() }
		}},
		{"compare", func() func(string) (int, string) {
			return func(u string) (int, string) { w := get(handleComparePage, u); return w.Code, w.Body.String() }
		}},
	} {
		call := p.h()
		code, body := call("/" + p.name + "?group=g1")
		if code != 200 || !strings.Contains(body, "/api/groups/graphs?id=g1") {
			t.Errorf("%s ?group=g1: %d, data url missing (%.120s)", p.name, code, body)
		}
		if strings.Contains(body, "/api/results/graphs?id=") {
			t.Errorf("%s ?group=g1 still reads the run data", p.name)
		}
		if code, _ := call("/" + p.name + "?group=nope"); code != 404 {
			t.Errorf("%s unknown group: %d", p.name, code)
		}
		if code, _ := call("/" + p.name + "?group="); code != 400 {
			t.Errorf("%s empty group: %d", p.name, code)
		}
		// A plain run keeps reading its own data.
		code, body = call("/" + p.name + "?id=r1")
		if code != 200 || !strings.Contains(body, "/api/results/graphs?id=r1") {
			t.Errorf("%s ?id=r1: %d", p.name, code)
		}
	}
}

// /api/results carries the group of a member run (and its value); the group
// API lists the group.
func TestBrowseList_Groups(t *testing.T) {
	seedGroup(t)
	globalResultsStore.AddResult(&BenchmarkResult{ID: "plain"})
	var list []map[string]any
	if err := json.Unmarshal(get(handleResultsList, "/api/results").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	by := map[string]map[string]any{}
	for _, e := range list {
		by[e["id"].(string)] = e
	}
	if by["r1"]["group_id"] != "g1" || by["r2"]["group_id"] != "g1" {
		t.Errorf("member runs: %v %v", by["r1"], by["r2"])
	}
	if vals, _ := by["r1"]["setting_values"].([]any); len(vals) != 1 || vals[0] != "enabled" {
		t.Errorf("r1 setting_values = %v", by["r1"]["setting_values"])
	}
	if _, ok := by["plain"]["group_id"]; ok {
		t.Errorf("plain run carries a group: %v", by["plain"])
	}
	if _, ok := by["plain"]["setting_values"]; ok {
		t.Errorf("plain run carries values: %v", by["plain"])
	}
	var groups []map[string]any
	if err := json.Unmarshal(get(handleGroupsList, "/api/groups").Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0]["id"] != "g1" {
		t.Errorf("groups = %v", groups)
	}
}

// The run detail JSON carries the group and the per-drive labels.
func TestResultDetail_ShowsLabels(t *testing.T) {
	seedGroup(t)
	var out struct {
		GroupID string `json:"group_id"`
		Labels  map[string]struct {
			Value string `json:"value"`
			Valid bool   `json:"valid"`
			After *struct {
				Value  string `json:"value"`
				Kernel string `json:"kernel"`
			} `json:"after"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(get(handleResultDetail, "/api/results/detail?id=r2").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	l, ok := out.Labels["sda"]
	if out.GroupID != "g1" || !ok || l.Value != "disabled" || l.Valid || l.After == nil || l.After.Kernel != "write disabled" {
		t.Errorf("detail = %+v", out)
	}
	// The details page has the block that shows them.
	body := get(handleResultDetailPage, "/result?id=r2").Body.String()
	if !strings.Contains(body, `id="group-card"`) {
		t.Error("details page has no group block")
	}
}

// swapGroupResult is one run of a parallel-swap group over sda and sdb.
func swapGroupResult(id string, ts time.Time, values map[string]string, invalid map[string]bool) *BenchmarkResult {
	res := &BenchmarkResult{ID: id, GroupID: "g2", Timestamp: ts, Mode: "parallel-sync", Labels: map[string]rungroup.Label{}}
	ph := PhaseResult{PhaseNumber: 1, PhaseName: "randwrite"}
	for _, dev := range []string{"sda", "sdb"} {
		res.Drives = append(res.Drives, DriveInfo{Device: blockdev.Device{Name: dev, Model: "M", Serial: "S" + dev}})
		ph.Jobs = append(ph.Jobs, JobResult{Device: dev, WriteStats: &IOStats{IOPS: 100, Bandwidth: 1024, AvgLatNS: 1000}})
		res.Labels[dev] = rungroup.Label{Setting: "write_cache", Value: values[dev], Valid: !invalid[dev],
			Before: &settings.Reading{Value: values[dev], Kernel: "<i>k</i>"}, After: &settings.Reading{Value: values[dev]},
			Note: map[bool]string{true: "<b>changed</b> during the run"}[invalid[dev]]}
	}
	res.Phases = []PhaseResult{ph}
	return res
}

// seedSwapGroup stores a parallel-swap group (values "on" and "<b>off</b>",
// two rounds, sdb invalid in round 2) and a plain run "p1".
func seedSwapGroup(t *testing.T) {
	t.Helper()
	globalGroupStore.Clear()
	globalResultsStore.Clear()
	t.Cleanup(func() { globalGroupStore.Clear(); globalResultsStore.Clear() })
	off := "<b>off</b>"
	globalGroupStore.AddGroup(&rungroup.GroupFile{
		ID: "g2", Created: time.Unix(5000, 0), Setting: "write_cache", Strategy: rungroup.ParallelSwap,
		Values: []string{"on", off},
		Runs: []rungroup.GroupRun{
			{RunID: "r10", Round: 0, Status: rungroup.RunDone},
			{RunID: "r11", Round: 1, Status: rungroup.RunInvalid},
		},
	})
	globalResultsStore.AddResult(swapGroupResult("r10", time.Unix(6000, 0), map[string]string{"sda": "on", "sdb": off}, nil))
	globalResultsStore.AddResult(swapGroupResult("r11", time.Unix(7000, 0), map[string]string{"sda": off, "sdb": "on"}, map[string]bool{"sdb": true}))
	globalResultsStore.AddResult(&BenchmarkResult{ID: "p1", Timestamp: time.Unix(8000, 0), Mode: "sequential",
		Drives: []DriveInfo{{Device: blockdev.Device{Name: "sda", Model: "M", Serial: "Ssda"}}},
		Phases: []PhaseResult{{PhaseNumber: 1, PhaseName: "randwrite",
			Jobs: []JobResult{{Device: "sda", WriteStats: &IOStats{IOPS: 10, Bandwidth: 1024, AvgLatNS: 1000}}}}}})
}

// inlineScript returns the page's inline <script> (not the Chart.js one).
func inlineScript(t *testing.T, name, body string) string {
	t.Helper()
	i := strings.Index(body, "<script>")
	j := strings.Index(body[i+1:], "</script>")
	if i < 0 || j < 0 {
		t.Fatalf("%s: no inline script", name)
	}
	return body[i+len("<script>") : i+1+j]
}

// The real graphs, compare, results and run-details pages, dumped through
// their handlers, pass node --check and behave in node (fake DOM) as the
// run-group UI requires; plain-run data is unaffected. Skipped without node.
func TestGroupPages_Scripts(t *testing.T) {
	node := requireNode(t)
	seedSwapGroup(t)
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	page := func(name string, h http.HandlerFunc, u string) string {
		w := get(h, u)
		body := w.Body.String()
		if w.Code != 200 || !strings.HasPrefix(body, "<!DOCTYPE html>") || strings.Contains(body, "%!") {
			t.Fatalf("%s %s: %d %.200s", name, u, w.Code, body)
		}
		p := write(name+".js", inlineScript(t, name, body))
		if out, err := exec.Command(node, "--check", p).CombinedOutput(); err != nil {
			t.Fatalf("node --check %s: %v\n%s", name, err, out)
		}
		return p
	}
	scripts := map[string]string{
		"graphs":       page("graphs", handleGraphsPage, "/graphs?group=g2"),
		"graphs_plain": page("graphs_plain", handleGraphsPage, "/graphs?id=p1"),
		"compare":      page("compare", handleComparePage, "/compare?group=g2"),
		"results":      page("results", handleBrowsePage, "/results"),
		"result":       page("result", handleResultDetailPage, "/result?id=r11"),
	}
	data := map[string]json.RawMessage{
		"group":       get(handleGroupGraphs, "/api/groups/graphs?id=g2").Body.Bytes(),
		"plain":       get(handleResultGraphs, "/api/results/graphs?id=p1").Body.Bytes(),
		"results":     get(handleResultsList, "/api/results").Body.Bytes(),
		"groups":      get(handleGroupsList, "/api/groups").Body.Bytes(),
		"detail":      get(handleResultDetail, "/api/results/detail?id=r11").Body.Bytes(),
		"detailPlain": get(handleResultDetail, "/api/results/detail?id=p1").Body.Bytes(),
	}
	dj, _ := json.Marshal(data)
	dataPath := write("data.json", string(dj))
	harness := write("harness.js", groupPagesHarness)
	for _, c := range []struct{ check, script string }{
		{"graphs", "graphs"}, {"graphs_plain", "graphs_plain"}, {"compare", "compare"},
		{"compare_plain", "compare"}, {"results", "results"}, {"result", "result"},
	} {
		out, err := exec.Command(node, harness, c.check, scripts[c.script], dataPath).CombinedOutput()
		if err != nil {
			t.Errorf("harness %s: %v\n%s", c.check, err, out)
		} else {
			t.Logf("%s: %s", c.check, strings.TrimSpace(string(out)))
		}
	}
}

const groupPagesHarness = `
const fs = require('fs'), vm = require('vm'), assert = require('assert');
const [check, scriptPath, dataPath] = process.argv.slice(2);
const DATA = JSON.parse(fs.readFileSync(dataPath, 'utf8'));
const HTML = [], ELS = [], CHARTS = [], byId = {};
function el(tag) {
  const e = { tag, children: [], style: {}, dataset: {}, className: '', _t: '', _h: '',
    classList: { add() {}, remove() {}, toggle() { return false; } },
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) { this.children = this.children.filter(x => x !== c); },
    replaceChild() {}, addEventListener() {},
    querySelector() { return el('div'); },
    querySelectorAll() { const all = []; (function walk(n) { (n.children || []).forEach(c => { all.push(c); walk(c); }); })(this); return all.filter(c => c.tag === 'input'); },
    set innerHTML(v) { this._h = String(v); this.children = []; HTML.push(String(v)); }, get innerHTML() { return this._h; },
    set textContent(v) { this._t = String(v); this.children = []; },
    get textContent() { return this._t + this.children.map(c => c.textContent || '').join(''); },
  };
  ELS.push(e);
  return e;
}
const ctx = {
  document: {
    getElementById(id) { return byId[id] || (byId[id] = el('div')); },
    createElement: el, createTextNode(t) { return { textContent: String(t) }; },
    addEventListener() {}, querySelector() { return el('div'); },
  },
  fetch: () => new Promise(() => {}), setInterval() {}, setTimeout() {}, console,
  Chart: class { constructor(c, cfg) { CHARTS.push(cfg); } destroy() {} },
};
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(scriptPath, 'utf8'), ctx);
const run = s => vm.runInContext(s, ctx);
ctx.__data = DATA;
const noRawMarkup = () => HTML.forEach(h => assert(!h.includes('<b>off') && !h.includes('<i>k') && !h.includes('<b>changed'), 'unescaped: ' + h));
const titles = () => ELS.filter(e => e.className === 'card-title').map(e => e.textContent.trim());
const listLabels = id => byId[id].children.map(li => li.children[0].textContent.replace(/ /g, '').trim());

switch (check) {
case 'graphs': {
  run('allData = __data.group; initFilters();');
  assert(run('DATA_DIMS.includes("setting")'), 'setting dimension missing for group data');
  assert.strictEqual(byId['group-filters'].style.display, '', 'group filters not shown');
  assert.deepStrictEqual(listLabels('list-setting').sort(), ['<b>off</b>', 'on'], 'setting list: ' + listLabels('list-setting'));
  assert.strictEqual(run('filteredData().length'), 3, 'invalid entry not hidden by default');
  assert(run('filteredData().every(d => !d.invalid)'));
  run('renderAll()');
  let t = titles();
  assert(t.some(x => x.startsWith('sda · write_cache: on · round 1')), 'titles: ' + t);
  assert(!t.some(x => x.includes('(invalid)')), 'invalid shown by default: ' + t);
  run('setShowInvalid(true)');
  assert.strictEqual(run('filteredData().length'), 4, 'toggle does not show the invalid entry');
  ELS.length = 0; run('renderAll()'); t = titles();
  assert(t.some(x => x.startsWith('sdb · write_cache: on · round 2 (invalid)')), 'invalid not flagged: ' + t);
  assert(t.some(x => x.startsWith('sda · write_cache: <b>off</b> · round 2')), 'markup value not kept as text: ' + t);
  run('SEL.setting.delete("on"); setShowInvalid(false)');
  assert(run('filteredData().every(d => d.setting_value === "<b>off</b>")'), 'setting filter not applied');
  noRawMarkup();
  console.log('graphs ok: setting dimension, invalid hidden then shown and flagged, titles', JSON.stringify(t.slice(0, 2)));
  break;
}
case 'graphs_plain': {
  run('allData = __data.plain; initFilters();');
  assert(!run('DATA_DIMS.includes("setting")'), 'setting dimension for a plain run');
  assert(!run('"setting" in DIM_F'));
  assert.strictEqual(byId['group-filters'] && byId['group-filters'].style.display, undefined, 'group filters shown for a plain run');
  assert.strictEqual(run('filteredData().length'), 1);
  run('renderAll()');
  const t = titles();
  assert(t.length > 0 && t.every(x => x === 'sda fio' || x === 'sda ftrace'), 'plain titles changed: ' + t);
  console.log('graphs_plain ok: no setting dimension, titles', JSON.stringify(t));
  break;
}
case 'compare': {
  run('allData = __data.group; initSidebar();');
  assert(run('HAS_SETTING'), 'no setting filter for group data');
  assert.strictEqual(byId['group-filters'].style.display, '');
  assert.deepStrictEqual(listLabels('value-list').sort(), ['<b>off</b>', 'on']);
  assert.strictEqual(run('getEntries().length'), 3, 'invalid not hidden by default');
  run('setShowInvalid(true)');
  assert.strictEqual(run('getEntries().length'), 4, 'toggle does not show invalid');
  const e = run('allData.find(d => d.run_id === "r10" && d.disk === "sda")');
  assert.strictEqual(run('seriesLabel(allData.find(d => d.run_id === "r10" && d.disk === "sda"), 2, 1, RPV)'), 'sda · write_cache: on · round 1');
  assert.strictEqual(run('seriesLabel(allData.find(d => d.run_id === "r11" && d.disk === "sdb"), 2, 1, RPV)'), 'sdb · write_cache: on · round 2 (invalid)');
  assert.strictEqual(run('seriesLabel(allData.find(d => d.run_id === "r10" && d.disk === "sda"), 2, 2, RPV)'), 'sda · randwrite · write_cache: on · round 1');
  // one run per value: no round
  assert.strictEqual(run('seriesLabel(allData[0], 2, 1, {on: 1, "<b>off</b>": 1})'), 'sda · write_cache: on');
  byId['dir-read'] = el('input'); byId['dir-read'].checked = true;
  byId['dir-write'] = el('input'); byId['dir-write'].checked = true;
  run('renderCompare()');
  const labels = CHARTS[CHARTS.length - 1].data.datasets.map(d => d.label);
  assert.strictEqual(labels.length, 4, 'datasets: ' + labels);
  assert(labels.every(l => /· round [12]/.test(l)), 'labels: ' + labels);
  noRawMarkup();
  console.log('compare ok: setting filter, invalid toggle, series', JSON.stringify(labels));
  break;
}
case 'compare_plain': {
  run('allData = __data.plain; initSidebar();');
  assert(!run('HAS_SETTING'));
  assert.strictEqual(byId['group-filters'] && byId['group-filters'].style.display, undefined);
  assert.strictEqual(run('getEntries().length'), 1);
  const d = 'allData[0]';
  assert.strictEqual(run('seriesLabel(' + d + ', 2, 1, RPV)'), 'sda');
  assert.strictEqual(run('seriesLabel(' + d + ', 1, 1, RPV)'), 'randwrite');
  assert.strictEqual(run('seriesLabel(' + d + ', 2, 2, RPV)'), 'sda · randwrite');
  console.log('compare_plain ok: no setting filter, labels unchanged');
  break;
}
case 'results': {
  ctx.__r = DATA.results; ctx.__g = DATA.groups;
  let rows = run('buildRows(__r, __g)');
  assert(rows.includes('<tr class="group-row">'), rows);
  assert(rows.includes('href="/group?id=g2"') && rows.includes('href="/graphs?group=g2"') && rows.includes('href="/compare?group=g2"'), rows);
  assert(rows.includes('<a href="/group?id=g2">Details</a>') && !rows.includes('>Group</a>'), 'group row action is Details: ' + rows);
  assert(rows.includes('write_cache: on, &lt;b&gt;off&lt;/b&gt;'), 'group setting/values: ' + rows);
  assert((rows.match(/class="member" data-group="g2" style="display:none"/g) || []).length === 2, 'members not collapsed: ' + rows);
  assert(rows.includes('group <a href="/group?id=g2">g2</a> · &lt;b&gt;off&lt;/b&gt;, on'), 'member group link/value: ' + rows);
  assert(!rows.includes('<b>off'), 'unescaped value');
  const plainRow = '<tr><td class="timestamp">' + run('formatTimestamp(__r.find(r => r.id === "p1").timestamp)') + '</td><td><code>p1</code></td>' +
    '<td><span class="badge badge-sequential">Sequential</span></td><td class="stats">1 drive(s)</td><td class="stats">1 phase(s)</td>' +
    '<td class="actions"><a href="/result?id=p1">Details</a><a href="/summary?id=p1">Summary</a><a href="/graphs?id=p1">Graphs</a><a href="/compare?id=p1">Compare</a></td></tr>';
  assert(rows.startsWith(plainRow), 'plain row changed: ' + rows.slice(0, 400));
  run('EXPANDED.add("g2")');
  rows = run('buildRows(__r, __g)');
  assert(!rows.includes('display:none') && rows.includes('▾'), 'expanded group still collapsed');
  // without the group API every run is a row of its own
  rows = run('EXPANDED.clear(); buildRows(__r, [])');
  assert(!rows.includes('group-row') && !rows.includes('class="member"') && rows.includes('href="/group?id=g2"'), rows);
  console.log('results ok: group row, collapsed members, links, escaped values, plain row unchanged');
  break;
}
case 'result': {
  ctx.__d = DATA.detail; ctx.__p = DATA.detailPlain;
  const h = run('groupBlockHTML(__d)');
  assert(h.includes('Part of run group <a href="/group?id=g2"><code>g2</code></a>'), h);
  assert(h.includes('<td>sdb</td><td>write_cache</td><td>on</td>') && h.includes('invalid'), h);
  assert(h.includes('<td>sda</td><td>write_cache</td><td>&lt;b&gt;off&lt;/b&gt;</td>') && h.includes('valid'), h);
  assert(h.includes('(kernel: &lt;i&gt;k&lt;/i&gt;)') && h.includes('&lt;b&gt;changed&lt;/b&gt; during the run'), h);
  assert(!h.includes('<b>off') && !h.includes('<i>k') && !h.includes('<b>changed'), 'unescaped: ' + h);
  assert.strictEqual(run('groupBlockHTML(__p)'), '', 'plain run shows a group block');
  console.log('result ok: group link, per-drive value, readings, validity, escaped');
  break;
}
default: throw new Error('unknown check ' + check);
}
`

// layoutPages dumps the real pages of the seeded layout group (status,
// group, graphs, compare, run details of the mdadm and the lvm run), checks
// each is a page (never an error page) whose inline script passes
// node --check, and returns the script paths and the JSON the pages read.
func layoutPages(t *testing.T, node, dir string) (map[string]string, string) {
	t.Helper()
	seedLayoutGroup(t)
	globalGroupStore.AddGroup(serveTestGroup()) // a write-cache group next to it
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	scripts := map[string]string{}
	for _, p := range []struct {
		name string
		h    http.HandlerFunc
		u    string
	}{
		{"status", handleStatusPage, "/status"},
		{"group", handleGroupPage, "/group?id=g3"},
		{"graphs", handleGraphsPage, "/graphs?group=g3"},
		{"compare", handleComparePage, "/compare?group=g3"},
		{"result", handleResultDetailPage, "/result?id=r20"},
		{"result_lv", handleResultDetailPage, "/result?id=r21"},
	} {
		w := get(p.h, p.u)
		body := w.Body.String()
		if w.Code != 200 || !strings.HasPrefix(body, "<!DOCTYPE html>") || strings.Contains(body, "%!") {
			t.Fatalf("%s %s: %d %.200s", p.name, p.u, w.Code, body)
		}
		s := write(p.name+".js", inlineScript(t, p.name, body))
		if out, err := exec.Command(node, "--check", s).CombinedOutput(); err != nil {
			t.Fatalf("node --check %s: %v\n%s", p.name, err, out)
		}
		scripts[p.name] = s
	}
	data := map[string]json.RawMessage{
		"group":    get(handleGroupDetail, "/api/groups/detail?id=g3").Body.Bytes(),
		"groupWC":  get(handleGroupDetail, "/api/groups/detail?id=g1").Body.Bytes(),
		"graphs":   get(handleGroupGraphs, "/api/groups/graphs?id=g3").Body.Bytes(),
		"detail":   get(handleResultDetail, "/api/results/detail?id=r20").Body.Bytes(),
		"detailLV": get(handleResultDetail, "/api/results/detail?id=r21").Body.Bytes(),
	}
	for k, v := range data {
		if !json.Valid(v) {
			t.Fatalf("%s is not JSON: %s", k, v)
		}
	}
	dj, _ := json.Marshal(data)
	return scripts, write("data.json", string(dj))
}

// runLayoutHarness runs one check of layoutPagesHarness on script.
func runLayoutHarness(t *testing.T, check string) {
	t.Helper()
	node := requireNode(t)
	dir := t.TempDir()
	scripts, dataPath := layoutPages(t, node, dir)
	harness := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(harness, []byte(layoutPagesHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	script := map[string]string{"group": "group", "group_wc": "group", "graphs": "graphs", "compare": "compare", "result": "result", "result_lv": "result_lv", "compare_same_name": "compare", "graphs_same_name": "graphs"}[check]
	out, err := exec.Command(node, harness, check, scripts[script], dataPath).CombinedOutput()
	if err != nil {
		t.Fatalf("harness %s: %v\n%s", check, err, out)
	}
	t.Logf("%s: %s", check, strings.TrimSpace(string(out)))
}

// The group page draws an array target with its members, the layout reading
// as 'layout:', the variants and each run's teardown; a write-cache group's
// page is as before.
func TestGroupPage_ArrayScript(t *testing.T) {
	runLayoutHarness(t, "group")
	runLayoutHarness(t, "group_wc")
}

// The graphs and compare pages name an array target's members.
func TestGraphsPage_MembersSubtitle(t *testing.T) {
	runLayoutHarness(t, "graphs")
	runLayoutHarness(t, "compare")
}

// Two layouts that got the same kernel name (md127 twice) are two entries of
// the compare disk list, each with its own members, and two graph cards.
func TestComparePage_SameKernelNameTwoLayouts(t *testing.T) {
	runLayoutHarness(t, "compare_same_name")
	runLayoutHarness(t, "graphs_same_name")
}

// The run details JSON carries the members of an array target and the
// details page lists them under it.
func TestResultDetail_ShowsArrayMembers(t *testing.T) {
	seedLayoutGroup(t)
	var out struct {
		Drives []struct {
			Device  struct{ Name string }
			Members []struct {
				Name string `json:"name"`
				Role string `json:"role"`
			}
		} `json:"drives"`
	}
	if err := json.Unmarshal(get(handleResultDetail, "/api/results/detail?id=r20").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Drives) != 1 || out.Drives[0].Device.Name != "md127" || len(out.Drives[0].Members) != 3 || out.Drives[0].Members[2].Role != "spare" {
		t.Errorf("drives = %+v", out.Drives)
	}
	runLayoutHarness(t, "result")
	runLayoutHarness(t, "result_lv")
}

const layoutPagesHarness = `
const fs = require('fs'), vm = require('vm'), assert = require('assert');
const [check, scriptPath, dataPath] = process.argv.slice(2);
const DATA = JSON.parse(fs.readFileSync(dataPath, 'utf8'));
const HTML = [], ELS = [], CHARTS = [], byId = {};
function el(tag) {
  const e = { tag, children: [], style: {}, dataset: {}, className: '', _t: '', _h: '',
    classList: { add() {}, remove() {}, toggle() { return false; } },
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) { this.children = this.children.filter(x => x !== c); },
    replaceChild() {}, addEventListener() {},
    querySelector() { return el('div'); },
    querySelectorAll() { return []; },
    set innerHTML(v) { this._h = String(v); this.children = []; HTML.push(String(v)); }, get innerHTML() { return this._h; },
    set textContent(v) { this._t = String(v); this.children = []; },
    get textContent() { return this._t + this.children.map(c => c.textContent || '').join(''); },
  };
  ELS.push(e);
  return e;
}
const ctx = {
  document: {
    getElementById(id) { return byId[id] || (byId[id] = el('div')); },
    createElement: el, createTextNode(t) { return { textContent: String(t) }; },
    addEventListener() {}, querySelector() { return el('div'); },
  },
  fetch: () => new Promise(() => {}), setInterval() {}, setTimeout() {}, console,
  Chart: class { constructor(c, cfg) { CHARTS.push(cfg); } destroy() {} },
};
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(scriptPath, 'utf8'), ctx);
const run = s => vm.runInContext(s, ctx);
ctx.__data = DATA;
const noRaw = s => assert(!s.includes('<b>x') && !s.includes('<i>clean'), 'unescaped: ' + s);
const one = i => JSON.stringify(Object.assign({}, DATA.group, {runs: [DATA.group.runs[i]]}));

switch (check) {
case 'group': {
  // mdadm sample (md127) and lvm sample (dm-3)
  [[0, 'md127', ['sdb', 'sdc', 'sdd']], [1, 'dm-3', ['sdb', 'sdc']]].forEach(([i, dev, names]) => {
    const h = run('renderRuns(' + one(i) + ')');
    assert(h.includes(dev + ' · array'), dev + ': ' + h);
    names.forEach(n => assert(h.includes(n), dev + ' lacks ' + n + ': ' + h));
    assert(h.includes('layout: array_state=&lt;i&gt;clean&lt;/i&gt;'), dev + ' reading: ' + h);
    assert(!h.includes('write_cache:'), dev + ': ' + h);
    noRaw(h);
  });
  let h = run('renderRuns(__data.group)');
  assert(h.includes('torn down') && h.includes('teardown failed on bbbench_g3_2'), 'teardown in runs: ' + h);
  h = run('renderVariants(__data.group)');
  assert(h.includes('mdadm raid1 · 2 data + 1 spare') && h.includes('lvm striped · 2 disks'), h);
  ['sdb', 'sdc', 'sdd', 'spare', 'data', '&lt;b&gt;x&lt;/b&gt;'].forEach(s => assert(h.includes(s), 'variants lack ' + s + ': ' + h));
  noRaw(h);
  assert.strictEqual(run('renderTeardown(__data.group.runs[0])'), '<span class="ok">torn down</span>');
  h = run('renderTeardown(__data.group.runs[1])');
  assert(h.includes('teardown failed on bbbench_g3_2'), h);
  assert.strictEqual(run('renderTeardown({status: "done"})'), '');
  h = run('renderRestore(__data.group)');
  assert(h.includes('Teardown failed on: bbbench_g3_2') && !h.includes('Restore'), h);
  h = run('renderRestore(Object.assign({}, __data.group, {restore: {status: "done"}}))');
  assert(h.includes('Torn down') && !h.includes('restored'), h);
  run('render(__data.group)');
  assert.strictEqual(byId['restore-title'].textContent, 'Teardown');
  assert(byId['variants'].innerHTML.includes('lvm striped'), 'variants not rendered');
  assert.strictEqual(byId['variants-card'].style.display, '');
  HTML.forEach(noRaw);
  console.log('group ok: array drive cells, layout reading, variants, teardown');
  break;
}
case 'group_wc': {
  const g = DATA.groupWC;
  assert(!('variants' in g));
  const h = run('renderRuns(__data.groupWC)');
  assert(h.includes('write_cache: write back') && !h.includes('layout:') && !h.includes('· array'), h);
  assert.strictEqual(run('renderRestore(__data.groupWC)'), '<span class="ok">Original values restored.</span>');
  assert.strictEqual(run('renderVariants(__data.groupWC)'), '');
  run('render(__data.groupWC)');
  assert(!byId['restore-title'] && !byId['variants-card'], 'write-cache page touches the layout elements');
  console.log('group_wc ok: write-cache page as before');
  break;
}
case 'graphs': {
  const arr = DATA.graphs.find(d => d.disk === 'md127');
  ctx.__arr = arr;
  assert.strictEqual(run('membersText(__arr)'), 'members: sdb (data), sdc (data), sdd (spare)');
  assert.strictEqual(run('membersText({disk: "sda"})'), '');
  assert.strictEqual(run('membersText({disk: "sda", members: []})'), '');
  run('allData = __data.graphs; initFilters(); renderAll()');
  const subs = ELS.filter(e => e.className === 'card-subtitle').map(e => e.textContent);
  assert(subs.some(s => s.includes('members: sdb (data), sdc (data), sdd (spare)')), 'subtitles: ' + subs);
  assert(subs.some(s => s.includes('members: sdb (data), sdc (data)') && !s.includes('sdd')), 'lv subtitle: ' + subs);
  run('openModal(__arr, METRICS[0], 0, 1)');
  assert(byId['modal-title'].textContent.includes('members: sdb (data), sdc (data), sdd (spare)'), 'modal: ' + byId['modal-title'].textContent);
  console.log('graphs ok: membersText, card subtitle, modal title');
  break;
}
case 'compare': {
  ctx.__arr = DATA.graphs.find(d => d.disk === 'dm-3');
  assert.strictEqual(run('membersText(__arr)'), 'members: sdb (data), sdc (data)');
  assert.strictEqual(run('membersText({disk: "sda"})'), '');
  run('allData = __data.graphs; initSidebar()');
  const subs = ELS.filter(e => e.className === 'disk-sub').map(e => e.textContent);
  assert(subs.some(s => s.includes('members: sdb (data), sdc (data), sdd (spare)')), 'disk list: ' + subs);
  console.log('compare ok: membersText in the disk list');
  break;
}
case 'compare_same_name': case 'graphs_same_name': {
  // A second layout of the group was built as md127 too: other uuid, other members.
  const a = DATA.graphs.find(d => d.disk === 'md127');
  const b = Object.assign({}, a, {serial: 'u-second', setting_value: 'mdadm raid0 · 2 disks',
    members: [{name: 'sde', role: 'data'}, {name: 'sdf', role: 'data'}]});
  ctx.__two = [a, b];
  if (check === 'compare_same_name') {
    run('allData = __two; initSidebar()');
    const subs = ELS.filter(e => e.className === 'disk-sub').map(e => e.textContent);
    assert.strictEqual(byId['disk-list'].children.length, 2, 'disk list: ' + subs);
    assert(subs.some(s => s.includes('members: sdb (data), sdc (data), sdd (spare)')), 'first layout: ' + subs);
    assert(subs.some(s => s.includes('u-second') && s.includes('members: sde (data), sdf (data)')), 'second layout: ' + subs);
    assert.strictEqual(run('getEntries().length'), 2, 'both layouts selected');
    console.log('compare_same_name ok: one disk-list entry per layout', JSON.stringify(subs));
  } else {
    run('allData = __two; initFilters(); renderAll()');
    const subs = ELS.filter(e => e.className === 'card-subtitle').map(e => e.textContent);
    assert(subs.some(s => s.includes('members: sdb (data), sdc (data), sdd (spare)')), 'first card: ' + subs);
    assert(subs.some(s => s.includes('members: sde (data), sdf (data)')), 'second card: ' + subs);
    console.log('graphs_same_name ok: each card its own members');
  }
  break;
}
case 'result': case 'result_lv': {
  const d = check === 'result' ? DATA.detail : DATA.detailLV;
  ctx.__d = d;
  const h = run('drivesHTML(__d)');
  const dev = check === 'result' ? 'md127' : 'dm-3';
  assert(h.includes(dev) && h.includes('sdb') && h.includes('sdc') && h.includes('data'), h);
  if (check === 'result') assert(h.includes('sdd') && h.includes('spare') && h.includes('&lt;b&gt;x&lt;/b&gt;'), h);
  noRaw(h);
  assert.strictEqual(run('drivesHTML({drives: [{Device: {Name: "sda", Model: "M"}}]})'), '', 'plain run gets a drives block');
  assert.strictEqual(run('drivesHTML({})'), '');
  console.log(check + ' ok: drives with members');
  break;
}
default: throw new Error('unknown check ' + check);
}
`
