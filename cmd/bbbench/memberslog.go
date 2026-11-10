package main

import (
	"context"
	"os"
	"sync"
	"time"

	"bbbench/internal/diskstat"
)

// MemberSeries is the I/O of one member disk of an array over a phase,
// sampled from its kernel counters. R is read, W is write; IOPS per second,
// BW in MB/s, Lat the mean per I/O in µs (0 when the direction did no I/O).
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
	s := diskstat.NewSampler(names, sysBlock)
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
			if err := writeMembersLog(logPrefix, meta, s.Windows()); err != nil && logger != nil {
				logger.Warn("storing member counters", "array", drive.Device.Name, "err", err)
			}
		})
	}
}

func writeMembersLog(logPrefix string, meta diskstat.Meta, ws []diskstat.Window) error {
	f, err := os.Create(diskstat.LogName(logPrefix))
	if err != nil {
		return err
	}
	if err := diskstat.WriteLog(f, meta, ws, nil); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// loadMembersLog reads the stored members log of a phase: per member the
// IOPS, bandwidth and latency series. Like the Flush series, windows that
// ended inside fio's ramp-up are left out and points are stamped at the window
// START. ok is false when there is no usable log.
func loadMembersLog(logPrefix string) (map[string]MemberSeries, bool) {
	f, err := os.Open(diskstat.LogName(logPrefix))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	meta, windows, _, err := diskstat.ReadLog(f)
	if err != nil {
		if logger != nil {
			logger.Warn("unreadable members log", "prefix", logPrefix, "err", err)
		}
		return nil, false
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
		if w.EndMs <= int64(meta.OffsetMs) {
			continue // entirely in the ramp-up
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
	return out, true
}

// applyMembersLog fills the phase's member series for one device from the
// stored log; nothing when the device has none.
func applyMembersLog(phase *PhaseResult, device, logPrefix string) {
	series, ok := loadMembersLog(logPrefix)
	if !ok || len(series) == 0 {
		return
	}
	if phase.MemberSeries == nil {
		phase.MemberSeries = make(map[string]map[string]MemberSeries)
	}
	phase.MemberSeries[device] = series
}
