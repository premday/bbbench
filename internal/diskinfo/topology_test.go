package diskinfo

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"bbbench/internal/smbios"
)

const lspciNVMe = `04:00.0 "Non-Volatile memory controller [0108]" "Samsung Electronics Co Ltd [144d]" "NVMe SSD Controller PM9A1/PM9A3/980PRO [a80a]" -p02 "Samsung Electronics Co Ltd [144d]" "Device [a801]"`

// pciDevice makes a PCI device directory with its identity and driver.
func pciDevice(t *testing.T, sys, dir, class, vendor, device, driver string) {
	t.Helper()
	put(t, sys, map[string]string{
		dir + "/class": class, dir + "/vendor": vendor, dir + "/device": device,
		dir + "/subsystem_vendor": vendor, dir + "/subsystem_device": "0x1234",
		"bus/pci/drivers/" + driver + "/.keep": "",
	})
	link(t, sys, filepath.Join(sys, "bus/pci/drivers", driver), dir+"/driver")
}

func tableWith(slots []smbios.Slot, onboard ...smbios.Onboard) func(string) (*smbios.Table, error) {
	return func(string) (*smbios.Table, error) { return &smbios.Table{Slots: slots, Onboard: onboard}, nil }
}

func noTable(string) (*smbios.Table, error) { return nil, errors.New("permission denied") }

// nvmeTree builds an NVMe namespace under a root port, as sysfs lays it out.
func nvmeTree(t *testing.T) (sys string) {
	sys = t.TempDir()
	rp := "devices/pci0000:00/0000:00:01.0"
	ep := rp + "/0000:04:00.0"
	pciDevice(t, sys, rp, "0x060400", "0x1022", "0x1453", "pcieport")
	pciDevice(t, sys, ep, "0x010802", "0x144d", "0xa80a", "nvme")
	put(t, sys, map[string]string{
		ep + "/nvme/nvme0/firmware_rev":              "GDC7102Q",
		ep + "/nvme/nvme0/transport":                 "pcie",
		ep + "/nvme/nvme0/nvme0n1/queue/write_cache": "write back",
	})
	link(t, sys, filepath.Join(sys, ep), ep+"/nvme/nvme0/device")
	link(t, sys, filepath.Join(sys, ep, "nvme/nvme0"), "class/nvme/nvme0")
	link(t, sys, filepath.Join(sys, ep, "nvme/nvme0/nvme0n1"), "block/nvme0n1")
	return sys
}

func TestTopology_NVMeInSMBIOSSlot(t *testing.T) {
	sys := nvmeTree(t)
	got := Collect("nvme0n1", Options{Sys: sys, Run: fakeRun(map[string]string{"lspci -s 0000:04:00.0 -nn -mm": lspciNVMe}),
		SMBIOS: tableWith([]smbios.Slot{
			{Designation: "PCIe Slot 3", Type: "PCI Express x4", Usage: "in use", ID: 3, HasAddr: true, Addr: smbios.PCIAddr{Bus: 4}},
		})})
	if s := got.Slot; s == nil || s.Name != "PCIe Slot 3" || s.Source != "SMBIOS slot" || s.Detail != "PCI Express x4, in use, slot ID 3" {
		t.Errorf("slot = %+v", s)
	}
	c := got.Controller
	if c == nil || c.Kind != "NVMe controller" || c.Driver != "nvme" || c.PCI != "0000:04:00.0" || c.Firmware != "GDC7102Q" ||
		c.Name != "Samsung Electronics Co Ltd NVMe SSD Controller PM9A1/PM9A3/980PRO" {
		t.Errorf("controller = %+v", c)
	}
	if c.IDs != "144d:a80a (subsystem 144d:1234)" {
		t.Errorf("ids = %q", c.IDs)
	}
}

// Many BIOSes list the root port a drive bay hangs off, not the drive itself.
func TestTopology_SlotMatchesAnAncestor(t *testing.T) {
	sys := nvmeTree(t)
	got := Collect("nvme0n1", Options{Sys: sys, Run: fakeRun(nil), SMBIOS: tableWith([]smbios.Slot{
		{Designation: "NVMe Bay 7", Type: "PCI Express Gen 4", ID: 7, HasAddr: true, Addr: smbios.PCIAddr{Bus: 0, Device: 1, Function: 0}},
	})})
	if got.Slot == nil || got.Slot.Name != "NVMe Bay 7" {
		t.Errorf("root port match: %+v", got.Slot)
	}
}

