package diskstat

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Meta is the header of a members log.
type Meta struct {
	AvgMsec, OffsetMs int
	Members           []MemberMeta
	Array             string
}

// MemberMeta names one member of the array and its role (data, spare...).
type MemberMeta struct{ Name, Role string }

// LogName is the file name of a phase's members log, next to fio's logs.
func LogName(prefix string) string { return prefix + "_members.1.log" }

// WriteLog writes the header as '#' lines, then one line per window:
//
//	# avg_msec 1000
//	# offset_ms 10000
//	# array md0
//	# device sda data
//	# end_ms, span_ms, device, read_ios, write_ios, read_sectors, write_sectors, read_ticks_ms, write_ticks_ms
//	1000, 1000, sda, 10, 20, 80, 160, 5, 9
func WriteLog(w io.Writer, m Meta, ws []Window) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "# avg_msec %d\n# offset_ms %d\n", m.AvgMsec, m.OffsetMs)
	if m.Array != "" {
		fmt.Fprintf(bw, "# array %s\n", m.Array)
	}
	for _, d := range m.Members {
		fmt.Fprintf(bw, "# device %s %s\n", d.Name, d.Role)
	}
	fmt.Fprint(bw, "# end_ms, span_ms, device, read_ios, write_ios, read_sectors, write_sectors, read_ticks_ms, write_ticks_ms\n")
	for _, x := range ws {
		fmt.Fprintf(bw, "%d, %d, %s, %d, %d, %d, %d, %d, %d\n", x.EndMs, x.SpanMs, x.Device,
			x.D.ReadIOs, x.D.WriteIOs, x.D.ReadSectors, x.D.WriteSectors, x.D.ReadTicksMs, x.D.WriteTicksMs)
	}
	return bw.Flush()
}

// ReadLog parses a members log. A malformed line is an error.
func ReadLog(r io.Reader) (Meta, []Window, error) {
	var m Meta
	var ws []Window
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "#"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				continue
			}
			var err error
			switch f[0] {
			case "avg_msec", "offset_ms":
				if len(f) != 2 {
					return m, nil, fmt.Errorf("diskstat log line %d: want '%s N'", n, f[0])
				}
				var v int
				if v, err = strconv.Atoi(f[1]); err != nil {
					return m, nil, fmt.Errorf("diskstat log line %d: %w", n, err)
				}
				if f[0] == "avg_msec" {
					m.AvgMsec = v
				} else {
					m.OffsetMs = v
				}
			case "array":
				if len(f) != 2 {
					return m, nil, fmt.Errorf("diskstat log line %d: want 'array <name>'", n)
				}
				m.Array = f[1]
			case "device":
				if len(f) != 3 {
					return m, nil, fmt.Errorf("diskstat log line %d: want 'device <name> <role>'", n)
				}
				m.Members = append(m.Members, MemberMeta{f[1], f[2]})
			}
			continue // other comments (column header) are ignored
		}
		f := strings.Split(line, ",")
		if len(f) != 9 {
			return m, nil, fmt.Errorf("diskstat log line %d: %d fields, want 9", n, len(f))
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		var w Window
		var err error
		if w.EndMs, err = strconv.ParseInt(f[0], 10, 64); err != nil {
			return m, nil, fmt.Errorf("diskstat log line %d: %w", n, err)
		}
		if w.SpanMs, err = strconv.ParseInt(f[1], 10, 64); err != nil {
			return m, nil, fmt.Errorf("diskstat log line %d: %w", n, err)
		}
		if f[2] == "" {
			return m, nil, fmt.Errorf("diskstat log line %d: empty device", n)
		}
		w.Device = f[2]
		for i, p := range []*uint64{&w.D.ReadIOs, &w.D.WriteIOs, &w.D.ReadSectors, &w.D.WriteSectors, &w.D.ReadTicksMs, &w.D.WriteTicksMs} {
			if *p, err = strconv.ParseUint(f[3+i], 10, 64); err != nil {
				return m, nil, fmt.Errorf("diskstat log line %d: %w", n, err)
			}
		}
		ws = append(ws, w)
	}
	if err := sc.Err(); err != nil {
		return m, nil, err
	}
	return m, ws, nil
}
