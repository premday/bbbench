package diskinfo

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"bbbench/internal/smbios"
)

// Slot is where the disk physically sits, as far as the machine says.
type Slot struct {
	// Name is the slot or bay: "PCIe Slot 3", "Slot03", "Bay 5".
	Name string `json:"name"`
	// Source says who knows: "SES enclosure", "SAS bay", "SMBIOS slot",
	// "SMBIOS onboard device" or "PCI slot (sysfs)".
	Source string `json:"source"`
	// Detail is the rest: slot type and use, enclosure id, SATA port.
	Detail string `json:"detail,omitempty"`
}

// Controller is what the disk hangs off.
type Controller struct {
	// Kind: "NVMe controller", "SATA controller (AHCI)", "SAS HBA", "RAID
	// controller", "SCSI controller", "USB", "virtio", "storage controller".
	Kind   string `json:"kind"`
	Driver string `json:"driver,omitempty"`
	// PCI is the controller's PCI address, IDs its vendor:device (and subsystem).
	PCI string `json:"pci,omitempty"`
	IDs string `json:"ids,omitempty"`
	// Name is its board name (HBAs) or its name from lspci.
	Name     string `json:"name,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	BIOS     string `json:"bios,omitempty"`
	// SASAddress is the controller's (host) or the disk's (end device) SAS address.
	SASAddress string `json:"sas_address,omitempty"`
	// Mode, behind a RAID-capable controller: "RAID virtual drive" (the
	// controller presents a logical volume, the physical disks are hidden) or
	// "passthrough". A guess from the drive's model and the driver.
	Mode string `json:"mode,omitempty"`
}

var (
	pciAddrRe  = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	endDevRe   = regexp.MustCompile(`/(end_device-\d+:\d+(?::\d+)?)/`)
	quotedRe   = regexp.MustCompile(`"([^"]*)"`)
	idSuffixRe = regexp.MustCompile(`\s*\[[0-9a-f]{4}\]$`)
	raidModel  = regexp.MustCompile(`(?i)logical volume|virtual|megaraid|\bperc\b|^mr\d|mrromb|smart ?array|raid`)
)

// raidDrivers are the drivers of RAID controllers, whether the disks behind
// them are exposed as virtual drives or passed through.
var raidDrivers = map[string]bool{
	"megaraid_sas": true, "megaraid_mbox": true, "megaraid_sas_fusion": true,
	"aacraid": true, "hpsa": true, "smartpqi": true, "3w-9xxx": true, "3w-sas": true, "arcmsr": true,
}

type pciDev struct{ addr, dir string }

// pciChain lists the PCI devices on a resolved sysfs path, root port first.
func pciChain(real string) []pciDev {
	var out []pciDev
	parts := strings.Split(real, "/")
	for i, p := range parts {
		if pciAddrRe.MatchString(p) {
			out = append(out, pciDev{p, strings.Join(parts[:i+1], "/")})
		}
	}
	return out
}

// topology fills the disk's slot and controller. real is the resolved
// /sys/block/<name> path; for NVMe the controller path is used for the PCI chain.
func (c *collector) topology(info *Info, real, nvmeCtrl string) {
	chainPath := real
	if nvmeCtrl != "" {
		if p, err := filepath.EvalSymlinks(c.sys("class", "nvme", nvmeCtrl)); err == nil {
			chainPath = p // .../0000:04:00.0/nvme/nvme0, also for multipath namespaces
		}
	}
	chain := pciChain(chainPath)
	ctrl := &Controller{}
	c.pciController(ctrl, chain)

	switch {
	case nvmeCtrl != "":
		c.nvmeController(ctrl, nvmeCtrl)
	case strings.Contains(real, "/virtio"):
		ctrl.Kind = "virtio"
	case strings.Contains(real, "/usb"):
		ctrl.Kind = "USB"
	default:
		c.scsiController(ctrl, real)
	}
	c.controllerName(ctrl)
	if ctrl.Kind != "" || ctrl.PCI != "" {
		info.Controller = ctrl
	}

	info.Slot = c.slot(real, chain, ctrl)
}

