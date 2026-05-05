package blockdev

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// fakeEntry implements fs.DirEntry.
type fakeEntry struct {
	name  string
	isDir bool
}

func (e fakeEntry) Name() string { return e.name }
func (e fakeEntry) IsDir() bool  { return e.isDir }
func (e fakeEntry) Type() fs.FileMode {
	if e.isDir {
		return fs.ModeDir
	}
	return 0
}
func (e fakeEntry) Info() (fs.FileInfo, error) { return nil, nil }

type mockFS struct {
	dirs     map[string][]fakeEntry
	files    map[string]string
	symlinks map[string]string
}

func (m *mockFS) ReadDir(name string) ([]fs.DirEntry, error) {
	ents, ok := m.dirs[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	out := make([]fs.DirEntry, len(ents))
	for i, e := range ents {
		out[i] = e
	}
	return out, nil
}

func (m *mockFS) ReadFile(name string) ([]byte, error) {
	s, ok := m.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(s), nil
}

func (m *mockFS) Readlink(name string) (string, error) {
	s, ok := m.symlinks[name]
	if !ok {
		return "", os.ErrNotExist
	}
	return s, nil
}

// validSDADevice returns a mockFS pre-populated with a single valid SSD "sda"
// device under /sys/block.
func validSDADevice() *mockFS {
	return &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {{name: "sda"}},
		},
		files: map[string]string{
			"/sys/block/sda/queue/rotational":         "0\n",
			"/sys/block/sda/device/vendor":            "ATA     ",
			"/sys/block/sda/device/model":             "Samsung SSD 990",
			"/sys/block/sda/device/serial":            "S6EVNX0T123456",
			"/sys/block/sda/size":                     "1953525168",
			"/sys/block/sda/queue/logical_block_size": "512",
		},
		symlinks: map[string]string{
			"/sys/block/sda/device/subsystem": "/sys/bus/scsi",
		},
	}
}

// TestDiscover_FiltersLoopDevices verifies that devices prefixed with "loop" are skipped.
func TestDiscover_FiltersLoopDevices(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {
				{name: "loop0"},
				{name: "loop1"},
			},
		},
		files:    map[string]string{},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("expected 0 devices, got %d: %v", len(devs), devs)
	}
}

// TestDiscover_FiltersRamDevices verifies that devices prefixed with "ram" are skipped.
func TestDiscover_FiltersRamDevices(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {
				{name: "ram0"},
				{name: "ram1"},
			},
		},
		files:    map[string]string{},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("expected 0 devices, got %d: %v", len(devs), devs)
	}
}

// TestDiscover_SkipsDeviceWhenReadOneFails verifies that a device with no
// readable files is silently skipped (readOne returns an error only when
// individual required files are absent; here the device has model+serial
// missing so it is skipped at the model/serial filter stage — but a device
// whose sysfs path is entirely absent from ReadDir also behaves the same way
// because readOne returns an empty Device).
func TestDiscover_SkipsDeviceWithNoFiles(t *testing.T) {
	// "nvme0n1" appears in ReadDir but has no files at all, so model and serial
	// will both be empty strings and the device will be filtered out.
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {{name: "nvme0n1"}},
		},
		files:    map[string]string{},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("expected 0 devices, got %d", len(devs))
	}
}

// TestDiscover_SkipsDeviceWithEmptyModel verifies that a device with an empty
// model is excluded.
func TestDiscover_SkipsDeviceWithEmptyModel(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {{name: "sda"}},
		},
		files: map[string]string{
			"/sys/block/sda/device/serial": "SERIAL123",
			// device/model intentionally absent → empty
		},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("expected device to be skipped due to empty model, got %d devices", len(devs))
	}
}

// TestDiscover_SkipsDeviceWithEmptySerial verifies that a device with an empty
// serial (and no wwid) is excluded.
func TestDiscover_SkipsDeviceWithEmptySerial(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {{name: "sda"}},
		},
		files: map[string]string{
			"/sys/block/sda/device/model": "SomeModel",
			// device/serial and device/wwid intentionally absent → empty
		},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("expected device to be skipped due to empty serial, got %d devices", len(devs))
	}
}

