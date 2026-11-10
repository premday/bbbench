# Changelog

## [Unreleased]

### Added

- Software layout setting test: compare ways of assembling the same disks (the no-array baseline, md arrays with the `mdadm` backend, logical volumes with the `lvm` backend: levels, chunk or stripe size, bitmap, spares, journal) by running the whole test plan on each layout. **It erases every disk it uses.** Disks in use (mounted, swap, bbbench or its output, stacked, with an md/ZFS/LVM/LUKS/btrfs/bcachefs signature, LVM physical volumes) are never offered and when the root or output disk cannot be attributed no disk is offered (new `internal/inuse` package); one `y` confirmation; every layout is built, waited on until its resync ends, read back and verified, benchmarked as one md/dm device (workload type from its data members) and torn down on every exit, with its members wiped. The recovery file `/var/lib/bbbench/group-recovery.json` now also records each layout before it is built, so the next start offers the teardown after a crash and refuses to start while it is declined or a member never comes back; a layout that cannot be torn down stops the group and prints the manual commands. The setting interface is generalized (`Lifetime`, `Prepare`/`Apply`/`Read`/`Undo`, `Settler`, `PerDrive`, `CommandHinter.UndoCommand`) and software layouts are backends (`settings.Backend`) that hardware RAID controllers will plug into. The group file, recovery file, status, group, results, graphs and compare pages show layouts, their members, resync progress and teardown. New optional dependencies: `mdadm`, `lvm`, `udevadm`. Not verified on hardware yet: the `mdadm --create` command line and sysfs values, the `lvcreate` command lines and `lvs`/`vgs`/`pvs` JSON fields, the LVM devices file handling, Flush tracing on md and dm devices (a phase may show 0 Flushes) and trim on arrays without discard; known gaps: an LVM devices file that `pvcreate` creates when there was none is not recorded and not removed, and udev could re-assemble md members between `mdadm --stop` and `--zero-superblock` (the wipe then fails busy: a safe teardown failure with the manual command)
- Setting tests (run groups): the orchestrator can run the whole test plan once per value of a setting; the first setting is the drive write cache policy (enabled vs disabled, NVMe `set-feature`, `hdparm -W`, `sdparm WCE`). Values are spread one after the other, in parallel on identical drives, or in parallel with swap rounds (every value measured on every drive subset). bbbench reads each drive's original value, applies and verifies the value before each run and reads it back after (a run whose value did not hold is marked invalid), and restores every drive when the group ends, also after an error or Ctrl+C; a recovery file (`/var/lib/bbbench/group-recovery.json`) lets the next start restore the drives after a crash. `group_<id>_group.json` records the runs, readings and the restore result; `--resume` continues an interrupted group after a confirmation. Web UI: a Group page and `/api/groups*`, the status page shows the group and per drive verification, and the results list, graphs, compare and run details select and overlay runs by setting value and hide invalid results. New packages `internal/settings` and `internal/rungroup`
- The rule that bbbench never changes a drive's configuration is replaced: it changes a drive's setting only in setting-test mode, after an explicit confirmation, verifies it before and after each run and always restores the original; everything else (single runs, the status page configuration, the disk text dump, Flush tracing) only reads
- README lists the files an orchestrator run writes to its output directory
- Status page and disk dump: where each disk physically sits (SES enclosure slot or SAS bay; SMBIOS system slot for NVMe, with the kernel's PCI slot table as the fallback without root; SMBIOS onboard device for SATA) and what controller it hangs off (NVMe, SATA/AHCI, SAS HBA, RAID, USB, virtio) with its name, driver, PCI address and IDs, firmware, BIOS and SAS address, flagging RAID virtual drives (new `internal/smbios` package; `lspci` is an optional dependency)
- `orchestrator` checks its external programs when it starts: a missing `fio`, or an I/O engine a selected workload needs that fio lacks (e.g. `io_uring`), stops the run with what to install; missing `smartctl`, `nvme` or `hdparm` and an unavailable tracefs are warnings saying what is lost. `doctor` runs the same checks
- Status page: the phase list shows the real time of each finished phase (`took 5m 2s`, with the expected length beside it) and the time spent so far, and the ETA adjusts as phases finish: the remaining expected lengths are scaled by how the finished phases really ran against theirs
- Status page: each drive card shows its interface (NVMe/SATA/SAS/USB), negotiated and maximum link speed, power configuration (NVMe power state and APST, runtime PM, ASPM, SATA link policy), write cache policy (kernel and drive) and SMART health (`smartctl`, or `nvme smart-log` for NVMe), read once at the start of a run (new `internal/diskinfo` package); the full information (sysfs values and the complete `smartctl -x` / `nvme id-ctrl` / `hdparm -I` output) is also written as a text file per disk, `<output>/<device>_diskinfo_<start>.txt`
- Status page: each drive lists its phases with a summary of the key fio options (`rw`, `bs`, `iodepth`, `runtime`, ...) and the fio config the phase runs; served by the new `/api/status/phases` endpoint so the 2-second status poll stays small; finished phases are struck through, each phase shows its expected length, and a per-drive ETA sums the known lengths (`time_based` jobs: `runtime` + `ramp_time`) and counts phases of unknown length, such as full-disk writes and trims, separately
- Release workflow rejects tags that are not `vMAJOR.MINOR.FIX` and tags that are not higher than all earlier tags or already have a release (`scripts/check-tag.sh`, `scripts/check-tag-order.sh`, tested in CI; `make check-tag TAG=vX.Y.Z` before tagging)
- `bbbench version` and `check-config --version` print the version, commit and build date, embedded at build time (new `internal/buildinfo` package)
- GitHub Actions CI on every pull request: gofmt, `go vet`, `go test -race`, template validation, and a build of all release archives
- Tag-triggered release workflow publishing static linux archives (`amd64`, `arm64`, `386`) with binaries, documentation, `dist/` config and templates, and `SHA256SUMS`
- `make check` (CI checks) and `make package` (release archives)
- Flush tracing: phases that fsync also report the device's own Flush latency, measured from kernel trace events (NVMe tracepoints with exact pairing, generic block tracepoints for any other device), saved per phase; `--no-flush-trace` disables it. The drive's `write_cache` state is recorded, and a write-through drive, which the kernel never sends a Flush, is reported as such ("ftrace saw 0 Flush commands") instead of "no data"
- The Flush latency is stored next to the fio logs in fio's shape (`<prefix>_flush.1.log`): one line per `--log-avg-msec` window plus the whole-phase summary in the header, a few hundred lines (about 10 KB) per phase instead of one line per Flush; the summary and the series (`flush_series`, `ts_flush`) are read back from it when a run is loaded
- Flush-latency workloads `ssd_fsync_psync_qd1`, `ssd_fsync_uring_qd1` and `ssd_fsync_uring_qd32` (4k randwrite with fsync after every write, 5 minutes each), added to the default `ssd` list
- Run summary shows the mean fsync latency for phases that issue fsyncs
- Workloads can override `ioengine`, `iodepth`, `direct` and `fsync` per job in `bbbench.workloadsdb`
- Release archives ship a `VERSION` file (version, commit, date, arch) and a generated `RELEASE_NOTES.md`; the GitHub release notes are generated from the commits since the previous tag (`scripts/changelog.sh`)
- Web UI: "Flush latency" graph type (graphs and compare pages) drawn from the stored log and averaged over fio's polling interval (`--log-avg-msec`), so it lines up with the fio series; every graph type shows a `fio` / `ftrace` source tag
- Docs and the write-through warning state that a plain run only reads the write cache mode and the user applies any fix (since changed: see the setting-test entry, a setting test can change it)
- `documentation/flush-latency.md` explains what the Flush latency measures, with a diagram of the kernel stack
- Phases that fsync on a write-through device (the kernel sends no Flush) still run, with a warning on the console, the status page and the run details (`warnings` in the run JSON); `--skip-write-through` skips them instead, with the reason shown in the web UI
- With `--skip-write-through`, a drive that sits out a phase while others run it is shown as "Skipped this phase" with the reason on the status page, and the console names it as soon as the phase starts; a parallel phase that ran elsewhere ends "COMPLETE, skipped on N drive(s)"
- All time-series graphs share one x axis starting at 0 (the start of the phase) and spanning the longest series shown; fio's ramp-up (`ramp_time`, saved as `ramp_sec`) is shaded and fio's points start after it. Points are drawn at the start of their averaging window (fio stamps the end, so its points are moved back one window and there is no gap after the ramp-up); the Flush series has no points and no summary contribution for the ramp-up, like fio (`offset_ms` in the flush log header)

### Changed

- The orchestrator drops the template's `log_avg_msec=30000` from the fio files it runs, so `--log-avg-msec` (default 1 s) is the only polling interval. The file's value could override the flag, giving 30 s points and a 30 s blank after the ramp-up on the graphs; hand-run fio on a generated file keeps the 30 s
- CI workflows use Node 24 action versions (`checkout@v7`, `setup-go@v7`, `upload-artifact@v7`) and run on `ubuntu-26.04` instead of `ubuntu-latest`
- `make` builds with `CGO_ENABLED=0` instead of musl-gcc: the code is pure Go, so binaries stay static without a C toolchain
- Generated fio log names and the fioplot `iodepth` reflect a workload's effective iodepth and `direct`/`buffered` mode (unchanged for existing workloads)

### Fixed

- fio log points stamped a millisecond early (9999 for 10 s) no longer fall into the previous 1 s bucket, which merged two windows and left a gap after them

## [0.1.0] - 2026-03-31

This is the first usable release.

### Added

- Orchestrator command for benchmarking multiple drives sequentially or in parallel
- Drive selection TUI with keyboard shortcuts (space to toggle, `a`/`n` for all/none)
- Resume capability for interrupted benchmark runs
- Interrupt handling with graceful shutdown
- Dry-run mode for the orchestrator
- Drive filtering options (`--include`, `--exclude`)
- Verbose mode for detailed execution output
- Web server GUI (`bbbench serve`) with live status, results browsing, and graph visualization
- Auto-refreshing status page during benchmark execution
- Persistent benchmark results with JSON summary files
- fio JSON parsing and per-job metric extraction (IOPS, bandwidth, latency)
- Time-series log parsing with configurable coalesce threshold
- Incomplete-point detection when fewer threads than expected contribute to a sample
- Graph view (`/graphs`) with sidebar filters, foldable sections, chart cards, click-to-maximize
- Compare view (`/compare`) to overlay multiple disks and/or phases on a single chart
- Summary page with aggregated per-disk metrics across phases
- HTML export functionality for sharing benchmark results offline
- `fulldisk: true` in fio job files emits `size=100%`; recorded in fioplot output
- Apache 2.0 license

### Changed

- Removed Criteo-specific naming; codebase is now generic
- Rewrote README to be usage-oriented
- Removed unused YAML config entries (`fioplot` theme/x/y settings, `bbbench.default.template*`)
- Removed `BBBenchConfig.Default` struct (fields were never accessed)
- Trim and blkdiscard phases are hidden by default in both graph views
- Y-axis scaling: baseline at 0 for read-only phases, ceiling at 0 for write-only phases, bidirectional when both read and write data are present

### Fixed

- fio JSON parse failures caused by large-device warning preamble in output
- IOPS aggregation across multiple jobs
- Drive status display (running / complete / pending states, `Device` field population)
- `wait_for` errors across phase boundaries in generated fio job files
- Template rendering bug that caused the orchestrator to find zero jobs

## [0.0.0-moab] - initial

Project skeleton from Go template.
