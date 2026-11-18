package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// handleResultSummaryAPI returns the flat DiskPhaseData for the summary page.
func handleResultSummaryAPI(w http.ResponseWriter, r *http.Request) {
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
			logger.Error("encode summary data", "err", err)
		}
	}
}

// handleSummaryPage serves the hierarchical summary statistics page.
func handleSummaryPage(w http.ResponseWriter, r *http.Request) {
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

	sidebar := filterGroup("technology", "Drive Type") +
		filterGroup("vendor", "Vendor") +
		filterGroup("model", "Model") +
		filterGroup("serial", "Serial") +
		filterGroup("disk", "Device") +
		filterGroup("benchmark", "Benchmark")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, summaryPageHTML,
		navbarCSS,
		navbarHTML("/results"),
		sidebar,
		escapeHTML(resultID),
	)
}

const summaryPageHTML = `<!DOCTYPE html>
<html>
<head>
    <title>bbbench - Summary</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            display: flex; flex-direction: column; height: 100vh; overflow: hidden;
            background: #f1f5f9;
        }
        %s
        .workspace { display: flex; flex: 1; overflow: hidden; }

        /* ── Sidebar (identical to graphs page) ── */
        .sidebar {
            width: 240px; flex-shrink: 0;
            background: #fff; border-right: 1px solid #e2e8f0;
            overflow-y: auto; padding: 12px 12px 0;
            display: flex; flex-direction: column; gap: 0;
        }
        .fg { border-bottom: 1px solid #f1f5f9; padding: 10px 0 8px; }
        .fg-hdr { display: flex; justify-content: space-between; align-items: center; margin-bottom: 5px; }
        .fg-title { font-size: 10px; font-weight: 700; color: #64748b; text-transform: uppercase; letter-spacing: 0.08em; }
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
            position: sticky; bottom: 0; background: #fff; border-top: 1px solid #e2e8f0;
        }
        .count { font-size: 11px; color: #64748b; }
        .btn-show {
            width: 100%%; padding: 8px; background: #2563eb; color: #fff;
            border: none; border-radius: 6px; font-size: 13px; font-weight: 600;
            cursor: pointer; transition: background 0.15s;
        }
        .btn-show:hover { background: #1d4ed8; }

        /* ── Main panel ── */
        .main { flex: 1; overflow-y: auto; padding: 16px; display: flex; flex-direction: column; gap: 16px; }
        .placeholder {
            display: flex; align-items: center; justify-content: center;
            flex: 1; color: #94a3b8; font-size: 14px; padding: 60px; text-align: center;
        }

        /* ── Benchmark section (top-level fold) ── */
        .bench-section { display: flex; flex-direction: column; }
        .sec-hdr {
            display: flex; align-items: center; gap: 8px; cursor: pointer; user-select: none;
            font-size: 14px; font-weight: 700; color: #1e293b;
            padding: 10px 14px; background: #fff; border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.06);
        }
        .sec-hdr:hover { background: #f8fafc; }
        .sec-hdr .chev { display: inline-block; font-size: 10px; color: #94a3b8; transition: transform 0.18s; flex-shrink: 0; }
        .sec-hdr.collapsed .chev { transform: rotate(-90deg); }
        .sec-body { overflow-x: auto; }
        .sec-body.hidden { display: none; }

        /* ── Summary table ── */
        .summary-table {
            width: 100%%; border-collapse: collapse; font-size: 12px;
            background: #fff; border-radius: 0 0 8px 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.06);
        }
        .summary-table thead th {
            background: #f8fafc; padding: 6px 8px;
            font-size: 10px; font-weight: 700; color: #64748b;
            text-transform: uppercase; letter-spacing: 0.05em;
            border-bottom: 1px solid #e2e8f0; white-space: nowrap;
            text-align: right;
        }
        .summary-table thead th.th-label { text-align: left; min-width: 180px; }
        .summary-table thead th.th-n { min-width: 36px; }
        .summary-table thead th.th-grp {
            text-align: center; border-left: 2px solid #e2e8f0; color: #475569;
            font-size: 11px; letter-spacing: 0;
        }
        .summary-table thead th.th-sub { min-width: 52px; }
        .summary-table thead th.th-sub.grp-first { border-left: 2px solid #e2e8f0; }

        /* row types */
        .summary-table tbody td {
            padding: 6px 8px; border-bottom: 1px solid #f1f5f9;
            white-space: nowrap; text-align: right; color: #334155;
        }
        .summary-table tbody td.td-label { text-align: left; font-weight: 600; }
        .summary-table tbody td.td-n { color: #94a3b8; font-size: 11px; text-align: center; }
        .summary-table tbody td.grp-first { border-left: 2px solid #e2e8f0; }
        .summary-table tbody td.na { color: #cbd5e1; }
        .summary-table tbody tr:last-child td { border-bottom: none; }

        .row-type td { background: #f8fafc; font-weight: 700; color: #1e293b; }
        .row-type td.td-label { cursor: pointer; }
        .row-type:hover td { background: #f1f5f9; }

        .row-model td { background: #fff; font-weight: 600; color: #475569; }
        .row-model td.td-label { cursor: pointer; }
        .row-model:hover td { background: #f8fafc; }

        .row-disk td { background: #fff; color: #64748b; font-weight: 400; }
        .row-disk:hover td { background: #f8fafc; }

        .summary-table tr.hidden { display: none; }

        /* chevron in table rows */
        .row-chev { display: inline-block; font-size: 9px; color: #94a3b8; transition: transform 0.15s; margin-right: 4px; }
        .collapsed-row .row-chev { transform: rotate(-90deg); }

        /* leaf value cell spans all 4 stat cols */
        .leaf-val { text-align: center !important; color: #334155; }
    </style>
</head>
<body>
%s
<div class="workspace">
    <aside class="sidebar">
        %s
        <div class="sidebar-bottom">
            <div class="count" id="count"></div>
            <button class="btn-show" id="btn-show">Show Summary</button>
        </div>
    </aside>
    <main class="main" id="main">
        <div class="placeholder">Select filters and click "Show Summary".</div>
    </main>
</div>

<script>
const resultID = '%s';
let allData = [];

// ── Sidebar filter state (same pattern as graphs page) ──
const SEL = {
    technology: new Set(),
    vendor:     new Set(),
    model:      new Set(),
    serial:     new Set(),
    disk:       new Set(),
    benchmark:  new Set(),
};
const DATA_DIMS = ['technology', 'vendor', 'model', 'serial', 'disk', 'benchmark'];
const DIM_F = {
    technology: d => d.technology || '',
    vendor:     d => d.vendor     || '',
    model:      d => d.model      || '',
    serial:     d => d.serial     || '',
    disk:       d => d.disk       || '',
    benchmark:  d => d.phase_name || ('Phase ' + d.phase_num),
};

function dataExcept(dim) {
    return allData.filter(d =>
        DATA_DIMS.every(fd => fd === dim || SEL[fd].has(DIM_F[fd](d)))
    );
}
function availableFor(dim) {
    const seen = new Set();
    dataExcept(dim).forEach(d => seen.add(DIM_F[dim](d)));
    return [...seen].sort();
}
function buildList(dim, values) {
    const ul = document.getElementById('list-' + dim);
    ul.innerHTML = '';
    values.forEach(v => {
        const li = document.createElement('li');
        const lbl = document.createElement('label');
        const cb = document.createElement('input');
        cb.type = 'checkbox'; cb.value = v; cb.checked = SEL[dim].has(v);
        cb.addEventListener('change', () => {
            if (cb.checked) SEL[dim].add(v); else SEL[dim].delete(v);
            updateLists();
        });
        lbl.appendChild(cb);
        lbl.appendChild(document.createTextNode('\u00a0' + (v || '(unknown)')));
        li.appendChild(lbl);
        ul.appendChild(li);
    });
}
function updateLists() {
    DATA_DIMS.forEach(dim => buildList(dim, availableFor(dim)));
    updateCount();
}
function updateCount() {
    const n = filteredData().length;
    document.getElementById('count').textContent =
        n + ' disk\u00d7phase combination' + (n !== 1 ? 's' : '');
}
function setAll(dim, checked) {
    const ul = document.getElementById('list-' + dim);
    ul.querySelectorAll('input[type=checkbox]').forEach(cb => {
        cb.checked = checked;
        if (checked) SEL[dim].add(cb.value); else SEL[dim].delete(cb.value);
    });
    updateLists();
}
function filteredData() {
    return allData.filter(d =>
        DATA_DIMS.every(fd => SEL[fd].has(DIM_F[fd](d)))
    );
}
function initFilters() {
    DATA_DIMS.forEach(dim => {
        const vals = [...new Set(allData.map(d => DIM_F[dim](d)))].sort();
        vals.forEach(v => SEL[dim].add(v));
        buildList(dim, vals);
    });
    updateCount();
}

// ── Stats helpers ──
function statsOf(vals) {
    if (!vals.length) return null;
    const n = vals.length;
    const min = Math.min(...vals);
    const max = Math.max(...vals);
    const avg = vals.reduce((a, b) => a + b, 0) / n;
    const stddev = Math.sqrt(vals.reduce((a, v) => a + (v - avg) ** 2, 0) / n);
    return { min, avg, max, stddev, n };
}
function levelStats(entries) {
    const rv = v => (v > 0 ? v : null);
    return {
        riops: statsOf(entries.filter(d => d.has_read).map(d => d.read_iops).filter(Boolean)),
        wiops: statsOf(entries.filter(d => d.has_write).map(d => d.write_iops).filter(Boolean)),
        rbw:   statsOf(entries.filter(d => d.has_read).map(d => d.read_bw_mbps).filter(Boolean)),
        wbw:   statsOf(entries.filter(d => d.has_write).map(d => d.write_bw_mbps).filter(Boolean)),
        rlat:  statsOf(entries.filter(d => d.has_read  && d.read_lat_us  > 0).map(d => d.read_lat_us)),
        wlat:  statsOf(entries.filter(d => d.has_write && d.write_lat_us > 0).map(d => d.write_lat_us)),
    };
}

// ── Formatters ──
function fmtNum(v) {
    if (v == null || v === 0) return '\u2014';
    if (v >= 1e6) return (v / 1e6).toFixed(1) + 'M';
    if (v >= 1e3) return (v / 1e3).toFixed(1) + 'k';
    return v.toFixed(0);
}
function fmtBw(v) {
    if (v == null || v === 0) return '\u2014';
    if (v >= 1000) return (v / 1000).toFixed(1) + 'k';
    return v.toFixed(1);
}
function fmtLat(v) {
    if (v == null || v === 0) return '\u2014';
    if (v >= 1e6) return (v / 1e6).toFixed(2) + 's';
    if (v >= 1000) return (v / 1000).toFixed(1) + 'ms';
    return v.toFixed(1);
}

const COL_GROUPS = [
    { key: 'riops', label: 'Read IOPS',    fmt: fmtNum },
    { key: 'wiops', label: 'Write IOPS',   fmt: fmtNum },
    { key: 'rbw',   label: 'Read BW MB/s', fmt: fmtBw  },
    { key: 'wbw',   label: 'Write BW MB/s',fmt: fmtBw  },
    { key: 'rlat',  label: 'Read Lat \u00b5s',  fmt: fmtLat },
    { key: 'wlat',  label: 'Write Lat \u00b5s', fmt: fmtLat },
];

// ── Table builder ──
function makeTableHead() {
    const row1 = document.createElement('tr');
    const row2 = document.createElement('tr');

    const th0 = document.createElement('th');
    th0.className = 'th-label'; th0.rowSpan = 2; th0.textContent = 'Disk';
    row1.appendChild(th0);

    const thn = document.createElement('th');
    thn.className = 'th-n'; thn.rowSpan = 2; thn.textContent = 'n';
    row1.appendChild(thn);

    COL_GROUPS.forEach(g => {
        const thg = document.createElement('th');
        thg.className = 'th-grp'; thg.colSpan = 4; thg.textContent = g.label;
        row1.appendChild(thg);

        ['Min', 'Avg', 'Max', '\u03c3'].forEach((lbl, i) => {
            const th = document.createElement('th');
            th.className = 'th-sub' + (i === 0 ? ' grp-first' : '');
            th.textContent = lbl;
            row2.appendChild(th);
        });
    });

    const thead = document.createElement('thead');
    thead.appendChild(row1);
    thead.appendChild(row2);
    return thead;
}

function statCells(st, fmtFn) {
    // Returns 4 <td> elements: min, avg, max, σ
    const vals = st ? [st.min, st.avg, st.max, st.stddev] : [null, null, null, null];
    return vals.map((v, i) => {
        const td = document.createElement('td');
        td.className = (i === 0 ? 'grp-first ' : '') + (v == null ? 'na' : '');
        td.textContent = v == null ? '\u2014' : fmtFn(v);
        return td;
    });
}

function leafCells(val, fmtFn) {
    // Single value spanning 4 columns (leaf disk row)
    const td = document.createElement('td');
    td.className = 'grp-first leaf-val';
    td.colSpan = 4;
    td.textContent = fmtFn(val);
    return [td];
}

function appendStatRow(tbody, stats, labelEl, nVal, isLeaf) {
    const tr = document.createElement('tr');
    const tdLabel = document.createElement('td');
    tdLabel.className = 'td-label';
    tdLabel.appendChild(labelEl);
    tr.appendChild(tdLabel);

    const tdN = document.createElement('td');
    tdN.className = 'td-n';
    tdN.textContent = nVal != null ? nVal : '';
    tr.appendChild(tdN);

    COL_GROUPS.forEach(g => {
        const cells = isLeaf
            ? leafCells(stats[g.key], g.fmt)
            : statCells(stats[g.key], g.fmt);
        cells.forEach(td => tr.appendChild(td));
    });
    tbody.appendChild(tr);
    return tr;
}

// ── Toggle helpers ──
function collapseGroup(tbody, gid) {
    tbody.querySelectorAll('tr[data-parent="' + gid + '"]').forEach(row => {
        row.classList.add('hidden');
        if (row.dataset.gid) collapseGroup(tbody, row.dataset.gid);
    });
}
function expandGroup(tbody, gid) {
    tbody.querySelectorAll('tr[data-parent="' + gid + '"]').forEach(row => {
        row.classList.remove('hidden');
    });
}
function toggleGroup(tbody, gid, chevEl, rowEl) {
    const isOpen = !tbody.querySelector('tr[data-parent="' + gid + '"]').classList.contains('hidden');
    if (isOpen) {
        collapseGroup(tbody, gid);
    } else {
        expandGroup(tbody, gid);
    }
    rowEl.classList.toggle('collapsed-row', isOpen);
}

// ── Main render ──
let gidSeq = 0;
function nextGid() { return 'g' + (++gidSeq); }

function renderAll() {
    gidSeq = 0;
    const main = document.getElementById('main');
    main.innerHTML = '';

    const data = filteredData();
    if (data.length === 0) {
        main.innerHTML = '<div class="placeholder">No data matches the current filters.</div>';
        return;
    }

    // Group: benchmark -> technology -> vendor+model -> [DiskPhaseData]
    const benchOrder = [];
    const byBench = new Map();
    for (const d of data) {
        const bench = DIM_F.benchmark(d);
        const tech  = d.technology || 'unknown';
        const model = [d.vendor, d.model].filter(s => s && s.trim()).join(' ') || 'Unknown';
        const serial = d.serial || d.disk;

        if (!byBench.has(bench)) { byBench.set(bench, new Map()); benchOrder.push(bench); }
        const byType = byBench.get(bench);
        if (!byType.has(tech)) byType.set(tech, new Map());
        const byModel = byType.get(tech);
        if (!byModel.has(model)) byModel.set(model, []);
        byModel.get(model).push(d);
    }

    benchOrder.forEach(bench => {
        const byType = byBench.get(bench);
        const allEntries = [...byType.values()].flatMap(bm => [...bm.values()].flat());

        // ── Benchmark section header (foldable) ──
        const section = document.createElement('div');
        section.className = 'bench-section';

        const secHdr = document.createElement('div');
        secHdr.className = 'sec-hdr';
        secHdr.innerHTML = '<span class="chev">\u25bc</span>' + bench
            + ' <span style="font-size:11px;font-weight:400;color:#94a3b8;margin-left:6px">('
            + allEntries.length + ' disk' + (allEntries.length !== 1 ? 's' : '') + ')</span>';
        section.appendChild(secHdr);

        const secBody = document.createElement('div');
        secBody.className = 'sec-body';
        section.appendChild(secBody);
        main.appendChild(section);

        secHdr.addEventListener('click', () => {
            const collapsed = secBody.classList.toggle('hidden');
            secHdr.classList.toggle('collapsed', collapsed);
        });

        // ── Table inside section ──
        const table = document.createElement('table');
        table.className = 'summary-table';
        table.appendChild(makeTableHead());
        const tbody = document.createElement('tbody');
        table.appendChild(tbody);
        secBody.appendChild(table);

        // Sort types: ssd first, then hdd, then others
        const typeOrder = [...byType.keys()].sort((a, b) => {
            const rank = t => t === 'ssd' ? 0 : t === 'hdd' ? 1 : 2;
            return rank(a) - rank(b);
        });

        typeOrder.forEach(tech => {
            const byModel = byType.get(tech);
            const typeEntries = [...byModel.values()].flat();
            const typeGid = nextGid();
            const typeStats = levelStats(typeEntries);

            // Type row
            const typeLabelEl = document.createElement('span');
            const typeChev = document.createElement('span');
            typeChev.className = 'row-chev';
            typeChev.textContent = '\u25bc';
            typeLabelEl.appendChild(typeChev);
            typeLabelEl.appendChild(document.createTextNode(tech.toUpperCase()));

            const typeRow = appendStatRow(tbody, typeStats, typeLabelEl, typeEntries.length, false);
            typeRow.className = 'row-type';
            typeRow.dataset.gid = typeGid;
            typeRow.querySelector('td.td-label').addEventListener('click', () => {
                toggleGroup(tbody, typeGid, typeChev, typeRow);
            });

            // Sort models alphabetically
            const modelOrder = [...byModel.keys()].sort();

            modelOrder.forEach(model => {
                const diskEntries = byModel.get(model);
                const modelGid = nextGid();
                const modelStats = levelStats(diskEntries);

                // Model row
                const modelLabelEl = document.createElement('span');
                modelLabelEl.style.paddingLeft = '18px';
                const modelChev = document.createElement('span');
                modelChev.className = 'row-chev';
                modelChev.textContent = '\u25bc';
                modelLabelEl.appendChild(modelChev);
                modelLabelEl.appendChild(document.createTextNode(model));

                const modelRow = appendStatRow(tbody, modelStats, modelLabelEl, diskEntries.length, false);
                modelRow.className = 'row-model';
                modelRow.dataset.gid = modelGid;
                modelRow.dataset.parent = typeGid;
                modelRow.querySelector('td.td-label').addEventListener('click', () => {
                    toggleGroup(tbody, modelGid, modelChev, modelRow);
                });

                // Disk leaf rows — sorted by device name
                const sorted = [...diskEntries].sort((a, b) => a.disk.localeCompare(b.disk));
                sorted.forEach(d => {
                    const diskLabelEl = document.createElement('span');
                    diskLabelEl.style.paddingLeft = '36px';
                    diskLabelEl.textContent = d.disk + (d.serial ? '  \u00b7  ' + d.serial : '');

                    // Build per-metric single values for leaf
                    const leafStats = {
                        riops: d.has_read  ? { min: d.read_iops,    avg: d.read_iops,    max: d.read_iops,    stddev: 0 } : null,
                        wiops: d.has_write ? { min: d.write_iops,   avg: d.write_iops,   max: d.write_iops,   stddev: 0 } : null,
                        rbw:   d.has_read  ? { min: d.read_bw_mbps, avg: d.read_bw_mbps, max: d.read_bw_mbps, stddev: 0 } : null,
                        wbw:   d.has_write ? { min: d.write_bw_mbps,avg: d.write_bw_mbps,max: d.write_bw_mbps,stddev: 0 } : null,
                        rlat:  (d.has_read  && d.read_lat_us  > 0) ? { min: d.read_lat_us,  avg: d.read_lat_us,  max: d.read_lat_us,  stddev: 0 } : null,
                        wlat:  (d.has_write && d.write_lat_us > 0) ? { min: d.write_lat_us, avg: d.write_lat_us, max: d.write_lat_us, stddev: 0 } : null,
                    };

                    const diskRow = appendStatRow(tbody, leafStats, diskLabelEl, null, true);
                    diskRow.className = 'row-disk';
                    diskRow.dataset.parent = modelGid;
                });
            });
        });
    });
}

document.getElementById('btn-show').addEventListener('click', () => {
    if (typeof bbLoadingRun === 'function') bbLoadingRun('Drawing summary\u2026', renderAll); else renderAll();
});

if (typeof bbLoading === 'function') bbLoading('Loading summary\u2026');
fetch('/api/results/summary?id=' + resultID)
    .then(r => r.json())
    .then(data => { allData = data || []; initFilters(); if (typeof bbLoadingDone === 'function') bbLoadingDone(); })
    .catch(err => {
        if (typeof bbLoadingDone === 'function') bbLoadingDone();
        document.getElementById('main').innerHTML =
            '<div class="placeholder">Error loading data: ' + err.message + '</div>';
    });
</script>
</body>
</html>`
