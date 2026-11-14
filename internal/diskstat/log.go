package diskstat

import (
	"bufio"
	"fmt"
	"io"
	"sort"
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
//
// Each md window adds, after the device lines:
//
//	md, end_ms, span_ms, action, completed_sectors, total_sectors, speed_kibps, degraded, array_state, raid_disks
//	mdmember, end_ms, device, state
func WriteLog(w io.Writer, m Meta, ws []Window, md []MDWindow) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "# avg_msec %d\n# offset_ms %d\n", m.AvgMsec, m.OffsetMs)
	if m.Array != "" {
		fmt.Fprintf(bw, "# array %s\n", m.Array)
	}
	for _, d := range m.Members {
		role := d.Role
		if role == "" {
			role = "data" // a member recorded without a role is a data disk; ReadLog needs the field
		}
		fmt.Fprintf(bw, "# device %s %s\n", d.Name, role)
	}
	fmt.Fprint(bw, "# end_ms, span_ms, device, read_ios, write_ios, read_sectors, write_sectors, read_ticks_ms, write_ticks_ms\n")
	for _, x := range ws {
		fmt.Fprintf(bw, "%d, %d, %s, %d, %d, %d, %d, %d, %d\n", x.EndMs, x.SpanMs, x.Device,
			x.D.ReadIOs, x.D.WriteIOs, x.D.ReadSectors, x.D.WriteSectors, x.D.ReadTicksMs, x.D.WriteTicksMs)
	}
	for _, x := range md {
		fmt.Fprintf(bw, "md, %d, %d, %s, %d, %d, %d, %d, %s, %d\n", x.EndMs, x.SpanMs, x.S.Action,
			x.S.Completed, x.S.Total, x.S.SpeedKiBps, x.S.Degraded, x.S.ArrayState, x.S.RaidDisks)
		names := make([]string, 0, len(x.S.Members))
		for n := range x.S.Members {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(bw, "mdmember, %d, %s, %s\n", x.EndMs, n, x.S.Members[n])
		}
	}
	return bw.Flush()
}

func parseMDLine(n int, line string, md []MDWindow) ([]MDWindow, error) {
	bad := func(format string, a ...any) ([]MDWindow, error) {
		return nil, fmt.Errorf("diskstat log line %d: %s", n, fmt.Sprintf(format, a...))
	}
	if strings.HasPrefix(line, "mdmember,") {
		f := strings.SplitN(line, ",", 4) // the state itself holds commas
		if len(f) != 4 {
			return bad("%d fields, want 4", len(f))
		}
		end, err := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		if err != nil {
			return bad("%v", err)
		}
		dev := strings.TrimSpace(f[2])
		if dev == "" {
			return bad("empty device")
		}
		for i := len(md) - 1; i >= 0; i-- {
			if md[i].EndMs == end {
				if md[i].S.Members == nil {
					md[i].S.Members = map[string]string{}
				}
				md[i].S.Members[dev] = strings.TrimSpace(f[3])
				return md, nil
			}
		}
		return bad("mdmember without an md line at %d", end)
	}
	f := strings.Split(line, ",")
	if len(f) != 10 {
		return bad("%d fields, want 10", len(f))
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	var w MDWindow
	var err error
	if w.EndMs, err = strconv.ParseInt(f[1], 10, 64); err != nil {
		return bad("%v", err)
	}
	if w.SpanMs, err = strconv.ParseInt(f[2], 10, 64); err != nil {
		return bad("%v", err)
	}
	w.S.Action, w.S.ArrayState = f[3], f[8]
	for i, p := range []*uint64{&w.S.Completed, &w.S.Total, &w.S.SpeedKiBps} {
		if *p, err = strconv.ParseUint(f[4+i], 10, 64); err != nil {
			return bad("%v", err)
		}
	}
	if w.S.Degraded, err = strconv.Atoi(f[7]); err != nil {
		return bad("%v", err)
	}
	if w.S.RaidDisks, err = strconv.Atoi(f[9]); err != nil {
		return bad("%v", err)
	}
	return append(md, w), nil
}

// ReadLog parses a members log. A malformed line is an error. A log without
// md lines returns no md windows.
func ReadLog(r io.Reader) (Meta, []Window, []MDWindow, error) {
	var m Meta
	var ws []Window
	var md []MDWindow
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
					return m, nil, nil, fmt.Errorf("diskstat log line %d: want '%s N'", n, f[0])
				}
				var v int
				if v, err = strconv.Atoi(f[1]); err != nil {
					return m, nil, nil, fmt.Errorf("diskstat log line %d: %w", n, err)
				}
				if f[0] == "avg_msec" {
					m.AvgMsec = v
				} else {
					m.OffsetMs = v
				}
			case "array":
				if len(f) != 2 {
					return m, nil, nil, fmt.Errorf("diskstat log line %d: want 'array <name>'", n)
				}
				m.Array = f[1]
			case "device":
				if len(f) != 3 {
					return m, nil, nil, fmt.Errorf("diskstat log line %d: want 'device <name> <role>'", n)
				}
				m.Members = append(m.Members, MemberMeta{f[1], f[2]})
			}
			continue // other comments (column header) are ignored
		}
		if strings.HasPrefix(line, "md,") || strings.HasPrefix(line, "mdmember,") {
			var err error
			if md, err = parseMDLine(n, line, md); err != nil {
				return m, nil, nil, err
			}
			continue
		}
		f := strings.Split(line, ",")
		if len(f) != 9 {
			return m, nil, nil, fmt.Errorf("diskstat log line %d: %d fields, want 9", n, len(f))
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		var w Window
		var err error
		if w.EndMs, err = strconv.ParseInt(f[0], 10, 64); err != nil {
			return m, nil, nil, fmt.Errorf("diskstat log line %d: %w", n, err)
		}
		if w.SpanMs, err = strconv.ParseInt(f[1], 10, 64); err != nil {
			return m, nil, nil, fmt.Errorf("diskstat log line %d: %w", n, err)
		}
		if f[2] == "" {
			return m, nil, nil, fmt.Errorf("diskstat log line %d: empty device", n)
		}
		w.Device = f[2]
		for i, p := range []*uint64{&w.D.ReadIOs, &w.D.WriteIOs, &w.D.ReadSectors, &w.D.WriteSectors, &w.D.ReadTicksMs, &w.D.WriteTicksMs} {
			if *p, err = strconv.ParseUint(f[3+i], 10, 64); err != nil {
				return m, nil, nil, fmt.Errorf("diskstat log line %d: %w", n, err)
			}
		}
		ws = append(ws, w)
	}
	if err := sc.Err(); err != nil {
		return m, nil, nil, err
	}
	return m, ws, md, nil
}
