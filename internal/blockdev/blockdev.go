package blockdev

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type FS interface {
	ReadDir(name string) ([]fs.DirEntry, error)
	ReadFile(name string) ([]byte, error)
	Readlink(name string) (string, error)
}

type OSFS struct{}

func (OSFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (OSFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (OSFS) Readlink(name string) (string, error)       { return os.Readlink(name) }

// Device represents a block disk.
type Device struct {
	Name            string
	Path            string
	Model           string
	Serial          string
	Vendor          string
	Rotational      bool
	ParentSubsystem string
	CapacityBytes   uint64
	BlockCount      uint64
	BlockSize       uint64
}

func (d Device) WorkloadType() string {
	if d.Rotational {
		return "hdd"
	}
	return "ssd"
}

func Discover(fsys FS, sysBlock string) (map[string]Device, error) {
	if sysBlock == "" {
		sysBlock = "/sys/block"
	}
	ents, err := fsys.ReadDir(sysBlock)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Device)
	for _, e := range ents {
		name := e.Name()
		if name == "" || strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}
		dev, err := readOne(fsys, sysBlock, name)
		if err != nil {
			continue
		}
		// keep only devices with model+serial if possible
		if dev.Model == "" || dev.Serial == "" {
			continue
		}
		out[name] = dev
	}
	return out, nil
}

func readOne(fsys FS, sysBlock, name string) (Device, error) {
	base := filepath.Join(sysBlock, name)
	readStr := func(rel string) string {
		b, err := fsys.ReadFile(filepath.Join(base, rel))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	readU64 := func(rel string) uint64 {
		s := readStr(rel)
		if s == "" {
			return 0
		}
		u, _ := strconv.ParseUint(s, 10, 64)
		return u
	}
	rot := readStr("queue/rotational")
	rotational := rot == "1" || strings.EqualFold(rot, "true")

	blockCount := readU64("size")
	blockSize := readU64("queue/logical_block_size")
	capacity := blockCount * blockSize

	// vendor/model/serial are best-effort
	vendor := readStr("device/vendor")
	model := readStr("device/model")
	serial := readStr("device/serial")
	if serial == "" {
		if raw, err := fsys.ReadFile(filepath.Join(base, "device/vpd_pg80")); err == nil {
			serial = parseVPDPage80(raw)
		}
	}
	if serial == "" {
		serial = readStr("device/wwid")
	}

	parentSubsystem := ""
	// /sys/block/<dev>/device/subsystem is a symlink to /sys/bus/<...>
	if link, err := fsys.Readlink(filepath.Join(base, "device/subsystem")); err == nil {
		parentSubsystem = filepath.Base(link)
	}
	if parentSubsystem == "" {
		// fallback by inspecting symlink target of device
		if link, err := fsys.Readlink(filepath.Join(base, "device")); err == nil {
			parentSubsystem = filepath.Base(filepath.Dir(link))
		}
	}

	return Device{
		Name:            name,
		Path:            filepath.Join("/dev", name),
		Vendor:          strings.TrimSpace(vendor),
		Model:           strings.TrimSpace(model),
		Serial:          strings.TrimSpace(serial),
		Rotational:      rotational,
		ParentSubsystem: parentSubsystem,
		CapacityBytes:   capacity,
		BlockCount:      blockCount,
		BlockSize:       blockSize,
	}, nil
}

// Extracts the serial number from a SCSI VPD page 80 blob
func parseVPDPage80(data []byte) string {
	if len(data) < 4 || data[1] != 0x80 {
		return ""
	}
	pageLen := int(data[2])<<8 | int(data[3])
	if len(data) < 4+pageLen {
		return ""
	}
	return strings.TrimSpace(string(data[4 : 4+pageLen]))
}

func (d Device) CapacityGB() uint64 {
	return d.CapacityBytes / 1_000_000_000
}

func (d Device) String() string {
	return fmt.Sprintf("%s(%s %s serial=%s rot=%v cap=%dGB)", d.Name, d.Vendor, d.Model, d.Serial, d.Rotational, d.CapacityGB())
}
