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
- `.github/workflows/ci.yml` (PRs and main) must pass before merge: `test` and `package` jobs. `release.yml` publishes on `v*` tags, and rejects (first step, `scripts/check-tag.sh`) any that is not `vMAJOR.MINOR.FIX`; also `scripts/check-tag-order.sh` (strictly higher than all other release tags, numeric compare) and an existing-release check; the rules are table-tested by `scripts/check-tag_test.sh` and `scripts/check-tag-order_test.sh`. Runners are pinned to `ubuntu-26.04` (not `ubuntu-latest`, so the image only changes when we choose); move to a newer image deliberately. Adding a new architecture means adding it to `scripts/package-all.sh` and the README/CHANGELOG lists.
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

**internal/smbios**: Parses the DMI table (`/sys/firmware/dmi/tables/DMI`, root-only) for system slots (type 9: designation, type, usage, slot ID, PCI address) and onboard devices (type 41). Tested on hand-built structures (no real table was available): validate a new BIOS with `dmidecode -t 9 -t 41`.

**internal/diskinfo**: Read-only description of a disk for the status page (`Collect`): interface and link speed (sysfs: PCIe `current/max_link_speed/width`, `ata_link`, `sas_phy`, USB speed), power (runtime PM, ASPM, NVMe power state/APST via `nvme get-feature`, SATA link policy), write cache (kernel `queue/write_cache`, drive via `nvme get-feature 6` or `scsi.GetWCE`) and SMART (`smartctl -j`, else `nvme smart-log -o json`). `Options` replaces sysfs, `/dev` and command execution for tests. `CollectDump` also returns a text dump (summary, sysfs values read, full output of every command incl. `smartctl -x`, `nvme id-ctrl/id-ns/smart-log`, `hdparm -I`); the orchestrator runs it in the background per drive (`collectDiskConfig`, `orchestrator_diskinfo.go`) into `DriveStatus.Config` and writes the dump to `<output>/<device>_diskinfo_<start>.txt`. `Collect`/`CollectDump` never change the device (a setting test changes drives only through `internal/settings`). `InterfaceOf(name, opts)` and `DriveFirmware(name, opts)` read the interface class and the drive's firmware revision from sysfs only (no command runs); the setup TUI and the parallel-swap firmware warning use them. `Slot` and `Controller` (`topology.go`): the PCI chain of the resolved sysfs path gives the controller (deepest PCI device of class 01xx: driver, IDs, `lspci -mm` name, scsi_host firmware/BIOS/SAS address, RAID virtual drive vs passthrough guessed from the driver and the disk model); the slot comes from, in order, SES enclosure (`/sys/class/enclosure/*/*/device`), SAS bay (`/sys/class/sas_device`), SMBIOS type 9 matched on the chain (device, then ancestors, then another function), `/sys/bus/pci/slots`, SMBIOS type 41. The SMBIOS source is `Options.SMBIOS`.

**internal/settings**: the things a setting test can vary. `Setting` interface: `Name`, `Kind` (`Applied` = bbbench changes it, `Manual` = the user between groups), `Scope` (`ScopeDrive`, `ScopeLayout`), `Input` (`InputValues`: tick values over one drive selection; `InputVariants`: list variants each with its own disks, used by the layout setting), `Lifetime` (`LifetimeGroup`: prepared once for the group and undone when it ends, drive values; `LifetimeRun`: prepared before each run and undone right after it, layouts), `Values`, `Applicable(Target)` / `ApplicableVariant` (bool + reason), `Prepare(Variant)` (reads what `Apply` will change and returns the `[]Undo` that puts it back; changes nothing), `Apply(Variant, prepared)` returning the `[]BenchTarget` to benchmark, `Read(BenchTarget)`, `Undo(Undo)` (writes only when something differs, then checks). `Undo` is a drive's original `Reading` or an `ArrayUndo` (backend, name, uuid, members, `Extra` that only the backend reads). `Settler.WaitSteady` waits until the targets are steady (an array resyncing) before a run. `CommandHinter.UndoCommand` gives the command to put a record back by hand when an undo fails. `Target` is the drive (name, path, serial, model, vendor, interface), `Reading` the value (+ informational kernel value, time), `Verify` re-reads every 200 ms until the value matches or the settle time ends. `Env` replaces exec, LookPath, sysfs and `scsi.GetWCE` for tests. **To add a setting**: for a drive-scope setting implement `DriveOps` (`Read`, `Apply`, `Restore`, optional `DriveCommandHinter.RestoreCommand`) and wrap it with `PerDrive` (`perdrive.go`); for anything else implement `Setting` (and `CommandHinter`, `Settler`); register it in `settings.All`; the mode screen, setup TUI, group runner and web pages work from the interface, nothing else needs a case for it. `writecache.go` is the drive's own volatile cache (`nvme set-feature -f 6`, `hdparm -W`, `sdparm WCE`, `scsi.GetWCE`), a `DriveOps`. The software layout setting is `layout.go` (`NewLayout(env, backends...)`, a `LayoutSetting`: `Backends`, `ValidateLayout`, `LayoutValue`, `ParseLayout`; scope layout, input variants, lifetime run) over `Backend` (`backend.go`): `Name`, `Available`, `Levels`, `Roles`, `Options`, `Validate`, `Label`, `Parse`, `Prepare`, `Apply`, `WaitSteady`, `Read`, `Undo`, `UndoCommand`, with the safety contract in its doc comment (`Prepare` changes nothing and returns the record `Apply`/`Undo` work from; `Undo` touches only what it can prove it built). Backends: `mdadm.go` (levels raid0/1/5/6/10/linear, chunk, bitmap, spares, journal) and `lvm.go` (linear, striped, mirror, raid1/5/6/10, stripe count and size; the LVM devices file). **To add a layout backend**: implement `settings.Backend` and pass it to `NewLayout` in `All`. `All` lists the write cache and the layout setting with mdadm and LVM.

