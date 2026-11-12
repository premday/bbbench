package iotrace

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// LogName is the file holding the I/O latency of a phase, named like fio's
// own per-job time-series logs next to it.
func LogName(prefix string) string { return prefix + "_iolat.1.log" }

const logMagic = "# bbbench-iolat-log v1"

func ff(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func statsFields(s Stats) string {
	return fmt.Sprintf("count=%d mean_ns=%s min_ns=%d max_ns=%d p50_ns=%d p99_ns=%d",
		s.Count, ff(s.MeanNs), s.MinNs, s.MaxNs, s.P50Ns, s.P99Ns)
}

// WriteLog stores a phase like fio stores its logs: a header, one line per
// device and per summary (kind, direction), then one data line per window
// stamped at the window END:
//
//	# bbbench-iolat-log v1 avg_msec=1000 offset_ms=10000 overrun=0 lost=0
//	# device nvme0n1 source=nvme exact=true block_layer=true unpaired=0 pair=ctrl,qid,cmdid events=a->b,c->d
//	# summary nvme0n1 device r count=312 mean_ns=85000.5 min_ns=60000 max_ns=410000 p50_ns=84000 p99_ns=120000
//	# end_ms, kind, dir, count, mean_ns, min_ns, max_ns, p50_ns, p99_ns, dev
//	1000, device, r, 312, 85000, 60000, 410000, 84000, 120000, nvme0n1
func WriteLog(w io.Writer, r Result) error {
	bw := bufio.NewWriter(w)
	s := &r.Summary
	fmt.Fprintf(bw, "%s avg_msec=%d offset_ms=%d overrun=%d lost=%d", logMagic, s.AvgMsec, s.OffsetMs, s.Overrun, s.Lost)
	if s.StartMissing {
		fmt.Fprint(bw, " start_missing=true")
	}
	fmt.Fprintln(bw)
	for _, d := range s.Devices {
		fmt.Fprintf(bw, "# device %s", d.Name)
		if d.Source != "" {
			fmt.Fprintf(bw, " source=%s exact=%t block_layer=%t unpaired=%d", d.Source, d.Exact, d.BlockLayer, d.Unpaired)
			if d.Pair != "" {
				fmt.Fprintf(bw, " pair=%s", d.Pair)
			}
			if len(d.Events) > 0 {
				fmt.Fprintf(bw, " events=%s", strings.ReplaceAll(strings.Join(d.Events, ","), " -> ", "->"))
			}
		}
		if d.Note != "" {
			fmt.Fprintf(bw, " note=%s", url.QueryEscape(d.Note))
		}
		fmt.Fprintln(bw)
	}
	for _, d := range s.Devices {
		kinds := make([]string, 0, len(d.Stats))
		for k := range d.Stats {
			kinds = append(kinds, string(k))
		}
		slices.Sort(kinds)
		for _, k := range kinds {
			dirs := make([]string, 0, 2)
			for dir := range d.Stats[Kind(k)] {
				dirs = append(dirs, string(dir))
			}
			slices.Sort(dirs)
			for _, dir := range dirs {
				fmt.Fprintf(bw, "# summary %s %s %s %s\n", d.Name, k, dir, statsFields(d.Stats[Kind(k)][Dir(dir)]))
			}
		}
	}
	fmt.Fprintln(bw, "# end_ms, kind, dir, count, mean_ns, min_ns, max_ns, p50_ns, p99_ns, dev")
	for _, x := range r.Windows {
		fmt.Fprintf(bw, "%d, %s, %s, %d, %s, %d, %d, %d, %d, %s\n",
			x.EndMs, x.Kind, x.Dir, x.Count, ff(x.MeanNs), x.MinNs, x.MaxNs, x.P50Ns, x.P99Ns, x.Dev)
	}
	return bw.Flush()
}

// ReadLog parses a log written by WriteLog and fills Lost and Text.
func ReadLog(rd io.Reader) (Result, error) {
	sc := bufio.NewScanner(rd)
	var res Result
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if !strings.HasPrefix(line, logMagic) {
				return Result{}, fmt.Errorf("not a bbbench I/O latency log")
			}
			for _, f := range strings.Fields(strings.TrimPrefix(line, logMagic)) {
				k, v, _ := strings.Cut(f, "=")
				n, _ := strconv.ParseInt(v, 10, 64)
				switch k {
				case "avg_msec":
					res.Summary.AvgMsec = int(n)
				case "offset_ms":
					res.Summary.OffsetMs = int(n)
				case "overrun":
					res.Summary.Overrun = n
				case "start_missing":
					res.Summary.StartMissing = v == "true"
				}
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# device "); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				continue
			}
			d := DeviceSummary{Name: f[0]}
			for _, kv := range f[1:] {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "source":
					d.Source = v
				case "exact":
					d.Exact = v == "true"
				case "block_layer":
					d.BlockLayer = v == "true"
				case "unpaired":
					d.Unpaired, _ = strconv.ParseInt(v, 10, 64)
				case "pair":
					d.Pair = v
				case "events":
					d.Events = strings.Split(strings.ReplaceAll(v, "->", " -> "), ",")
				case "note":
					d.Note, _ = url.QueryUnescape(v)
				}
			}
			res.Summary.Devices = append(res.Summary.Devices, d)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# summary "); ok {
			f := strings.Fields(rest)
			if len(f) < 3 {
				continue
			}
			var st Stats
			for _, kv := range f[3:] {
				k, v, _ := strings.Cut(kv, "=")
				n, _ := strconv.ParseInt(v, 10, 64)
				switch k {
				case "count":
					st.Count = n
				case "mean_ns":
					st.MeanNs, _ = strconv.ParseFloat(v, 64)
				case "min_ns":
					st.MinNs = n
				case "max_ns":
					st.MaxNs = n
				case "p50_ns":
					st.P50Ns = n
				case "p99_ns":
					st.P99Ns = n
				}
			}
			for i := range res.Summary.Devices {
				d := &res.Summary.Devices[i]
				if d.Name != f[0] {
					continue
				}
				if d.Stats == nil {
					d.Stats = map[Kind]map[Dir]Stats{}
				}
				if d.Stats[Kind(f[1])] == nil {
					d.Stats[Kind(f[1])] = map[Dir]Stats{}
				}
				d.Stats[Kind(f[1])][Dir(f[2])] = st
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		x, ok := parseWindow(line)
		if !ok {
			return Result{}, fmt.Errorf("bad I/O latency log line %q", line)
		}
		res.Windows = append(res.Windows, x)
	}
	if err := sc.Err(); err != nil {
		return Result{}, err
	}
	if first {
		return Result{}, fmt.Errorf("empty I/O latency log")
	}
	res.Summary.Fill()
	return res, nil
}

// parseWindow reads one data line: end_ms, kind, dir, count, mean_ns, min_ns,
// max_ns, p50_ns, p99_ns, dev.
func parseWindow(line string) (w Window, ok bool) {
	f := strings.Split(line, ",")
	if len(f) != 10 {
		return w, false
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	var err error
	num := func(i int) int64 {
		n, e := strconv.ParseInt(f[i], 10, 64)
		if e != nil {
			err = e
		}
		return n
	}
	mean, e := strconv.ParseFloat(f[4], 64)
	if e != nil {
		return w, false
	}
	w = Window{EndMs: num(0), Kind: Kind(f[1]), Dir: Dir(f[2]), Dev: f[9]}
	w.Count, w.MeanNs, w.MinNs, w.MaxNs, w.P50Ns, w.P99Ns = num(3), mean, num(5), num(6), num(7), num(8)
	return w, err == nil && w.Dev != ""
}