// Without root the DMI table is unreadable: the kernel's slot table still helps.
func TestTopology_FallsBackToTheKernelPCISlotTable(t *testing.T) {
	sys := nvmeTree(t)
	put(t, sys, map[string]string{"bus/pci/slots/5/address": "0000:04:00"})
	got := Collect("nvme0n1", Options{Sys: sys, Run: fakeRun(nil), SMBIOS: noTable})
	if s := got.Slot; s == nil || s.Name != "slot 5" || s.Source != "PCI slot (sysfs)" {
		t.Errorf("slot = %+v", s)
	}
	// Nothing known: no slot, but the controller is still described.
	sys = nvmeTree(t)
	got = Collect("nvme0n1", Options{Sys: sys, Run: fakeRun(nil), SMBIOS: noTable})
	if got.Slot != nil || got.Controller == nil || got.Controller.PCI != "0000:04:00.0" {
		t.Errorf("slot %+v controller %+v", got.Slot, got.Controller)
	}
}

// sasTree builds a SAS disk behind a PCI HBA.
func sasTree(t *testing.T, driver, class, vendor, model string) (sys, disk string) {
	sys = t.TempDir()
	hba := "devices/pci0000:00/0000:00:02.0/0000:03:00.0"
	pciDevice(t, sys, "devices/pci0000:00/0000:00:02.0", "0x060400", "0x8086", "0x1", "pcieport")
	pciDevice(t, sys, hba, class, "0x1000", "0x0097", driver)
	scsi := hba + "/host2/port-2:0/end_device-2:0/target2:0:0/2:0:0:0"
	put(t, sys, map[string]string{
		scsi + "/block/sdb/queue/write_cache": "write back",
		scsi + "/vendor":                      vendor,
		scsi + "/model":                       model,
	})
	link(t, sys, "../..", scsi+"/block/sdb/device") // sysfs: block/sdb/device -> the SCSI device
	link(t, sys, filepath.Join(sys, scsi, "block/sdb"), "block/sdb")
	put(t, sys, map[string]string{
		"class/scsi_host/host2/proc_name":                      driver,
		"class/scsi_host/host2/version_fw":                     "16.00.12.00",
		"class/scsi_host/host2/version_bios":                   "8.37.00.00",
		"class/scsi_host/host2/board_name":                     "SAS9300-8i",
		"class/scsi_host/host2/host_sas_address":               "0x500605b00ab12345",
		"class/sas_device/end_device-2:0/sas_address":          "0x4433221100000000",
		"class/sas_device/end_device-2:0/bay_identifier":       "3",
		"class/sas_device/end_device-2:0/enclosure_identifier": "0x500304800123abcd",
	})
	return sys, filepath.Join(sys, scsi)
}

func TestTopology_SASHBAWithEnclosure(t *testing.T) {
	sys, disk := sasTree(t, "mpt3sas", "0x010700", "SEAGATE", "ST4000NM0035")
	put(t, sys, map[string]string{"class/enclosure/2:0:5:0/Slot03/slot": "3", "class/enclosure/2:0:5:0/id": "500304800123abcd"})
	link(t, sys, disk, "class/enclosure/2:0:5:0/Slot03/device")

	got := Collect("sdb", Options{Sys: sys, Run: fakeRun(nil), SMBIOS: noTable, DriveWCE: func(string) (bool, error) { return true, nil }})
	if s := got.Slot; s == nil || s.Name != "Slot03" || s.Source != "SES enclosure" || s.Detail != "slot 3, enclosure 500304800123abcd" {
		t.Errorf("slot = %+v", s)
	}
	c := got.Controller
	if c == nil || c.Kind != "SAS HBA" || c.Driver != "mpt3sas" || c.Firmware != "16.00.12.00" || c.BIOS != "8.37.00.00" ||
		c.Name != "SAS9300-8i" || c.SASAddress != "0x4433221100000000" || c.Mode != "" {
		t.Errorf("controller = %+v", c)
	}
	if got.Interface != "SAS" {
		t.Errorf("interface = %q", got.Interface)
	}
}

