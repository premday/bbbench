package main

// chartSharedJS is the chart code the graphs and compare pages have in common,
// spliced into each page's script at __CHART_SHARED_JS__. The pages keep their
// own X_MAX (the graphs page spans all charts, the compare page one chart).
//
// Time axis: every series counts seconds from the start of the phase. fio's
// own logs start when its ramp-up ends, so fioShift moves their points by the
// ramp, and back by one window (fio stamps a point at the END of its averaging
// window); the Flush series already counts from the start of tracing, in
// window starts. The ramp-up is shaded by rampPlugin.
//
// It must not contain backticks (raw string). It also holds the run-group
// helpers both pages use (setting values, rounds, invalid results, the
// members of an array target) and esc.
const chartSharedJS = `function rampOf(d) {
        if (d.ramp_sec) return d.ramp_sec;
        if (d.flush && d.flush.offset_ms) return d.flush.offset_ms / 1000;
        return d.io_trace && d.io_trace.offset_ms ? d.io_trace.offset_ms / 1000 : 0;
    }
    // fio stamps a log point at the END of its averaging window; moving it back
    // one window (to the window start) puts the first point right where the
    // ramp-up ends instead of one polling interval later. Flush points are
    // already window starts.
    function windowOf(d) {
        // What fio really used: its first point closes the first window, so its
        // time is the window width. The saved --log-avg-msec can disagree (the
        // job file's [global] sets its own log_avg_msec), so it is only the
        // fallback when there is no fio series.
        let m = Infinity;
        ['ts_iops', 'ts_bw', 'ts_lat'].forEach(k => {
            if (d[k] && d[k].length) m = Math.min(m, d[k][0].t);
        });
        if (isFinite(m)) return m;
        return d.log_avg_msec ? d.log_avg_msec / 1000 : 0;
    }
    function fioShift(d) {
        return rampOf(d) - windowOf(d);
    }
    const rampPlugin = {
        id: 'rampZone',
        beforeDatasetsDraw(chart, args, opts) {
            if (!opts || !opts.sec) return;
            const x = chart.scales.x, a = chart.chartArea;
            const x0 = Math.max(a.left, x.getPixelForValue(0));
            const x1 = Math.min(a.right, x.getPixelForValue(opts.sec));
            if (x1 <= x0) return;
            const c = chart.ctx;
            c.save();
            c.fillStyle = 'rgba(148,163,184,0.22)';
            c.fillRect(x0, a.top, x1 - x0, a.bottom - a.top);
            c.fillStyle = '#64748b';
            c.font = '9px sans-serif';
            c.textBaseline = 'top';
            c.fillText('ramp-up', x0 + 3, a.top + 3);
            c.restore();
        }
    };
    function seriesMaxT(d, metric) {
        const ts = d[metric.tsKey];
        if (!ts || ts.length === 0) return 0;
        const shift = metric.key === 'flush' || metric.key === 'iolat' ? 0 : fioShift(d);
        return Math.max(...ts.map(p => p.t)) + shift;
    }

    function fmtTick(v) {
        const a = Math.abs(v);
        if (a === 0) return '0';
        return a >= 1000 ? (a/1000).toFixed(1)+'k' : a.toFixed(0);
    }

    // SI scale of the chart axes and tooltips. The data stays in the units of
    // the JSON (MB/s, IOPS, us); a chart plots it in the metric's base unit
    // (toBase) and prints the number with an SI prefix letter, the base unit
    // after it: 2.41G bytes/s, 1.2M IOPS, 2.5m s. Bandwidth values are fio's
    // KiB/s divided by 1024 (MiB/s, labelled MB/s), so bytes/s = value *
    // 1048576 and the axis is true decimal SI bytes/s: 2300 MiB/s = 2.41G.
    //
    // siScale returns {mul, prefix, div, unit}: mul converts a value of the
    // metric to its base unit, unit is that base unit, and prefix/div put the
    // maximum maxData (in the metric's units) in [1, 1000). bytes/s and IOPS
    // never take a prefix below none, seconds take n, \u03bc, m or none. maxData
    // 0 or not a number gives no prefix; a unit without a base is kept as it
    // is (no conversion).
    function siScale(metric, maxData) {
        const base = {
            'MB/s':    {mul: 1048576, unit: 'bytes/s', lo: 3, hi: 8},
            'IOPS':    {mul: 1,       unit: 'IOPS',    lo: 3, hi: 8},
            '\u03bcs': {mul: 1e-6,    unit: 's',       lo: 0, hi: 3},
        }[metric.unit] || {mul: 1, unit: metric.unit, lo: 3, hi: 8};
        const P = [['n', 1e-9], ['\u03bc', 1e-6], ['m', 1e-3], ['', 1], ['k', 1e3], ['M', 1e6], ['G', 1e9], ['T', 1e12], ['P', 1e15]];
        const sc = {mul: base.mul, prefix: '', div: 1, unit: base.unit};
        const max = maxData * base.mul;
        if (!(max > 0)) return sc;
        let k = base.lo;
        for (let i = base.lo; i <= base.hi; i++) if (P[i][1] <= max * (1 + 1e-9)) k = i;
        sc.prefix = P[k][0];
        sc.div = P[k][1];
        return sc;
    }
    // fmtScaled formats |v|, a value in the base unit of sc (writes are drawn
    // below zero, their ticks show the magnitude), as the number and the
    // prefix letter, no space: no decimals from 100, one from 10, two from 1,
    // two significant digits below 1, trailing zeros trimmed ('2G', '1.5M',
    // '800\u03bc'); 0 is '0'.
    // With fixed (tooltips) there are always at least two digits after the
    // dot and zeros are kept ('2.00G', '950.00', '0.0033').
    function fmtScaled(v, sc, fixed) {
        const x = Math.abs(v) / (sc && sc.div ? sc.div : 1);
        if (fixed) {
            const pre = sc && sc.prefix ? sc.prefix : '';
            if (!isFinite(x) || x === 0) return '0.00';
            return x.toFixed(Math.min(20, Math.max(2, 1 - Math.floor(Math.log10(x))))) + pre;
        }
        if (!isFinite(x) || x === 0) return '0';
        let s = x >= 100 ? x.toFixed(0) : x >= 10 ? x.toFixed(1) : x >= 1 ? x.toFixed(2) : x.toPrecision(2);
        if (s.indexOf('.') !== -1 && s.indexOf('e') === -1) s = s.replace(/0+$/, '').replace(/\.$/, '');
        return s + (sc && sc.prefix ? sc.prefix : '');
    }
    // toBase converts the datasets' values ({x, y} points or bar numbers) to
    // the base unit of sc, in place; null stays null. Chart.js then places
    // its ticks at round base-unit values (500M, 1G), not at round MB/s.
    function toBase(datasets, sc) {
        datasets.forEach(ds => {
            ds.data = ds.data.map(p => typeof p === 'number' ? p * sc.mul
                : (p && p.y !== null && p.y !== undefined ? Object.assign({}, p, {y: p.y * sc.mul}) : p));
        });
        return datasets;
    }

    function esc(s) {
        return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    // Run groups: the entries of labelled drives carry setting_value (and
    // run_id, round, invalid). A plain run's entries carry none of them, so
    // these helpers change nothing for it.
    function hasSettingData(data) {
        return data.some(d => !!d.setting_value);
    }
    // Invalid results are left out unless showInvalid.
    function visibleEntries(data, showInvalid) {
        return showInvalid ? data : data.filter(d => !d.invalid);
    }
    // runsPerValue maps each value to the number of distinct runs it was
    // measured in.
    function runsPerValue(data) {
        const runs = {};
        data.forEach(d => {
            if (!d.setting_value) return;
            (runs[d.setting_value] = runs[d.setting_value] || new Set()).add(d.run_id);
        });
        const n = {};
        Object.keys(runs).forEach(v => { n[v] = runs[v].size; });
        return n;
    }
    // settingTag names an entry's setting and value, its round when the value was
    // measured in several runs, and flags an invalid result: ' · setting: value ·
    // round N (invalid)'. '' for an unlabelled entry. Plain text: escape it
    // before putting it into HTML.
    function settingTag(d, rpv) {
        if (!d.setting_value) return '';
        let s = ' · ' + (d.setting ? d.setting + ': ' : '') + d.setting_value;
        if (d.round && rpv && rpv[d.setting_value] > 1) s += ' · round ' + d.round;
        if (d.invalid) s += ' (invalid)';
        return s;
    }
    // membersText names the member disks of an array target and their roles:
    // 'members: sdb (data), sdc (spare)'; '' for a drive. Plain text: escape
    // it before putting it into HTML.
    function membersText(d) {
        if (!d.members || d.members.length === 0) return '';
        return 'members: ' + d.members.map(m => m.name + ' (' + m.role + ')').join(', ');
    }
`
