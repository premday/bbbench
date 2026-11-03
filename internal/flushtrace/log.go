package flushtrace

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// LogName is the file holding the Flush samples of a phase, next to fio's own
// time-series logs (<prefix>_iops.1.log, <prefix>_lat.1.log, ...).
func LogName(prefix string) string { return prefix + "_flush.log" }

const logMagic = "# bbbench-flush-log v1"

// WriteLog stores the samples so graphs and statistics can be rebuilt for a
// finished run. Format (text, one Flush per line, like fio's logs):
//
//	# bbbench-flush-log v1 source=nvme exact=true lost=0
//	# time_ms, latency_ns
//	1523.411, 195000
//
// time_ms is the completion time since tracing started. The header carries
// what Summarize needs besides the samples.
func WriteLog(w io.Writer, sum Summary, all []Sample) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "%s source=%s exact=%t lost=%d\n# time_ms, latency_ns\n",
		logMagic, sum.Source, sum.Exact, sum.Lost)
	for _, x := range all {
		fmt.Fprintf(bw, "%.3f, %d\n", float64(x.TimeNs)/1e6, x.LatencyNs)
	}
	return bw.Flush()
}

// ReadLog parses a log written by WriteLog and recomputes the Summary from
// the samples, so the result always reflects the current statistics code.
func ReadLog(r io.Reader) (Summary, []Sample, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var (
		source string
		exact  bool
		lost   int
		all    []Sample
		first  = true
	)
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
					source = v
				case "exact":
					exact = v == "true"
				case "lost":
					lost, _ = strconv.Atoi(v)
				}
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		t, l, ok := strings.Cut(line, ",")
		if !ok {
			return Summary{}, nil, fmt.Errorf("bad flush log line %q", line)
		}
		ms, err1 := strconv.ParseFloat(strings.TrimSpace(t), 64)
		lat, err2 := strconv.ParseInt(strings.TrimSpace(l), 10, 64)
		if err1 != nil || err2 != nil {
			return Summary{}, nil, fmt.Errorf("bad flush log line %q", line)
		}
		all = append(all, Sample{TimeNs: int64(ms*1e6 + 0.5), LatencyNs: lat})
	}
	if err := sc.Err(); err != nil {
		return Summary{}, nil, err
	}
	if first {
		return Summary{}, nil, fmt.Errorf("empty flush log")
	}
	return Summarize(source, exact, lost, all), all, nil
}

// Bin is the Flush latency over one time window.
type Bin struct {
	TSec   float64 // window start, seconds since tracing started
	MeanNs float64
	Count  int
}

// Bins averages the samples over fixed windows (by completion time), for
// time-series graphs. Empty windows are omitted.
func Bins(all []Sample, windowNs int64) []Bin {
	if windowNs <= 0 {
		windowNs = 1_000_000_000
	}
	type acc struct {
		sum int64
		n   int
	}
	m := map[int64]*acc{}
	for _, x := range all {
		k := x.TimeNs / windowNs
		if x.TimeNs < 0 {
			k = 0
		}
		a := m[k]
		if a == nil {
			a = &acc{}
			m[k] = a
		}
		a.sum += x.LatencyNs
		a.n++
	}
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Bin, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		out = append(out, Bin{TSec: float64(k*windowNs) / 1e9, MeanNs: float64(a.sum) / float64(a.n), Count: a.n})
	}
	return out
}
