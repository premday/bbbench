# Commands

## `bbbench generate`

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

## `bbbench orchestrator`

Runs fio benchmarks. Presents a TUI to select drives, then executes the benchmark phases and saves results as JSON.

**Single run or setting test.** The first question is what to do:

- *Single run* is the flow described here: select drives, each gets the plan for its type, everything is tested at once. bbbench only reads the drives.
- *Setting test* runs the whole test plan once per value of one setting (the settings are the drive write cache policy, enabled vs disabled, and the storage layout of a set of disks, see [Layout setting test (mdadm, LVM)](layout-test.md)), to measure what the setting costs or gains. The settings are listed with the number of discovered drives each applies to and, for the others, the reason (for example no controllable write cache on USB). A setting test asks, in order: the drives (the same selection screen), the values to test (both start ticked; at least two), and how to spread them:
  - *one value after the other* (default): every drive gets the same value in a run, one run per value;
  - *parallel*: identical drives (same vendor, model and capacity) are divided evenly among the values and all values run at the same time in one run. Offered only when every class of identical drives divides evenly; otherwise the screen says why;
  - *parallel with swap rounds*: parallel, then the values rotate over the drive subsets and the plan runs again, so each value is measured on every subset (cancels drive-to-drive variance). Never preselected.

  Drives of one model with different firmware get a warning (parallel stays offered). Drives the setting does not apply to are left out and listed with the reason. The last screen shows what will change on which drives, the number of runs and an estimated time; **nothing is changed on a drive before you confirm it** there. The storage layout setting is laid out per variant instead of per value: it asks for the layouts with their own disks, see [Layout setting test (mdadm, LVM)](layout-test.md), and **erases** those disks.

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
- `--no-flush-trace` — do not trace device Flush latency during fsync phases (see [Flush tracing](flush-tracing.md))
- `--no-io-trace` — do not trace read and write latency from kernel trace events (see [I/O latency tracing](io-latency-tracing.md))
- `--io-trace auto|ebpf|ftrace` — how read and write latency is collected: `ebpf` times requests in the kernel (no events lost at high IOPS, root + BTF, amd64/arm64), `ftrace` reads every trace event, `auto` (default) uses eBPF when it loads and says why when it falls back (see [I/O latency tracing](io-latency-tracing.md))
- `--trace-buffer-mb N` — MiB of kernel ring buffer shared by the concurrent trace sessions (default 1024; see [I/O latency tracing](io-latency-tracing.md), "Cost and opt-out"); raise it when the trace reports events lost in the ring buffer
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
| `<device>_phase<N>_<ts>_log_flush.1.log` | the device Flush latency, for phases that fsync (see [Flush tracing](flush-tracing.md)) |
| `<device>_phase<N>_<ts>_log_iolat.1.log` | the read and write latency from kernel trace events, at the device and in the block layer, for every phase (see [I/O latency tracing](io-latency-tracing.md)); an array target also has its members in it |
| `<device>_phase<N>_<ts>_log_members.1.log` | an array target (md or LVM layout) only: the kernel's I/O counters of each member disk, one line per member and `--log-avg-msec` window (see [Layout setting test](layout-test.md)) |
| `<device>_diskinfo_<start>.txt` | the drive's configuration (interface, link, power, write cache, SMART, request queue) and the raw reports it is drawn from, with the full queue and scheduling dump and the queue modules' parameters (see [Queue and scheduling dump](queue-dump.md)), one per run; an array run also has one for the array node (md or dm), next to one per member |
| `run_<unix time>_summary.json` | the run's summary, which `bbbench serve` lists and graphs; for an array phase it also holds the members' series (`member_series`) and the md state (`md_series`), N members x 3 series, like `flush_series`; with I/O latency tracing each phase also has `io_trace` (summary per traced device), `io_trace_series` and `io_trace_notes` (why a device was not traced) |
| `group_<id>_group.json` | a setting test: its runs, the value each drive had, the readings before and after each run, and the restore result |
| `<host>/{host}_md_{level}_{uuid}_md_config.fio` and `<host>/{host}_lvm_{type}_{lv uuid}_lvm_config.fio` | a layout setting test: the generated fio file of an md array or of an LV (under `fio.generated.path`, like the files of `generate`), named after the array's level and uuid |
| `.bbbench-orchestrator-state.json` | what is done, for `--resume`; removed when the run completes |

The original drive values of a running setting test, and the layouts it has built, are not in the output directory: they are in `/var/lib/bbbench/group-recovery.json`, removed when the group has restored everything.

Results are written to `~/.bbbench/output/` by default (one JSON file per run).

---

## `bbbench serve`

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
- `/status` — live benchmark status while the orchestrator runs: progress, and per drive its configuration (interface, physical slot, controller, link speed, power, write cache policy, SMART, request queue: blk-mq or bio-based, hardware queues, scheduler, tunables, an md array's state and the `dm_mod` queue parameters), and its phases with the fio config, real and expected time and an ETA (details in [documentation/web-ui.md](documentation/web-ui.md))
- `/results` — browse and search past runs
- `/summary?id=<id>` — statistics table (min/avg/max/σ) per phase, broken down by disk
- `/graphs?id=<id>` — time-series charts per phase: IOPS, bandwidth and latency (from fio) and Flush latency (from kernel traces), on one time axis per phase (0 to the phase's longest series) with the ramp-up shaded, the y range shared per phase, across all phases (Global) or per card (Y scale selector); for an array phase, IOPS and bandwidth are stacked per member and latency has one line per member. Everything starts folded and a graph is drawn when its phase is opened; the address bar always holds the view (filters, open graphs, the full screen graph and its zoom), so the URL can be shared as it is; in full screen, drag over the chart to zoom on a time range and press Esc to reset the zoom, then to close. See documentation/web-ui.md
- `/compare?id=<id>` — overlay several disks and/or phases on one chart
- `/result?id=<id>` — details of one run: drives, phases, and any skipped phase or warning (e.g. a write-through drive)
- `/group?id=<group id>` — a setting test: its runs by drive and value, the verification, the restore result and the metrics per value; `/graphs?group=<group id>` and `/compare?group=<group id>` show its runs together, by setting value
- `/export?id=<id>` — download self-contained HTML report
- `/health`, `/api/status`, `/api/status/phases`, `/api/results`, `/api/results/detail`, `/api/results/graphs`, `/api/results/summary`, `/api/groups`, `/api/groups/detail`, `/api/groups/graphs` — JSON behind the pages

The web UI is also started automatically by `orchestrator` during a run, on `0.0.0.0:12345` unless `orchestrator --addr <host:port>` says otherwise (the same flag as `serve`).

---

## `bbbench doctor`

Checks host readiness: Linux, root access, sysfs, block device discovery, and the external programs (`fio` is required; `smartctl`, `nvme` and `hdparm` are optional and a warning says what is lost without them). It also warns about trace instances left by a killed or crashed bbbench (still enabled, they cost CPU on every I/O) and says what removes them: the next `bbbench orchestrator` start, or `rmdir` of each.

```bash
sudo ./bbbench doctor
```

---

## `bbbench validate-templates`

Validates template syntax without generating anything. Useful when editing templates.

```bash
./bbbench validate-templates
./bbbench validate-templates --templates ./dist/templates
```

---

## `bbbench completion`

Generates shell completion scripts.

```bash
# Bash
./bbbench completion bash | sudo tee /etc/bash_completion.d/bbbench

# Zsh
./bbbench completion zsh > ~/.zsh/completions/_bbbench

# Fish
./bbbench completion fish > ~/.config/fish/completions/bbbench.fish
```