**internal/inuse**: which disks must not be erased (`Scan(Options)` returns a `Report`: `Busy` disk -> reasons, `All` when no disk can be offered, `Warnings`, `Carries` disk -> what a free disk still carries, e.g. `gpt partition table, ext4 on sdb1`, from udev `ID_PART_TABLE_TYPE`/`ID_FS_TYPE`; shown as "carries data" on the layout screens, never blocking). Reads only. Mounted, swap, the bbbench binary and the output directory (`Options.Paths`), holders (md, dm, stacked), md/ZFS/LUKS/btrfs/bcachefs/LVM/Ceph BlueStore/DRBD/VMFS signatures from udev (missing udev data = not seen), and LVM PVs (`Options.PVs`, from `lvm pvs`). A mount or path that cannot be attributed to a disk (tmpfs, NFS, overlay) or an undetermined root disk sets `All`.

**internal/rungroup**: pure data and logic of run groups. `Label` (setting, value, before/after `Reading`, `Valid`, `Note`) is stored with each drive's result in the run JSON. `PlanRuns`/`ParallelPossible`/`subsets` build `[]RunPlan` (`Round`, `[]Variant{Value, Drives}`) for the strategies `sequential`, `parallel`, `parallel-swap` (identical drives = same vendor, model, capacity are split evenly). `Policy(before, after)` turns the readings into a `RunStatus` and the set of invalid drives. `GroupFile` is `<output>/group_<id>_group.json` (id, created, host, plan, setting, strategy, values, originals, runs[run_id, round, status, drives[device, serial, model, value, label]], restore{status pending|done|failed, failed[]}), written atomically after every step and loaded by `LoadGroup(s)`. `Recovery` is `/var/lib/bbbench/group-recovery.json` (`groupRecoveryPath` in `group_flow.go`): setting and, per drive, serial, model, name and original `Reading`; written before the first change, removed once everything is restored; `Match` pairs it with the present drives.

**Run groups in cmd/bbbench**: `executeRun(ctx, runOptions, runSpec)` (`orchestrator.go`) runs the whole plan once on `runSpec.Drives` (the orchestrator calls it for a single run, `runGroup` once per `RunPlan`); `runOptions.OnStarted` is called once the live result is registered so the web server starts after the run is visible. `runGroup(ctx, groupConfig)` (`group_run.go`) reads and saves the originals (recovery file), then per run applies the values and verifies them, calls `Execute`, verifies again, labels the result (`saveRunLabels`), applies `Policy`, and finally restores (`finishGroup`, also on error and Ctrl+C; `restoreFromRecovery` does it from the recovery file after a crash, offered by `offerRecovery` at the next orchestrator start, which also refuses a setting test while the file exists). `group_flow.go` has the resume logic (`findResumableGroup`, `resumesGroup`, `prepareGroupResume` asking for confirmation, `resumeChoice`). `group_undo.go` is the undo ledger (`undoLedger`, `undoOne`, `reportUndoFailure` printing the manual command, `restoreFromRecovery`) used by the group loop and the recovery offer; `group_targets.go` is `arrayDrive` (a composite `BenchTarget` becomes the `DriveInfo` a run benchmarks: workload type from the data members, fio file from `fioFileName`, size from its sysfs entry); `group_layout_tui.go` is the layout screens (layout list, backend picker, editor, confirmation; grayed disks from `inuse`). A layout group (`LifetimeRun`) runs `runLayoutOne` per layout: re-identify every disk (`reidentify` through `groupConfig.Identify` = `blockdev.Lookup`: same serial, model, capacity as confirmed, else the run is `not_run` and nothing is built; `checkLayoutDisks` also requires each layout disk's serial, model and path to be its drive's), prepare, record in the recovery file before the build, apply, `WaitSteady`, benchmark, verify, undo; a failed teardown stops the group; `offerLayoutCleanup` offers the teardown of recorded layouts at the next start and refuses to start when declined. The setup TUI is `group_tui.go`: `runModeTUI` (single run vs setting, with applicable drive counts and reasons) and `runGroupSetupTUI` (values, strategy, warnings, the confirmation screen; nothing is changed before it). Web: `serve_groups.go` serves `/group?id=`, `/api/groups`, `/api/groups/detail`, `/api/groups/graphs`; results, graphs, compare, summary and status pages read the labels (`setting_value`, `invalid`, `round`); the status page shows the group banner and per-drive before/after badges.

