# eBPF I/O latency collector (design)

## Why

The ftrace I/O latency path copies every trace event out of the kernel and
decodes, merges, pairs and aggregates it in one pipeline. A real run (4 NVMe,
md127, 64 CPUs, ~0.9 M IOPS, ~6.5 M events/s) showed the pipeline handles
0.3-0.85 M events/s, so 85-95 % of the events overran the ring buffers and
every ftrace series of those phases is incomplete. A bigger buffer only delays
the overrun. The work has to move into the kernel: pair a request's start and
completion there, keep a latency histogram per device, and let bbbench read
the histograms once per window.

## Goals

- The same series as today (device latency, block-layer latency, a bio-based
  node's bio latency): mean, min, max, p50, p99, count per `--log-avg-msec`
  window and per phase, with the same relative error bound (< 2 %).
- No loss at high IOPS: bbbench CPU close to zero, kernel cost spread over the
  CPUs; the lost count stays meaningful (start-map full, unmatched completion).
- The existing result types (`Summary`, `Window`, `Hist`), the `_iolat.1.log`
  format, the graphs and the loss warning keep working unchanged.

## Non-goals

- Replacing the Flush session (`internal/flushtrace` Flush timing): low rate,
  no problem seen, stays on ftrace.
- Per-I/O storage or tracing of individual requests.
- Removing the ftrace path: it stays as the fallback.

## Decisions (my calls, to be confirmed)

1. **Opt-in order:** `--io-trace=auto|ebpf|ftrace` (default `auto`: eBPF when
   it loads, else ftrace with a note saying why). `--no-io-trace` unchanged.
2. **Same session API.** `iotrace.Start(devices, Options)` returns the same
   `Session`; a backend (`ebpf` or `ftrace`) is chosen inside. Callers in
   `cmd/bbbench` do not change except the flag.
3. **Library:** `github.com/cilium/ebpf` (pure Go, so `CGO_ENABLED=0` and the
   static builds stay). No cgo, no libbpf.
4. **Program:** one small C file (`internal/iotrace/ebpf/iolat.bpf.c`) using
   `tp_btf` programs (CO-RE, request pointer as the pairing key, exact):
   `block_rq_issue`/`block_rq_insert` -> `block_rq_complete` (device and
   block-layer latency), `nvme_setup_cmd` -> `nvme_complete_rq` for NVMe
   (device latency), `block_bio_queue` -> `block_bio_complete` for a bio-based
   node. Start timestamp in a hash map keyed by the pointer, per-CPU log
   histograms keyed by (device, kind, direction), a small per-CPU lost
   counter. The same bucket layout as `iotrace.Hist` so the userspace reuses
   it.
5. **Build:** the object is compiled with clang (`-target bpfel`, CO-RE) by
   `go generate` and committed, so releases and CI need no clang. A test fails
   when the committed object is older than the C file. amd64 and arm64 only;
   `386` and any kernel without BTF/BPF use ftrace.
6. **Window handling:** bbbench reads and clears the per-CPU histograms at the
   fio log window boundary (monotonic clock, aligned to the start marker) and
   builds the same `Window`s; the per-phase summary is the sum of the windows
   after the ramp.
7. **Filtering:** devices to trace are passed in a map (dev number, kind,
   role), so non-target devices cost one lookup.

## Risks

- **Not verifiable here**: no BPF run on this machine's kernel is the real
  target; the first test is on the user's RHEL 10 server (kernel 6.12, BTF
  present). The log records which program types attached and how many
  start/complete events were seen, like the kernel view for ftrace.
- Verifier or tracepoint-argument differences between kernels: loading
  failure falls back to ftrace with the reason in `ReaderNote`.
- Start-map overflow under extreme queue depth: counted as lost, map sized
  from the queue depths.

## Left to the builder

File layout, map sizes, the exact histogram layout (must round-trip to
`Hist`), tests with a fake map reader (the loader is behind an interface so
the aggregation and window logic are tested without a kernel).
