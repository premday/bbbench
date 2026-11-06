package smbios

import (
	"os"
	"path/filepath"
	"testing"
)

// structure builds one SMBIOS structure: header, fields, strings.
func structure(typ byte, handle uint16, fields []byte, strs ...string) []byte {
	b := []byte{typ, byte(4 + len(fields)), byte(handle), byte(handle >> 8)}
	b = append(b, fields...)
	if len(strs) == 0 {
		return append(b, 0, 0)
	}
	for _, s := range strs {
		b = append(append(b, s...), 0)
	}
	return append(b, 0)
}

// slotFields is the formatted area of a type 9 structure after the header:
// designation(1) type width usage length id(2) char1 char2 segment(2) bus devfn.
func slotFields(desig, typ, usage byte, id uint16, seg uint16, bus, dev, fn byte) []byte {
	return []byte{desig, typ, 0x0A, usage, 0x03, byte(id), byte(id >> 8), 0x04, 0x01,
		byte(seg), byte(seg >> 8), bus, dev<<3 | fn}
}

func TestParse_Slots(t *testing.T) {
	var table []byte
	table = append(table, structure(0, 0, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, "BIOS")...) // another type
	table = append(table, structure(9, 0x20, slotFields(1, 0xA8, 4, 3, 0, 0x04, 0, 0), "PCIe Slot 3")...)
	table = append(table, structure(9, 0x21, slotFields(1, 0xB6, 3, 7, 0, 0x1d, 2, 1), "NVMe Bay 7")...)
	table = append(table, structure(9, 0x22, slotFields(1, 0xA5, 3, 9, 0xFFFF, 0xFF, 0, 0), "Not PCI")...)
	table = append(table, structure(127, 0xFF, nil)...)
	table = append(table, structure(9, 0x23, slotFields(1, 0xA5, 3, 1, 0, 1, 0, 0), "after the end marker")...)

	got, err := Parse(table)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Slots) != 3 {
		t.Fatalf("slots = %+v", got.Slots)
	}
	s := got.Slots[0]
	if s.Designation != "PCIe Slot 3" || s.Type != "PCI Express x4" { // 0xA8: the generation is not stated
		t.Errorf("slot 0 = %+v", s)
	}
	if s.Usage != "in use" || s.ID != 3 || !s.HasAddr || s.Addr.String() != "0000:04:00.0" {
		t.Errorf("slot 0 = %+v", s)
	}
	b := got.Slots[1]
	if b.Type != "PCI Express Gen 3 x16" || b.Usage != "available" || b.Addr.String() != "0000:1d:02.1" {
		t.Errorf("slot 1 = %+v", b)
	}
	if n := got.Slots[2]; n.HasAddr || n.Designation != "Not PCI" {
		t.Errorf("a slot with no bus address must say so: %+v", n)
	}
}

func TestParse_OnboardDevices(t *testing.T) {
	// type 41: designation(1) devtype instance segment(2) bus devfn
	f := []byte{1, 0x80 | 0x09, 1, 0, 0, 0x00, 0x17<<3 | 0}
	got, err := Parse(structure(41, 0x30, f, "Onboard SATA"))
	if err != nil || len(got.Onboard) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	o := got.Onboard[0]
	if o.Designation != "Onboard SATA" || o.Type != "SATA controller" || !o.Enabled || o.Addr.String() != "0000:00:17.0" {
		t.Errorf("%+v", o)
	}
}

// Strings are optional, and a string index of 0 means "none".
func TestParse_StringEdgeCases(t *testing.T) {
	f := slotFields(0, 0xA5, 3, 1, 0, 2, 0, 0) // designation index 0
	got, err := Parse(structure(9, 1, f))
	if err != nil || len(got.Slots) != 1 || got.Slots[0].Designation != "" {
		t.Errorf("%+v %v", got, err)
	}
	f = slotFields(5, 0xA5, 3, 1, 0, 2, 0, 0) // index beyond the strings
	if got, _ := Parse(structure(9, 1, f, "only one")); got.Slots[0].Designation != "" {
		t.Errorf("out of range index gave %q", got.Slots[0].Designation)
	}
}

// An older BIOS (before 2.6) has a short type 9 with no bus address.
func TestParse_ShortSlotHasNoAddress(t *testing.T) {
	short := []byte{1, 0xA5, 0x0A, 4, 0x03, 5, 0, 0x04, 0x01} // ends at characteristics 2
	got, err := Parse(structure(9, 1, short, "Slot 5"))
	if err != nil || len(got.Slots) != 1 || got.Slots[0].HasAddr || got.Slots[0].ID != 5 {
		t.Errorf("%+v %v", got, err)
	}
}

func TestParse_Truncated(t *testing.T) {
	good := structure(9, 1, slotFields(1, 0xA5, 3, 1, 0, 1, 0, 0), "Slot")
	got, err := Parse(append(good, 9, 40, 0, 0, 1)) // second structure claims 40 bytes
	if err == nil {
		t.Error("a truncated structure must be reported")
	}
	if got == nil || len(got.Slots) != 1 {
		t.Errorf("what was read before the damage must survive: %+v", got)
	}
}

func TestLoad(t *testing.T) {
	sys := t.TempDir()
	if _, err := Load(sys); err == nil {
		t.Error("no table must be an error (it is root-only)")
	}
	dir := filepath.Join(sys, "firmware", "dmi", "tables")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "DMI"), structure(9, 1, slotFields(1, 0xA5, 4, 2, 0, 3, 0, 0), "Slot 2"), 0o644)
	got, err := Load(sys)
	if err != nil || len(got.Slots) != 1 || got.Slots[0].Designation != "Slot 2" {
		t.Errorf("%+v %v", got, err)
	}
}
