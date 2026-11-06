package disktemp

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// LogName is the temperature log of a phase, named like fio's own per-job
// logs next to it (<prefix>_iops.1.log, ...).
func LogName(prefix string) string { return prefix + "_temp.1.log" }

const logMagic = "# bbbench-temp-log v1"

// Summary describes the temperature of a drive over a phase.
type Summary struct {
	Source     string  `json:"source"`
	Sensor     string  `json:"sensor,omitempty"`
	Count      int     `json:"count"`
	MinC       float64 `json:"min_c"`
	MaxC       float64 `json:"max_c"`
	AvgC       float64 `json:"avg_c"`
	WarnC      float64 `json:"warn_c,omitempty"` // the drive's own warning limit
	CritC      float64 `json:"crit_c,omitempty"` // the drive's own critical limit
	IntervalMs int     `json:"interval_ms,omitempty"`
}

// Summarize computes the Summary of the samples of a sensor.
func Summarize(s *Sensor, interval int, all []Sample) Summary {
	out := Summary{Source: s.Source, Sensor: s.Name, WarnC: s.WarnC, CritC: s.CritC, IntervalMs: interval, Count: len(all)}
	if len(all) == 0 {
		return out
	}
	lo, hi, sum := all[0].MilliC, all[0].MilliC, int64(0)
	for _, x := range all {
		lo, hi, sum = min(lo, x.MilliC), max(hi, x.MilliC), sum+x.MilliC
	}
	out.MinC, out.MaxC, out.AvgC = float64(lo)/1000, float64(hi)/1000, float64(sum)/1000/float64(len(all))
	return out
}

// WriteLog stores the samples like fio stores its logs, one line per sample in
// fio's columns (time_ms, value, dir, bs, offset; the value is milli-°C), so a
// fio log reader reads it. The header carries the sensor and its limits:
//
//	# bbbench-temp-log v1 source=hwmon sensor=Composite interval_ms=5000 warn_mc=82850 crit_mc=84850
//	# time_ms, temp_millic, dir, bs, offset
//	0, 41850, 0, 0, 0
func WriteLog(w io.Writer, sum Summary, all []Sample) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "%s source=%s sensor=%s interval_ms=%d", logMagic, sum.Source, strings.ReplaceAll(sum.Sensor, " ", "_"), sum.IntervalMs)
	if sum.WarnC > 0 {
		fmt.Fprintf(bw, " warn_mc=%d", int64(sum.WarnC*1000+0.5))
	}
	if sum.CritC > 0 {
		fmt.Fprintf(bw, " crit_mc=%d", int64(sum.CritC*1000+0.5))
	}
	fmt.Fprint(bw, "\n# time_ms, temp_millic, dir, bs, offset\n")
	for _, x := range all {
		fmt.Fprintf(bw, "%d, %d, 0, 0, 0\n", x.TimeMs, x.MilliC)
	}
	return bw.Flush()
}

// ReadLog parses a log written by WriteLog; the Summary is recomputed from the
// samples, so it always reflects the current statistics code.
func ReadLog(r io.Reader) (Summary, []Sample, error) {
	var (
		meta  Summary
		all   []Sample
		first = true
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if !strings.HasPrefix(line, logMagic) {
				return Summary{}, nil, fmt.Errorf("not a bbbench temperature log")
			}
			for _, f := range strings.Fields(strings.TrimPrefix(line, logMagic)) {
				k, v, _ := strings.Cut(f, "=")
				n, _ := strconv.ParseInt(v, 10, 64)
				switch k {
				case "source":
					meta.Source = v
				case "sensor":
					meta.Sensor = strings.ReplaceAll(v, "_", " ")
				case "interval_ms":
					meta.IntervalMs = int(n)
				case "warn_mc":
					meta.WarnC = float64(n) / 1000
				case "crit_mc":
					meta.CritC = float64(n) / 1000
				}
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 2 {
			return Summary{}, nil, fmt.Errorf("bad temperature log line %q", line)
		}
		t, err1 := strconv.ParseInt(strings.TrimSpace(f[0]), 10, 64)
		c, err2 := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		if err1 != nil || err2 != nil {
			return Summary{}, nil, fmt.Errorf("bad temperature log line %q", line)
		}
		all = append(all, Sample{TimeMs: t, MilliC: c})
	}
	if err := sc.Err(); err != nil {
		return Summary{}, nil, err
	}
	if first {
		return Summary{}, nil, fmt.Errorf("empty temperature log")
	}
	sum := Summarize(&Sensor{Source: meta.Source, Name: meta.Sensor, WarnC: meta.WarnC, CritC: meta.CritC}, meta.IntervalMs, all)
	return sum, all, nil
}