**External programs** (`cmd/bbbench/deps.go`): `fio` (required), `smartctl`, `nvme`, `hdparm` (optional, for `internal/diskinfo`), `sdparm`, `lspci`, `mdadm`, `lvm`, `udevadm` (optional, for the layout setting: the mdadm backend, the LVM backend and the PV listing, waiting for udev after a teardown). `checkDependencies`/`printDependencies` run at orchestrator start (before the TUI) and in `doctor`; `ec.checkEnvironment` (after the workloads are parsed) checks that this fio has every `ioengine` the workloads use (`fio --enghelp`) and warns when tracefs is missing for fsync phases. A new external program must be added to `dependencies`.

**internal/dmi**: Reads DMI information from sysfs for host identification. Currently only uses `chassis_serial`.

**internal/flushtrace**: Times device Flush commands from kernel trace events (tracefs), isolating the Flush from the rest of an fsync() that fio measures.
- `ParseLine` parses ftrace text lines into `Event`s.
- `NVMe` collector: `nvme_setup_cmd`/`nvme_complete_rq`, paired exactly by (controller, queue, cmdid); reports mean, min, max, p50, p99.
- `Start`/`Session.Stop`: runs a collector against a private tracefs instance (`instances/bbbench-flush-*`), so global tracing is untouched and sessions can run in parallel. NVMe namespaces use the NVMe tracepoints when present, else the block ones. Needs root and tracefs; returns `ErrUnsupported` otherwise. `executePhase` starts a session for phases whose fio jobs set `fsync=N` (`phaseHasFsync`) unless `--no-flush-trace`; the `Summary` lands in `FioResult.FlushTrace` and `PhaseResult.FlushTrace`; the per-window log is stored as `<fio log prefix>_flush.1.log` (`cmd/bbbench/flushlog.go`), and loading a run (`loadBenchmarkSummary`, `updateLivePhase`) reads `FlushTrace`/`FlushSeries` back from that file. Rationale: fio only times the whole fsync() call, so the device's own Flush time is taken from kernel trace events instead (see README, "Flush tracing"). Tests use a fake tracefs tree (`Options.TracefsRoot`, `SysBlock`).
- Chart code shared by the graphs and compare pages (time axis: `rampOf`, `windowOf`, `fioShift`, the ramp-up shading plugin, `fmtTick`) lives once in `cmd/bbbench/serve_shared.go` (`chartSharedJS`, spliced in at `__CHART_SHARED_JS__`; no backticks in it).
- bbbench changes a drive's setting only in setting-test mode, after an explicit confirmation on the confirmation screen, verifies it before and after each run, and always restores the original (a recovery file at `/var/lib/bbbench/group-recovery.json` covers a crash); everything else it only reads (single runs, the status page configuration, the per-disk text dump, Flush tracing, `queue/write_cache`). Warnings and docs must say so, and that outside a setting test the user applies fixes themselves. A layout test is the exception that erases: its disks are wiped (data not kept), in-use disks (mounted, swap, bbbench or its output, stacked, with an md/ZFS/LVM/LUKS/btrfs/bcachefs signature, LVM PVs included) are never offered, there is one confirmation (`y`, nothing built before it), the layout is torn down on every exit, and the recovery file records each layout before it is built. Any new code that writes to a drive or the kernel belongs in `internal/settings` and must only run from a run group after the confirmation.
- Write-through rule (`cmd/bbbench/orchestrator_skip.go`): a phase with `fsync=N` on a device whose `queue/write_cache` is `write through` (`flushtrace.WriteCache`) still runs, with `FioResult.Warning` -> `PhaseResult.Warnings` (device -> text) and `DriveStatus.Warning`; with `--skip-write-through` `executePhase` refuses it instead: `FioResult.Skipped` -> `PhaseResult.Skipped` -> `DiskPhaseData.Skipped`, not marked completed in the state file.
- Flush log (`log.go`): `WriteLog`/`ReadLog` store a phase as `<fio log prefix>_flush.1.log`, in fio's shape: one line per `--log-avg-msec` window stamped at the window END (`end_ms, mean_ns, 3, 0, 0, count, min_ns, max_ns`; the first five columns are fio's), plus `#` header lines with the metadata (`source`, `exact`, `lost`, `write_cache`, `avg_msec`, `offset_ms`) and the whole-phase summary. The summary is built once at stop by `Finish` (`Summarize` over the Flushes after the ramp, `offset_ms` = fio's `ramp_time`), from the raw `Sample`s, which are not stored (exact percentiles cannot be rebuilt from windows); `Windows` aggregates them. `loadFlushLog` turns window ends into series points stamped at window START (windows that ended inside the ramp-up are left out, like fio's own logs) (the graph JS moves fio's end-of-window points back by one window; fio's own series start when its ramp ends, so the JS shifts them by `ramp_sec` and draws the ramp-up zone; all time-series charts share one x axis 0..max). The old per-Flush format (`_flush.log`) is not read. `Session.Samples()` gives times relative to the start marker.
- `Block` collector (any block device): `block_rq_issue`/`block_rq_complete` for requests whose rwbs starts with `F` and cover 0 sectors, paired first-in first-out. The mean is exact; percentiles are not reported because pairing is not exact under concurrent Flushes.

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

