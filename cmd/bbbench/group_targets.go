package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"bbbench/internal/blockdev"
	"bbbench/internal/config"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
	"bbbench/internal/templating"
)

// fioGenerator renders the fio and fioplot files of one device into the
// configured output directory (generateFioFilesForDrives) and returns the
// fio file's path.
func fioGenerator(cfg *config.Root, eng *templating.Engine, host string) func(blockdev.Device) (string, error) {
	return func(d blockdev.Device) (string, error) {
		outBase := expandUser(cfg.Fio.Generated.Path)
		if err := generateFioFilesForDrives(cfg, eng, []blockdev.Device{d}, host, outBase); err != nil {
			return "", err
		}
		return filepath.Join(outBase, host, fioFileName(host, d)), nil
	}
}

// arrayDrive turns a composite target built by a layout into the drive a
// run benchmarks. Its identity (name, node, vendor, model, serial) comes
// from the target and its subsystem from the target's interface, so the fio
// file is named after it. The workload type comes from the data members
// (journal and spare disks do not count): all rotational or none, mixed
// media is refused. The size is read from the composite's own sysfs entry
// (sysBlock, "" for /sys/block). gen writes the fio file.
//
// Flush tracing and the write-through gate need no case for a composite:
// flushtrace.Start sees from sysfs that an md or dm node is bio-based and
// times its flush bios (block_bio_queue -> block_bio_complete) on the
// composite's own dev numbers, and flushGate reads its own
// queue/write_cache. UNVERIFIED on hardware: whether block_bio_complete
// fires for the node's own flush bios; a kernel without the bio
// tracepoints takes the request path, which sees no Flush on a bio-based
// node: the phase then shows "ftrace saw 0 Flush commands" (pinned by
// TestArrayFlush_NoRequestEventsShowsZeroFlushNote).
func arrayDrive(bt settings.BenchTarget, drives map[string]DriveInfo, sysBlock string, gen func(blockdev.Device) (string, error)) (DriveInfo, error) {
	t := bt.Target
	var hdd, ssd string
	for _, m := range bt.Members {
		d, ok := drives[m.Name]
		if !ok {
			return DriveInfo{}, fmt.Errorf("member %s is not a discovered drive", m.Name)
		}
		if m.Role != settings.RoleData && m.Role != "" {
			continue
		}
		if d.Device.Rotational {
			if hdd == "" {
				hdd = m.Name
			}
		} else if ssd == "" {
			ssd = m.Name
		}
	}
	if hdd != "" && ssd != "" {
		return DriveInfo{}, fmt.Errorf("mixed media: %s is HDD, %s is SSD", hdd, ssd)
	}
	if hdd == "" && ssd == "" {
		return DriveInfo{}, fmt.Errorf("%s has no data member", t.Name)
	}

	if sysBlock == "" {
		sysBlock = "/sys/block"
	}
	base := filepath.Join(sysBlock, t.Name)
	count, err := readSysUint(filepath.Join(base, "size"))
	if err != nil {
		return DriveInfo{}, err
	}
	bs, err := readSysUint(filepath.Join(base, "queue", "logical_block_size"))
	if err != nil {
		return DriveInfo{}, err
	}
	dev := blockdev.Device{
		Name:            t.Name,
		Path:            t.Path,
		Vendor:          t.Vendor,
		Model:           t.Model,
		Serial:          t.Serial,
		Rotational:      hdd != "",
		ParentSubsystem: t.Interface,
		// sysfs "size" is always in 512-byte sectors.
		CapacityBytes: count * 512,
		BlockCount:    count,
		BlockSize:     bs,
	}
	path, err := gen(dev)
	if err != nil {
		return DriveInfo{}, fmt.Errorf("generate the fio file of %s: %w", t.Name, err)
	}
	return DriveInfo{
		Device:      dev,
		FioFilePath: path,
		FioExists:   true,
		Members:     rungroup.MembersOf(bt.Members),
		UUID:        bt.UUID,
	}, nil
}

// readSysUint reads one unsigned number from a sysfs file.
func readSysUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
}

// drivesByName indexes drives by kernel name.
func drivesByName(drives []DriveInfo) map[string]DriveInfo {
	out := make(map[string]DriveInfo, len(drives))
	for _, d := range drives {
		out[d.Device.Name] = d
	}
	return out
}
