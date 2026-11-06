# bbbench

Block-device benchmark tool. Discovers drives, generates [fio](https://github.com/axboe/fio) job files from templates, runs benchmarks across multiple drives in parallel, and serves results via a web UI.

## Requirements

- Linux
- Root privileges for `generate`, `doctor`, and `orchestrator`
- `fio` installed for `orchestrator`

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
- `/status` — live benchmark status (when orchestrator is running), with each drive's interface, link speed, power configuration, write cache policy and SMART health (see [documentation/web-ui.md](documentation/web-ui.md)); the same information in full is written to `<output>/<device>_diskinfo_<start>.txt`, one text file per disk; each drive lists its phases, with a one-line summary of the fio options and the fio job file the phase runs (click a phase to expand); finished phases are struck through and an ETA is computed from the phases with a fixed duration, with phases of unknown length (full-disk writes, trims) reported separately
- `/results` — browse and search past runs
- `/summary?id=<id>` — statistics table (min/avg/max/σ) per phase, broken down by disk
- `/graphs?id=<id>` — time-series charts (IOPS, bandwidth, latency) per phase
- `/export?id=<id>` — download self-contained HTML report

The web UI is also started automatically by `orchestrator` on `0.0.0.0:12345` during a run.

---

### `bbbench doctor`

Checks host readiness: Linux, root access, sysfs, and block device discovery.

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
**bbbench does not configure the device**: it only reads the write cache mode
and never changes it, so set the drive up yourself before the run and note the
setup with the results.

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
