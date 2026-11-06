// Package diskinfo reads how a disk is connected and configured: interface,
// link speed, power settings, write cache policy and SMART health. It only
// reads (sysfs, and smartctl / nvme-cli when installed); it never changes the
// drive's configuration.
package diskinfo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bbbench/internal/scsi"
	"bbbench/internal/smbios"
)

// Info is what is known about one disk. Fields that could not be read are
// empty; nothing here is an error, since most sources are optional.
type Info struct {
	// Interface is how the disk is attached: "NVMe (PCIe)", "NVMe (tcp)",
	// "SATA", "SAS", "USB", "virtio", or "" when unknown.
	Interface string `json:"interface,omitempty"`
	// LinkSpeed is the negotiated link ("16.0 GT/s PCIe x4", "6.0 Gbps").
	LinkSpeed string `json:"link_speed,omitempty"`
	// LinkMax is the best the link could do, when known and different.
	LinkMax string `json:"link_max,omitempty"`
	// LinkBelowMax is true when the link runs below its maximum (a bad slot or
	// cable, or the link saving power while idle).
	LinkBelowMax bool `json:"link_below_max,omitempty"`
	// Slot is where the disk sits and Controller what it hangs off; nil when unknown.
	Slot       *Slot       `json:"slot,omitempty"`
	Controller *Controller `json:"controller,omitempty"`
	Power      PowerInfo   `json:"power"`
	WriteCache WriteCache  `json:"write_cache"`
	Smart      Smart       `json:"smart"`
}

// PowerInfo is the power management configuration.
type PowerInfo struct {
	RuntimePM  string `json:"runtime_pm,omitempty"`  // device power/control + runtime_status, e.g. "auto (active)"
	ASPM       string `json:"aspm,omitempty"`        // PCIe: the system policy and the link's L0s/L1 state
	NVMeState  string `json:"nvme_state,omitempty"`  // current NVMe power state, e.g. "PS0"
	APST       string `json:"apst,omitempty"`        // NVMe autonomous power state transitions: "enabled"/"disabled"
	LinkPolicy string `json:"link_policy,omitempty"` // SATA link_power_management_policy
}

// WriteCache is the write cache policy at both levels.
type WriteCache struct {
	// Kernel is queue/write_cache: "write back" (the kernel sends Flush
	// commands) or "write through" (it does not).
	Kernel string `json:"kernel,omitempty"`
	// Drive is the drive's own volatile write cache: "enabled", "disabled" or
	// "" when it could not be read.
	Drive string `json:"drive,omitempty"`
}

// Smart is the health summary.
type Smart struct {
	// Status is "PASSED", "FAILED" or "" (unknown, see Detail).
	Status       string `json:"status,omitempty"`
	Source       string `json:"source,omitempty"` // "smartctl" or "nvme smart-log"
	TempC        *int   `json:"temp_c,omitempty"`
	PowerOnHours *int   `json:"power_on_hours,omitempty"`
	PercentUsed  *int   `json:"percent_used,omitempty"` // NVMe wear
	MediaErrors  *int   `json:"media_errors,omitempty"` // NVMe
	Reallocated  *int   `json:"reallocated,omitempty"`  // ATA
	Pending      *int   `json:"pending,omitempty"`      // ATA
	Detail       string `json:"detail,omitempty"`       // why Status is unknown, or what failed
}

