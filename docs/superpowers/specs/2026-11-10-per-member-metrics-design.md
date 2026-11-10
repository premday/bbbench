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
- The metrics come from **ftrace** (block tracepoints), like the Flush latency, not
  from `/sys/block/*/stat` counters.

## Goals

- Per-member IOPS, bandwidth and mean latency, read and write, over time, for
  every phase of an array run, on the same time axis as fio's series.
- Provider-neutral: any array target with `Members` gets it (mdadm and LVM
  today); the core never names a provider.
- Old runs, single-drive runs and runs where sampling failed render exactly as
  today.

## Non-goals / anti-goals

- No change to what fio runs; no extra I/O on the disks (tracing only reads kernel events).
- A sampling failure never fails or invalidates a phase (warn and go on, like
  Flush tracing).
- No percentile latency per member (per-window mean, min and max only).
- No compare-page support.

## Design (the builder's contract; items marked "call" are the author's choices)

- **Member tracing** (in `internal/flushtrace`, which owns tracefs sessions):
  `StartIO(names []string, opts Options) (*Session, error)` opens a private ftrace
  instance with `block:block_rq_issue` and `block:block_rq_complete`, filtered
  (best effort, kernel side, and always in userspace) to the member devices'
  major:minor and `nr_sector != 0` (data requests, not Flushes). The session
  reader is generalised to feed any collector with `Feed(Event)`; the start and
  stop markers and the cleanup stay as they are. The **IO collector** pairs an
  issue with its completion by (device, sector, nr_sector, first-in first-out per
  key), takes direction from rwbs (`W` = write, `R` = read, other = ignored) and
  size from nr_sector x 512. It does not keep samples (a fast NVMe does hundreds
  of thousands of requests a second): it **aggregates into windows** of the phase's
  `log_avg_msec` as events arrive, per device and direction: completed requests,
  bytes, sum/min/max of issue-to-complete time. Windows are counted from the start
  marker. Reader notices `[LOST n EVENTS]` are counted and reported.
- **What the numbers mean**: IOPS and MB/s are completed requests and bytes per
  second at the member's request queue (so they include md overhead: parity,
  read-modify-write, mirrored writes, resync). Latency is issue-to-completion at
  the device (not queue wait before issue), mean per window. The md device is
  bio-based and has no request events, so it is **not traced**: its line is fio's
  own series, as today (call).
- **Log**: `<fio log prefix>_members.1.log` next to fio's logs: `#` header lines
  (`avg_msec`, `offset_ms` = fio's ramp, `lost_events`, one `device <name> <role>`
  per member), then one line per window, device and direction:
  `end_ms, device, dir(r|w), count, bytes, sum_lat_ns, min_lat_ns, max_lat_ns`
  (raw sums, so the rates and means are derived at load and can be improved
  later). Window ends are stamped like fio's and the Flush log; loading drops
  windows that ended inside the ramp-up and stamps points at the window start.
  Member names are stored in the log, so a later kernel renaming does not matter.
- **Load**: `loadMembersLog(logPrefix)` (like `loadFlushLog`) fills
  `PhaseResult.MemberSeries` (device -> member -> `{role, iops, bw, lat}`, each a
  `[]TimePoint` with R and W like fio's series, bw in MB/s and lat in us) and
  `DiskPhaseData.TSmembers` (JSON `ts_members`), plus `members_lost` (the lost
  event count). Absent or unreadable log = field omitted. A run saved without the
  log renders as before.
- **Wiring**: `executePhase` starts the IO session next to fio (and stops it
  after) when `drive.IsArray()`, tracefs is available and the new flag
  `--no-member-trace` is not given (call: tracing every request can perturb a
  fast device, so there is an off switch, like `--no-flush-trace`). Failure to
  trace warns and never fails the phase. The phase's own time zero is fio's launch,
  as for the Flush series.
- **Charts** (`serve_graphs.go` JS; shared helpers in `serve_shared.go`): existing
  charts draw read up and write mirrored down. When `ts_members` is present the
  IOPS and bandwidth charts draw one stacked area per member (fixed colour per
  member name; read stacked up, write stacked down) and the array's fio series as
  a line outside the stack (its own stack key). The latency chart draws one line
  per member and direction plus the array line, no stack. The tooltip lists every
  member's value, the stack total and the array's value (the user's decision), so
  the gap between sum and array is visible. The per-phase shared Y scale includes
  the stack totals. A phase whose log reports lost events shows
  "ftrace lost N events: member values are too low" on the card.
- **Failure**: tracefs missing or member not traceable: no `ts_members`, one
  warning line at run start naming it.

## Tests

Unit (fake tracefs tree and fake pipe, as `session_test.go` does): IO collector
pairing (two requests on one sector, read vs write, flush ignored, unpaired
issue), window aggregation and rates, lost-events counting; log write/read round
trip and the ramp-window drop; loader returns nothing for a missing log; graph
data JSON has `ts_members` only for arrays; chart JS has the stack setup, the
separate stack key for the array and no stack for latency (existing page-test
style); `--no-member-trace` skips the session.

## Docs

README (graphs section; what the per-member charts show, that they come from
ftrace, the perturbation and lost-event notes, `--no-member-trace`), CHANGELOG,
`documentation/web-ui.md`, CLAUDE.md (the IO session and the log).