// TestDiscover_IncludesDeviceWithModelAndSerial verifies that a fully-populated
// device is returned by Discover.
func TestDiscover_IncludesDeviceWithModelAndSerial(t *testing.T) {
	fsys := validSDADevice()
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devs))
	}
	dev, ok := devs["sda"]
	if !ok {
		t.Fatal("expected device named 'sda'")
	}
	if dev.Model != "Samsung SSD 990" {
		t.Errorf("unexpected model: %q", dev.Model)
	}
	if dev.Serial != "S6EVNX0T123456" {
		t.Errorf("unexpected serial: %q", dev.Serial)
	}
}

// TestDiscover_FallsBackToWWID verifies that when device/serial is empty the
// code reads device/wwid instead.
func TestDiscover_FallsBackToWWID(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {{name: "sda"}},
		},
		files: map[string]string{
			"/sys/block/sda/queue/rotational": "0\n",
			"/sys/block/sda/device/vendor":    "VENDOR",
			"/sys/block/sda/device/model":     "SomeModel",
			// device/serial absent
			"/sys/block/sda/device/wwid":              "eui.0025388b910027af",
			"/sys/block/sda/size":                     "100",
			"/sys/block/sda/queue/logical_block_size": "512",
		},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev, ok := devs["sda"]
	if !ok {
		t.Fatal("expected device 'sda' to be present")
	}
	if dev.Serial != "eui.0025388b910027af" {
		t.Errorf("expected serial from wwid, got %q", dev.Serial)
	}
}

// TestDiscover_RotationalTrue verifies that "1" in queue/rotational sets Rotational=true.
func TestDiscover_RotationalTrue(t *testing.T) {
	fsys := validSDADevice()
	fsys.files["/sys/block/sda/queue/rotational"] = "1\n"

	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev := devs["sda"]
	if !dev.Rotational {
		t.Error("expected Rotational=true for value '1'")
	}
}

// TestDiscover_RotationalFalse verifies that "0" in queue/rotational sets Rotational=false.
func TestDiscover_RotationalFalse(t *testing.T) {
	fsys := validSDADevice()
	fsys.files["/sys/block/sda/queue/rotational"] = "0\n"

	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev := devs["sda"]
	if dev.Rotational {
		t.Error("expected Rotational=false for value '0'")
	}
}

// TestDiscover_ReadsVendorModelSerial verifies that vendor, model, and serial
// are read and whitespace-trimmed.
func TestDiscover_ReadsVendorModelSerial(t *testing.T) {
	fsys := validSDADevice()
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev := devs["sda"]
	if dev.Vendor != "ATA" {
		t.Errorf("expected Vendor 'ATA', got %q", dev.Vendor)
	}
	if dev.Model != "Samsung SSD 990" {
		t.Errorf("expected model 'Samsung SSD 990', got %q", dev.Model)
	}
	if dev.Serial != "S6EVNX0T123456" {
		t.Errorf("expected serial 'S6EVNX0T123456', got %q", dev.Serial)
	}
}

// TestDiscover_ReadsCapacity verifies BlockCount, BlockSize, and CapacityBytes.
func TestDiscover_ReadsCapacity(t *testing.T) {
	fsys := validSDADevice()
	// size=1953525168, logical_block_size=512
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev := devs["sda"]

	const wantBlockCount uint64 = 1953525168
	const wantBlockSize uint64 = 512
	const wantCapacity = wantBlockCount * wantBlockSize

	if dev.BlockCount != wantBlockCount {
		t.Errorf("BlockCount: want %d, got %d", wantBlockCount, dev.BlockCount)
	}
	if dev.BlockSize != wantBlockSize {
		t.Errorf("BlockSize: want %d, got %d", wantBlockSize, dev.BlockSize)
	}
	if dev.CapacityBytes != wantCapacity {
		t.Errorf("CapacityBytes: want %d, got %d", wantCapacity, dev.CapacityBytes)
	}
}

