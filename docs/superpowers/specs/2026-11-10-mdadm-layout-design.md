# Software layouts (mdadm, LVM) as a run-group setting — design

Branch `run_groups_mdadm` (from `run_groups`, PR #15 not merged yet). Builds on
`docs/superpowers/specs/2026-11-06-run-groups-design.md`; read its sections on
variants, scope/kind, "Later settings" and the safety rule first. Written from
scratch: nothing is taken from the `raid_testing` branch (the user said to
trash it; the branch itself is left alone). The file keeps its first name
(`mdadm-layout`) so that existing references stay valid.

## Intent

Let the user compare **software layouts**: run the whole test plan once per
layout (backend, level, member disks, chunk or stripe size, extras), on the
device each layout builds, and compare the results in the same web pages that
already compare write-cache values. A layout is built by a **backend**; this
branch has two, **mdadm first, LVM second** (decided by the user, both in this
branch). The second goal is that **hardware RAID controllers (Dell PERC through
`perccli`/`storcli`, HPE Smart Array through `ssacli`) are the next branch**:
they plug in as further backends through the same interface, and the run-group
core must not learn the name of any backend.

## Goals

- A new setting, *software layout*, selectable in setting-test mode. Each layout
  names its **backend** (`mdadm` or `lvm`); one group may mix backends (e.g.
  mdadm raid5 against lvm raid5) and the baseline.
- The user lists **layouts one by one** (layout scope, input = variants). When
  more than one backend is available, the screen asks for the backend first;
  then it offers that backend's levels and options:
  - **mdadm**: RAID level (0, 1, 5, 6, 10; linear optional), its member disks,
    the chunk size (default 512K), and optional extras (hot spare,
    write-intent bitmap, write-journal).
  - **lvm**: linear, striped (number of stripes + stripe size), mirrored, and
    the LVM raid types raid1, raid5, raid6, raid10 (stripe size for the
    striped and parity/striped raid types), on its member disks.
  The screen rejects impossible combinations (mdadm RAID 5 needs 3 disks, RAID 6
  needs 4, RAID 10 needs 4 here, RAID 1 needs 2, ...; the LVM minimums are in
  the LVM section).
- **A "no array" baseline variant** (decided by the user): a layout group can
  include the value *none*: the selected disks tested individually, as plain
  drive targets (one target per disk, all at once like a single run), next to
  layout variants. It belongs to no backend. Its label is `none · N disks`. So
  one group may hold variants that yield N drive targets and variants that yield
  one composite target; the target model, group file, labels and pages must
  handle both from the start (this is also what hardware RAID needs later: JBOD
  / passthrough against a virtual drive).
- A disk may appear in several layouts (variants run one after the other). No
  parallel or swap strategy for layouts.
- bbbench **builds** the layout, waits for the initial resync to finish (steady
  state; my call: a resync running during the benchmark would be measured),
  **verifies** that it exists with the expected level, members, chunk/stripes
  and state **before and after every run**, runs the plan on the resulting
  block device (`/dev/mdX`, or the LV's `/dev/dm-N`), then **tears it down**.
- Labels per run/target as for the write cache: setting name, value (a layout
  string that starts with the backend, e.g. `mdadm raid10 · 4 disks · 512K`,
  `lvm raid5 · 3 disks · 64K stripe`), before/after readings, validity,
  members. The group, graphs, compare, results, status and run-details pages
  compare by label and **show the members of each layout**.
- Everything visible in the web UI, like the rest of run groups.

## Non-goals / anti-goals

- No hardware RAID in this branch (it is the next one; see the last section),
  no ZFS, btrfs, bcache, no partitioning, no filesystem on the layout: fio runs
  on the raw md or LV block device.
- No LVM thin pools, caches (dm-cache, writecache), VDO, snapshots or
  integrity in this version; no `--nosync` / `--assume-clean`.
- No attempt to preserve or restore previous contents of the member disks: the
  test **destroys** them. (My call, the user did not object: after the test the
  disks are left with no array, no md superblock and no LVM label.)
- Never touch a disk that is in use. Failing safe beats offering a disk: when
  unsure, the disk is not selectable.
- No resilvering / degraded-mode / rebuild benchmarks in this version.

## Safety (the new, stronger rule)

The existing rule (CLAUDE.md "bbbench changes a drive's setting only in
setting-test mode, after an explicit confirmation...") extends: building a
layout **erases** the member disks, whatever the backend. Therefore:

- **In-use disks are shown grayed and cannot be selected**: mounted filesystems
  (`/proc/self/mountinfo`), active swap (`/proc/swaps`), the disk holding the
  root filesystem, the bbbench binary or the output directory (resolve partitions
  and stacked devices — dm, md, LVM, LUKS holders in `/sys/block/*/holders` — to
  the whole disk), members of an existing md array, and **existing LVM objects**:
  a disk (or one of its partitions) that is an LVM physical volume, in a volume
  group or not (from `pvs` when the lvm2 tools are present, and from the udev
  signature otherwise), or that holds an active logical volume (a dm holder).
  Each grayed line says why. This check is the same for every backend: an mdadm
  layout must not take a PV either.
- **One global confirmation** (the user's choice; not a typed phrase) on the
  confirmation screen: it lists every layout and every disk that will be erased,
  and nothing is built before the user accepts it.
- Teardown runs on **every exit path** exactly like the write-cache restore
  (normal end, error, Ctrl+C, SIGTERM/SIGHUP, panic), and a **recovery file**
  records "layouts built by bbbench" (backend, name, uuid when the backend has
  one, member serials) before the first build, so that after a crash the next
  start offers to tear down the leftover layouts and wipe their members (it must
  only touch layouts recorded in the file and still matching their members).
- Refuse to start when a previous run left a recorded layout and the user
  declines the cleanup.

These rules, the confirmation and the failure policy below are **the same for
both backends**: the backend only decides how to build, read and tear down.

## What the run-group core has to learn

Today a run's targets are the selected drives (`DriveInfo`, one target per drive).
A layout changes **what is benchmarked**: four drives become one composite
target for a layout, four targets for the value *none*. So:

- A **target** is either a drive or a composite (an array or LV with members).
  Runs, labels, the group file, the run summary, the status singleton and the
  pages carry the target and its members; comparison is by label, never by
  assuming the same drives. The workload type of a composite comes from its
  members (rotational or not; mixed media refused).
- The `settings.Setting` interface gains what layout scope needs and the
  write-cache setting does not: variants with their own disks as input
  (`InputVariants` exists and is unused), `Applicable` per variant, and Apply /
  Verify / Restore that operate on a **variant** (a set of disks and a value) and
  may **produce targets** (`Apply` returns the benchmarkable targets). Keep it one
  interface: drive-scope settings keep working unchanged.
- The software-layout setting is **one** `Setting` that dispatches to a
  **backend** by name (the first word of the layout value, recorded on every
  target and undo record). Everything specific to a tool lives in its backend.
- The recovery file generalizes from "drive values to restore" to "things bbbench
  changed and must undo" (a drive value, or a built layout with its backend);
  one restore path handles both, keeping the guarantees of Task 4 of the first
  plan (verified, per-item recover, failure reported with the manual command,
  file kept while something is not undone).
- Resume of an interrupted layout group: restart the interrupted layout from its
  first phase on a freshly built layout (old one torn down first), as for values.

## Build details (mdadm), left to the builder unless stated

- Commands: `mdadm --create` with `--run` and non-interactive, `--level`,
  `--raid-devices`, `--chunk`, `--spare-devices`, `--bitmap`, `--write-journal`;
  stop with `mdadm --stop`, wipe with `mdadm --zero-superblock`; read state from
  `/proc/mdstat`, `/sys/block/mdX/md/*` and `mdadm --detail`. Dependency: `mdadm`
  added to `deps.go` (optional, needed only for this backend; the backend says
  why it is unavailable, like the write-cache tools).
- Wait for resync to finish before the first measurement (poll
  `/sys/block/mdX/md/sync_action` == idle), with a visible "resyncing" status.
- Array naming: a private name per run (`bbbench_<groupid>_<n>`), never reuse an
  existing `/dev/mdN`.
- Label: `mdadm <level> · <roles>[ · <chunk>K][ · bitmap]`, e.g.
  `mdadm raid5 · 3 data + 1 spare · 512K · bitmap`.

## Build details (LVM)

- **Layouts**: `linear`, `striped` (number of stripes, 2 up to the number of
  disks, default all of them; stripe size), `mirror` (the old mirror segment
  type, with an in-memory log so that no extra log disk is needed), and the LVM
  raid types `raid1`, `raid5`, `raid6`, `raid10` (stripe size for raid5, raid6
  and raid10). Member disks are data disks only (no spare or journal roles).
  Minimum disks: linear 1, striped 2, mirror 2, raid1 2, raid5 3, raid6 5 (LVM
  needs at least 3 data stripes), raid10 4 and an even number. Stripe size
  choices 64K to 1024K, default 64K (LVM's default).
- **Naming**: the volume group and its single logical volume are both named
  `bbbench_<groupid>_<n>` (n is the run), so the LV is
  `/dev/bbbench_<groupid>_<n>/bbbench_<groupid>_<n>`; the physical volumes are
  the whole member disks. An existing VG of that name refuses the build.
- **Build** with the lvm2 tools, non-interactive: `pvcreate` on every member,
  `vgcreate` over them, `lvcreate` with the type, stripes, mirrors and stripe
  size, using all the free extents of the VG and only the listed PVs. The
  benchmark runs on the LV's dm device.
- **Verify** by read-back with `lvs` / `vgs` / `pvs` (JSON report): segment
  type, stripe count and size, the PVs of the VG (= the recorded members), the
  LV active, no health status, sync complete. Any difference makes the reading
  differ from the wanted value, as for mdadm.
- **Resync wait** for raid and mirror LVs: poll the LV's sync percentage until
  100%, with the same visible progress and no timeout; linear and striped LVs
  are steady at once.
- **Teardown** on every exit path: `lvremove`, `vgremove`, `pvremove` on every
  member, each verified (the VG gone, no PV label left). It only touches a VG
  whose PVs are all recorded members, and only removes PV labels from recorded
  members. If the host uses an LVM devices file, the entries that `pvcreate`
  added are removed again.
- **Recovery file**: the record of an LVM layout is the backend `lvm`, the VG/LV
  name and the member serials and models (a VG name, unlike an md kernel name,
  survives a reboot), written before `pvcreate`.
- **Label**: `lvm <type> · N disks[ · <s> stripes][ · <size>K stripe]`, e.g.
  `lvm raid5 · 3 disks · 64K stripe`, `lvm striped · 4 disks · 2 stripes · 128K
  stripe`, `lvm mirror · 2 disks`. The stripe count appears only for `striped`
  when it is less than the number of disks.
- Dependency: the lvm2 tools (`lvm`) in `deps.go`, optional, needed only for
  this backend.

## Common build rules

- All command execution goes through an injectable runner, like `settings.Env`,
  so everything is tested without root or disks. Real mdadm and LVM behaviour
  could not be tested in development: list it as unverified on hardware in the
  PR.
- After a teardown, wait for udev to settle before the next layout, so that the
  in-use check of the next layout does not see the signature bbbench has just
  wiped.

## Constraints

- Go, pure, existing conventions (see CLAUDE.md); no new dependencies.
- A plain single run and the write-cache setting test must be unchanged in
  behaviour and output (existing tests keep passing).
- Documentation updated together with the code (README, CLAUDE.md, CHANGELOG,
  documentation/web-ui.md), including the "destructive" warning wording.
- Atomic commits, no push and no PR unless the user asks.

## Left to the builder

File/package/function names, the TUI layouts of the layout screens, the exact
group-file and recovery-file shape, how members are drawn on the pages, test
structure.

## Calls made after the builder check (mine, to be checked by the user)

- **In-use protection** is a new reusable component used only by the layout
  selection. Single runs and the write-cache flow are unchanged (their selection
  and tests do not change).
- **Roles of the extras** (mdadm): in the layout screen each selected disk gets a
  role: data, spare or journal. Validation counts data disks per level (RAID 1:
  2, RAID 5: 3, RAID 6: 4, RAID 10: 4, RAID 0: 2). The journal disk is not
  benchmarked as data; it counts for "mixed media" only among the data disks,
  and capacity is the array's. The label lists the roles, e.g.
  `mdadm raid5 · 3 data + 1 spare · 512K · bitmap`. LVM disks are data only.
- **Members keep their per-drive information**: status cards, the diskinfo dump
  and SMART are still collected for every member disk (keyed by serial and model,
  as today). Benchmark results (fio, flush) attach to the **composite target**,
  whose identity is its backend name and uuid (md array uuid, LV uuid); the
  members are listed on it.
- **Flush tracing / write-through** on an md or dm device: use the existing
  `Block` collector on that device (exact mean, no percentiles) and its own
  `queue/write_cache` as the kernel reports it. No other branch for composite
  targets.
- **Resync**: wait with a visible progress (status shows "resyncing x%") and no
  timeout; Ctrl+C aborts the group with the normal teardown. The ETA on the
  confirmation screen says the resync time is not included. No `--assume-clean`
  / `--nosync` in this version.
- **Failure policy** (both backends): a failed *before* verification (layout
  missing, degraded, wrong level/members/chunk/stripes) marks the run `not_run`,
  tears the layout down and the group continues with the next layout. A failed
  *after* verification marks it `invalid` (as for values). If a layout cannot be
  torn down (busy), the group **stops**, the recovery file is kept, and the
  manual commands are reported: members still assembled would contaminate the
  next layout.
- **Parameters** (mdadm): exactly level, members with roles, chunk size, bitmap,
  spare, write-journal. Nothing else (no RAID 10 near/far/offset choice: the
  mdadm default layout is used and read back for the label). (LVM): exactly the
  type, members, stripes (striped only) and stripe size; the raid5/6 layout
  (left-symmetric) and the raid10 near layout are LVM's defaults, read back for
  the kernel reading.

## What the hardware-RAID branch plugs into

The next branch adds controller backends (Dell PERC through `perccli`/`storcli`,
HPE Smart Array through `ssacli`). This branch gives them a fixed plug-in point:

- **The backend interface** (`settings.Backend`), which mdadm and LVM implement:
  `Name()` (the first word of every value), `Available()` (tool installed,
  reason otherwise), `Levels()`, `Options(level, dataDisks)` (the option rows
  the editor shows, each with its choices and default), `Roles(level)` (data,
  spare, journal, ...), `Validate(layout, disks)`, `Label(layout, disks)` and
  `Parse(value)` (the canonical value, both ways), `Prepare` (checks and the
  undo record, changes nothing), `Apply` (builds, returns the targets),
  `WaitSteady` (initialisation / resync), `Read` (read-back for verification),
  `Undo` (teardown, verified) and `UndoCommand` (the manual command). The undo
  record carries the backend name, the name, an optional uuid, the members and
  a backend-owned `Extra` map that the core stores but never reads.
- **A controller backend** implements the same methods: its levels are the
  controller's RAID levels and JBOD/passthrough, its options the controller's
  (stripe size, read and write cache policy, ...), its roles data and hot spare.
  `Apply` creates the virtual drive and returns it as a composite target with the
  physical disks as members (found through `diskinfo.Controller` and the vendor
  tool, run through `Env.Run`); `Read` verifies the virtual drive through the
  vendor tool; `Undo` deletes it; the recovery record keeps the controller and
  virtual-drive ids in `Extra`. In-use protection applies to the physical disks.
- **A controller mode** that only the user can change (RAID vs HBA mode in the
  BIOS) is not a backend but a separate setting of kind `Manual`, with the
  multi-invocation groups of the first spec ("Controller mode" section), which
  `runGroup` refuses today.
- **What the core must not know**: `internal/rungroup`, the group loop, the undo
  ledger, the recovery offer, the composite-target code, the layout TUI and the
  web pages never name a backend or a tool (mdadm, LVM, perccli, ...), and never
  branch on a backend name: they carry it as data. A test enforces it for the
  names that exist in this branch.
