package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"bbbench/internal/diskstat"
)

// MemberSeries is the I/O of one member disk of an array over a phase,
// sampled from its kernel counters. R is read, W is write; IOPS per second,
// BW in MiB/s, Lat the mean per I/O in µs (0 when the direction did no I/O).
type MemberSeries struct {
	Role string      `json:"role,omitempty"` // "" = data disk
	IOPS []TimePoint `json:"iops,omitempty"`
	BW   []TimePoint `json:"bw,omitempty"`
	Lat  []TimePoint `json:"lat,omitempty"`
}

// startMemberSampler samples the members of an array until the returned stop
// is called, which writes the members log next to fio's logs. It does nothing
// for a drive that is not an array. Failures only warn.
func startMemberSampler(drive DriveInfo, logPrefix string, avgMsec, rampMs int) (stop func()) {
	return startMemberSamplerAt(drive, logPrefix, avgMsec, rampMs, "")
}

func startMemberSamplerAt(drive DriveInfo, logPrefix string, avgMsec, rampMs int, sysBlock string) (stop func()) {
	if !drive.IsArray() {
		return func() {}
	}
	meta := diskstat.Meta{AvgMsec: avgMsec, OffsetMs: rampMs, Array: drive.Device.Name}
	names := make([]string, 0, len(drive.Members))
	for _, m := range drive.Members {
		names = append(names, m.Name)
		meta.Members = append(meta.Members, diskstat.MemberMeta{Name: m.Name, Role: string(m.Role)})
	}
	s := diskstat.NewSampler(names, sysBlock).WithArray(drive.Device.Name, names)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, time.Duration(avgMsec)*time.Millisecond)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			if err := writeMembersLog(logPrefix, meta, s.Windows(), s.MDWindows()); err != nil && logger != nil {
				logger.Warn("storing member counters", "array", drive.Device.Name, "err", err)
			}
		})
	}
}

