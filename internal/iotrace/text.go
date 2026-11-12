package iotrace

import (
	"strconv"
	"strings"
)

// SourceLine says which tracepoints each device was timed from, how the
// events were paired and how many were lost, e.g.
//
//	ftrace: block_rq_issue -> block_rq_complete, block_rq_insert -> block_rq_complete · pairing: first-in first-out (dev,sector) · lost events: 0
//
// NVMe commands pair exactly ("pairing: exact (ctrl,qid,cmdid)"), block
// requests first-in first-out per sector.
//
// Several devices are named one by one, separated by "; ". A device without
// any request events (an md or dm node: bio-based) says so instead of
// showing a series; a device that could not be traced gives the reason.
func SourceLine(s *Summary) string {
	var b strings.Builder
	b.WriteString("ftrace: ")
	if len(s.Devices) == 0 {
		b.WriteString("no device traced")
	}
	one := len(s.Devices) == 1
	for i, d := range s.Devices {
		if i > 0 {
			b.WriteString("; ")
		}
		if !one {
			b.WriteString(d.Name + ": ")
		}
		switch {
		case d.Source == "":
			b.WriteString("not traced (" + d.Note + ")")
		case len(d.Stats) == 0 && one:
			b.WriteString(strings.Join(d.Events, ", ") + " · no request events seen")
		case len(d.Stats) == 0:
			b.WriteString("no request events seen")
		default:
			pairing := "first-in first-out"
			if d.Exact {
				pairing = "exact"
			}
			b.WriteString(strings.Join(d.Events, ", ") + " · pairing: " + pairing + " (" + d.Pair + ")")
		}
	}
	var unpaired int64
	for _, d := range s.Devices {
		unpaired += d.Unpaired
	}
	lost := s.Overrun + unpaired
	b.WriteString(" · lost events: " + strconv.FormatInt(lost, 10))
	if lost > 0 {
		b.WriteString(" (ring buffer " + strconv.FormatInt(s.Overrun, 10) + ", unpaired " + strconv.FormatInt(unpaired, 10))
		if s.StartMissing {
			b.WriteString("; start marker not seen")
		}
		b.WriteString(")")
	}
	return b.String()
}
