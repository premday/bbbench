# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`bbbench` is a CLI tool that generates **fio job files** for block-device benchmarking. It introspects Linux host hardware (via sysfs and DMI) to automatically discover block devices and generate customized fio benchmark configurations using Go templates.

## Build and Test Commands

### Build
```bash
go build ./cmd/bbbench
```
Produces `./bbbench` binary.

### Test
```bash
# Run all tests
go test ./...

# Run tests for specific package
go test ./internal/config
go test ./internal/templating

# Run with verbose output
go test -v ./...
```

### Release and CI

- `make check` runs the CI checks (gofmt, vet, `go test -race`); `make package` builds all release archives into `./build`.
- Builds are pure Go (`CGO_ENABLED=0`), static on every arch. The version is injected with `-ldflags "-X bbbench/internal/buildinfo.Version=..."` (also `.Commit`, `.Date`; package `internal/buildinfo`, used by both binaries). `bbbench version` and `check-config --version` print them; a plain `go build` reports `dev`..
- `scripts/package.sh` builds one archive (binaries, README/LICENSE/CHANGELOG, `documentation/`, `dist/` without `assets.go`); `package-all.sh` loops over the architectures and writes `SHA256SUMS` and `build/RELEASE_NOTES.md` (from `scripts/changelog.sh`: non-merge commit subjects since the previous tag; `REF=<tag>` selects the end ref). Each archive also gets a `VERSION` file; `smoke-test.sh` checks an archive's contents, static linking and version.
- `.github/workflows/ci.yml` (PRs and main) must pass before merge: `test` and `package` jobs. `release.yml` publishes on `v*` tags. Runners are pinned to `ubuntu-26.04` (not `ubuntu-latest`, so the image only changes when we choose); move to a newer image deliberately. Adding a new architecture means adding it to `scripts/package-all.sh` and the README/CHANGELOG lists.
- Any new CI check should also be added to `make check` so it can be run locally.

### Running the Tool
Requires root privileges for `generate` and `doctor` commands:
```bash
# Generate fio job files (requires root)
sudo ./bbbench generate

# Validate templates without generating
./bbbench validate-templates

# Check system readiness
sudo ./bbbench doctor
```

## Architecture

### Command Structure
The main binary (`cmd/bbbench/main.go`) is a simple command router with no external CLI framework. Commands are dispatched via switch statement to `run*()` functions.

### Core Generation Flow (cmd/bbbench/generate.go)

1. **Config Loading**: Search order for `default.yml`:
   - `~/.bbbench/config/default.yml`
   - `/etc/bbbench/default.yml`
   - `./dist/default.yml`
   - Embedded fallback in binary

2. **Template Loading**: Search order for templates directory:
   - `~/.bbbench/config/templates/`
   - `/etc/bbbench/templates/`
   - `./dist/templates/`
   - Embedded fallback in binary

3. **Hardware Introspection**:
   - `internal/dmi`: Reads `/sys/class/dmi/id/chassis_serial` for host identification
   - `internal/blockdev`: Discovers block devices via `/sys/block`, filters out loop/ram devices
   - `internal/scsi`: Queries SCSI write cache (WCE) via SG_IO ioctl on Linux

4. **Template Rendering**: For each discovered device, renders two files:
   - `disk.fio.gotmpl` → fio job file
   - `output.yml.gotmpl` → fioplot configuration

### Key Packages

**internal/config**: Loads YAML configuration containing:
- `fio.generated.path`: Output directory for generated files
- `bbbench.wce`: Target write cache setting
- `bbbench.workloadsdb`: Named workload definitions (blocksize, duration, etc.)
- `bbbench.workloads`: Maps device types (hdd/ssd) to workload names

**internal/templating**: Go `text/template` engine with Sprig functions plus custom functions:
- `include`: Dynamic template execution (allows variable template names)
- `deref`: Dereferences pointers for optional fields like `*bool`
- `includeName`: Maps process type to include template (e.g., `trim.gotmpl`)
- `includeOutputName`: Returns default output template name

Templates are stored in `dist/templates/` with subdirectory `includes/` for reusable fragments. ParseFS names templates by **base filename only**, regardless of subdirectory.

**internal/blockdev**: Abstracts sysfs reading via `FS` interface for testability. Key fields:
- `Rotational`: Determines workload type (hdd vs ssd)
- `ParentSubsystem`: Extracted from symlink resolution (used for scsi detection)
- Filters devices without both model and serial

**internal/scsi**: Linux-only SCSI SG_IO implementation for MODE SENSE(6) caching page queries. Uses unsafe pointers and syscall for ioctl. Stub implementation exists for non-Linux builds.

**internal/dmi**: Reads DMI information from sysfs for host identification. Currently only uses `chassis_serial`.

**internal/util**: Contains `SanitizeFilename()` for making hardware identifiers filesystem-safe.

### Embedded Assets

`dist/assets.go` embeds the entire `dist/` directory (config + templates) into the binary using Go embed directives. This ensures the tool works even without external files.

### Test Strategy

- Unit tests use interface abstractions (`FS`, `Reader`) to avoid filesystem dependencies
- `internal/templating/all_templates_test.go` validates all embedded templates parse correctly
- Test files can override sysfs paths via flags like `--dmi-path` and `--sys-block`

## Development Notes

### Adding New Workload Types

1. Add workload definition to `dist/default.yml` under `bbbench.workloadsdb`
2. Map device type to workload in `bbbench.workloads` section
3. Create corresponding include template in `dist/templates/includes/` if needed

### Template Context Variables

Templates receive a context map with:
- `Manufacturer`: Sanitized vendor name
- `WorkloadsDB`: Full workload definitions from config
- `Workloads`: Filtered list of workload names for device type
- `Disk`: Map containing path, model, serial, rotational, capacity, block_count, block_size, vendor
- `Device`: Base device name (e.g., "sda")
- `Serial`: Sanitized serial number
- `Host`: Sanitized chassis serial
- `WCE`: `*bool` pointer (nil if not SCSI or query failed)

### Logging

Uses Go `log/slog`. Set level via `BBBENCH_LOG_LEVEL` environment variable (debug, info, warn, error).

### Platform-Specific Code

- `internal/scsi/wce_linux.go`: SG_IO implementation (build tag `//go:build linux`)
- `internal/scsi/wce_stub.go`: No-op stub for other platforms
- DMI and sysfs introspection are Linux-specific

### File Naming Convention

Generated files use pattern:
```
{host}_{manufacturer}_{model}_{serial}_{subsystem}_config.fio
{host}_{manufacturer}_{model}_{serial}_{subsystem}_fioplot.yml
```
All components are sanitized using `util.SanitizeFilename()`.
