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
func SourceLine(s *Summary) string { return sourceLine(s, "") }

// PairBlock is the pairing key of block requests, first-in first-out.
const PairBlock = "dev,sector"

// PairBio is the pairing key of the bios of a bio-based node, first-in
// first-out (through the end sector, see the package doc).
const PairBio = "dev,sector,nr_sectors"

// bioEventsText is the tracepoint pair of a bio-based node.
var bioEventsText = strings.TrimPrefix(evBioQueue, "block/") + " -> " + strings.TrimPrefix(evBioDone, "block/")

// insertTracepoint names the block-layer start event in the texts.
var insertTracepoint = strings.TrimPrefix(evBlockIns, "block/")

// noneSeen is the text of a traced device without events of a kind.
func noneSeen(d DeviceSummary, kind Kind) string {
	switch {
	case kind == KindBlockLayer:
		return "no block-layer events seen"
	case d.Source == SourceBio:
		return "no bio events seen"
	}
	return "no request events seen"
}

// SourceLineOf is SourceLine for one kind of latency: for KindDevice the
// device tracepoints and their pairing, for KindBlockLayer the block-layer
// ones, which pair block requests first-in first-out by (dev, sector) for
// every source (NVMe included). A device without that kind's events says
// so; one without block-layer tracepoint says that instead. A bio-based
// node's bio series is drawn on the Device latency card, so KindDevice names
// its bio tracepoints; KindBlockLayer says it has no block layer.
func SourceLineOf(s *Summary, kind Kind) string { return sourceLine(s, kind) }

// sourceLine builds SourceLine (kind "": every event of a device, any kind
// of series counts as seen) and SourceLineOf (one kind).
func sourceLine(s *Summary, kind Kind) string {
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
		b.WriteString(deviceSource(d, kind, one))
	}
	lostTail(&b, s)
	return b.String()
}

// deviceSource is one device's part of sourceLine.
func deviceSource(d DeviceSummary, kind Kind, one bool) string {
	events, pairing, pair := deviceEvents(d), "first-in first-out", d.Pair
	if d.Exact {
		pairing = "exact"
	}
	none := kind
	var seen bool
	switch {
	case kind == "":
		events, none, seen = d.Events, KindDevice, len(d.Stats) > 0
	case kind == KindBlockLayer:
		events, pairing, pair = blockLayerEvents(d), "first-in first-out", PairBlock
		seen = len(d.Stats[kind]) > 0
	case d.Source == SourceBio && kind == KindDevice:
		seen = len(d.Stats[KindBio]) > 0 // the node's series on the Device latency card
	default:
		seen = len(d.Stats[kind]) > 0
	}
	switch {
	case d.Source == "":
		return "not traced (" + d.Note + ")"
	case kind == KindBlockLayer && d.Source == SourceBio:
		return "bio-based, no block layer (bio-level latency on the Device latency card)"
	case kind == KindBlockLayer && d.BioBased && d.Source == SourceBlock:
		return "bio-based, no block layer"
	case kind == KindBlockLayer && !d.BlockLayer:
		return "no " + insertTracepoint + " tracepoint"
	case !seen && one:
		return strings.Join(events, ", ") + " · " + noneSeen(d, none)
	case !seen:
		return noneSeen(d, none)
	}
	return strings.Join(events, ", ") + " · pairing: " + pairing + " (" + pair + ")"
}

// deviceEvents and blockLayerEvents split a device's Events: Start appends
// the block-layer pair last when the device has one.
func deviceEvents(d DeviceSummary) []string {
	if d.BlockLayer && len(d.Events) > 1 {
		return d.Events[:len(d.Events)-1]
	}
	return d.Events
}

func blockLayerEvents(d DeviceSummary) []string {
	if d.BlockLayer && len(d.Events) > 1 {
		return d.Events[len(d.Events)-1:]
	}
	return nil
}

// blockLayerNote says why a device has no block-layer series, "" when it
// has one. A native NVMe multipath drive queues its block requests on the
// hidden path disks (nvme0c1n1), whose dev numbers its filter does not match.
func blockLayerNote(d DeviceSummary) string {
	switch {
	case d.Source == "":
		return "not traced (" + d.Note + ")"
	case d.Source == SourceBio:
		return "bio-based (no request queue): no block-layer series; bio-level latency (" + bioEventsText + ") on the Device latency card"
	case d.BioBased && d.Source == SourceBlock:
		return "bio-based (no request queue): no block-layer series; the kernel has no " + strings.Join([]string{strings.TrimPrefix(evBioQueue, "block/"), strings.TrimPrefix(evBioDone, "block/")}, "/") + " tracepoints"
	case !d.BlockLayer:
		return "the kernel has no " + insertTracepoint + " tracepoint"
	case len(d.Stats[KindBlockLayer]) > 0:
		return ""
	case d.Source == SourceNVMe:
		return "ftrace saw no block-layer requests on " + d.Name + " (a native NVMe multipath drive queues them on its hidden path disks)"
	}
	return "ftrace saw no block-layer requests on " + d.Name
}

// lostTail appends the lost-events part of a source line, from s.Lost
// (set by Fill).
func lostTail(b *strings.Builder, s *Summary) {
	b.WriteString(" · lost events: " + strconv.FormatInt(s.Lost, 10))
	if s.Lost > 0 {
		b.WriteString(" (ring buffer " + strconv.FormatInt(s.Overrun, 10) + ", unpaired " + strconv.FormatInt(s.Lost-s.Overrun, 10))
		if s.StartMissing {
			b.WriteString("; start marker not seen")
		}
		b.WriteString(")")
	}
}
