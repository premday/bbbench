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
// helpers both pages use (setting values, rounds, invalid results) and esc.
const chartSharedJS = `function rampOf(d) {
        return d.ramp_sec || (d.flush && d.flush.offset_ms ? d.flush.offset_ms / 1000 : 0);
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
        const shift = metric.key === 'flush' ? 0 : fioShift(d);
        return Math.max(...ts.map(p => p.t)) + shift;
    }

    function fmtTick(v) {
        const a = Math.abs(v);
        if (a === 0) return '0';
        return a >= 1000 ? (a/1000).toFixed(1)+'k' : a.toFixed(0);
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
`
