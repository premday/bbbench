# bbbench

Block-device benchmark tool. Discovers drives, generates [fio](https://github.com/axboe/fio) job files from templates, runs benchmarks across multiple drives in parallel, and serves results via a web UI.

## Requirements

- Linux
- Root privileges for `generate`, `doctor`, and `orchestrator`
- `fio` installed for `orchestrator`, built with the I/O engines of the workloads you run (the default flush workloads use `io_uring`, which needs an fio built with liburing)
- Optional, for the disk configuration on the status page and in the text dump: `lspci` (pciutils) for the storage controller's name, `smartctl` (smartmontools) for SMART health, `nvme` (nvme-cli) for NVMe power state, APST, the drive's write cache and a SMART fallback, `hdparm` for the SATA identify report. For a layout setting test: `mdadm` (the mdadm backend), `lvm` (lvm2: the LVM backend, and the list of LVM physical volumes so that they are never offered) and `udevadm` (waiting for udev after a teardown). For Flush tracing, tracefs mounted (`/sys/kernel/tracing`)

`bbbench orchestrator` checks these when it starts, before anything interactive: a missing `fio`, or an I/O engine a selected workload needs that this fio lacks, stops the run with what to install; a missing optional program is a warning saying what is lost; an unavailable tracefs is a warning that the fsync phases will report fio's fsync latency only. `bbbench doctor` runs the same program checks.

## Build

```bash
make clean && make
```

Produces statically linked `./bbbench` and `./check-config` binaries. The code is
pure Go, so no C toolchain is needed (`CGO_ENABLED=0`). `bbbench version` and
`check-config --version` print the version, commit and build date (version from
`git describe` via the Makefile, `dev` outside a git checkout).

Developers do not need `make` or any version setup: `go build ./cmd/bbbench`,
`go run ./cmd/bbbench ...` and `go test ./...` all work as usual and report
`dev`. Use `make` only when you want a stamped static binary.

### Release archives

Every tagged release (`vX.Y.Z`) publishes static linux archives on the GitHub
releases page, for `amd64`, `arm64` and `386`, with a `SHA256SUMS` file:

```
bbbench_<version>_linux_<arch>.tar.gz
  bbbench, check-config      static binaries
  dist/                      default.yml and templates (found from ./dist)
  documentation/             web UI documentation and images
  VERSION                    version, commit, build date, arch
  RELEASE_NOTES.md           changes since the previous tag (generated)
  README.md LICENSE CHANGELOG.md
```

Build them locally with `make package` (archives land in `./build`).
To cut a release: `git tag vX.Y.Z && git push origin vX.Y.Z`. The release workflow
builds the archives and publishes the GitHub release, whose notes are generated
from the commit subjects since the previous tag (`scripts/changelog.sh`), so
write commit subjects as you want them read. `CHANGELOG.md` stays the curated,
human-written history.
Tags must be exactly `v<major>.<minor>.<fix>` (e.g. `v1.2.3`; numbers without
leading zeros, no `-rc1` style suffix). The release workflow fails on any other
tag before building or publishing. It also rejects a tag that is not strictly
higher than every earlier release tag (numeric comparison: `v0.10.0` > `v0.9.0`)
and a tag that already has a GitHub release. Before tagging, run
`make check-tag TAG=v1.2.3`, which also fails if the tag already exists.

### Continuous integration

Every pull request runs `.github/workflows/ci.yml`: gofmt, `go mod tidy` check,
`go vet`, `go test -race ./...`, template validation, then builds and
smoke-tests the release archives for all architectures. Run the same checks
locally with `make check`. Both jobs (`test`, `package`) are meant to be
required status checks on `main`.

---

## Typical workflow

```
1. bbbench generate      → create fio job files for discovered drives
2. bbbench orchestrator  → select drives, run benchmarks, save results
3. bbbench serve         → browse results in a web UI
```

---

## Commands

### `bbbench generate`

Discovers block devices and generates fio job files using templates and host hardware info.

```bash
sudo ./bbbench generate

# Custom config and output location
sudo ./bbbench generate --config /path/to/my.yml --out ./jobs
```

Flags:
- `--config` — path to config file (uses search order if omitted, see below)
- `--templates` — path to templates directory (uses search order if omitted)
- `--out` — output directory for generated fio files (overrides config)
- `--dist` — override the dist path (default `./dist`, or `BBBENCH_DIST_DIR`)

---

### `bbbench orchestrator`

Runs fio benchmarks. Presents a TUI to select drives, then executes the benchmark phases and saves results as JSON.

**Single run or setting test.** The first question is what to do:

- *Single run* is the flow described here: select drives, each gets the plan for its type, everything is tested at once. bbbench only reads the drives.
- *Setting test* runs the whole test plan once per value of one setting (the settings are the drive write cache policy, enabled vs disabled, and the storage layout of a set of disks, see [Layout setting test (mdadm, LVM)](#layout-setting-test-mdadm-lvm)), to measure what the setting costs or gains. The settings are listed with the number of discovered drives each applies to and, for the others, the reason (for example no controllable write cache on USB). A setting test asks, in order: the drives (the same selection screen), the values to test (both start ticked; at least two), and how to spread them:
  - *one value after the other* (default): every drive gets the same value in a run, one run per value;
  - *parallel*: identical drives (same vendor, model and capacity) are divided evenly among the values and all values run at the same time in one run. Offered only when every class of identical drives divides evenly; otherwise the screen says why;
  - *parallel with swap rounds*: parallel, then the values rotate over the drive subsets and the plan runs again, so each value is measured on every subset (cancels drive-to-drive variance). Never preselected.

  Drives of one model with different firmware get a warning (parallel stays offered). Drives the setting does not apply to are left out and listed with the reason. The last screen shows what will change on which drives, the number of runs and an estimated time; **nothing is changed on a drive before you confirm it** there. The storage layout setting is laid out per variant instead of per value: it asks for the layouts with their own disks, see [Layout setting test (mdadm, LVM)](#layout-setting-test-mdadm-lvm), and **erases** those disks.

During a setting test bbbench reads the value of every drive before changing it, applies and verifies it before each run and again after it (when the check before a run fails, that run is *not run* and the group goes on with the next one; a run whose value did not hold until after it is marked invalid; an interrupted run is kept, marked interrupted and left out of comparisons), and puts every drive back as it found it when the group ends, also after an error, Ctrl+C, `kill` (SIGTERM) or a closed terminal (SIGHUP). The original values are kept in `/var/lib/bbbench/group-recovery.json` while the group runs. If bbbench is killed before restoring (`kill -9`, a crash, a power cut), the setting under test can stay applied on the drives until the next start of `orchestrator`, which finds that file and offers to restore the drives; if a restore fails it names the drive and prints the command to set it back by hand, and keeps the file. A setting test refuses to start while the file exists. bbbench changes a drive's setting only in setting-test mode, after the explicit confirmation on the last screen; it verifies the value before and after each run and always restores the original (the recovery file covers a crash). The storage layout test is the one that erases its disks and does not restore their data. Everything else it only reads. `--dry-run` cannot be combined with a setting test.

`--resume` continues an interrupted setting test when the output directory holds a group (`group_<id>_group.json`) with runs that are not finished and the per-run state file is tagged with that group (or absent). An untagged state file is a single run's: `--resume` then resumes that single run as before and names the unfinished group file. Resuming a group asks for confirmation first (it shows the setting, values, drives and runs left, and that the originals are restored), then continues with the same setting, values, strategy and drives (matched by serial and model), skipping the other questions; `n` stops with nothing changed. Finished runs (done, invalid, and not run) are kept as they are; the others (planned or interrupted) start again from their first phase. A group whose only open runs were not run is finished, so `--resume` does not offer it; start a new setting test to try those values again. If the group cannot be resumed (a recorded drive is gone), bbbench names the group file; removing or renaming it unblocks `--resume`.

```bash
sudo ./bbbench orchestrator

# Sequential mode, custom output dir
sudo ./bbbench orchestrator --mode sequential --output ~/benchmark-results
```

**Execution modes:**
- `parallel-sync` *(default)* — all selected drives run phase 1 together, then phase 2, etc.
- `sequential` — complete all phases on drive 1, then drive 2, etc.

**Drive selection TUI keys:**
- `Space` — toggle current drive
- `a` — select all drives
- `n` — unselect all drives
- `i` — invert selection
- `Enter` — confirm and start benchmarking
- `q` — quit

**Flags:**
- `--mode` — `parallel-sync` or `sequential`
- `--output` — output directory for JSON results (default: from config)
- `--resume` — resume a previously interrupted run (skips completed phases)
- `--dry-run` — show what would run without executing fio
- `--verbose` — show fio commands and debug output
- `--skip-write-through` — do not run fsync (Flush-latency) phases on write-through devices (by default they run, with a warning)
- `--log-avg-msec` — fio's polling interval in ms (default 1000): one point per interval on every graph, and the first point of a series appears one interval after the ramp-up ends. It overrides the `log_avg_msec` of the generated job files
- `--dmi-path`, `--sys-block` — override `/sys/class/dmi/id` and `/sys/block` (for tests)
- `--no-flush-trace` — do not trace device Flush latency during fsync phases (see [Flush tracing](#flush-tracing))
- `--config` — path to config file
- `--dist` — override dist path

**Drive filters:**
- `--filter-type` — `hdd` or `ssd`
- `--filter-vendor` — case-insensitive substring match on vendor name
- `--filter-model` — case-insensitive substring match on model name
- `--filter-min-capacity` — minimum capacity in GB
- `--filter-max-capacity` — maximum capacity in GB (0 = no limit)

```bash
# Only SSDs
sudo ./bbbench orchestrator --filter-type ssd

# Samsung drives ≥ 1 TB
sudo ./bbbench orchestrator --filter-vendor samsung --filter-min-capacity 1000
```

**What a run writes** to the output directory (`--output`), for each drive `<device>` and phase `<N>` (counted from 0, so the first phase is `phase0`), with `<ts>` the Unix time the phase started:

| File | Content |
|---|---|
| `<device>_phase<N>_<ts>.json` | fio's JSON output for the phase |
| `<device>_phase<N>_<ts>_log_{iops,bw,lat,clat,slat}.1.log` | fio's time-series logs, one point per `--log-avg-msec` |
| `<device>_phase<N>_<ts>_log_flush.1.log` | the device Flush latency, for phases that fsync (see [Flush tracing](#flush-tracing)) |
| `<device>_phase<N>_<ts>_log_members.1.log` | an array target (md or LVM layout) only: the kernel's I/O counters of each member disk, one line per member and `--log-avg-msec` window (see [Layout setting test](#layout-setting-test-mdadm-lvm)) |
| `<device>_diskinfo_<start>.txt` | the drive's configuration (interface, link, power, write cache, SMART) and the raw reports it is drawn from, one per run |
| `run_<unix time>_summary.json` | the run's summary, which `bbbench serve` lists and graphs; for an array phase it also holds the members' series (`member_series`) and the md state (`md_series`), N members x 3 series, like `flush_series` |
| `group_<id>_group.json` | a setting test: its runs, the value each drive had, the readings before and after each run, and the restore result |
| `<host>/{host}_md_{level}_{uuid}_md_config.fio` and `<host>/{host}_lvm_{type}_{lv uuid}_lvm_config.fio` | a layout setting test: the generated fio file of an md array or of an LV (under `fio.generated.path`, like the files of `generate`), named after the array's level and uuid |
| `.bbbench-orchestrator-state.json` | what is done, for `--resume`; removed when the run completes |

The original drive values of a running setting test, and the layouts it has built, are not in the output directory: they are in `/var/lib/bbbench/group-recovery.json`, removed when the group has restored everything.

Results are written to `~/.bbbench/output/` by default (one JSON file per run).

---

### `bbbench serve`

Starts a web server to browse, graph, and export benchmark results.

```bash
./bbbench serve

# Custom address and results directory
./bbbench serve --addr 127.0.0.1:8080 --output ~/benchmark-results
```

Flags:
- `--addr` — bind address (default `0.0.0.0:12345`)
- `--output` — directory containing benchmark result JSON files (default `~/.bbbench/output`)

Pages available:
- `/` — home
- `/status` — live benchmark status while the orchestrator runs: progress, and per drive its configuration (interface, physical slot, controller, link speed, power, write cache policy, SMART), and its phases with the fio config, real and expected time and an ETA (details in [documentation/web-ui.md](documentation/web-ui.md))
- `/results` — browse and search past runs
- `/summary?id=<id>` — statistics table (min/avg/max/σ) per phase, broken down by disk
- `/graphs?id=<id>` — time-series charts per phase: IOPS, bandwidth and latency (from fio) and Flush latency (from kernel traces), on one shared time axis with the ramp-up shaded; for an array phase, IOPS and bandwidth are stacked per member and latency has one line per member
- `/compare?id=<id>` — overlay several disks and/or phases on one chart
- `/result?id=<id>` — details of one run: drives, phases, and any skipped phase or warning (e.g. a write-through drive)
- `/group?id=<group id>` — a setting test: its runs by drive and value, the verification, the restore result and the metrics per value; `/graphs?group=<group id>` and `/compare?group=<group id>` show its runs together, by setting value
- `/export?id=<id>` — download self-contained HTML report
- `/health`, `/api/status`, `/api/status/phases`, `/api/results`, `/api/results/detail`, `/api/results/graphs`, `/api/results/summary`, `/api/groups`, `/api/groups/detail`, `/api/groups/graphs` — JSON behind the pages

The web UI is also started automatically by `orchestrator` on `0.0.0.0:12345` during a run.

---

### `bbbench doctor`

Checks host readiness: Linux, root access, sysfs, block device discovery, and the external programs (`fio` is required; `smartctl`, `nvme` and `hdparm` are optional and a warning says what is lost without them).

```bash
sudo ./bbbench doctor
```

---

### `bbbench validate-templates`

Validates template syntax without generating anything. Useful when editing templates.

```bash
./bbbench validate-templates
./bbbench validate-templates --templates ./dist/templates
```

---

### `bbbench completion`

Generates shell completion scripts.

```bash
# Bash
./bbbench completion bash | sudo tee /etc/bash_completion.d/bbbench

# Zsh
./bbbench completion zsh > ~/.zsh/completions/_bbbench

# Fish
./bbbench completion fish > ~/.config/fish/completions/bbbench.fish
```

---

## Layout setting test (mdadm, LVM)

**A layout setting test ERASES every disk it uses: the layouts are built on them and the benchmark writes to them. Their data is not kept and not restored; after the test they carry no array, no md superblock and no LVM label.**

The *storage layout* setting compares ways of assembling the same disks (for example a single disk, a RAID 1, a RAID 10, an LVM stripe) by running the whole test plan on each layout. Choose *Setting test*, then *storage layout*. It is the only setting that has its own screens and it needs root. bbbench changes disks only here, after the single confirmation described below; every other mode only reads them.

**Providers.** A provider builds layouts with one tool. When there are several, the screen asks which one builds the new layout; one that is not installed is listed with the reason and cannot be picked.

- `mdadm` (md arrays, needs `mdadm`). Levels and the least number of data disks: `raid0` 2, `raid1` 2, `raid5` 3, `raid6` 4, `raid10` 4, `linear` 2. Options: chunk size (`64K` to `1024K`, default `512K`; `raid0`, `raid5`, `raid6`, `raid10`) and write-intent bitmap (`no`/`yes`, default `no`; `raid1`, `raid5`, `raid6`, `raid10`). Roles: data, plus spare for `raid1`, `raid10`, `raid5`, `raid6`, and one write journal for `raid5`, `raid6` (a journal cannot be combined with a bitmap).
- `lvm` (logical volumes, needs `lvm2`). Types and the least number of disks: `linear` 1, `striped` 2, `mirror` 2, `raid1` 2, `raid5` 3, `raid6` 5, `raid10` 4 (an even number of disks). Options: stripe count (`striped` only, up to the number of disks) and stripe size (`64K` to `1024K`, default `64K`; `striped`, `raid5`, `raid6`, `raid10`). Every disk is a data disk.

**The screens.** The layout list screen collects the layouts to compare: `a` adds a layout (provider, then the editor), `b` adds the *no-array baseline* (it takes every free disk, each tested on its own with nothing built, to see what the layout costs or gains; Enter on it drops disks), Enter edits, `d` deletes, `c` continues (at least two layouts, the baseline counts as one) and `q` quits. In the editor, up/down moves, left/right changes the level or an option, Space gives a disk its role (data, spare, journal where the level allows it) or takes it out, and Enter saves. A layout that is not valid yet shows why; two layouts cannot be the same; members that differ in capacity get a warning (the layout uses the smallest).

**Disks that are not offered.** A disk is grayed, with the reason, when it is mounted or used as swap, holds the bbbench binary or the output directory, is held by md, device mapper or another stacked device, carries an md, ZFS, LVM, LUKS, btrfs, bcachefs, Ceph BlueStore, DRBD or VMFS signature, is an LVM physical volume (also an orphan one, which is why `lvm` is checked even when the LVM backend is not used), or has no serial (a teardown finds the members again by serial). If the output directory or the bbbench binary sits on a filesystem that bbbench cannot attribute to a disk (tmpfs, NFS, overlay), or the disk holding the root filesystem cannot be determined, **no disk is offered**: the screen shows the reason, and moving the output (`--output`) to a local disk is the usual fix. A mount, swap area, the output directory or the bbbench binary that bbbench cannot attribute to a disk, or a root disk it cannot determine, offers no disk at all; signatures are read from udev, so a signature udev has not recorded is not seen. A disk with an unmounted filesystem or partition table IS offered (marked "carries data", with what udev reports, on the layout screens and the confirmation) and will be erased if you confirm.

**Confirmation.** The last screen lists every layout with its disks (name, model, serial, role) and the disks that will be erased. **Nothing is built before you press `y`**; Enter does not start the test. The layouts then run one after the other in the order given.

**One run per layout.** For each layout bbbench re-checks that its disks are still free, records the layout in the recovery file *before* building it, builds it, waits until it is steady, benchmarks it (the fio file is generated for the md or dm device, the workload type comes from the data members, mixed HDD and SSD members are refused), verifies it again, and tears it down.

- *Resync wait.* An array that resyncs (`mdadm`, and the mirror and RAID types of `lvm`) is benchmarked only when the resync is done; the status page shows `resyncing <x>%`. The estimated time on the last screen does not include the initial resync (it depends on the disk size and speed), so the real time is longer.
- *Verification before and after.* The layout is read back from the kernel (and from `lvs`/`vgs` for LVM): level, members and roles, chunk or stripe size, bitmap. A layout that is not as asked is not benchmarked (the run is *not run*); one that changed during the run is marked invalid, as for any setting.
- *Teardown on every exit.* The layout is torn down and its members are wiped (the signatures bbbench created) after the run, also after an error, Ctrl+C, SIGTERM or SIGHUP. Teardown only touches what it can prove it built: it refuses an array, volume group or name that has a member that is not a recorded member.
- *Failure policy.* A layout that cannot be built or that fails verification is *not run*, and the group goes on with the next layout. **A layout that cannot be torn down stops the group** (no further layout is built on disks that are still assembled): bbbench prints the commands to tear it down and wipe its members by hand, and keeps its record.
- *Recovery file.* `/var/lib/bbbench/group-recovery.json` also holds the layouts (provider, name, uuid and the members with serial and model). If bbbench is killed (`kill -9`, a crash, a power cut) the next start of `orchestrator` shows the layouts left and asks `Tear down these layouts and wipe their members now? [y/n]`. Members are found again by serial and model, since kernel names change across reboots. **Declining makes bbbench refuse to start** until the layouts are cleaned: answer `y` at the next start, or tear them down and wipe their members by hand and remove the file (the message says so). A layout whose member disk never comes back (removed, dead) keeps its record in the file and bbbench refuses to start until it is cleaned by hand, because the other members still carry the superblock; the message and the printed command name the layout and its members.
- *Resume.* `--resume` continues an interrupted layout test after a confirmation: the layouts are rebuilt from the group file, each disk is found again by serial and model, must be free and must not have changed role; finished runs are kept, the others start again from the beginning. If a disk is gone or in use, bbbench names the group file; removing or renaming it unblocks `--resume`.

**Per-member charts.** While a phase runs on an array, bbbench reads `/sys/block/<dev>/stat` of every member (data, spare and journal disks) once per `--log-avg-msec` window and stores the per-window deltas in `<fio log prefix>_members.1.log`. The graphs page draws IOPS and bandwidth of the phase as a stacked area of the members (reads up, writes mirrored down) with the array's own fio series as a line outside the stack, and latency as one line per member and direction (never stacked); hovering lists each member, `Stack total` and the array value. The member values are the kernel's counters on each member, so they include what md or LVM adds (parity, mirroring, read-modify-write, resync) and the stack total can exceed the array value. Member latency is the kernel's per-request time (queue plus service, 1 ms tick resolution, coarse at low load); the array line is fio's. Only runs made with this version have the series, the compare page does not draw it, and a sampling failure only logs a warning. **md state.** For an md array the sampler also reads, once per window, `sync_action`, `sync_completed`, `sync_speed`, `degraded`, `array_state` and `raid_disks` under `/sys/block/<array>/md/` and each member's `md/dev-<member>/state`, into the same `_members.1.log`. The graphs mark windows where `sync_action` is not `idle` with an opaque strip across the top of the plot, labelled with the action (resync, recover, check, repair, reshape, frozen) and a light tint, and draw every change between two windows (`sync_action`, `array_state`, `degraded`, `raid_disks`, a member's state, a removed member as `sdb: in_sync -> removed`) as a vertical marker with its text; a non-idle `sync_action` or a `degraded` count above 0 already present when the phase starts is reported as `(at phase start)` (a faulty member or a non-clean `array_state` present at the start is not reported until it changes), and an unreadable value is not a change. The hover adds `md: <action> <pct>% <speed> MiB/s, degraded <n>` for a busy window and `md: idle, degraded <n>` for an idle one, the card gets a badge (`resync 43% -> 61%`, `degraded`) and the line `array busy: results include administrative work`. bbbench still waits for the initial resync before a run starts, so a sync seen during a phase is one that started during that phase (a scheduled check, a rebuild after a member failure, a reshape), and the phase's numbers include the sync's own I/O. md arrays only (LVM has the member series only), not the compare page; a phase shorter than the ramp-up loses its md events with its points; an unreadable `degraded` is unknown (shown as `degraded unknown` in the hover, never an event).

**Not verified on hardware.** The layout code is tested against fake tools and a fake sysfs; this has not been run on real disks yet. Unverified: the exact `mdadm --create` command line and the sysfs values it is checked against, the `lvcreate` command lines and the fields of the `lvs`/`vgs`/`pvs` JSON reports, the handling of the LVM devices file (`lvmdevices`: bbbench records which member entries `pvcreate` adds and removes only those, leaving the entries of foreign physical volumes alone), Flush tracing on md and dm devices (they are bio-based, so a phase may show 0 Flushes, see [Flush tracing](#flush-tracing)), and trim on arrays that do not support discard. Two known gaps: when `lvmdevices` reported no devices file at the start, `pvcreate` may create one; nothing records it, so the teardown does not remove it. And after `mdadm --stop`, udev change events could assemble the members again before `--zero-superblock`; the wipe then fails because the disk is busy, which is a safe teardown failure (the group stops and prints the manual command). The per-member sampler and charts and the md state track (above) have not been run on real arrays either (the md sysfs files are read from a fake tree in tests; the charts were checked in headless Chrome only). Run it first on disks you can lose and report what differs.

**Hardware RAID** (Dell PERC, HPE Smart Array, each as its own provider) is not part of this; it comes in a later branch as another provider (see CLAUDE.md, "What the hardware-RAID branch plugs into").

## Flush-latency workloads

The default `ssd` workload list ends with three 5-minute jobs that measure
what a Flush costs: 4k random writes with an `fsync` after every write, so each
write is followed by one device Flush.

| Workload | Engine | I/O | Queue depth |
|---|---|---|---|
| `ssd_fsync_psync_qd1` | `psync` | buffered | 1 |
| `ssd_fsync_uring_qd1` | `io_uring` | direct | 1 |
| `ssd_fsync_uring_qd32` | `io_uring` | direct | 32 |

The orchestrator summary prints `fsync avg: … us` next to the IOPS of these
phases; the full fio JSON in the output directory has the `sync` latency
statistics. That is the latency of the whole `fsync()` call; the part spent in
the drive is measured separately, see [Flush tracing](#flush-tracing). To compare setups (write cache enabled vs cleared, stock vs patched
kernel), run the workloads once per setup and compare the results.
**A plain run does not configure the device**: it only reads the write cache
mode, so set the drive up yourself before the run and note the setup with the
results. A *setting test* (see [`bbbench orchestrator`](#bbbench-orchestrator))
can do the comparison for you: it applies each write cache value, verifies it
and restores the original; that is the only mode in which bbbench changes a
drive's setting.

Like the other write workloads they overwrite the device. The `io_uring` jobs
need an fio built with io_uring support and a kernel that has it. To run only
these workloads, generate the job files from a config that lists just them under
`bbbench.workloads.ssd` (`sudo ./bbbench generate --config my.yml`); the
orchestrator has no workload-selection flag and runs whatever was generated.

---

## Flush tracing

During phases that issue fsyncs (such as the flush-latency workloads above),
the orchestrator also times the device's own Flush commands and prints, for
example, `Flush (nvme): avg 194.8 us, p99 210.0 us, max 480.0 us (4012 flushes)`.
The result is saved in the run's JSON summary under each phase's `flush_trace`.
It needs root and a mounted tracefs; if tracing is unavailable the phase still
runs and a warning is logged. Use `--no-flush-trace` to turn it off. For a
diagram of the Linux stack and exactly what is timed, see
[documentation/flush-latency.md](documentation/flush-latency.md).

**Why it exists.** fio reports how long `fsync()` took. That covers the
filesystem, the block layer's flush handling, the driver and the drive, so it
cannot say whether a slow fsync comes from the drive. This matters because some
drives take a long time to complete a Flush even when their volatile write cache
is disabled, where the spec says it should be a no-op (see the linux-nvme thread
"nvme: honor the volatile write cache enable state": about 194 µs for the Flush
inside a 236 µs fsync). fio cannot observe a single kernel call, so bbbench reads
the kernel's trace events instead: the time between a Flush being issued to the
device and completing is the device's Flush latency as seen by the driver, and
comparing it with the fsync time shows how much of the cost is the drive.

**Two sources.**
- *NVMe namespaces* use the `nvme_setup_cmd` / `nvme_complete_rq` tracepoints.
  Each command carries a queue and command id, so every Flush is paired with its
  own completion exactly, even with many in flight (QD32). Reported: average,
  p99 and max.
- *Any other block device* (SATA, SAS, virtio, ...), and NVMe on kernels without
  those tracepoints, use `block_rq_issue` / `block_rq_complete`. Those carry no
  request id, so issues and completions are paired in order. The average is still
  exact, because it only depends on the sum of times, but with several Flushes in
  flight individual samples can be swapped, so percentiles and max are not
  reported.

**Stored with the run.** The Flush latency is written like fio's own logs, to
`<output>/<device>_phase<N>_<ts>_log_flush.1.log` next to the `_iops/_bw/_lat`
logs: one line per polling window (`--log-avg-msec`, 1 s by default), stamped at
the end of the window, in fio's columns (`time_ms, mean_latency_ns, dir, bs,
offset`, with direction 3 and zeros) followed by `count, min_ns, max_ns`, so a
5-minute phase is a few hundred lines (about 10 KB) however many Flushes it had.
Comment lines at the top hold the run's metadata (`source`, `exact`, `lost`,
`write_cache`, `avg_msec`, `offset_ms`) and the whole-phase summary (count,
mean, min, max, p50, p99, computed from every Flush after the ramp-up when the
phase ended; exact percentiles cannot be rebuilt from windows, so the individual
Flushes are not kept). The summary and the series (`flush_series` in the run,
`ts_flush` and `flush` in the graph data of `bbbench serve`) are read back from
this file whenever a run is loaded; the `flush_trace` saved in the run's JSON is
only a fallback for runs without a log. Logs from earlier development builds
(`_log_flush.log`, one line per Flush) are not read. In the web UI the Flush latency is its own graph type
(`/graphs`, and a metric on `/compare`), separate from IOPS, bandwidth and
latency, and every graph type carries a small `fio` or `ftrace` tag saying where
the numbers come from.

**Needs the drive's write cache on.** The kernel only sends Flush commands to a
device whose volatile write cache is enabled; for a `write through` device
(`/sys/block/<dev>/queue/write_cache`) fsync never reaches the drive, so zero
Flushes are seen and the summary says so (`none seen (the kernel sends no Flush
to a write-through device ...)`, and the UI explains the empty graph). The
cache state is stored in the flush log header. To measure Flushes on an NVMe
drive that has a volatile cache (`nvme id-ctrl /dev/nvme0 | grep vwc`), enable
it first: `nvme set-feature /dev/nvme0 -f 6 -v 1`, then check that `write_cache`
reads `write back`.

**Write-through devices.** Before a phase that fsyncs (the flush workloads),
the orchestrator reads the device's cache mode. For `write through` the kernel
sends no Flush, so there is no device Flush latency to trace (fio's own fsync
latency is still measured). The phase still runs, with a warning: printed
when the phase starts, shown on the status page, saved with the run
(`warnings` per phase and device) and shown in the run details; the Flush graph
says "ftrace saw 0 Flush commands" and why. `--skip-write-through` refuses such
phases instead: they are reported as `SKIPPED` with the reason (output, saved
run, web UI) and not counted as completed, so `--resume` runs them once the
cache is enabled.

**Cost and limits.**
- Tracing uses a private ftrace instance (`instances/bbbench-flush-*`), so system
  tracing settings are untouched and drives can be traced in parallel. It is only
  active during phases that fsync, with kernel-side filters to keep event volume
  low, but it still adds a little overhead to those phases.
- Timestamps have microsecond resolution, which is coarse for fast Flushes.
- Stacked devices (md arrays, device-mapper volumes) are bio-based or remap
  requests to their member disks, so they may issue no Flush requests of their
  own; the summary then shows `Flush (block): none seen`. Trace the underlying
  disks (benchmark them directly) to get a number. This has not been verified
  on a live kernel.
- This has been tested against hand-written sample trace lines and a fake tracefs, not yet
  on a live kernel; kernels differ in tracepoint fields, so check the first real
  run.

---

## Config and template discovery

If `--config` is not provided, bbbench searches in order:

1. `~/.bbbench/config/default.yml`
2. `/etc/bbbench/default.yml`
3. `<dist>/default.yml` (default `./dist`, override via `--dist` or `BBBENCH_DIST_DIR`)
4. Embedded fallback bundled in the binary

Same search order applies to `--templates` (looking for a directory instead of a file).

---

## Logging

```bash
BBBENCH_LOG_LEVEL=debug sudo ./bbbench orchestrator
```

Levels: `debug`, `info`, `warn`, `error`.

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
