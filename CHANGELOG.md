# Changelog

## [Unreleased]

### Added

- Status page: each drive lists its phases with a summary of the key fio options (`rw`, `bs`, `iodepth`, `runtime`, ...) and the fio config the phase runs; served by the new `/api/status/phases` endpoint so the 2-second status poll stays small; finished phases are struck through, each phase shows its expected length, and a per-drive ETA sums the known lengths (`time_based` jobs: `runtime` + `ramp_time`) and counts phases of unknown length, such as full-disk writes and trims, separately
- Release workflow rejects tags that are not `vMAJOR.MINOR.FIX` and tags that are not higher than all earlier tags or already have a release (`scripts/check-tag.sh`, `scripts/check-tag-order.sh`, tested in CI; `make check-tag TAG=vX.Y.Z` before tagging)
- `bbbench version` and `check-config --version` print the version, commit and build date, embedded at build time (new `internal/buildinfo` package)
- GitHub Actions CI on every pull request: gofmt, `go vet`, `go test -race`, template validation, and a build of all release archives
- Tag-triggered release workflow publishing static linux archives (`amd64`, `arm64`, `386`) with binaries, documentation, `dist/` config and templates, and `SHA256SUMS`
- `make check` (CI checks) and `make package` (release archives)
- Flush tracing: phases that fsync also report the device's own Flush latency, measured from kernel trace events (NVMe tracepoints with exact pairing, generic block tracepoints for any other device); saved as `flush_trace` per phase; `--no-flush-trace` disables it
- Flush samples are stored next to the fio logs (`<prefix>_flush.log`); the flush summary and a per-second series (`flush_series`, `ts_flush`) are rebuilt from them whenever a run is loaded, so past runs follow later changes to statistics and graphs
- Flush-latency workloads `ssd_fsync_psync_qd1`, `ssd_fsync_uring_qd1` and `ssd_fsync_uring_qd32` (4k randwrite with fsync after every write), added to the default `ssd` list
- Run summary shows the mean fsync latency for phases that issue fsyncs
- Workloads can override `ioengine`, `iodepth`, `direct` and `fsync` per job in `bbbench.workloadsdb`

- Release archives ship a `VERSION` file (version, commit, date, arch) and a generated `RELEASE_NOTES.md`; the GitHub release notes are generated from the commits since the previous tag (`scripts/changelog.sh`)

### Changed

- CI workflows use Node 24 action versions (`checkout@v7`, `setup-go@v7`, `upload-artifact@v7`) and run on `ubuntu-26.04` instead of `ubuntu-latest`
- `make` builds with `CGO_ENABLED=0` instead of musl-gcc: the code is pure Go, so binaries stay static without a C toolchain
- Generated fio log names and the fioplot `iodepth` reflect a workload's effective iodepth and `direct`/`buffered` mode (unchanged for existing workloads)

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