func writeMembersLog(logPrefix string, meta diskstat.Meta, ws []diskstat.Window, md []diskstat.MDWindow) error {
	f, err := os.Create(diskstat.LogName(logPrefix))
	if err != nil {
		return err
	}
	if err := diskstat.WriteLog(f, meta, ws, md); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// inRamp reports whether a sampling window belongs to fio's ramp-up. EndMs
// comes from the wall clock (k*avg plus a few ms of jitter), so the window is
// judged by its midpoint: one closing the ramp at offset+1ms is still ramp.
func inRamp(endMs, spanMs, offsetMs int64) bool {
	return endMs-spanMs/2 <= offsetMs
}

// MDPoint is the md state of an array at the start of one sampling window.
// Pct is the sync_completed progress (0 when the total is unknown), SpeedKiBps
// the kernel's sync speed in KiB/s as stored.
type MDPoint struct {
	T          float64 `json:"t"`
	Action     string  `json:"action"`
	Pct        float64 `json:"pct"`
	SpeedKiBps uint64  `json:"speed"`
	Degraded   int     `json:"degraded"` // -1 = unreadable (unknown)
	State      string  `json:"state"`
}

// MDEvent is one change of the md state, stamped at the start of the window
// in which it was first seen.
type MDEvent struct {
	T    float64 `json:"t"`
	Text string  `json:"text"`
}

// MDInfo is the md state series and its events for one array phase.
type MDInfo struct {
	Points []MDPoint `json:"points,omitempty"`
	Events []MDEvent `json:"events,omitempty"`
}

func mdPoint(w diskstat.MDWindow) MDPoint {
	p := MDPoint{T: float64(w.EndMs-w.SpanMs) / 1000, Action: w.S.Action, SpeedKiBps: w.S.SpeedKiBps,
		Degraded: w.S.Degraded, State: w.S.ArrayState}
	if w.S.Total > 0 {
		p.Pct = float64(w.S.Completed) / float64(w.S.Total) * 100
	}
	return p
}

// mdEventsFrom derives the events of consecutive md windows. base is the
// state before the first window (nil = the phase start: a non-idle action or a
// degraded array is then reported with "(at phase start)").
func mdEventsFrom(base *diskstat.MDState, wins []diskstat.MDWindow) []MDEvent {
	var ev []MDEvent
	prev := base
	for _, w := range wins {
		t := float64(w.EndMs-w.SpanMs) / 1000
		add := func(f string, a ...any) { ev = append(ev, MDEvent{T: t, Text: fmt.Sprintf(f, a...)}) }
		cur := w.S
		if prev == nil {
			if cur.Action != "" && cur.Action != "idle" {
				add("sync_action %s (at phase start)", cur.Action)
			}
			if cur.Degraded > 0 {
				add("degraded %d (at phase start)", cur.Degraded)
			}
		} else {
			// an unreadable value ("", 0 raid_disks or -1 degraded) is not a change; the
			// last known value stays the baseline for the next window
			if cur.Action != "" && cur.Action != prev.Action {
				add("sync_action %s -> %s", prev.Action, cur.Action)
			}
			if cur.ArrayState != "" && cur.ArrayState != prev.ArrayState {
				add("array_state %s -> %s", prev.ArrayState, cur.ArrayState)
			}
			if cur.Degraded >= 0 && prev.Degraded >= 0 && cur.Degraded != prev.Degraded {
				add("degraded %d -> %d", prev.Degraded, cur.Degraded)
			}
			if cur.RaidDisks != 0 && cur.RaidDisks != prev.RaidDisks {
				add("raid_disks %d -> %d", prev.RaidDisks, cur.RaidDisks)
			}
			names := make([]string, 0, len(cur.Members))
			for n := range cur.Members {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if o, ok := prev.Members[n]; ok && o != "" && cur.Members[n] != "" && o != cur.Members[n] {
					add("%s: %s -> %s", n, o, cur.Members[n])
				}
			}
		}
		next := cur
		if prev != nil {
			if next.Action == "" {
				next.Action = prev.Action
			}
			if next.ArrayState == "" {
				next.ArrayState = prev.ArrayState
			}
			if next.Degraded < 0 {
				next.Degraded = prev.Degraded
			}
			if next.RaidDisks == 0 {
				next.RaidDisks = prev.RaidDisks
			}
			next.Members = make(map[string]string, len(cur.Members))
			for n, v := range cur.Members {
				if o := prev.Members[n]; v == "" {
					v = o
				}
				next.Members[n] = v
			}
		}
		prev = &next
	}
	return ev
}

// loadMDLog turns the stored md windows into points and events. Windows that
// ended inside the ramp-up are not shown as points, but the events are derived
// from every window (the phase start state comes from the very first one) and
// those that happened in the ramp are stamped at the start of the first visible
// window, so nothing is lost and nothing is reported twice. A phase shorter
// than the ramp has no visible window: its md events are dropped with the points.
func loadMDLog(md []diskstat.MDWindow, offsetMs int64) (MDInfo, bool) {
	var vis []diskstat.MDWindow
	for _, w := range md {
		if !inRamp(w.EndMs, w.SpanMs, offsetMs) {
			vis = append(vis, w)
		}
	}
	if len(vis) == 0 {
		return MDInfo{}, false
	}
	var info MDInfo
	for _, w := range vis {
		info.Points = append(info.Points, mdPoint(w))
	}
	t0 := info.Points[0].T
	for _, e := range mdEventsFrom(nil, md) {
		if e.T < t0 {
			e.T = t0
		}
		info.Events = append(info.Events, e)
	}
	return info, true
}

// loadMembersAndMD reads the stored members log of a phase: per member the
// IOPS, bandwidth and latency series, and the md state. Windows mostly in
// fio's ramp-up are left out and points are stamped at the window START, like
// the Flush series. ok is false when there is no usable log.
func loadMembersAndMD(logPrefix string) (map[string]MemberSeries, MDInfo, bool) {
	f, err := os.Open(diskstat.LogName(logPrefix))
	if err != nil {
		return nil, MDInfo{}, false
	}
	defer f.Close()
	meta, windows, md, err := diskstat.ReadLog(f)
	if err != nil {
		if logger != nil {
			logger.Warn("unreadable members log", "prefix", logPrefix, "err", err)
		}
		return nil, MDInfo{}, false
	}
	out := make(map[string]MemberSeries, len(meta.Members))
	for _, m := range meta.Members {
		role := m.Role
		if role == "data" {
			role = ""
		}
		out[m.Name] = MemberSeries{Role: role}
	}
	for _, w := range windows {
		if inRamp(w.EndMs, w.SpanMs, int64(meta.OffsetMs)) {
			continue // mostly in the ramp-up
		}
		ms, ok := out[w.Device]
		if !ok {
			continue
		}
		t := float64(w.EndMs-w.SpanMs) / 1000
		rI, wI, rB, wB, rL, wL := w.Rates()
		ms.IOPS = append(ms.IOPS, TimePoint{T: t, R: rI, W: wI})
		ms.BW = append(ms.BW, TimePoint{T: t, R: rB, W: wB})
		ms.Lat = append(ms.Lat, TimePoint{T: t, R: rL, W: wL})
		out[w.Device] = ms
	}
	info, _ := loadMDLog(md, int64(meta.OffsetMs))
	return out, info, true
}

// applyMembersLog fills the phase's member series for one device from the
// stored log; nothing when the device has none.
func applyMembersLog(phase *PhaseResult, device, logPrefix string) {
	series, info, ok := loadMembersAndMD(logPrefix)
	if !ok || len(series) == 0 {
		return
	}
	if len(info.Points) > 0 {
		if phase.MDSeries == nil {
			phase.MDSeries = make(map[string]MDInfo)
		}
		phase.MDSeries[device] = info
	}
	if phase.MemberSeries == nil {
		phase.MemberSeries = make(map[string]map[string]MemberSeries)
	}
	phase.MemberSeries[device] = series
}
