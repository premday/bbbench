package flushtrace

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// LogName is the file holding the Flush latency of a phase, named like fio's
// own per-job time-series logs next to it (<prefix>_iops.1.log, ...).
func LogName(prefix string) string { return prefix + "_flush.1.log" }

const logMagic = "# bbbench-flush-log v2"

// DefaultWindowMs is the averaging window (fio's --log-avg-msec default) when
// none is recorded.
const DefaultWindowMs = 1000

// WriteLog stores a phase's Flush latency like fio stores its logs: one line
// per averaging window (fio's --log-avg-msec), stamped at the END of the
// window, so it is small (hundreds of lines, not one per Flush):
//
//	# bbbench-flush-log v2 source=nvme exact=true lost=0 write_cache=write_back avg_msec=1000 offset_ms=10000
//	# summary count=94213 mean_ns=195000.5 min_ns=120000 max_ns=480000 p50_ns=190000 p99_ns=210000
//	# end_ms, mean_latency_ns, dir, bs, offset, count, min_ns, max_ns
//	1000, 194800, 3, 0, 0, 312, 120000, 260000
//
// The first five columns are fio's (3 is fio's direction for sync), so a fio
// log parser reads it; count, min and max follow. times are ms since tracing
// started. The summary is the whole phase after the ramp-up (see Finish),
// computed from the raw samples, which are not kept: exact percentiles cannot
// be rebuilt from windows.
func WriteLog(w io.Writer, sum Summary, all []Sample) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "%s source=%s exact=%t lost=%d", logMagic, sum.Source, sum.Exact, sum.Lost)
	if sum.WriteCache != "" {
		fmt.Fprintf(bw, " write_cache=%s", strings.ReplaceAll(sum.WriteCache, " ", "_"))
	}
	if sum.AvgMsec > 0 {
		fmt.Fprintf(bw, " avg_msec=%d", sum.AvgMsec)
	}
	if sum.OffsetMs > 0 {
		fmt.Fprintf(bw, " offset_ms=%d", sum.OffsetMs)
	}
	fmt.Fprintf(bw, "\n# summary count=%d mean_ns=%s min_ns=%d max_ns=%d p50_ns=%d p99_ns=%d\n",
		sum.Count, strconv.FormatFloat(sum.MeanNs, 'g', -1, 64), sum.MinNs, sum.MaxNs, sum.P50Ns, sum.P99Ns)
	fmt.Fprint(bw, "# end_ms, mean_latency_ns, dir, bs, offset, count, min_ns, max_ns\n")
	windowMs := sum.AvgMsec
	if windowMs <= 0 {
		windowMs = DefaultWindowMs
	}
	for _, x := range Windows(all, windowMs) {
		fmt.Fprintf(bw, "%d, %d, 3, 0, 0, %d, %d, %d\n", x.EndMs, x.MeanNs, x.Count, x.MinNs, x.MaxNs)
	}
	return bw.Flush()
}

