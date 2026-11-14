# ftrace I/O latency and graph series toggles — design

Date: 2026-11-12. Branch `run_groups_mdadm`. Requested by the user (owner of the machines).

## Why

fio's latency is what the application sees. To know how much of it is the
device, the user wants the kernel's own view of every read and write next to
fio's, for **every** phase of **every** run, a single drive included (today
only Flush latency is traced, and only for `fsync` phases). Two gaps in what
exists today:

1. In array (md/LVM) runs the per-member latency lines come from
   `/sys/block/<dev>/stat`, and the Flush chart from ftrace, but a chart's
   legend does not say where a series comes from.
2. The UI and the output data do not say **which kernel tracepoints** were
   monitored (`nvme_setup_cmd`/`nvme_complete_rq` or `block_rq_*`), so a number
   cannot be interpreted.

## What

### Measurement (decisions of the user: "all that")

For each phase on a block device (single drive: the drive; array: the array
node and each member where traceable), run an I/O latency session next to the
existing Flush session, in a private ftrace instance, recording **both**:

- **device latency**: request issue to completion at the device
  (`nvme_setup_cmd` -> `nvme_complete_rq` for NVMe namespaces with those
  tracepoints, else `block_rq_issue` -> `block_rq_complete`);
- **block-layer latency**: `block_rq_insert` -> `block_rq_complete` (includes
  the time in the I/O scheduler and software queue), when the kernel has the
  tracepoints.

Per direction (read, write; discard and flush are not counted here, Flush
keeps its own session). Events are paired exactly (NVMe: controller, queue,
cmdid; block: device, sector, length). Latencies are **aggregated in the
process into one window per `--log-avg-msec`** (mean, min, max, count and
p50/p99 from a log-bucketed histogram), never stored one by one; the phase
summary uses the same histogram over the time after the ramp. Lost trace
events (ring buffer overrun, unpaired completions) are counted per phase and
shown as a warning on the chart.

Opt out with `--no-io-trace` (tracefs missing, not root, or no tracepoints =
the phase simply has no ftrace latency and says why, like Flush does today).
**It is on by default** (the user asked for it on every benchmark); the cost is
CPU in the tracer on high-IOPS phases, which the loss counter makes visible.
Ruling: if loss is common in practice the default flips to opt-in; that is a
one-line change.

### Stored data

`<fio log prefix>_iolat.1.log`, same shape rules as `_flush.1.log` and
`_members.1.log`: `#` header lines with the metadata (`source` = `nvme` or
`block`, `events` = the exact tracepoints monitored, `pair`, `exact`, `lost`,
`avg_msec`, `offset_ms`, the phase summary per direction and kind), then one
line per window end per direction and kind with
`end_ms, kind, dir, count, mean_ns, min_ns, max_ns, p50_ns, p99_ns`.
`PhaseResult`/summary JSON carry `io_trace` (per device: the metadata
including the tracepoint list, the summaries) and `io_trace_series`.
The graphs API serves `ts_iolat` (per window: kind, dir, mean, p50, p99) and
`io_trace` (metadata). Old runs have none and render as before.

### Display

- **Latency chart**: the fio read/write lines stay; the ftrace series are
  added as dashed lines of the same colour family, legend names say the source:
  `Read (fio)`, `Read device (ftrace)`, `Read block layer (ftrace)`, and the
  same for write. Member series in array mode are labelled
  `sdb read (kernel stat)`; the array line `Array read (fio)`.
- A new graph type **Device latency (ftrace)** (`key: iolat`) shows only the
  ftrace series (mean and p99 as separate toggles), so they can be read at
  their own scale.
- Every card shows a source line naming the monitored tracepoints
  (`ftrace: nvme_setup_cmd -> nvme_complete_rq, block_rq_insert ->
  block_rq_complete`), the pairing exactness and the lost-event count; the
  modal and the run summary page show the same text. The Flush chart gets the
  same line (its tracepoints are already in `Summary.Source`).
- **Series visibility toggles**: every chart card and the modal get a
  legend-like row of checkboxes, one per series (fio read, fio write, device
  mean, device p99, block layer mean, ..., and each member), to show or hide
  it. The choice is per metric and series kind, applies to all cards of the
  metric, survives re-rendering (kept in the page, not storage), and the
  y scale of the phase is recomputed from the visible series only.
- Compare page: not in scope (it plots fio only); the legend text there is
  unchanged.

## Non-goals

- No per-I/O storage or trace export; no tracing of trim/discard.
- No change to how fio is run; no ftrace use for IOPS/bandwidth.
- Nothing changes in drives or settings (tracefs private instance only, as
  Flush tracing today).

## Anti-goals

- Tracing must never fail or slow down a run silently: a failed start is a
  per-phase warning with the reason, never an error of the run.
- A missing tracepoint must not make a series look like zero latency.

## Constraints

- Reuse `internal/flushtrace` (instances, markers, stop protocol, trace clock
  alignment with the ramp, `Options.TracefsRoot`/`SysBlock` fakes for tests);
  put the new collectors and histogram there or in a sibling package
  `internal/iotrace` if the Flush code would get unwieldy.
- Page JS lives in the existing embedded scripts (no backticks in
  `chartSharedJS`); keep SI units and two-decimal tooltips.
- `make check` stays green; node tests for the page code; docs (README,
  CHANGELOG, `documentation/web-ui.md`, CLAUDE.md) updated.

## Left to the builder

- Histogram bucket layout (relative error under ~3 %), reading `trace_pipe`
  throughput (one reader per session), whether the block-layer pair uses
  `block_rq_insert` or `block_rq_merge` handling, array member tracing by
  device number, and the exact checkbox/legend styling.
- Not verifiable here: real tracepoint formats on the user's kernel; tests use
  fake tracefs trees and hand-written trace lines.
