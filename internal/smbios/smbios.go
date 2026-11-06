// Package smbios reads the parts of the SMBIOS (DMI) table that say where
// storage hangs: the system slots (type 9) and the onboard devices (type 41).
// The table is /sys/firmware/dmi/tables/DMI, readable by root only.
package smbios

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PCIAddr is a PCI address from an SMBIOS structure.
type PCIAddr struct {
	Segment  uint16
	Bus      uint8
	Device   uint8
	Function uint8
}

// String renders it the way sysfs names PCI devices: "0000:04:00.0".
func (a PCIAddr) String() string {
	return fmt.Sprintf("%04x:%02x:%02x.%d", a.Segment, a.Bus, a.Device, a.Function)
}

// Slot is a system slot (SMBIOS type 9): where a card or a drive plugs in.
type Slot struct {
	Designation string // "PCIe Slot 3", "NVMe Bay 5"
	Type        string // "PCI Express x4", or "type 0xNN" when not in the table below
	Usage       string // "in use", "available", ...
	ID          uint16 // the slot ID the firmware uses (ACPI _SUN)
	Addr        PCIAddr
	HasAddr     bool // false when the BIOS gives no bus address (older tables, or not applicable)
}

// Onboard is an onboard device (SMBIOS type 41), e.g. the chipset's SATA controller.
type Onboard struct {
	Designation string // "SATA Controller"
	Type        string // "SATA controller", "SAS controller", "NVMe controller", ...
	Enabled     bool
	Addr        PCIAddr
}

// Table is what was read from the DMI table.
type Table struct {
	Slots   []Slot
	Onboard []Onboard
}

// Load reads the DMI table below the sysfs root (default /sys). It fails
// without root, since the file is mode 0400.
func Load(sys string) (*Table, error) {
	if sys == "" {
		sys = "/sys"
	}
	b, err := os.ReadFile(filepath.Join(sys, "firmware", "dmi", "tables", "DMI"))
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse reads the structure table: each structure is a formatted area (type,
// length, handle, fields) followed by NUL-terminated strings and an extra NUL.
func Parse(b []byte) (*Table, error) {
	t := &Table{}
	for off := 0; off+4 <= len(b); {
		typ, length := b[off], int(b[off+1])
		if length < 4 || off+length > len(b) {
			return t, fmt.Errorf("smbios: bad structure at offset %d (type %d, length %d)", off, typ, length)
		}
		formatted := b[off : off+length]
		strs, next := readStrings(b, off+length)
		switch typ {
		case 9:
			if s, ok := parseSlot(formatted, strs); ok {
				t.Slots = append(t.Slots, s)
			}
		case 41:
			if o, ok := parseOnboard(formatted, strs); ok {
				t.Onboard = append(t.Onboard, o)
			}
		case 127: // end of table
			return t, nil
		}
		off = next
	}
	return t, nil
}

// readStrings returns the strings after a formatted area and the offset of the
// next structure. A structure without strings ends in two NULs.
func readStrings(b []byte, off int) (strs []string, next int) {
	if off+1 < len(b) && b[off] == 0 && b[off+1] == 0 {
		return nil, off + 2
	}
	start := off
	for i := off; i < len(b); i++ {
		if b[i] != 0 {
			continue
		}
		strs = append(strs, string(b[start:i]))
		start = i + 1
		if i+1 < len(b) && b[i+1] == 0 {
			return strs, i + 2
		}
	}
	return strs, len(b)
}

func str(strs []string, idx byte) string {
	if idx == 0 || int(idx) > len(strs) {
		return ""
	}
	return strings.TrimSpace(strs[idx-1])
}

// parseSlot reads a type 9 structure. Fields from SMBIOS 2.6 on (segment, bus,
// device/function) need a formatted area of at least 0x11 bytes.
func parseSlot(f []byte, strs []string) (Slot, bool) {
	if len(f) < 0x0D {
		return Slot{}, false
	}
	s := Slot{
		Designation: str(strs, f[4]),
		Type:        slotType(f[5]),
		Usage:       slotUsage(f[7]),
		ID:          binary.LittleEndian.Uint16(f[9:11]),
	}
	if len(f) >= 0x11 {
		seg := binary.LittleEndian.Uint16(f[0x0D:0x0F])
		bus, devfn := f[0x0F], f[0x10]
		// Segment 0xFFFF with bus 0xFF means "not a PCI slot / not applicable".
		if !(seg == 0xFFFF && bus == 0xFF) {
			s.Addr = PCIAddr{Segment: seg, Bus: bus, Device: devfn >> 3, Function: devfn & 7}
			s.HasAddr = true
		}
	}
	return s, true
}

// parseOnboard reads a type 41 structure.
func parseOnboard(f []byte, strs []string) (Onboard, bool) {
	if len(f) < 0x0B {
		return Onboard{}, false
	}
	devfn := f[0x0A]
	return Onboard{
		Designation: str(strs, f[4]),
		Type:        onboardType(f[5] & 0x7F),
		Enabled:     f[5]&0x80 != 0,
		Addr:        PCIAddr{Segment: binary.LittleEndian.Uint16(f[0x07:0x09]), Bus: f[0x09], Device: devfn >> 3, Function: devfn & 7},
	}, true
}

func slotUsage(b byte) string {
	switch b {
	case 3:
		return "available"
	case 4:
		return "in use"
	case 5:
		return "unavailable"
	}
	return ""
}

// slotType names the slot types that matter for storage; anything else is
// shown as its code, which still identifies it in the spec.
func slotType(b byte) string {
	switch b {
	case 0x06:
		return "PCI"
	case 0x12:
		return "PCI-X"
	case 0x16:
		return "M.2 Socket 2"
	case 0x17:
		return "M.2 Socket 3"
	case 0xA5:
		return "PCI Express"
	case 0xA6, 0xAC, 0xB2:
		return pcieGen(b) + " x1"
	case 0xA7, 0xAD, 0xB3:
		return pcieGen(b) + " x2"
	case 0xA8, 0xAE, 0xB4:
		return pcieGen(b) + " x4"
	case 0xA9, 0xAF, 0xB5:
		return pcieGen(b) + " x8"
	case 0xAA, 0xB0, 0xB6:
		return pcieGen(b) + " x16"
	case 0xAB:
		return "PCI Express Gen 2"
	case 0xB1:
		return "PCI Express Gen 3"
	}
	return fmt.Sprintf("type 0x%02X", b)
}

func pcieGen(b byte) string {
	switch {
	case b >= 0xB2:
		return "PCI Express Gen 3"
	case b >= 0xAC:
		return "PCI Express Gen 2"
	}
	return "PCI Express"
}

func onboardType(b byte) string {
	switch b {
	case 4:
		return "SCSI controller"
	case 8:
		return "PATA controller"
	case 9:
		return "SATA controller"
	case 0x0A:
		return "SAS controller"
	case 0x0E:
		return "eMMC"
	case 0x0F:
		return "NVMe controller"
	case 0x10:
		return "UFS controller"
	}
	return fmt.Sprintf("device type 0x%02X", b)
}