// ReadLog parses a log written by WriteLog: the Summary from its header and
// the windows from its lines.
func ReadLog(r io.Reader) (Summary, []Window, error) {
	sc := bufio.NewScanner(r)
	var (
		sum Summary
		win []Window
	)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if !strings.HasPrefix(line, logMagic) {
				return Summary{}, nil, fmt.Errorf("not a bbbench flush log")
			}
			for _, f := range strings.Fields(strings.TrimPrefix(line, logMagic)) {
				k, v, _ := strings.Cut(f, "=")
				switch k {
				case "source":
					sum.Source, sum.Events = v, EventsOf(v)
				case "exact":
					sum.Exact = v == "true"
				case "lost":
					sum.Lost, _ = strconv.Atoi(v)
				case "avg_msec":
					sum.AvgMsec, _ = strconv.Atoi(v)
				case "offset_ms":
					sum.OffsetMs, _ = strconv.Atoi(v)
				case "write_cache":
					sum.WriteCache = strings.ReplaceAll(v, "_", " ")
				}
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# summary "); ok {
			for _, f := range strings.Fields(rest) {
				k, v, _ := strings.Cut(f, "=")
				n, _ := strconv.ParseInt(v, 10, 64)
				switch k {
				case "count":
					sum.Count = int(n)
				case "mean_ns":
					sum.MeanNs, _ = strconv.ParseFloat(v, 64)
				case "min_ns":
					sum.MinNs = n
				case "max_ns":
					sum.MaxNs = n
				case "p50_ns":
					sum.P50Ns = n
				case "p99_ns":
					sum.P99Ns = n
				}
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		x, ok := parseWindow(line)
		if !ok {
			return Summary{}, nil, fmt.Errorf("bad flush log line %q", line)
		}
		win = append(win, x)
	}
	if err := sc.Err(); err != nil {
		return Summary{}, nil, err
	}
	if first {
		return Summary{}, nil, fmt.Errorf("empty flush log")
	}
	return sum, win, nil
}

// parseWindow reads one data line: end_ms, mean_ns, dir, bs, offset, count,
// min_ns, max_ns (the middle fio columns are ignored).
func parseWindow(line string) (w Window, ok bool) {
	f := strings.Split(line, ",")
	if len(f) < 8 {
		return w, false
	}
	var err error
	num := func(i int) int64 {
		n, e := strconv.ParseInt(strings.TrimSpace(f[i]), 10, 64)
		if e != nil {
			err = e
		}
		return n
	}
	w = Window{EndMs: num(0), MeanNs: num(1), Count: int(num(5)), MinNs: num(6), MaxNs: num(7)}
	return w, err == nil
}

// Finish is the one place a traced phase's Summary is built, live and when a
// stored log is loaded: Summarize over the samples that completed after the
// ramp (like fio's statistics), plus the metadata of meta (Source, Exact, Lost,
// WriteCache, AvgMsec, OffsetMs).
func Finish(meta Summary, all []Sample) Summary {
	sum := Summarize(meta.Source, meta.Exact, meta.Lost, afterMs(all, meta.OffsetMs))
	sum.WriteCache, sum.AvgMsec, sum.OffsetMs = meta.WriteCache, meta.AvgMsec, meta.OffsetMs
	return sum
}

// afterMs returns the samples that completed at or after ms milliseconds
// (never before time zero: a Flush that completed before the last start
// marker, i.e. before a start gate opened, has a negative time).
func afterMs(all []Sample, ms int) []Sample {
	ms = max(ms, 0)
	var out []Sample
	for _, x := range all {
		if x.TimeNs >= int64(ms)*1_000_000 {
			out = append(out, x)
		}
	}
	return out
}

// Window is the Flush latency over one averaging window.
type Window struct {
	EndMs  int64 // end of the window, ms since tracing started (fio stamps window ends)
	Count  int
	MeanNs int64
	MinNs  int64
	MaxNs  int64
}

// Windows averages the samples over fixed windows of windowMs (by completion
// time), in time order. Empty windows are omitted, and so are samples before
// time zero (negative times: completed before the last start marker).
func Windows(all []Sample, windowMs int) []Window {
	if windowMs <= 0 {
		windowMs = DefaultWindowMs
	}
	windowNs := int64(windowMs) * 1_000_000
	type acc struct {
		sum, min, max int64
		n             int
	}
	m := map[int64]*acc{}
	for _, x := range all {
		if x.TimeNs < 0 {
			continue
		}
		k := x.TimeNs / windowNs
		a := m[k]
		if a == nil {
			a = &acc{min: x.LatencyNs, max: x.LatencyNs}
			m[k] = a
		}
		a.sum += x.LatencyNs
		a.n++
		a.min, a.max = min(a.min, x.LatencyNs), max(a.max, x.LatencyNs)
	}
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]Window, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		out = append(out, Window{EndMs: (k + 1) * int64(windowMs), Count: a.n,
			MeanNs: a.sum / int64(a.n), MinNs: a.min, MaxNs: a.max})
	}
	return out
}