// pciController describes the storage controller on the PCI chain: the deepest
// device of PCI class 01xx (storage), else the last device of the chain.
func (c *collector) pciController(ctrl *Controller, chain []pciDev) {
	if len(chain) == 0 {
		return
	}
	dev := chain[len(chain)-1]
	class := ""
	for i := len(chain) - 1; i >= 0; i-- {
		if cl := c.read(filepath.Join(chain[i].dir, "class")); strings.HasPrefix(cl, "0x01") {
			dev, class = chain[i], cl
			break
		}
	}
	ctrl.PCI = dev.addr
	ven, did := c.read(filepath.Join(dev.dir, "vendor")), c.read(filepath.Join(dev.dir, "device"))
	if ven != "" && did != "" {
		ctrl.IDs = strings.TrimPrefix(ven, "0x") + ":" + strings.TrimPrefix(did, "0x")
		if sv, sd := c.read(filepath.Join(dev.dir, "subsystem_vendor")), c.read(filepath.Join(dev.dir, "subsystem_device")); sv != "" && sd != "" {
			ctrl.IDs += " (subsystem " + strings.TrimPrefix(sv, "0x") + ":" + strings.TrimPrefix(sd, "0x") + ")"
		}
	}
	if drv, err := filepath.EvalSymlinks(filepath.Join(dev.dir, "driver")); err == nil {
		ctrl.Driver = filepath.Base(drv)
	}
	switch {
	case raidDrivers[ctrl.Driver]:
		ctrl.Kind = "RAID controller"
	case strings.HasPrefix(class, "0x0108"):
		ctrl.Kind = "NVMe controller"
	case strings.HasPrefix(class, "0x0106"):
		ctrl.Kind = "SATA controller"
		if strings.HasPrefix(class, "0x010601") || ctrl.Driver == "ahci" {
			ctrl.Kind = "SATA controller (AHCI)"
		}
	case strings.HasPrefix(class, "0x0107"):
		ctrl.Kind = "SAS HBA"
	case strings.HasPrefix(class, "0x0104"):
		ctrl.Kind = "RAID controller"
	case strings.HasPrefix(class, "0x0100"):
		ctrl.Kind = "SCSI controller"
	case class != "":
		ctrl.Kind = "storage controller"
	}
}

func (c *collector) nvmeController(ctrl *Controller, name string) {
	dir := c.sys("class", "nvme", name)
	if ctrl.Kind == "" {
		ctrl.Kind = "NVMe controller"
	}
	if t := c.read(filepath.Join(dir, "transport")); t != "" && t != "pcie" {
		ctrl.Kind = "NVMe controller (" + t + ")"
	}
	ctrl.Firmware = c.read(filepath.Join(dir, "firmware_rev"))
}

// scsiController reads what the SCSI host of a SATA, SAS or RAID-backed disk
// says about its controller, and guesses whether the disk is a RAID volume.
func (c *collector) scsiController(ctrl *Controller, real string) {
	if m := hostRe.FindStringSubmatch(real); m != nil {
		host := c.sys("class", "scsi_host", "host"+m[1])
		first := func(files ...string) string {
			for _, f := range files {
				if v := c.read(filepath.Join(host, f)); v != "" {
					return v
				}
			}
			return ""
		}
		ctrl.Firmware = first("version_fw", "firmware_revision", "fw_version")
		ctrl.BIOS = first("version_bios")
		ctrl.SASAddress = first("host_sas_address", "sas_address")
		if board := first("board_name", "model"); board != "" && ctrl.Name == "" {
			ctrl.Name = board
		}
		if ctrl.Driver == "" {
			ctrl.Driver = first("proc_name")
		}
	}
	if m := endDevRe.FindStringSubmatch(real); m != nil {
		if a := c.read(c.sys("class", "sas_device", m[1], "sas_address")); a != "" {
			ctrl.SASAddress = a // the disk's own SAS address
		}
	}
	if ctrl.Kind == "" {
		switch {
		case ataRe.MatchString(real):
			ctrl.Kind = "SATA controller"
		case strings.Contains(real, "/end_device-") || sasPortRe.MatchString(real):
			ctrl.Kind = "SAS HBA"
		}
	}
	if raidDrivers[ctrl.Driver] || ctrl.Kind == "RAID controller" {
		dev := c.sys("block", c.name, "device")
		model := c.read(filepath.Join(dev, "vendor")) + " " + c.read(filepath.Join(dev, "model"))
		_, err := os.Stat(filepath.Join(dev, "raid_level")) // hpsa, smartpqi: logical volumes only
		hasLevel := err == nil
		ctrl.Mode = "passthrough"
		if hasLevel || raidModel.MatchString(model) {
			ctrl.Mode = "RAID virtual drive"
		}
	}
}

// controllerName asks lspci for the controller's name; without lspci the IDs
// still identify it.
func (c *collector) controllerName(ctrl *Controller) {
	if ctrl.PCI == "" || ctrl.Name != "" { // an HBA's board name beats lspci's chip name
		return
	}
	out, ok := c.run("lspci", "-s", ctrl.PCI, "-nn", "-mm")
	if !ok {
		return
	}
	q := quotedRe.FindAllStringSubmatch(string(out), -1)
	if len(q) < 3 {
		return
	}
	vendor := idSuffixRe.ReplaceAllString(q[1][1], "")
	device := idSuffixRe.ReplaceAllString(q[2][1], "")
	if vendor != "" || device != "" {
		ctrl.Name = strings.TrimSpace(vendor + " " + device)
	}
}

