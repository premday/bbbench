# Software RAID layout (mdadm) as a run-group setting — design

Branch `run_groups_mdadm` (from `run_groups`, PR #15 not merged yet). Builds on
`docs/superpowers/specs/2026-11-06-run-groups-design.md`; read its sections on
variants, scope/kind, "Later settings" and the safety rule first. Written from
scratch: nothing is taken from the `raid_testing` branch (the user said to
trash it; the branch itself is left alone).

## Intent

Let the user compare **software RAID layouts**: run the whole test plan once per
layout (level, member disks, chunk size, extras), on the md array each layout
builds, and compare the results in the same web pages that already compare
write-cache values. The second goal is **room for hardware RAID (Dell PERC, HPE
Smart Array) in another branch**: whatever is specific to mdadm stays behind an
interface; the run-group core must not learn the word "mdadm".

## Goals

- A new setting, *software RAID layout* (mdadm), selectable in setting-test mode.
- The user lists **layouts one by one** (layout scope, input = variants): for each,
  the RAID level (0, 1, 5, 6, 10; linear optional), its member disks, the chunk
  size (default 512K), and optional extras (hot spare, write-intent bitmap,
  write-journal). The screen rejects impossible combinations (RAID 5 needs 3
  disks, RAID 6 needs 4, RAID 10 needs 4 here, RAID 1 needs 2, ...).
- **A "no array" baseline variant** (decided by the user): a layout group can
  include the value *none*: the selected disks tested individually, as plain
  drive targets (one target per disk, all at once like a single run), next to
  array variants. Its label is `none · N disks`. So one group may hold variants
  that yield N drive targets and variants that yield one array target; the target
  model, group file, labels and pages must handle both from the start (this is
  also what hardware RAID needs later: JBOD / passthrough against a virtual
  drive).
- A disk may appear in several layouts (variants run one after the other). No
  parallel or swap strategy for layouts.
- bbbench **builds** the array, waits for the initial resync to finish (steady
  state; my call: a resync running during the benchmark would be measured),
  **verifies** that the array exists with the expected level, members, chunk and
  state **before and after every run**, runs the plan on `/dev/mdX`, then
  **tears it down** (stop the array, zero the md superblock of every member).
- Labels per run/target as for the write cache: setting name, value (a layout
  string, e.g. `raid10 · 4 disks · 512K`), before/after readings, validity,
  members. The group, graphs, compare, results, status and run-details pages
  compare by label and **show the members of each array**.
- Everything visible in the web UI, like the rest of run groups.

## Non-goals / anti-goals

- No hardware RAID, no LVM, ZFS, btrfs, no partitioning, no filesystem on the
  array: fio runs on the raw md device.
- No attempt to preserve or restore previous contents of the member disks: the
  test **destroys** them. (My call, the user did not object: after the test the
  disks are left with no array and no md superblock.)
- Never touch a disk that is in use. Failing safe beats offering a disk: when
  unsure, the disk is not selectable.
- No resilvering / degraded-mode / rebuild benchmarks in this version.

## Safety (the new, stronger rule)

The existing rule (CLAUDE.md "bbbench changes a drive's setting only in
setting-test mode, after an explicit confirmation...") extends: building an array
**erases** the member disks. Therefore:

- **In-use disks are shown grayed and cannot be selected**: mounted filesystems
  (`/proc/self/mountinfo`), active swap (`/proc/swaps`), the disk holding the
  root filesystem, the bbbench binary or the output directory (resolve partitions
  and stacked devices — dm, md, LVM, LUKS holders in `/sys/block/*/holders` — to
  the whole disk), and members of an existing md array. Each grayed line says why.
- **One global confirmation** (the user's choice; not a typed phrase) on the
  confirmation screen: it lists every layout and every disk that will be erased,
  and nothing is built before the user accepts it.
- Teardown runs on **every exit path** exactly like the write-cache restore
  (normal end, error, Ctrl+C, SIGTERM/SIGHUP, panic), and a **recovery file**
  records "arrays assembled by bbbench" (md name, uuid, member serials) before the
  first build, so that after a crash the next start offers to stop the leftover
  arrays and wipe their members (it must only touch arrays recorded in the file
  and still matching their members).
- Refuse to start when a previous run left a recorded array assembled and the user
  declines the cleanup.

## What the run-group core has to learn (the room for hardware RAID)

Today a run's targets are the selected drives (`DriveInfo`, one target per drive).
A layout changes **what is benchmarked**: four drives become one array target for
a layout, four targets for the value *none*. So:

- A **target** is either a drive or a composite (an array with members). Runs,
  labels, the group file, the run summary, the status singleton and the pages
  carry the target and its members; comparison is by label, never by assuming the
  same drives. The workload type of an array comes from its members (rotational
  or not; mixed media refused).
- The `settings.Setting` interface gains what layout scope needs and the
  write-cache setting does not: variants with their own disks as input
  (`InputVariants` exists and is unused), `Applicable` per variant, and Apply /
  Verify / Restore that operate on a **variant** (a set of disks and a value) and
  may **produce targets** (`Apply` returns the benchmarkable targets). Keep it one
  interface: drive-scope settings keep working unchanged. Whatever hardware RAID
  needs later (controller scope, Manual kind for modes the user sets, vendor tools
  `perccli`/`storcli`/`ssacli`) must fit the same interface; the design for it
  stays in the "Controller mode" section of the first spec and is **not built
  here**, but a short "what the hardware-RAID branch plugs into" note is part of
  this spec's documentation (CLAUDE.md).
- The recovery file generalizes from "drive values to restore" to "things bbbench
  changed and must undo" (a drive value, or an assembled array); one restore path
  handles both, keeping the guarantees of Task 4 of the first plan (verified,
  per-item recover, failure reported with the manual command, file kept while
  something is not undone).
- Resume of an interrupted layout group: restart the interrupted layout from its
  first phase on a freshly built array (old array torn down first), as for values.

## Build details (mdadm), left to the builder unless stated

- Commands: `mdadm --create` with `--run` and non-interactive, `--level`,
  `--raid-devices`, `--chunk`, `--spare-devices`, `--bitmap`, `--write-journal`;
  stop with `mdadm --stop`, wipe with `mdadm --zero-superblock`; read state from
  `/proc/mdstat`, `/sys/block/mdX/md/*` and `mdadm --detail`. Dependency: `mdadm`
  added to `deps.go` (required only for this setting; `Applicable` says why it is
  missing, like the write-cache tools).
- Wait for resync to finish before the first measurement (poll
  `/sys/block/mdX/md/sync_action` == idle), with a visible "resyncing" status and
  a timeout; the user may later want `--assume-clean` as an option, not now.
- Array naming: a private name per run (`bbbench_<groupid>_<n>`), never reuse an
  existing `/dev/mdN`.
- All command execution goes through an injectable runner, like `settings.Env`,
  so everything is tested without root or disks. Real mdadm behaviour could not be
  tested in development: list it as unverified on hardware in the PR.

## Constraints

- Go, pure, existing conventions (see CLAUDE.md); no new dependencies.
- A plain single run and the write-cache setting test must be unchanged in
  behaviour and output (existing tests keep passing).
- Documentation updated together with the code (README, CLAUDE.md, CHANGELOG,
  documentation/web-ui.md), including the "destructive" warning wording.
- Atomic commits, no push and no PR unless the user asks.

## Left to the builder

File/package/function names, the TUI layouts of the layout screens, the exact
group-file and recovery-file shape, the layout label format, how members are drawn
on the pages, test structure.

## Calls made after the builder check (mine, to be checked by the user)

- **In-use protection** is a new reusable component used only by the layout
  selection. Single runs and the write-cache flow are unchanged (their selection
  and tests do not change).
- **Roles of the extras**: in the layout screen each selected disk gets a role:
  data, spare or journal. Validation counts data disks per level (RAID 1: 2,
  RAID 5: 3, RAID 6: 4, RAID 10: 4, RAID 0: 2). The journal disk is not
  benchmarked as data; it counts for "mixed media" only among the data disks, and
  capacity is the array's. The label lists the roles, e.g.
  `raid5 · 3 data + 1 spare · 512K · bitmap`.
- **Members keep their per-drive information**: status cards, the diskinfo dump
  and SMART are still collected for every member disk (keyed by serial and model,
  as today). Benchmark results (fio, flush) attach to the **array target**, whose
  identity is its md name and uuid; the members are listed on it.
- **Flush tracing / write-through** on an md device: use the existing `Block`
  collector on the md device (exact mean, no percentiles) and its own
  `queue/write_cache` as the kernel reports it. No other branch for composite
  targets.
- **Resync**: wait with a visible progress (status shows "resyncing x%") and no
  timeout; Ctrl+C aborts the group with the normal teardown. The ETA on the
  confirmation screen says the resync time is not included. No `--assume-clean`
  in this version.
- **Failure policy**: a failed *before* verification (array missing, degraded,
  wrong level/members/chunk) marks the run `not_run`, tears the array down and the
  group continues with the next layout. A failed *after* verification marks it
  `invalid` (as for values). If an array cannot be torn down (busy), the group
  **stops**, the recovery file is kept, and the manual commands are reported:
  members still assembled would contaminate the next layout.
- **Parameters**: exactly level, members with roles, chunk size, bitmap, spare,
  write-journal. Nothing else (no RAID 10 near/far/offset choice: the mdadm default
  layout is used and read back for the label).
