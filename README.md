# bbbench

Block-device benchmark tool. Discovers drives, generates [fio](https://github.com/axboe/fio) job files from templates, runs benchmarks across several drives in parallel, and serves the results in a web UI.

```
1. bbbench generate      → create fio job files for the discovered drives
2. bbbench orchestrator  → select drives, run benchmarks, save results
3. bbbench serve         → browse results (graphs, compare, summary) in the web UI
```

## Requirements

- Linux, root for `generate`, `doctor` and `orchestrator`
- [fio](https://github.com/axboe/fio) for `orchestrator`, built with the I/O engines of the workloads you run (the flush workloads use `io_uring`)
- Optional, each one only adds detail and a missing one is a warning: `smartctl`, `nvme`, `hdparm`, `sdparm`, `lspci` (drive configuration and SMART), `mdadm`, `lvm`, `udevadm` (layout tests)

`orchestrator` and `doctor` check them and say what is missing.

## Build

```bash
make clean && make     # static ./bbbench and ./check-config, pure Go
make check             # what CI runs
```

Plain `go build ./cmd/bbbench` also works. Release archives and CI: [documentation/build-and-release.md](documentation/build-and-release.md).

## Commands

| Command | What it does |
|---------|--------------|
| `generate` | writes fio job files for the discovered drives |
| `orchestrator` | picks drives and runs the benchmark plan, as a single run or as a setting test (write cache, storage layout), and saves the results |
| `serve` | web UI over the results: graphs, compare, summary, status |
| `doctor` | checks that this machine is ready |
| `validate-templates`, `completion` | template check, shell completion |

All flags and examples: [documentation/commands.md](documentation/commands.md).

## Measuring

- **fio** times each phase (IOPS, bandwidth, latency).
- **Kernel tracing** adds what fio cannot see: the device's own latency and the block layer's, from eBPF (preferred; falls back to ftrace) with `--io-trace auto|ebpf|ftrace`, and the device's Flush time from ftrace. Needs root and tracefs; `--no-io-trace` turns it off. [documentation/tracing.md](documentation/tracing.md)
- **Array members** (md, LVM) get per-member IOPS, bandwidth and latency from the kernel's counters, and md state (sync, degraded).

## Documentation

| Topic | File |
|-------|------|
| Commands and flags | [commands.md](documentation/commands.md) |
| Web UI: graphs, compare, status, shareable links | [web-ui.md](documentation/web-ui.md) |
| ftrace and eBPF: what each measures, requirements | [tracing.md](documentation/tracing.md) |
| I/O latency tracing in detail | [io-latency-tracing.md](documentation/io-latency-tracing.md) |
| Flush tracing and what Flush latency means | [flush-tracing.md](documentation/flush-tracing.md), [flush-latency.md](documentation/flush-latency.md), [flush-workloads.md](documentation/flush-workloads.md) |
| Layout setting test (mdadm, LVM) | [layout-test.md](documentation/layout-test.md) |
| Queue and scheduling dump | [queue-dump.md](documentation/queue-dump.md) |
| Build, release archives, CI | [build-and-release.md](documentation/build-and-release.md) |
| Changes | [CHANGELOG.md](CHANGELOG.md) |

## Config and template discovery

If `--config` is not provided, bbbench searches in order:

1. `~/.bbbench/config/default.yml`
2. `/etc/bbbench/default.yml`
3. `<dist>/default.yml` (default `./dist`, override via `--dist` or `BBBENCH_DIST_DIR`)
4. Embedded fallback bundled in the binary

Same search order applies to `--templates` (looking for a directory instead of a file).

## Logging

```bash
BBBENCH_LOG_LEVEL=debug sudo ./bbbench orchestrator
```

Levels: `debug`, `info`, `warn`, `error`.

## License

Apache 2.0 — see [LICENSE](LICENSE).