// slot finds where the disk sits, from the most to the least specific source:
// the SES enclosure, the SAS bay, an SMBIOS slot on the PCI chain, the kernel's
// PCI slot table, an SMBIOS onboard device.
func (c *collector) slot(real string, chain []pciDev, ctrl *Controller) *Slot {
	if s := c.sesSlot(); s != nil {
		return s
	}
	if m := endDevRe.FindStringSubmatch(real); m != nil {
		dir := c.sys("class", "sas_device", m[1])
		bay, encl := c.read(filepath.Join(dir, "bay_identifier")), c.read(filepath.Join(dir, "enclosure_identifier"))
		if bay != "" && bay != "-1" && bay != "4294967295" {
			s := &Slot{Name: "Bay " + bay, Source: "SAS bay"}
			if encl != "" && encl != "0x0000000000000000" {
				s.Detail = "enclosure " + encl
			}
			return s
		}
	}
	tbl := c.smbiosTable()
	if tbl != nil {
		// The slot's address is the card's or its port's: try the deepest device first.
		for i := len(chain) - 1; i >= 0; i-- {
			for _, s := range tbl.Slots {
				if s.HasAddr && s.Addr.String() == chain[i].addr {
					return smbiosSlot(s)
				}
			}
		}
		for i := len(chain) - 1; i >= 0; i-- { // same bus and device, another function
			for _, s := range tbl.Slots {
				if s.HasAddr && sameDevice(s.Addr.String(), chain[i].addr) {
					return smbiosSlot(s)
				}
			}
		}
	}
	if s := c.pciSlotTable(chain); s != nil {
		return s
	}
	if tbl != nil && ctrl != nil && ctrl.PCI != "" {
		for _, o := range tbl.Onboard {
			if o.Addr.String() == ctrl.PCI {
				s := &Slot{Name: o.Designation, Source: "SMBIOS onboard device", Detail: o.Type}
				if m := ataRe.FindStringSubmatch(real); m != nil {
					s.Detail += ", port ata" + m[1]
				}
				return s
			}
		}
	}
	return nil
}

func smbiosSlot(s smbios.Slot) *Slot {
	detail := []string{s.Type}
	if s.Usage != "" {
		detail = append(detail, s.Usage)
	}
	detail = append(detail, fmt.Sprintf("slot ID %d", s.ID))
	return &Slot{Name: s.Designation, Source: "SMBIOS slot", Detail: strings.Join(detail, ", ")}
}

// sameDevice compares two PCI addresses ignoring the function.
func sameDevice(a, b string) bool {
	return len(a) == len(b) && len(a) > 2 && a[:len(a)-2] == b[:len(b)-2]
}

// sesSlot finds the disk in an SES enclosure: /sys/class/enclosure/<enc>/<slot>/device
// links to the disk's SCSI device.
func (c *collector) sesSlot() *Slot {
	me, err := filepath.EvalSymlinks(c.sys("block", c.name, "device"))
	if err != nil {
		return nil
	}
	links, _ := filepath.Glob(c.sys("class", "enclosure", "*", "*", "device"))
	for _, l := range links {
		if dev, err := filepath.EvalSymlinks(l); err == nil && dev == me {
			slotDir := filepath.Dir(l)
			encDir := filepath.Dir(slotDir)
			s := &Slot{Name: filepath.Base(slotDir), Source: "SES enclosure"}
			var detail []string
			if n := c.read(filepath.Join(slotDir, "slot")); n != "" {
				detail = append(detail, "slot "+n)
			}
			if id := c.read(filepath.Join(encDir, "id")); id != "" {
				detail = append(detail, "enclosure "+id)
			}
			s.Detail = strings.Join(detail, ", ")
			return s
		}
	}
	return nil
}

// pciSlotTable uses the kernel's /sys/bus/pci/slots/<n>/address ("0000:04:00",
// no function), which needs no root.
func (c *collector) pciSlotTable(chain []pciDev) *Slot {
	addrs, _ := filepath.Glob(c.sys("bus", "pci", "slots", "*", "address"))
	for i := len(chain) - 1; i >= 0; i-- {
		for _, a := range addrs {
			if v := c.read(a); v != "" && strings.HasPrefix(chain[i].addr, v+".") {
				return &Slot{Name: "slot " + filepath.Base(filepath.Dir(a)), Source: "PCI slot (sysfs)", Detail: "PCI " + v}
			}
		}
	}
	return nil
}

// smbiosTable returns the DMI table, or nil when it cannot be read (it is
// root-only); the reason is kept for the dump.
func (c *collector) smbiosTable() *smbios.Table {
	if !c.smbiosTried {
		c.smbiosTried = true
		load := c.opts.SMBIOS
		if load == nil {
			load = smbios.Load
		}
		c.smbios, c.smbiosErr = load(c.opts.Sys)
	}
	return c.smbios
}
