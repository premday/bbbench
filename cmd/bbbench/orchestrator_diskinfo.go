package main

import (
	"fmt"
	"os"
	"path/filepath"

	"bbbench/internal/diskinfo"
)

// collectDiskConfig reads every drive's interface, link, power, write cache and
// SMART state in the background (smartctl and nvme-cli can take seconds), puts
// it on the status page as it arrives and writes the full text dump next to the
// results. It only reads.
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

// writeDiskInfo stores a drive's dump as <output>/<device>_diskinfo_<start>.txt,
// one file per run, so a resumed run does not overwrite what the first one saw.
func (ec *ExecutionCoordinator) writeDiskInfo(device, dump string) (string, error) {
	if ec.outputDir == "" || dump == "" {
		return "", nil
	}
	path := filepath.Join(ec.outputDir, fmt.Sprintf("%s_diskinfo_%d.txt", device, ec.startTime.Unix()))
	return path, os.WriteFile(path, []byte(dump), 0o644)
}