// TestDiscover_SetsNameAndPath verifies that Name and Path are set correctly.
func TestDiscover_SetsNameAndPath(t *testing.T) {
	fsys := validSDADevice()
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev := devs["sda"]
	if dev.Name != "sda" {
		t.Errorf("Name: want 'sda', got %q", dev.Name)
	}
	if dev.Path != "/dev/sda" {
		t.Errorf("Path: want '/dev/sda', got %q", dev.Path)
	}
}

// TestDiscover_ParentSubsystemFromSubsystemLink verifies that ParentSubsystem
// is set to the base of the device/subsystem symlink target.
func TestDiscover_ParentSubsystemFromSubsystemLink(t *testing.T) {
	fsys := validSDADevice()
	// symlinks["/sys/block/sda/device/subsystem"] = "/sys/bus/scsi" → base = "scsi"
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev := devs["sda"]
	if dev.ParentSubsystem != "scsi" {
		t.Errorf("ParentSubsystem: want 'scsi', got %q", dev.ParentSubsystem)
	}
}

// TestDiscover_ParentSubsystemFallbackFromDeviceLink verifies the fallback path
// when device/subsystem is absent: uses filepath.Base(filepath.Dir(device link)).
func TestDiscover_ParentSubsystemFallbackFromDeviceLink(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {{name: "nvme0n1"}},
		},
		files: map[string]string{
			"/sys/block/nvme0n1/queue/rotational":         "0\n",
			"/sys/block/nvme0n1/device/vendor":            "Samsung",
			"/sys/block/nvme0n1/device/model":             "NVMe SSD",
			"/sys/block/nvme0n1/device/serial":            "NVMESERIAL",
			"/sys/block/nvme0n1/size":                     "1000",
			"/sys/block/nvme0n1/queue/logical_block_size": "512",
		},
		symlinks: map[string]string{
			// device/subsystem is absent; device points into nvme subsystem dir
			"/sys/block/nvme0n1/device": "../../devices/pci0000:00/nvme/nvme0n1",
		},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev := devs["nvme0n1"]
	// filepath.Dir("../../devices/pci0000:00/nvme/nvme0n1") = "../../devices/pci0000:00/nvme"
	// filepath.Base(...) = "nvme"
	if dev.ParentSubsystem != "nvme" {
		t.Errorf("ParentSubsystem: want 'nvme', got %q", dev.ParentSubsystem)
	}
}

// TestDiscover_ParentSubsystemEmptyWhenBothLinksAbsent verifies that
// ParentSubsystem is empty when neither symlink exists.
func TestDiscover_ParentSubsystemEmptyWhenBothLinksAbsent(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {{name: "sda"}},
		},
		files: map[string]string{
			"/sys/block/sda/queue/rotational":         "0\n",
			"/sys/block/sda/device/vendor":            "ACME",
			"/sys/block/sda/device/model":             "Generic Disk",
			"/sys/block/sda/device/serial":            "SERIAL000",
			"/sys/block/sda/size":                     "512",
			"/sys/block/sda/queue/logical_block_size": "512",
		},
		symlinks: map[string]string{}, // no symlinks at all
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev := devs["sda"]
	if dev.ParentSubsystem != "" {
		t.Errorf("ParentSubsystem: want empty string, got %q", dev.ParentSubsystem)
	}
}

