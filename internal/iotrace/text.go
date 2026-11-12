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
			b.WriteString(strings.Join(d.Events, ", ") + " · " + noneSeen(d, KindDevice))
		case len(d.Stats) == 0:
			b.WriteString(noneSeen(d, KindDevice))
		default:
			pairing := "first-in first-out"
			if d.Exact {
				pairing = "exact"
			}
			b.WriteString(strings.Join(d.Events, ", ") + " · pairing: " + pairing + " (" + d.Pair + ")")
		}
	}
	lostTail(&b, s)
	return b.String()
}

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
// its bio tracepoints; KindBlockLayer says it has no block layer; KindBio
// names the bio tracepoints of the nodes and calls the other devices
// request-based.
func SourceLineOf(s *Summary, kind Kind) string {
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
		events, pairing, pair := deviceEvents(d), "first-in first-out", d.Pair
		if d.Exact {
			pairing = "exact"
		}
		if kind == KindBlockLayer {
			events, pairing, pair = blockLayerEvents(d), "first-in first-out", PairBlock
		}
		k := kind
		if d.Source == SourceBio && kind == KindDevice {
			k = KindBio // the node's series on the Device latency card
		}
		switch {
		case d.Source == "":
			b.WriteString("not traced (" + d.Note + ")")
		case kind == KindBlockLayer && d.Source == SourceBio:
			b.WriteString("bio-based, no block layer (bio-level latency on the Device latency card)")
		case kind == KindBlockLayer && d.BioBased && d.Source == SourceBlock:
			b.WriteString("bio-based, no block layer")
		case kind == KindBio && d.BioBased && d.Source != SourceBio:
			b.WriteString("bio-based, no bio tracepoints")
		case kind == KindBio && d.Source != SourceBio:
			b.WriteString("request-based (no bio series)")
		case kind == KindBlockLayer && !d.BlockLayer:
			b.WriteString("no " + insertTracepoint + " tracepoint")
		case len(d.Stats[k]) == 0 && one:
			b.WriteString(strings.Join(events, ", ") + " · " + noneSeen(d, kind))
		case len(d.Stats[k]) == 0:
			b.WriteString(noneSeen(d, kind))
		default:
			b.WriteString(strings.Join(events, ", ") + " · pairing: " + pairing + " (" + pair + ")")
		}
	}
	lostTail(&b, s)
	return b.String()
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

// lostTail appends the lost-events part of a source line.
func lostTail(b *strings.Builder, s *Summary) {
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
}
