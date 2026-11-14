package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bbbench/internal/blockdev"
	"bbbench/internal/config"
	"bbbench/internal/scsi"
	"bbbench/internal/templating"
	"bbbench/internal/util"
)

// generateFioFilesForDrives generates fio configuration files for the specified drives.
// This is shared logic used by both 'generate' command and orchestrator auto-generation.
func generateFioFilesForDrives(
	cfg *config.Root,
	eng *templating.Engine,
	drives []blockdev.Device,
	host string,
	outBase string,
) error {
	for _, disk := range drives {
		var wcePtr *bool
		if disk.ParentSubsystem == "scsi" && disk.Rotational {
			wce, err := scsi.GetWCE(disk.Path)
			if err == nil {
				wcePtr = &wce
				if cfg.BBBench.WCE != wce {
					fmt.Printf("\twce: %s current:%v target:%v\n", disk.Path, wce, cfg.BBBench.WCE)
				}
			}
		}

		workloadType := disk.WorkloadType()
		workloads := cfg.BBBench.Workloads[workloadType]

		logger.Info("disk workloads", "device", disk.Path, "type", workloadType, "workloads_count", len(workloads))
		if len(workloads) == 0 {
			logger.Warn("no workloads defined for device type", "type", workloadType, "device", disk.Path)
		}

		diskMap := map[string]any{
			"path":        disk.Path,
			"model":       util.SanitizeFilename(disk.Model, "_"),
			"serial":      util.SanitizeFilename(disk.Serial, "_"),
			"rotational":  disk.Rotational,
			"capacity":    disk.CapacityBytes,
			"capacity_gb": disk.CapacityGB(),
			"block_count": disk.BlockCount,
			"block_size":  disk.BlockSize,
			"vendor":      strings.TrimSpace(disk.Vendor),
		}

		manufacturer := fileManufacturer(disk)

		path := filepath.Join(outBase, host)
		deviceName := filepath.Base(disk.Path)
		filename := fioFileName(host, disk)
		filenameFioplot := util.SanitizeFilename(fmt.Sprintf("%s_%s_%s_%s_%s_fioplot.yml", host, manufacturer, disk.Model, disk.Serial, disk.ParentSubsystem), "_")

		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", path, err)
		}

		ctx := map[string]any{
			"Manufacturer": manufacturer,
			"WorkloadsDB":  cfg.BBBench.WorkloadsDB,
			"Workloads":    workloads,
			"Disk":         diskMap,
			"Device":       deviceName,
			"Serial":       util.SanitizeFilename(disk.Serial, "_"),
			"Host":         host,
			"WCE":          wcePtr,
		}

		fioOut, err := eng.Render("disk.fio", ctx)
		if err != nil {
			return fmt.Errorf("render fio: %w", err)
		}
		fioPath := filepath.Join(path, filename)
		if err := os.WriteFile(fioPath, []byte(fioOut), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", fioPath, err)
		}
		fmt.Printf("\t%s\n", fioPath)

		plotOut, err := eng.Render("output.yml", ctx)
		if err != nil {
			return fmt.Errorf("render fioplot: %w", err)
		}
		plotPath := filepath.Join(path, filenameFioplot)
		if err := os.WriteFile(plotPath, []byte(plotOut), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", plotPath, err)
		}
		fmt.Printf("\t%s\n", plotPath)
	}

	return nil
}

// fileManufacturer is the sanitized vendor used in generated file names,
// "unknown" when the vendor is empty.
func fileManufacturer(d blockdev.Device) string {
	m := util.SanitizeFilename(strings.TrimSpace(d.Vendor), "_")
	if m == "" {
		m = "unknown"
	}
	return m
}

// fioFileName is the name of a device's generated fio file,
// {host}_{manufacturer}_{model}_{serial}_{subsystem}_config.fio, sanitized.
// generate and discovery both use it, so they always agree.
func fioFileName(host string, d blockdev.Device) string {
	return util.SanitizeFilename(fmt.Sprintf("%s_%s_%s_%s_%s_config.fio", host, fileManufacturer(d), d.Model, d.Serial, d.ParentSubsystem), "_")
}