// Runner runs a command and returns its standard output. A non-nil error with
// output is normal (smartctl encodes health in its exit status).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Options lets tests replace the machine.
type Options struct {
	Sys string // sysfs root, default /sys
	Dev string // device directory, default /dev
	Run Runner // default: exec with the context
	// DriveWCE reads the drive write cache of a SCSI/ATA disk; default scsi.GetWCE.
	DriveWCE func(devPath string) (bool, error)
	// Timeout bounds each external command; default 10 s.
	Timeout time.Duration
	// SMBIOS reads the DMI table below the sysfs root; default smbios.Load.
	// It fails without root.
	SMBIOS func(sys string) (*smbios.Table, error)
	// Label heads a dump ("nvme0n1 - model, serial"); only used by CollectDump.
	Label string
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
	if o.DriveWCE == nil {
		o.DriveWCE = scsi.GetWCE
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
}

var (
	nvmeNameRe = regexp.MustCompile(`^(nvme\d+)n\d+`)
	ataRe      = regexp.MustCompile(`/ata(\d+)/`)
	hostRe     = regexp.MustCompile(`/host(\d+)/`)
	sasPortRe  = regexp.MustCompile(`/port-(\d+):\d+/`)
)

// Collect gathers the Info of the block device `name` (e.g. "nvme0n1", "sda").
func Collect(name string, opts Options) Info {
	info, _ := collect(name, opts, false)
	return info
}

// CollectDump is Collect plus a text dump of everything that was read: the
// summary, the sysfs values and the full output of each command, with the
// extra identify and SMART reports (`smartctl -x`, `nvme id-ctrl`, ...) that
// the summary is drawn from.
func CollectDump(name string, opts Options) (Info, string) {
	info, c := collect(name, opts, true)
	return info, c.dump(info)
}

func collect(name string, opts Options, extras bool) (Info, *collector) {
	opts.fill()
	var info Info
	c := &collector{opts: opts, name: name}
	c.sys = func(parts ...string) string { return filepath.Join(append([]string{opts.Sys}, parts...)...) }

	info.WriteCache.Kernel = c.read(c.sys("block", name, "queue", "write_cache"))
	real, err := filepath.EvalSymlinks(c.sys("block", name))
	if err != nil {
		real = ""
	}
	nvmeCtrl := ""
	if m := nvmeNameRe.FindStringSubmatch(name); m != nil {
		nvmeCtrl = m[1]
		c.nvme(&info, m[1])
	} else {
		c.scsi(&info, real)
	}
	c.topology(&info, real, nvmeCtrl)
	info.Smart = c.smart(name, nvmeNameRe.MatchString(name))
	if extras {
		c.extraReports(info)
	}
	return info, c
}

// logged is one thing read, kept for the dump.
type logged struct{ label, body string }

type collector struct {
	opts        Options
	name        string
	sys         func(parts ...string) string
	smbios      *smbios.Table
	smbiosErr   error
	smbiosTried bool
	sysfs       []logged // sysfs files read: path -> value
	cmds        []logged // commands run: command line -> output
}

func (c *collector) read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	rel, _ := filepath.Rel(c.opts.Sys, path)
	c.sysfs = append(c.sysfs, logged{rel, v})
	return v
}

// run executes a command with the timeout; ok is false when it produced nothing.
func (c *collector) run(name string, args ...string) (out []byte, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()
	out, err := c.opts.Run(ctx, name, args...)
	body := strings.TrimSpace(string(out))
	if body == "" {
		body = "(no output"
		if err != nil {
			body += ": " + err.Error()
		}
		body += ")"
	}
	c.cmds = append(c.cmds, logged{name + " " + strings.Join(args, " "), body})
	return out, len(out) > 0
}

// extraReports runs the full identify / SMART reports for the dump only; the
// summary does not need them.
func (c *collector) extraReports(info Info) {
	if info.Controller != nil && info.Controller.PCI != "" {
		c.run("lspci", "-vv", "-s", info.Controller.PCI)
	}
	dev := filepath.Join(c.opts.Dev, c.name)
	if m := nvmeNameRe.FindStringSubmatch(c.name); m != nil {
		ctrl := filepath.Join(c.opts.Dev, m[1])
		c.run("nvme", "id-ctrl", ctrl)
		c.run("nvme", "id-ns", dev)
		c.run("nvme", "smart-log", ctrl)
	} else {
		c.run("hdparm", "-I", dev)
	}
	c.run("smartctl", "-x", dev)
}

// pcieAspmPolicy is the system policy: the bracketed word of
// "default performance [powersave] powersupersave".
func (c *collector) pcieAspmPolicy() string {
	s := c.read(c.sys("module", "pcie_aspm", "parameters", "policy"))
	if i, j := strings.Index(s, "["), strings.Index(s, "]"); i >= 0 && j > i {
		return s[i+1 : j]
	}
	return ""
}

func (c *collector) runtimePM(devDir string) string {
	ctl := c.read(filepath.Join(devDir, "power", "control"))
	if ctl == "" {
		return ""
	}
	if st := c.read(filepath.Join(devDir, "power", "runtime_status")); st != "" {
		return ctl + " (" + st + ")"
	}
	return ctl
}

// nvme fills an NVMe namespace's info from its controller nvmeN.
func (c *collector) nvme(info *Info, ctrl string) {
	ctrlDir := c.sys("class", "nvme", ctrl)
	transport := c.read(filepath.Join(ctrlDir, "transport"))
	if transport == "" {
		transport = "pcie"
	}
	switch transport {
	case "pcie":
		info.Interface = "NVMe (PCIe)"
	default:
		info.Interface = "NVMe (" + transport + ")"
	}
	if transport == "pcie" {
		if pci, err := filepath.EvalSymlinks(filepath.Join(ctrlDir, "device")); err == nil {
			speed := c.read(filepath.Join(pci, "current_link_speed"))
			width := c.read(filepath.Join(pci, "current_link_width"))
			maxSpeed := c.read(filepath.Join(pci, "max_link_speed"))
			maxWidth := c.read(filepath.Join(pci, "max_link_width"))
			info.LinkSpeed = linkString(speed, width)
			info.LinkMax = linkString(maxSpeed, maxWidth)
			info.LinkBelowMax = belowMax(speed, maxSpeed, width, maxWidth)
			if info.LinkMax == info.LinkSpeed {
				info.LinkMax = ""
			}
			info.Power.RuntimePM = c.runtimePM(pci)
			info.Power.ASPM = c.aspm(pci)
		}
	}
	// nvme-cli: current power state, APST, volatile write cache. Needs root.
	dev := filepath.Join(c.opts.Dev, ctrl)
	if v, ok := c.nvmeFeature(dev, "0x02"); ok {
		info.Power.NVMeState = "PS" + strconv.FormatUint(v&0x1f, 10)
	}
	if v, ok := c.nvmeFeature(dev, "0x0c"); ok {
		info.Power.APST = enabledWord(v&1 == 1)
	}
	if v, ok := c.nvmeFeature(dev, "0x06"); ok {
		info.WriteCache.Drive = enabledWord(v&1 == 1)
	}
}

