package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"bbbench/internal/blockdev"
	"bbbench/internal/diskinfo"
)

// collectDiskConfig reads every drive's interface, link, power, write cache and
// SMART state in the background (smartctl and nvme-cli can take seconds), puts
// it on the status page as it arrives and writes the full text dump next to the
// results. It only reads (a setting test changes drives elsewhere, in runGroup).
func (ec *ExecutionCoordinator) collectDiskConfig() {
	collect := ec.collectDisk
	if collect == nil {
		collect = func(drive DriveInfo) (diskinfo.Info, string) {
			d := drive.Device
			return diskinfo.CollectDump(d.Name, diskinfo.Options{
				Label: fmt.Sprintf("%s - %s %s, S/N %s, %d GB", d.Name, d.Vendor, d.Model, d.Serial, d.CapacityGB()),
			})
		}
	}
	for _, drive := range ec.drives {
		if drive.IsArray() {
			ec.collectMembers(drive, collect)
			continue
		}
		go func() {
			info, dump := collect(drive)
			markDrive(drive.Device.Name, func(d *DriveStatus) { d.Config = &info })
			if path, err := ec.writeDiskInfo(drive.Device.Name, dump); err != nil {
				logger.Warn("writing disk configuration", "device", drive.Device.Name, "err", err)
			} else if ec.verbose {
				fmt.Printf("[%s] disk configuration: %s\n", drive.Device.Name, path)
			}
		}()
	}
}

// collectMembers collects each member disk of a composite target (an
// array), not the composite: the drive's interface, SMART and topology
// belong to the disks, while the benchmark results attach to the composite.
// Each member's configuration lands in the composite's status entry, keyed
// by name and serial, on a fresh slice (a reader may hold the old one), and
// its dump is written as <member>_diskinfo_<start>.txt.
func (ec *ExecutionCoordinator) collectMembers(drive DriveInfo, collect func(DriveInfo) (diskinfo.Info, string)) {
	for _, m := range drive.Members {
		go func() {
			info, dump := collect(DriveInfo{Device: blockdev.Device{Name: m.Name, Serial: m.Serial, Model: m.Model}})
			markDrive(drive.Device.Name, func(d *DriveStatus) {
				ms := slices.Clone(d.Members)
				for i := range ms {
					if ms[i].Name == m.Name && ms[i].Serial == m.Serial {
						ms[i].Config = &info
					}
				}
				d.Members = ms
			})
			if path, err := ec.writeDiskInfo(m.Name, dump); err != nil {
				logger.Warn("writing disk configuration", "device", m.Name, "err", err)
			} else if ec.verbose {
				fmt.Printf("[%s] disk configuration of member %s: %s\n", drive.Device.Name, m.Name, path)
			}
		}()
	}
}

// writeDiskInfo stores a drive's dump as <output>/<device>_diskinfo_<start>.txt,
// one file per run, so a resumed run does not overwrite what the first one saw.
func (ec *ExecutionCoordinator) writeDiskInfo(device, dump string) (string, error) {
	if ec.outputDir == "" || dump == "" {
		return "", nil
	}
	path := filepath.Join(ec.outputDir, fmt.Sprintf("%s_diskinfo_%d.txt", device, ec.startTime.Unix()))
	return path, os.WriteFile(path, []byte(dump), 0o644)
}