// TestDiscover_UsesProvidedSysBlock verifies that a non-empty sysBlock argument
// is used as the root directory for discovery.
func TestDiscover_UsesProvidedSysBlock(t *testing.T) {
	customPath := "/custom/block"
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			customPath: {{name: "sdb"}},
		},
		files: map[string]string{
			customPath + "/sdb/queue/rotational":         "0\n",
			customPath + "/sdb/device/vendor":            "VENDOR",
			customPath + "/sdb/device/model":             "Model X",
			customPath + "/sdb/device/serial":            "SER001",
			customPath + "/sdb/size":                     "1000",
			customPath + "/sdb/queue/logical_block_size": "512",
		},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, customPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := devs["sdb"]; !ok {
		t.Error("expected device 'sdb' to be discovered under custom sysBlock path")
	}
}

// TestDiscover_DefaultSysBlockUsed verifies that passing an empty sysBlock
// causes the code to read from "/sys/block".
func TestDiscover_DefaultSysBlockUsed(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {{name: "sdc"}},
		},
		files: map[string]string{
			"/sys/block/sdc/queue/rotational":         "0\n",
			"/sys/block/sdc/device/vendor":            "V",
			"/sys/block/sdc/device/model":             "DefaultPathModel",
			"/sys/block/sdc/device/serial":            "DEFSERIAL",
			"/sys/block/sdc/size":                     "100",
			"/sys/block/sdc/queue/logical_block_size": "512",
		},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "") // empty → should default to /sys/block
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := devs["sdc"]; !ok {
		t.Error("expected device 'sdc' when sysBlock defaults to /sys/block")
	}
}

// ---------------------------------------------------------------------------
// Device method tests
// ---------------------------------------------------------------------------

// TestWorkloadType_HDD verifies that a rotational device reports "hdd".
func TestWorkloadType_HDD(t *testing.T) {
	d := Device{Rotational: true}
	if got := d.WorkloadType(); got != "hdd" {
		t.Errorf("WorkloadType: want 'hdd', got %q", got)
	}
}

// TestWorkloadType_SSD verifies that a non-rotational device reports "ssd".
func TestWorkloadType_SSD(t *testing.T) {
	d := Device{Rotational: false}
	if got := d.WorkloadType(); got != "ssd" {
		t.Errorf("WorkloadType: want 'ssd', got %q", got)
	}
}

// TestCapacityGB verifies integer division by 1_000_000_000.
func TestCapacityGB(t *testing.T) {
	d := Device{CapacityBytes: 2_000_000_000_000}
	if got := d.CapacityGB(); got != 2000 {
		t.Errorf("CapacityGB: want 2000, got %d", got)
	}
}

// TestCapacityGB_Zero verifies zero capacity.
func TestCapacityGB_Zero(t *testing.T) {
	d := Device{CapacityBytes: 0}
	if got := d.CapacityGB(); got != 0 {
		t.Errorf("CapacityGB: want 0, got %d", got)
	}
}

// TestString_ContainsFields verifies that String() includes name, vendor, model, serial.
func TestString_ContainsFields(t *testing.T) {
	d := Device{
		Name:          "sda",
		Vendor:        "ATA",
		Model:         "Samsung SSD 990",
		Serial:        "S6EVNX0T123456",
		CapacityBytes: 1_000_000_000,
	}
	s := d.String()
	for _, want := range []string{"sda", "ATA", "Samsung SSD 990", "S6EVNX0T123456"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, expected to contain %q", s, want)
		}
	}
}

// TestDiscover_MultipleDevices verifies that multiple valid devices are all returned.
func TestDiscover_MultipleDevices(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {
				{name: "sda"},
				{name: "sdb"},
			},
		},
		files: map[string]string{
			"/sys/block/sda/queue/rotational":         "1\n",
			"/sys/block/sda/device/vendor":            "SEAGATE",
			"/sys/block/sda/device/model":             "ST4000NM",
			"/sys/block/sda/device/serial":            "HDASERIAL",
			"/sys/block/sda/size":                     "7814037168",
			"/sys/block/sda/queue/logical_block_size": "512",

			"/sys/block/sdb/queue/rotational":         "0\n",
			"/sys/block/sdb/device/vendor":            "SAMSUNG",
			"/sys/block/sdb/device/model":             "MZ7LH960",
			"/sys/block/sdb/device/serial":            "SSDSERIAL",
			"/sys/block/sdb/size":                     "1875385008",
			"/sys/block/sdb/queue/logical_block_size": "512",
		},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devs))
	}
	if devs["sda"].Rotational != true {
		t.Error("sda should be rotational")
	}
	if devs["sdb"].Rotational != false {
		t.Error("sdb should not be rotational")
	}
}

