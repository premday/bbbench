package main

import (
	"fmt"
	"net/http"
	"strings"
)

// handleComparePage serves the comparison view where multiple disks and/or
// phases can be overlaid on the same chart.
func handleComparePage(w http.ResponseWriter, r *http.Request) {
	resultID := r.URL.Query().Get("id")
	if resultID == "" {
		http.Error(w, "Missing result ID", http.StatusBadRequest)
		return
	}
	_, ok := globalResultsStore.GetResult(resultID)
	if !ok {
		http.Error(w, "Result not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	html := `<!DOCTYPE html>
<html>
<head>
    <title>bbbench - Compare</title>
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
            display: flex; flex-direction: column;
        }
        .back-link {
            font-size: 11px; color: #2563eb; text-decoration: none;
            display: block; padding-bottom: 10px; margin-bottom: 2px;
            border-bottom: 1px solid #f1f5f9;
        }
        .back-link:hover { text-decoration: underline; }
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
        .flist li label input { flex-shrink: 0; margin-top: 2px; accent-color: #2563eb; }
        .disk-sub { font-size: 10px; color: #94a3b8; display: block; word-break: break-all; }
        .sidebar-bottom {
            padding: 12px 0; display: flex; flex-direction: column; gap: 8px;
            position: sticky; bottom: 0; background: #fff;
            border-top: 1px solid #e2e8f0; margin-top: auto;
        }
        .count { font-size: 11px; color: #64748b; }
        .btn-compare {
            width: 100%; padding: 8px; background: #2563eb; color: #fff;
            border: none; border-radius: 6px; font-size: 13px; font-weight: 600;
            cursor: pointer; transition: background 0.15s;
        }
        .btn-compare:hover { background: #1d4ed8; }

        /* ── Main ── */
        .main { flex: 1; display: flex; flex-direction: column; padding: 16px; min-width: 0; }
        .cmp-chart-wrap {
            flex: 1; background: #fff; border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.08);
            padding: 16px; display: flex; flex-direction: column; min-height: 0;
        }
        .cmp-canvas-wrap { position: relative; flex: 1; min-height: 0; }
        .src {
            font-size: 9px; font-weight: 600; letter-spacing: 0.03em; vertical-align: middle;
            padding: 0 5px; border-radius: 8px; border: 1px solid #cbd5e1;
            color: #64748b; background: #f8fafc; cursor: help;
        }
        .src-ftrace { color: #6d28d9; border-color: #c4b5fd; background: #f5f3ff; }
        .cmp-note {
            font-size: 11px; color: #92400e; background: #fef3c7;
            border: 1px solid #fcd34d; border-radius: 3px;
            padding: 3px 8px; margin-top: 8px;
        }
        .cmp-placeholder {
            flex: 1; display: flex; align-items: center; justify-content: center;
            color: #94a3b8; font-size: 14px; text-align: center; padding: 40px;
            background: #fff; border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.08);
        }
    </style>
</head>
<body>
__NAVBAR_HTML__
<div class="workspace">
    <aside class="sidebar">
        <a class="back-link" href="/graphs?id=__RESULT_ID__">&#8592; Filter View</a>

        <div class="fg">
            <div class="fg-hdr"><span class="fg-title">Metric</span></div>
            <ul class="flist" id="metric-list"></ul>
        </div>

        <div class="fg">
            <div class="fg-hdr">
                <span class="fg-title">Disks</span>
                <span class="fg-allnone">
                    <a href="#" onclick="setAll('disk-list',selDisks,true);return false">All</a>
                    <a href="#" onclick="setAll('disk-list',selDisks,false);return false">None</a>
                </span>
            </div>
            <ul class="flist" id="disk-list"></ul>
        </div>

        <div class="fg">
            <div class="fg-hdr">
                <span class="fg-title">Phases</span>
                <span class="fg-allnone">
                    <a href="#" onclick="setAll('phase-list',selPhases,true);return false">All</a>
                    <a href="#" onclick="setAll('phase-list',selPhases,false);return false">None</a>
                </span>
            </div>
            <ul class="flist" id="phase-list"></ul>
        </div>

        <div class="fg">
            <div class="fg-hdr"><span class="fg-title">Direction</span></div>
            <ul class="flist">
                <li><label><input type="checkbox" id="dir-read"  checked>&nbsp;Read</label></li>
                <li><label><input type="checkbox" id="dir-write" checked>&nbsp;Write</label></li>
            </ul>
        </div>

        <div class="sidebar-bottom">
            <div class="count" id="count"></div>
            <button class="btn-compare" id="btn-compare">Compare</button>
        </div>
    </aside>

    <main class="main" id="main">
        <div class="cmp-placeholder">Select disks and phases, then click &#8220;Compare&#8221;.</div>
    </main>
</div>

<script>
    const resultID = '__RESULT_ID__';
    let allData = [];
    let cmpChart = null;

    const METRICS = [
        { key: 'iops', label: 'IOPS',            unit: 'IOPS', tsKey: 'ts_iops', source: 'fio',
          sumR: d => d.read_iops    || 0, sumW: d => d.write_iops    || 0 },
        { key: 'bw',   label: 'Bandwidth (MB/s)', unit: 'MB/s', tsKey: 'ts_bw', source: 'fio',
          sumR: d => d.read_bw_mbps || 0, sumW: d => d.write_bw_mbps || 0 },
        { key: 'lat',  label: 'Latency (\u03bcs)', unit: '\u03bcs', tsKey: 'ts_lat', source: 'fio',
          sumR: d => d.read_lat_us  || 0, sumW: d => d.write_lat_us  || 0 },
        // Device Flush latency, timed from kernel trace events (not fio).
        { key: 'flush', label: 'Flush latency (\u03bcs)', unit: '\u03bcs', tsKey: 'ts_flush', source: 'ftrace',
          has:  d => !!(d.flush && d.flush.count > 0) || !!(d.ts_flush && d.ts_flush.length),
          none: d => !!d.flush && !(d.flush.count > 0),
          sumR: d => 0, sumW: d => d.flush ? (d.flush.mean_ns || 0) / 1000 : 0 },
    ];

    // [solid border, area fill, bar background]
    const PALETTE = [
        ['#2563eb', 'rgba(37,99,235,0.08)',   'rgba(37,99,235,0.75)'],
        ['#dc2626', 'rgba(220,38,38,0.08)',   'rgba(220,38,38,0.75)'],
        ['#16a34a', 'rgba(22,163,74,0.08)',   'rgba(22,163,74,0.75)'],
        ['#d97706', 'rgba(217,119,6,0.08)',   'rgba(217,119,6,0.75)'],
        ['#7c3aed', 'rgba(124,58,237,0.08)',  'rgba(124,58,237,0.75)'],
        ['#0891b2', 'rgba(8,145,178,0.08)',   'rgba(8,145,178,0.75)'],
        ['#db2777', 'rgba(219,39,119,0.08)',  'rgba(219,39,119,0.75)'],
        ['#65a30d', 'rgba(101,163,13,0.08)',  'rgba(101,163,13,0.75)'],
        ['#c2410c', 'rgba(194,65,12,0.08)',   'rgba(194,65,12,0.75)'],
        ['#4338ca', 'rgba(67,56,202,0.08)',   'rgba(67,56,202,0.75)'],
    ];

    const SRC_TITLE = {
        fio:    'Measured by fio',
        ftrace: 'Measured from kernel ftrace events (device Flush commands), not by fio',
    };
    function srcBadge(metric) {
        const b = document.createElement('span');
        b.className = 'src src-' + metric.source;
        b.textContent = metric.source;
        b.title = SRC_TITLE[metric.source];
        return b;
    }

    let selMetricKey = 'iops';
    const selDisks   = new Set();
    const selPhases  = new Set();

    function fmtTick(v) {
        const a = Math.abs(v);
        if (a === 0) return '0';
        return a >= 1000 ? (a / 1000).toFixed(1) + 'k' : a.toFixed(0);
    }

    function getEntries() {
        return allData.filter(d => selDisks.has(d.disk) && selPhases.has(d.phase_name));
    }

    function updateCount() {
        const n = getEntries().length;
        document.getElementById('count').textContent =
            n + ' entr' + (n !== 1 ? 'ies' : 'y') + ' selected';
    }

    function buildMetricList() {
        const ul = document.getElementById('metric-list');
        METRICS.forEach(m => {
            const li  = document.createElement('li');
            const lbl = document.createElement('label');
            const rb  = document.createElement('input');
            rb.type    = 'radio';
            rb.name    = 'metric';
            rb.value   = m.key;
            rb.checked = m.key === selMetricKey;
            rb.style.accentColor = '#2563eb';
            rb.addEventListener('change', () => { selMetricKey = m.key; updateCount(); });
            lbl.appendChild(rb);
            lbl.appendChild(document.createTextNode('\u00a0' + m.label + ' '));
            lbl.appendChild(srcBadge(m));
            li.appendChild(lbl);
            ul.appendChild(li);
        });
    }

    function isTrimPhase(name) {
        const n = (name || '').toLowerCase();
        return n.indexOf('trim') !== -1 || n.indexOf('blkdiscard') !== -1;
    }

    function buildCheckList(ulId, values, selSet, skipFn) {
        const ul = document.getElementById(ulId);
        ul.innerHTML = '';
        values.forEach(v => {
            const checked = !skipFn || !skipFn(v);
            if (checked) selSet.add(v);
            const li  = document.createElement('li');
            const lbl = document.createElement('label');
            const cb  = document.createElement('input');
            cb.type    = 'checkbox';
            cb.value   = v;
            cb.checked = checked;
            cb.addEventListener('change', () => {
                if (cb.checked) selSet.add(v); else selSet.delete(v);
                updateCount();
            });
            lbl.appendChild(cb);
            lbl.appendChild(document.createTextNode('\u00a0' + (v || '(unknown)')));
            li.appendChild(lbl);
            ul.appendChild(li);
        });
    }

    function setAll(ulId, selSet, checked) {
        document.getElementById(ulId).querySelectorAll('input[type=checkbox]').forEach(cb => {
            cb.checked = checked;
            if (checked) selSet.add(cb.value); else selSet.delete(cb.value);
        });
        updateCount();
    }

    function buildDiskList() {
        // Build a map of disk name → {model, serial} from first occurrence.
        const info = {};
        allData.forEach(d => {
            if (!info[d.disk]) info[d.disk] = { model: d.model || '', serial: d.serial || '' };
        });
        const disks = Object.keys(info).sort();
        const ul = document.getElementById('disk-list');
        ul.innerHTML = '';
        disks.forEach(v => {
            selDisks.add(v);
            const li  = document.createElement('li');
            const lbl = document.createElement('label');
            const cb  = document.createElement('input');
            cb.type    = 'checkbox';
            cb.value   = v;
            cb.checked = true;
            cb.addEventListener('change', () => {
                if (cb.checked) selDisks.add(v); else selDisks.delete(v);
                updateCount();
            });
            lbl.appendChild(cb);
            const textWrap = document.createElement('span');
            textWrap.appendChild(document.createTextNode('\u00a0' + v));
            const sub = [info[v].model, info[v].serial].filter(Boolean).join('  \u00b7  ');
            if (sub) {
                const subEl = document.createElement('span');
                subEl.className = 'disk-sub';
                subEl.textContent = sub;
                textWrap.appendChild(subEl);
            }
            lbl.appendChild(textWrap);
            li.appendChild(lbl);
            ul.appendChild(li);
        });
    }

    function initSidebar() {
        buildMetricList();
        buildDiskList();
        const phases = [...new Set(allData.map(d => d.phase_name))].sort();
        buildCheckList('phase-list', phases, selPhases, isTrimPhase);
        updateCount();
    }

    // Returns a series label smart enough to omit the constant dimension.
    function seriesLabel(d, nDisks, nPhases) {
        if (nDisks   === 1) return d.phase_name || ('Phase ' + d.phase_num);
        if (nPhases  === 1) return d.disk;
        return d.disk + ' \u00b7 ' + (d.phase_name || ('Phase ' + d.phase_num));
    }

    function renderCompare() {
        if (cmpChart) { cmpChart.destroy(); cmpChart = null; }
        const main = document.getElementById('main');
        main.innerHTML = '';

        const metric  = METRICS.find(m => m.key === selMetricKey);
        const all0    = getEntries();
        const skippedE = all0.filter(d => d.skipped);   // phases that were not run
        const all     = all0.filter(d => !d.skipped);
        const entries = metric.has ? all.filter(metric.has) : all;
        if (entries.length === 0 && skippedE.length > 0 && all.length === 0) {
            main.innerHTML = '<div class="cmp-placeholder">Skipped, not run: ' +
                skippedE[0].skipped.replace(/&/g, '&amp;').replace(/</g, '&lt;') + '</div>';
            return;
        }
        if (entries.length === 0) {
            const zero = metric.none ? all.filter(metric.none) : [];
            const wt = zero.some(d => d.flush.write_cache === 'write through');
            main.innerHTML = '<div class="cmp-placeholder">' + (all.length === 0
                ? 'No entries match. Select at least one disk and one phase.'
                : zero.length
                    ? 'ftrace saw 0 Flush commands for the selected disks and phases' + (wt
                        ? ': the kernel sends no Flush to a write-through drive (volatile write cache disabled or absent).'
                        : '.')
                    : 'No ' + metric.source + ' data for the selected disks and phases.') + '</div>';
            return;
        }

        // Flush latency has no read/write split: it is always drawn.
        const isFlush = metric.key === 'flush';
        const doRead  = isFlush ? false : document.getElementById('dir-read').checked;
        const doWrite = isFlush ? true  : document.getElementById('dir-write').checked;

        if (!isFlush && !doRead && !doWrite) {
            main.innerHTML = '<div class="cmp-placeholder">Select at least one direction (Read / Write).</div>';
            return;
        }

        const nDisks  = new Set(entries.map(e => e.disk)).size;
        const nPhases = new Set(entries.map(e => e.phase_name)).size;
        const hasTS   = entries.some(d => { const ts = d[metric.tsKey]; return ts && ts.length > 0; });

        // Source hint
        if (skippedE.length > 0) {
            const sk = document.createElement('div');
            sk.className = 'cmp-note';
            sk.textContent = '\u26a0 ' + skippedE.length + ' entr' + (skippedE.length !== 1 ? 'ies were' : 'y was') +
                ' skipped (not run): ' + skippedE[0].skipped;
            main.appendChild(sk);
        }
        const srcNote = document.createElement('div');
        srcNote.className = 'cmp-note';
        srcNote.appendChild(document.createTextNode('Source: '));
        srcNote.appendChild(srcBadge(metric));
        srcNote.appendChild(document.createTextNode(' ' + SRC_TITLE[metric.source].toLowerCase()));
        main.appendChild(srcNote);

        // Container
        const wrap = document.createElement('div');
        wrap.className = 'cmp-chart-wrap';
        const canvasWrap = document.createElement('div');
        canvasWrap.className = 'cmp-canvas-wrap';
        const canvas = document.createElement('canvas');
        canvasWrap.appendChild(canvas);
        wrap.appendChild(canvasWrap);
        main.appendChild(wrap);

        if (hasTS) {
            // ── Multi-line time-series chart ──
            const datasets = [];
            let skipped = 0;
            entries.forEach((d, i) => {
                const ts = d[metric.tsKey];
                const [solid, fill] = PALETTE[i % PALETTE.length];
                const lbl = seriesLabel(d, nDisks, nPhases);
                if (ts && ts.length > 0) {
                    if (doRead) {
                        const pts = ts.filter(p => p.r > 0).map(p => ({x: Math.round(p.t), y: p.r}));
                        if (pts.length > 0) datasets.push({
                            label:           lbl + (doWrite ? ' [R]' : ''),
                            data:            pts,
                            borderColor:     solid,
                            backgroundColor: fill,
                            fill:            false,
                            tension:         0.2,
                            pointRadius:     1.5,
                            borderWidth:     2,
                        });
                    }
                    if (doWrite) {
                        const pts = ts.filter(p => p.w > 0).map(p => ({x: Math.round(p.t), y: p.w}));
                        if (pts.length > 0) datasets.push({
                            label:           lbl + (doRead ? ' [W]' : ''),
                            data:            pts,
                            borderColor:     solid,
                            backgroundColor: 'transparent',
                            borderDash:      [5, 3],
                            fill:            false,
                            tension:         0.2,
                            pointRadius:     1.5,
                            borderWidth:     2,
                        });
                    }
                } else {
                    skipped++;
                }
            });

            if (datasets.length === 0) {
                main.innerHTML = '<div class="cmp-placeholder">No data to display for the selected direction(s).</div>';
                return;
            }

            cmpChart = new Chart(canvas, {
                type: 'line',
                data: { datasets },
                options: {
                    responsive: true, maintainAspectRatio: false, animation: false,
                    plugins: {
                        legend: { display: true, position: 'right',
                            labels: { boxWidth: 12, font: {size: 11}, padding: 10 } },
                        tooltip: { mode: 'index', intersect: false, callbacks: {
                            title: items => items[0].raw.x + ' s',
                            label: ctx => ctx.dataset.label + ': ' +
                                         fmtTick(ctx.raw.y) + ' ' + metric.unit,
                        }}
                    },
                    scales: {
                        x: { type: 'linear',
                             title: { display: true, text: 'seconds', color: '#94a3b8', font: {size: 10} },
                             ticks: { font: {size: 10}, color: '#64748b', maxTicksLimit: 8 } },
                        y: { min: 0,
                             ticks: { callback: fmtTick, font: {size: 10}, color: '#64748b' },
                             grid: { color: '#f1f5f9' },
                             title: { display: true,
                                      text: metric.label + ' (' + metric.unit + ')',
                                      color: '#64748b', font: {size: 10} } }
                    }
                }
            });

            if (skipped > 0) {
                const note = document.createElement('div');
                note.className = 'cmp-note';
                note.textContent = '\u26a0 ' + skipped + ' entr' + (skipped !== 1 ? 'ies' : 'y') +
                    ' without time-series data not shown.';
                wrap.appendChild(note);
            }
        } else {
            // ── Summary grouped bar chart ──
            const dirLabels = [];
            if (doRead)  dirLabels.push('Read');
            if (doWrite) dirLabels.push(isFlush ? 'Flush' : 'Write');

            const datasets = entries.map((d, i) => {
                const [solid, , barBg] = PALETTE[i % PALETTE.length];
                const vals = [];
                if (doRead)  vals.push(metric.sumR(d));
                if (doWrite) vals.push(metric.sumW(d));
                return {
                    label:           seriesLabel(d, nDisks, nPhases),
                    data:            vals,
                    backgroundColor: barBg,
                    borderColor:     solid,
                    borderWidth:     1,
                };
            });

            cmpChart = new Chart(canvas, {
                type: 'bar',
                data: { labels: dirLabels, datasets },
                options: {
                    responsive: true, maintainAspectRatio: false, animation: false,
                    plugins: {
                        legend: { display: true, position: 'right',
                            labels: { boxWidth: 12, font: {size: 11}, padding: 10 } },
                        tooltip: { callbacks: {
                            label: ctx => ctx.dataset.label + ': ' +
                                         fmtTick(ctx.raw) + ' ' + metric.unit,
                        }}
                    },
                    scales: {
                        y: { min: 0,
                             ticks: { callback: fmtTick, font: {size: 10}, color: '#64748b' },
                             title: { display: true,
                                      text: metric.label + ' (' + metric.unit + ')',
                                      color: '#64748b', font: {size: 10} } }
                    }
                }
            });
        }
    }

    document.getElementById('btn-compare').addEventListener('click', renderCompare);

    fetch('/api/results/graphs?id=' + resultID)
        .then(r => r.json())
        .then(data => { allData = data || []; initSidebar(); })
        .catch(err => {
            document.getElementById('main').innerHTML =
                '<div class="cmp-placeholder">Error loading data: ' + err.message + '</div>';
        });
</script>
</body>
</html>`

	html = strings.ReplaceAll(html, "__NAVBAR_CSS__", navbarCSS)
	html = strings.ReplaceAll(html, "__NAVBAR_HTML__", navbarHTML("/results"))
	html = strings.ReplaceAll(html, "__RESULT_ID__", resultID)

	fmt.Fprint(w, html)
}