A workload can optionally override the template's `[global]` fio settings
(libaio, iodepth 128, direct I/O, no fsync) for its own job:

- `ioengine`: fio engine, e.g. `psync`, `io_uring`
- `iodepth`: queue depth
- `direct`: `true`/`false` (`false` = buffered I/O)
- `fsync`: issue an fsync after every N writes (`1` = after each write)

Unset fields emit nothing, so existing workloads render exactly as before.
Generated log file names and the fioplot `iodepth` use the effective values
(`iodepth<N>_direct` or `iodepth<N>_buffered`).

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

### What the hardware-RAID branch plugs into

The next branch adds hardware RAID controllers (`perccli`/`storcli`, `ssacli`) to the same setting. The `settings.Backend` methods are enough for the virtual drive itself; several core pieces assume Linux block-device members and must change in that branch.

What already fits:

- A controller backend implements `settings.Backend` (`Name`, `Available`, `Levels`, `Roles`, `Options`, `Validate`, `Label`, `Parse`, `Prepare`, `Apply`, `WaitSteady`, `Read`, `Undo`, `UndoCommand`) and is passed to `NewLayout` in `All`. Its levels are the controller's RAID levels and JBOD/passthrough, its `Options` the controller's (stripe size, read and write cache policy), its `Roles` data and hot spare.
- `Apply` creates the virtual drive and returns it as `BenchTarget{Kind: TargetArray}` with the physical disks as `Members` (found from `diskinfo.Controller` and the vendor tool, run through `Env.Run`); `WaitSteady` waits for its initialization; `Read` verifies it through the vendor tool; `Undo` deletes it. The controller and virtual-drive ids go in `ArrayUndo.Extra`, which the recovery file stores as it is (`Prepare` must already put everything `Undo` needs in the record: it is saved before `Apply`).
- The group loop, the undo ledger, the recovery offer, the layout TUI and the pages never name a backend and never branch on one.

What assumes Linux block-device members and must change:

- Disk candidates: the layout screens offer only the discovered `/sys/block` drives. In RAID mode a controller's physical disks are not in `/sys/block`; this needs something like `Backend.Candidates` or a `Disk` with an empty `Path`.
- `layout.Prepare` runs the `inuse` check by kernel name for every backend. A controller needs a per-backend hook for its own view (disk already a member of a virtual drive, foreign configuration).
- `checkLayoutDisks` (and `reidentify` before each build) require every member to be one of `cfg.Drives`, by kernel name, serial, model, path and capacity.
- `Recovery.Undos` resolves members only against block `Target`s by serial and model.
- `arrayDrive` requires each member in the discovered drives map and derives HDD/SSD from it; a backend must be able to supply the media type.
- `maxArrayName = 32` and `ArrayName` are a core constant; PERC virtual-drive names are about 15 characters, so the name rule must become the backend's.
- A controller mode the user sets in the BIOS (RAID vs HBA) is a separate `Manual` setting and needs the multi-invocation groups of the first spec, which `runGroup` refuses today.
- `TestCoreDoesNotNameTheBackend` (`group_targets_test.go`) keeps the backend names out of the core (`mdadm|lvm|pvcreate|vgcreate|lvcreate`); the controller tool names (`perccli`, `storcli`, `ssacli`) must be added to it.
