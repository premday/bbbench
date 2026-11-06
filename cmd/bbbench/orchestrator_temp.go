package main

import (
	"fmt"
	"os"
	"time"

	"bbbench/internal/disktemp"
)

// defaultTempInterval is how often a drive's temperature is read while a phase
// runs. Reading an NVMe drive's hwmon temperature makes the driver issue a small
// admin command, so this is not worth making much faster.
const defaultTempInterval = 5 * time.Second

// startTempSampling reads the drive's temperature every ec.tempInterval until
// the returned function is called, which stores the samples next to fio's logs
// and puts the summary on result. The live value goes to the status page.
// Not being able to read a temperature never fails the phase.
func (ec *ExecutionCoordinator) startTempSampling(device, logPrefix string, result *FioResult) (stop func()) {
	stop = func() {}
	if ec.tempInterval <= 0 {
		return stop
	}
	find := ec.findTemp
	if find == nil {
		find = func(name string) (*disktemp.Sensor, error) { return disktemp.Find(name, disktemp.Options{}) }
	}
	sensor, err := find(device)
	if err != nil {
		logger.Warn("temperature not sampled", "device", device, "err", err)
		return stop
	}
	interval := ec.tempInterval
	sm := disktemp.StartSampler(sensor, interval, func(s disktemp.Sample) {
		c := float64(s.MilliC) / 1000
		markDrive(device, func(d *DriveStatus) { d.TempC = &c })
	})
	return func() {
		samples := sm.Stop()
		sum := disktemp.Summarize(sensor, int(interval.Milliseconds()), samples)
		result.Temp = &sum
		if err := writeTempLog(logPrefix, sum, samples); err != nil {
			logger.Warn("storing temperature samples", "device", device, "err", err)
		}
	}
}

func writeTempLog(logPrefix string, sum disktemp.Summary, samples []disktemp.Sample) error {
	f, err := os.Create(disktemp.LogName(logPrefix))
	if err != nil {
		return err
	}
	if err := disktemp.WriteLog(f, sum, samples); err != nil {
		f.Close()
		return fmt.Errorf("write temperature log: %w", err)
	}
	return f.Close()
}

// loadTempLog reads the stored temperature log of the phase run identified by
// its fio log prefix: the summary, and the series in °C (W column, seconds
// since the phase started, like the Flush series). ok is false without a log.
func loadTempLog(logPrefix string) (sum *disktemp.Summary, series []TimePoint, ok bool) {
	f, err := os.Open(disktemp.LogName(logPrefix))
	if err != nil {
		return nil, nil, false
	}
	defer f.Close()
	s, samples, err := disktemp.ReadLog(f)
	if err != nil {
		if logger != nil {
			logger.Warn("unreadable temperature log", "prefix", logPrefix, "err", err)
		}
		return nil, nil, false
	}
	for _, x := range samples {
		series = append(series, TimePoint{T: float64(x.TimeMs) / 1000, W: float64(x.MilliC) / 1000})
	}
	return &s, series, true
}

// applyTempLog fills the phase's temperature summary and series for one
// device from the stored log.
func applyTempLog(phase *PhaseResult, device, logPrefix string) {
	sum, series, ok := loadTempLog(logPrefix)
	if !ok {
		return
	}
	if phase.Temp == nil {
		phase.Temp = make(map[string]*disktemp.Summary)
	}
	phase.Temp[device] = sum
	if len(series) > 0 {
		if phase.TempSeries == nil {
			phase.TempSeries = make(map[string][]TimePoint)
		}
		phase.TempSeries[device] = series
	}
}

// formatTempSummary renders a phase's temperature for the run summary.
func formatTempSummary(s *disktemp.Summary) string {
	if s.Count == 0 {
		return "Temp: not read"
	}
	out := fmt.Sprintf("Temp: avg %.1f C, max %.1f C", s.AvgC, s.MaxC)
	if s.WarnC > 0 && s.MaxC >= s.WarnC {
		out += fmt.Sprintf(" (reached the drive's %.0f C warning limit)", s.WarnC)
	}
	return out
}