var nvmeValueRe = regexp.MustCompile(`(?i)current value:\s*(0x[0-9a-f]+)`)

// nvmeFeature returns the current value of an NVMe feature via nvme-cli.
func (c *collector) nvmeFeature(dev, fid string) (uint64, bool) {
	out, ok := c.run("nvme", "get-feature", dev, "-f", fid)
	if !ok {
		return 0, false
	}
	m := nvmeValueRe.FindSubmatch(out)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseUint(string(m[1]), 0, 64)
	return v, err == nil
}

func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

// aspm describes PCIe ASPM: the system policy and the device link's L0s/L1.
func (c *collector) aspm(pci string) string {
	var parts []string
	if p := c.pcieAspmPolicy(); p != "" {
		parts = append(parts, "policy "+p)
	}
	var states []string
	for _, s := range []struct{ file, label string }{{"l0s_aspm", "L0s"}, {"l1_aspm", "L1"}} {
		switch c.read(filepath.Join(pci, "link", s.file)) {
		case "1":
			states = append(states, s.label+" on")
		case "0":
			states = append(states, s.label+" off")
		}
	}
	if len(states) > 0 {
		parts = append(parts, strings.Join(states, ", "))
	}
	return strings.Join(parts, "; ")
}

// scsi fills a SCSI-layer disk (SATA, SAS, USB, virtio) from where it sits in
// the device tree (the resolved /sys/block/<name> path).
func (c *collector) scsi(info *Info, real string) {
	switch {
	case ataRe.MatchString(real):
		info.Interface = "SATA"
		n := ataRe.FindStringSubmatch(real)[1]
		spd := c.read(c.sys("class", "ata_link", "link"+n, "sata_spd"))
		if spd != "" && spd != "<unknown>" {
			info.LinkSpeed = spd
		}
	case sasPortRe.MatchString(real) || strings.Contains(real, "/end_device-"):
		info.Interface = "SAS"
		if m := sasPortRe.FindStringSubmatch(real); m != nil {
			phys, _ := filepath.Glob(c.sys("class", "sas_phy", "phy-"+m[1]+":*"))
			for _, p := range phys {
				if r := c.read(filepath.Join(p, "negotiated_linkrate")); r != "" && !strings.EqualFold(r, "unknown") {
					info.LinkSpeed = r
					if max := c.read(filepath.Join(p, "maximum_linkrate")); max != "" && max != r {
						info.LinkMax = max
						info.LinkBelowMax = true
					}
					break
				}
			}
		}
	case strings.Contains(real, "/usb"):
		info.Interface = "USB"
		for d := filepath.Dir(real); d != "/" && d != "." && strings.Contains(d, "/usb"); d = filepath.Dir(d) {
			if s := c.read(filepath.Join(d, "speed")); s != "" {
				info.LinkSpeed = s + " Mb/s"
				break
			}
		}
	case strings.Contains(real, "/virtio"):
		info.Interface = "virtio"
	}
	devDir := c.sys("block", c.name, "device")
	info.Power.RuntimePM = c.runtimePM(devDir)
	if m := hostRe.FindStringSubmatch(real); m != nil {
		info.Power.LinkPolicy = c.read(c.sys("class", "scsi_host", "host"+m[1], "link_power_management_policy"))
	}
	if wce, err := c.opts.DriveWCE(filepath.Join(c.opts.Dev, c.name)); err == nil {
		info.WriteCache.Drive = enabledWord(wce)
	}
}

// linkString renders a PCIe link: "16.0 GT/s PCIe x4".
func linkString(speed, width string) string {
	if speed == "" {
		return ""
	}
	if width == "" {
		return speed
	}
	return speed + " x" + width
}

// belowMax reports whether the PCIe link runs below its maximum speed or width.
func belowMax(speed, maxSpeed, width, maxWidth string) bool {
	return gts(speed) < gts(maxSpeed) || atoi(width) < atoi(maxWidth)
}

// gts is the number of a "16.0 GT/s PCIe" speed (0 if unreadable).
func gts(s string) float64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseFloat(f[0], 64)
	return n
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
