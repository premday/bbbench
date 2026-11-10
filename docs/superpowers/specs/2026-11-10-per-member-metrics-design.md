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
  (`NewSampler(names, sysBlock)`, `sysBlock` replaces the root for tests) for
  every member (all roles, so a spare shows as an idle band); the array itself
  is not sampled, its line is fio's own series. Every `log_avg_msec`
  (the phase's polling interval) it keeps the raw counter delta of each member
  (`Window`); `Window.Rates` turns it at load time into read/write IOPS, MiB/s
  and mean latency (call: `ticks` delta / `ios` delta; 0 when no I/O). The kernel's tick counters have 1 ms resolution, so latency is
  coarse at low load; the docs say so. The sampler starts and stops next to
  fio in `executePhase` (as the Flush trace does) and only when the drive has
  `Members`.
- **Log**: `<fio log prefix>_members.1.log` next to fio's logs: `#` header lines
  (`avg_msec`, `offset_ms` = fio's ramp, one `device <name> <role>` per member and
  `array <name>`), then one line per window and member holding the raw
  per-window deltas, not rates (the array has no line, only its members do):
  `end_ms, span_ms, device, read_ios, write_ios, read_sectors, write_sectors, read_ticks_ms, write_ticks_ms`,
  followed by the `md` and `mdmember` lines of the addendum for an md array.
  The persisted run summary JSON (`run_*_summary.json`) also carries
  `member_series` and `md_series` per phase (N members x 3 series; same
  precedent as `flush_series`).
  Window ends are stamped like fio's and the Flush log; loading drops windows
  that ended inside the ramp-up and stamps points at the window start.
  Member names are stored in the log, so a later kernel renaming does not matter.
- **Load**: `loadMembersAndMD(logPrefix)` (new, like `loadFlushLog`) fills
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

## Addendum: md state and administrative operations (user request)

The user asked that a resync/resilver in progress, or **any administrative
operation** on the array, be visible on the charts. Decision on the open point:
visibility only; there is no option to benchmark during the initial resync (the
md provider still waits for it before a run starts; author's call, the user did
not ask for it). A sync seen during a phase is therefore a mid-run event (a
scheduled `check`, a rebuild after a member failure, a reshape).

- **Source**: `/sys/block/<array>/md/` read every window by the same sampler, only
  when that directory exists (the core and `diskstat` name no provider; LVM RAID
  and hardware providers can add their own source later):
  `sync_action` (idle, resync, recover, check, repair, reshape, frozen),
  `sync_completed` (`done / total` sectors, or `none`), `sync_speed` (KiB/s),
  `degraded`, `array_state`, `raid_disks`, and per member
  `md/dev-<member>/state` (in_sync, faulty, spare, write_mostly, blocked,
  journal, ...). A value that cannot be read is recorded as empty, never fails.
- **Log**: extra lines in `<prefix>_members.1.log`, distinguished by a prefix so
  a reader of the old format that stops at unknown `#` lines is not affected:
  `md, end_ms, span_ms, action, completed_sectors, total_sectors, speed_kibps,
  degraded, array_state, raid_disks` and
  `mdmember, end_ms, device, state` per window.
- **Load**: `ts_md` on the phase data: per window the action, progress percent
  and speed, plus `md_events`: every change between two consecutive windows of
  the action, array_state, degraded, raid_disks or any member's state, as
  `{t, text}` (e.g. `sdb: in_sync -> faulty`, `degraded 0 -> 1`, `sync_action
  idle -> check`). A phase that starts with a non-idle action or a degraded
  count above 0 gets an event marked `(at phase start)` at its first window; a
  faulty member or non-clean array_state already present at the start is not
  reported until it changes. An unreadable `degraded` is -1 (unknown): it is
  not a change and never creates an event.
- **Charts** (IOPS, bandwidth and latency charts of an array phase): windows
  where `sync_action` is not idle are shaded and labelled with the action;
  events are vertical markers with their text; hover adds action, progress
  percent, speed and the degraded count for that window (`md: <action> <pct>%
  <speed> MiB/s, degraded <n>` for a busy window, `md: idle, degraded <n>` for an
  idle one, `unknown` when the count was unreadable). The card header shows a
  badge such as `resync 43% -> 61%` (progress at the first and last window of
  the activity) or `degraded`. A phase with a sync or a change of state is also
  flagged in its card as "array busy: results include administrative work".
- **Not**: no change to what is benchmarked, no extra writes, no `/proc/mdstat`
  parsing (sysfs carries the same facts without a text format).