// With no enclosure management, the SAS bay identifier still says where it is.
func TestTopology_SASBayWithoutEnclosure(t *testing.T) {
	sys, _ := sasTree(t, "mpt3sas", "0x010700", "SEAGATE", "ST4000NM0035")
	got := Collect("sdb", Options{Sys: sys, Run: fakeRun(nil), SMBIOS: noTable, DriveWCE: func(string) (bool, error) { return false, errors.New("x") }})
	if s := got.Slot; s == nil || s.Name != "Bay 3" || s.Source != "SAS bay" || s.Detail != "enclosure 0x500304800123abcd" {
		t.Errorf("slot = %+v", s)
	}
}

func TestTopology_RAIDControllerVirtualDriveOrPassthrough(t *testing.T) {
	for _, tc := range []struct {
		vendor, model, mode string
	}{
		{"DELL", "PERC H730P Mini", "RAID virtual drive"},
		{"HPE", "LOGICAL VOLUME", "RAID virtual drive"},
		{"ATA", "Samsung SSD 860", "passthrough"},
	} {
		sys, _ := sasTree(t, "megaraid_sas", "0x010400", tc.vendor, tc.model)
		got := Collect("sdb", Options{Sys: sys, Run: fakeRun(nil), SMBIOS: noTable, DriveWCE: func(string) (bool, error) { return true, nil }})
		c := got.Controller
		if c == nil || c.Kind != "RAID controller" || c.Driver != "megaraid_sas" || c.Mode != tc.mode {
			t.Errorf("%s %s: controller = %+v, want mode %q", tc.vendor, tc.model, c, tc.mode)
		}
	}
}

func TestTopology_OnboardSATA(t *testing.T) {
	sys := t.TempDir()
	ahci := "devices/pci0000:00/0000:00:17.0"
	pciDevice(t, sys, ahci, "0x010601", "0x8086", "0xa282", "ahci")
	real := ahci + "/ata1/host0/target0:0:0/0:0:0:0/block/sda"
	put(t, sys, map[string]string{real + "/queue/write_cache": "write back", "class/ata_link/link1/sata_spd": "6.0 Gbps"})
	link(t, sys, filepath.Join(sys, real), "block/sda")
	got := Collect("sda", Options{Sys: sys, Run: fakeRun(nil), DriveWCE: func(string) (bool, error) { return true, nil },
		SMBIOS: tableWith(nil, smbios.Onboard{Designation: "Onboard SATA", Type: "SATA controller", Enabled: true, Addr: smbios.PCIAddr{Device: 0x17}})})
	if s := got.Slot; s == nil || s.Name != "Onboard SATA" || s.Source != "SMBIOS onboard device" || s.Detail != "SATA controller, port ata1" {
		t.Errorf("slot = %+v", s)
	}
	if c := got.Controller; c == nil || c.Kind != "SATA controller (AHCI)" || c.Driver != "ahci" || c.PCI != "0000:00:17.0" {
		t.Errorf("controller = %+v", c)
	}
}

// The dump lists the whole SMBIOS table, or says it could not be read.
func TestDump_SMBIOSSection(t *testing.T) {
	sys := nvmeTree(t)
	_, dump := CollectDump("nvme0n1", Options{Sys: sys, Run: fakeRun(map[string]string{"lspci -vv -s 0000:04:00.0": "LnkSta: Speed 16GT/s"}),
		SMBIOS: tableWith([]smbios.Slot{{Designation: "PCIe Slot 3", Type: "PCI Express x4", Usage: "in use", ID: 3, HasAddr: true, Addr: smbios.PCIAddr{Bus: 4}}},
			smbios.Onboard{Designation: "Onboard SATA", Type: "SATA controller", Enabled: true})})
	dump = spaces.ReplaceAllString(dump, " ") // the summary aligns its columns
	for _, want := range []string{"== SMBIOS slots and onboard devices ==", `slot "PCIe Slot 3": PCI Express x4, in use, ID 3, 0000:04:00.0`,
		`onboard "Onboard SATA"`, "Slot: PCIe Slot 3 (SMBIOS slot, PCI Express x4, in use, slot ID 3)", "== lspci -vv -s 0000:04:00.0 ==\nLnkSta"} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q:\n%s", want, dump)
		}
	}
	_, dump = CollectDump("nvme0n1", Options{Sys: nvmeTree(t), Run: fakeRun(nil), SMBIOS: noTable})
	if !strings.Contains(dump, "not readable: permission denied; the table is root-only") {
		t.Errorf("dump must say why SMBIOS is missing:\n%s", dump)
	}
}