// TestDiscover_MixedFilteredAndValid verifies that loop/ram devices are
// filtered while valid devices are retained in the same ReadDir result.
func TestDiscover_MixedFilteredAndValid(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {
				{name: "loop0"},
				{name: "ram0"},
				{name: "sda"},
			},
		},
		files: map[string]string{
			"/sys/block/sda/queue/rotational":         "0\n",
			"/sys/block/sda/device/vendor":            "ATA",
			"/sys/block/sda/device/model":             "Good Drive",
			"/sys/block/sda/device/serial":            "GOODSERIAL",
			"/sys/block/sda/size":                     "1000",
			"/sys/block/sda/queue/logical_block_size": "512",
		},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devs))
	}
	if _, ok := devs["sda"]; !ok {
		t.Error("expected 'sda' in result")
	}
}

// TestParseVPDPage80 tests the VPD page 80 serial number parsing
func TestParseVPDPage80(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "valid real data with leading spaces",
			data: []byte{0x00, 0x80, 0x00, 0x14,
				0x20, 0x20, 0x20, 0x20, 0x20, 0x20, 0x20, 0x20,
				'S', 'E', 'R', 'I', 'A', 'L', 'N', 'U', 'M', 'B', 'E', 'R'},
			want: "SERIALNUMBER",
		},
		{
			name: "too short",
			data: []byte{0x00, 0x80, 0x00},
			want: "",
		},
		{
			name: "wrong page code",
			data: []byte{0x00, 0x83, 0x00, 0x04, 'A', 'B', 'C', 'D'},
			want: "",
		},
		{
			name: "truncated payload",
			data: []byte{0x00, 0x80, 0x00, 0x10, 'A', 'B'},
			want: "",
		},
		{
			name: "no padding",
			data: []byte{0x00, 0x80, 0x00, 0x04, 'A', 'B', 'C', 'D'},
			want: "ABCD",
		},
		{
			name: "empty data",
			data: []byte{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseVPDPage80(tt.data)
			if got != tt.want {
				t.Errorf("parseVPDPage80() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDiscover_FallsBackToVPDPage80 verifies that when device/serial is absent,
// the code parses device/vpd_pg80 before falling back to wwid.
func TestDiscover_FallsBackToVPDPage80(t *testing.T) {
	fsys := &mockFS{
		dirs: map[string][]fakeEntry{
			"/sys/block": {{name: "sda"}},
		},
		files: map[string]string{
			"/sys/block/sda/queue/rotational":         "0\n",
			"/sys/block/sda/device/vendor":            "ATA",
			"/sys/block/sda/device/model":             "MYLITTLEHDD",
			"/sys/block/sda/size":                     "1000",
			"/sys/block/sda/queue/logical_block_size": "512",
			// device/serial absent; vpd_pg80 present with binary data
			"/sys/block/sda/device/vpd_pg80": string([]byte{
				0x00, 0x80, 0x00, 0x14,
				0x20, 0x20, 0x20, 0x20, 0x20, 0x20, 0x20, 0x20,
				'S', 'E', 'R', 'I', 'A', 'L', 'N', 'U', 'M', 'B', 'E', 'R',
			}),
		},
		symlinks: map[string]string{},
	}
	devs, err := Discover(fsys, "/sys/block")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dev, ok := devs["sda"]
	if !ok {
		t.Fatal("expected device 'sda' to be present")
	}
	if dev.Serial != "SERIALNUMBER" {
		t.Errorf("expected serial from vpd_pg80, got %q", dev.Serial)
	}
}
