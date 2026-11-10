# Per-member metrics for array targets

## Intent

When a layout run benchmarks an array (md, LVM, later a controller's virtual
drive), fio only measures the array device. The user wants to see what each
member disk did while the array was being benchmarked: IOPS per disk when the
metric is IOPS, bandwidth per disk when it is bandwidth, latency per disk when
it is latency, with the array's own value on the same chart.

## What the user decided

- IOPS and bandwidth charts: a **stacked** graph of the per-member values. The
  array value is **not in the stack**: it is drawn as a line over it (the stack
  top is the sum of the members, the line is what the array reports).
- Hover shows each member's value, the stack total and the array's value.
- Latency: **never aggregated**. One line per member plus the array's line, no
  stack.
- Software RAID is the case asked for. Not on the compare page.
- The metrics come from `/sys/block/<dev>/stat` counters (user's decision, after
  considering ftrace block events: no tracing overhead on fast disks, no lost
  events, exact IOPS and bandwidth). ftrace stays for the Flush latency only.

## Goals

- Per-member IOPS, bandwidth and mean latency, read and write, over time, for
  every phase of an array run, on the same time axis as fio's series.
- Provider-neutral: any array target with `Members` gets it (mdadm and LVM
  today); the core never names a provider.
- Old runs, single-drive runs and runs where sampling failed render exactly as
  today.

## Non-goals / anti-goals

- No change to what fio runs; no extra I/O on the disks (counters are read only).
- A sampling failure never fails or invalidates a phase (warn and go on, like
  Flush tracing).
- No percentile latency per member (counters give a mean only).
- No compare-page support.

## Design (the builder's contract; items marked "call" are the author's choices)

- **Sampler** (new package `internal/diskstat`): reads `/sys/block/<name>/stat`
  (`Options.SysBlock` replaces the root for tests) for the array and every
  member (all roles, so a spare shows as an idle band). Every `log_avg_msec`
  (the phase's polling interval) it takes the counter deltas and turns them into
  read/write IOPS, MB/s and mean latency (call: `ticks` delta / `ios` delta;
  0 when no I/O). The kernel's tick counters have 1 ms resolution, so latency is
  coarse at low load; the docs say so. The sampler starts and stops next to
  fio in `executePhase` (as the Flush trace does) and only when the drive has
  `Members`.
- **Log**: `<fio log prefix>_members.1.log` next to fio's logs: `#` header lines
  (`avg_msec`, `offset_ms` = fio's ramp, one `device <name> <role>` per member and
  `array <name>`), then one line per window and device:
  `end_ms, device, read_iops, write_iops, read_mbps, write_mbps, read_lat_us, write_lat_us`.
  Window ends are stamped like fio's and the Flush log; loading drops windows
  that ended inside the ramp-up and stamps points at the window start.
  Member names are stored in the log, so a later kernel renaming does not matter.
- **Load**: `loadMembersLog(logPrefix)` (new, like `loadFlushLog`) fills
  `DiskPhaseData.TSmembers` (JSON `ts_members`: member name -> `{role, iops, bw,
  lat}` where each is a `[]TimePoint` using R and W like fio's series). Absent or
  unreadable log = field omitted.
- **Charts** (`serve_graphs.go` JS; shared helpers in `serve_shared.go`): when
  `ts_members` is present, the IOPS and bandwidth charts draw one stacked area per
  member (fixed colour per member name, per direction as the chart already splits
  read and write) and the array's fio series as a line, unstacked. The latency
  chart draws one line per member plus the array line. The tooltip lists every
  member's value, the stack total and the array's value (the user's decision), so the gap
  between sum and array (raid5 parity, read-modify-write, mirroring) is visible.
  The array line is fio's own series (call: same line as today, not md's own
  counters). The per-phase shared scale rule still applies.
- **Failure**: unreadable stat file or a member that disappears: warn, keep the
  windows already taken, the missing member's points are absent.

## Tests

Unit: counter-delta to rates (including zero I/O and counter wrap-free deltas),
log write/read round trip, window drop in the ramp, loader returns nothing for a
missing log; sampler on a fake sysfs tree with a stepping clock; graph data JSON
has `ts_members` only for arrays; chart JS string contains the stack and
unstacked-line setup and no stack for latency (existing page tests style).

## Docs

README (graphs section: what the per-member charts show and the latency
resolution note), CHANGELOG, `documentation/web-ui.md`, CLAUDE.md (new package and
the log).
