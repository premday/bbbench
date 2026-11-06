// Package disktemp reads a disk's temperature, samples it while a phase runs
// and stores the samples in a fio-style log. It only reads.
package disktemp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sensor is where one disk's temperature is read from.
type Sensor struct {
	// Source is "hwmon" (the kernel's nvme or drivetemp driver: a file read,
	// cheap, no root) or "smartctl" (an ATA/NVMe query, needs root).
	Source string
	// Name says which sensor: "Composite" for NVMe, "drivetemp", "smartctl".
	Name string
	// WarnC and CritC are the drive's own limits in °C (0 = unknown).
	WarnC, CritC float64

	read func() (float64, error)
}

// NewSensor builds a sensor from a read function (for tests and other sources).
func NewSensor(source, name string, warnC, critC float64, read func() (float64, error)) *Sensor {
	return &Sensor{Source: source, Name: name, WarnC: warnC, CritC: critC, read: read}
}

// Read returns the current temperature in °C.
func (s *Sensor) Read() (float64, error) { return s.read() }

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Options lets tests replace the machine.
type Options struct {
	Sys string // sysfs root, default /sys
	Dev string // device directory, default /dev
	Run Runner // default: exec
}

func (o *Options) fill() {
	if o.Sys == "" {
		o.Sys = "/sys"
	}
	if o.Dev == "" {
		o.Dev = "/dev"
	}
	if o.Run == nil {
		o.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
}

// ErrNoSensor means no temperature source works for the disk.
var ErrNoSensor = errors.New("no temperature sensor")

var nvmeCtrlRe = regexp.MustCompile(`^(nvme\d+)n\d+`)

// Find locates the best temperature source of the block device `name`
// ("nvme0n1", "sda"): the kernel's hwmon driver when there is one, else
// smartctl. The returned sensor has been read once, so it works.
func Find(name string, opts Options) (*Sensor, error) {
	opts.fill()
	var dirs []string
	if m := nvmeCtrlRe.FindStringSubmatch(name); m != nil {
		dirs, _ = filepath.Glob(filepath.Join(opts.Sys, "class", "nvme", m[1], "hwmon*"))
	} else {
		dirs, _ = filepath.Glob(filepath.Join(opts.Sys, "block", name, "device", "hwmon", "hwmon*"))
	}
	for _, d := range dirs {
		if s := hwmonSensor(d); s != nil {
			if _, err := s.Read(); err == nil {
				return s, nil
			}
		}
	}
	s := smartctlSensor(name, opts)
	if _, err := s.Read(); err == nil {
		return s, nil
	}
	return nil, fmt.Errorf("%w for %s (no hwmon temperature: for SATA/SAS try `modprobe drivetemp`; smartctl unavailable or not root)", ErrNoSensor, name)
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// hwmonSensor reads temp1 of a hwmon directory (milli-°C). For NVMe temp1 is
// the Composite temperature; its _max and _crit are the drive's own limits.
func hwmonSensor(dir string) *Sensor {
	input := filepath.Join(dir, "temp1_input")
	if _, err := os.Stat(input); err != nil {
		return nil
	}
	name := readFile(filepath.Join(dir, "temp1_label"))
	if name == "" {
		name = readFile(filepath.Join(dir, "name"))
	}
	return &Sensor{
		Source: "hwmon", Name: name,
		WarnC: milli(readFile(filepath.Join(dir, "temp1_max"))),
		CritC: milli(readFile(filepath.Join(dir, "temp1_crit"))),
		read: func() (float64, error) {
			v, err := strconv.ParseFloat(readFile(input), 64)
			if err != nil {
				return 0, fmt.Errorf("read %s: %w", input, err)
			}
			return v / 1000, nil
		},
	}
}

// milli converts a milli-°C string to °C; 0 when unreadable or not a plausible
// limit (NVMe sensors report absurd thresholds such as 65261 °C when unset).
func milli(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v/1000 <= 0 || v/1000 > 200 {
		return 0
	}
	return v / 1000
}

// smartctlSensor reads `smartctl -j -A`, whose temperature.current is in °C.
func smartctlSensor(name string, opts Options) *Sensor {
	dev := filepath.Join(opts.Dev, name)
	return &Sensor{Source: "smartctl", Name: "smartctl", read: func() (float64, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, _ := opts.Run(ctx, "smartctl", "-j", "-A", dev) // exit status carries health bits
		var j struct {
			Temperature *struct {
				Current float64 `json:"current"`
			} `json:"temperature"`
		}
		if err := json.Unmarshal(out, &j); err != nil || j.Temperature == nil {
			return 0, errors.New("smartctl reported no temperature")
		}
		return j.Temperature.Current, nil
	}}
}
