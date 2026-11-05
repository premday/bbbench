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
- `/status` — live benchmark status (when orchestrator is running); each drive lists its phases, with a one-line summary of the fio options and the fio job file the phase runs (click a phase to expand); finished phases are struck through and an ETA is computed from the phases with a fixed duration, with phases of unknown length (full-disk writes, trims) reported separately
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
