# mdadm Layout Setting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a destructive *software RAID layout (mdadm)* setting to setting tests: the user lists layouts (and optionally the no-array baseline), bbbench builds each md array, waits for its resync, verifies it before and after the run, benchmarks `/dev/mdX`, tears it down, and the web pages compare the layouts and show each array's members.

**Architecture:** The run-group core learns *targets* (a drive, or an array with members) and a variant-based `settings.Setting` (Prepare, Apply returning targets, Read, Undo, plus a `Lifetime`). The write cache goes through a `PerDrive` adapter so its behaviour does not change. Everything specific to mdadm stays in `internal/settings/mdadm.go` and `layout.go`, behind that interface and an injectable runner. A new `internal/inuse` package decides which disks may be erased. One undo ledger mirrors every change to the recovery file, which now holds drive values and assembled arrays.

**Tech Stack:** Go 1.24 (stdlib only), bubbletea (TUI), the existing raw-string HTML/JS pages, `mdadm` called through `settings.Env.Run`.

**Spec:** `docs/superpowers/specs/2026-11-10-mdadm-layout-design.md` (binding), built on `docs/superpowers/specs/2026-11-06-run-groups-design.md`.

## Global Constraints

- Not built: hardware RAID, LVM, ZFS, btrfs, partitioning, any filesystem on the array (fio runs on the raw md device), `--assume-clean`, a RAID 10 near/far/offset choice (mdadm's default layout is used and read back for the label), resilver or degraded-mode benchmarks.
- Layout parameters are exactly: level (`none`, `raid0`, `raid1`, `raid5`, `raid6`, `raid10`, `linear`), member disks each with a role (`data`, `spare`, `journal`), chunk size (default `512K`) and write-intent bitmap. Minimum data disks: raid0 2, raid1 2, raid5 3, raid6 4, raid10 4, linear 2, none 1.
- Layouts have no parallel or swap strategy. Variants run one after the other, and a disk may appear in several variants.
- Nothing is built before the one global confirmation, which is a single key and not a typed phrase. In-use disks are grayed with the reason and cannot be selected. When bbbench is unsure, the disk is not selectable.
- Array name: `bbbench_<group id>_<n>` (n is the 1-based run), created as `/dev/md/<name>`, at most 32 characters. bbbench never reuses an existing `/dev/mdN`.
- Wait for the resync with visible progress and no timeout. Ctrl+C aborts with the normal teardown. The ETA says the resync time is not included.
- Teardown (stop the array, zero the superblock of every member) runs on every exit path: normal end, error, Ctrl+C, SIGTERM/SIGHUP, panic. The recovery file `/var/lib/bbbench/group-recovery.json` records each array (name, uuid, member serials) **before it is built**. Cleanup touches only recorded arrays that still match their members. If the user declines the cleanup, bbbench refuses to start.
- Failure policy: a failed *before* verification makes the run `not_run`, the array is torn down and the next layout runs. A failed *after* verification makes the run `invalid`. A failed teardown stops the group, keeps the recovery file and prints the manual commands.
- The run-group core never names mdadm: `internal/rungroup`, `cmd/bbbench/group_run.go`, `group_flow.go`, `group_undo.go`, `group_targets.go` and every `serve_*.go` (enforced by `TestCoreDoesNotNameTheBackend`, Task 6).
- A plain single run and the write-cache setting test are unchanged in behaviour and output. Every existing test keeps its assertions, and every new JSON field is `omitempty`, so the files they write are byte-identical.
- `internal/inuse` is used only by the layout selection and by the mdadm re-check before a build. Single runs and the write-cache flow never call it.
- Pure Go, no new module dependency. Every command goes through the injectable runner, so no test needs root or disks.
- For every changed page, run `node --check` on the real page served for a stored result (not an error page), and run the new JS functions in node.
- Run `make check` (gofmt, vet, `go test -race`) before every commit. One commit per task, the message states the intent and ends with the `Co-Authored-By` line the harness gives. **No push, no pull request.**

## Review Focus

Inputs the spec implies but no happy path covers; each has a test in the task named.

1. **A selected disk becomes busy after the confirmation** (it is mounted or added to swap before its layout's turn). The build is refused for that layout, the run is `not_run` with the reason, and nothing is created (Task 4 `TestMdadm_PrepareRefusesBusyDisk`, Task 5 `TestRunGroup_LayoutPrepareFailsNotRun`).
2. **The array changes during the benchmark**: the distro's periodic md `check` starts, or a member fails. The after-verification reads `(sync_action check)` or `(degraded)` and the run is `invalid` (Task 4 `TestMdadm_ReadDetectsDegradedAndCheck`, Task 5 `TestRunGroup_LayoutAfterFailsInvalid`).
3. **The array starts `read-auto`** (`md_mod start_ro=1`), so its resync never starts and the no-timeout wait would hang. Apply switches it to read-write (Task 4 `TestMdadm_ApplyReadAutoSwitchesReadWrite`).
4. **mdadm adds an internal bitmap on its own** to large redundant arrays, which would make a "no bitmap" label lie. bbbench passes `--bitmap=none` and Read checks `bitmap/location` (Task 4 `TestMdadm_CreateArgs`, `TestMdadm_ReadDetectsUnexpectedBitmap`).
5. **Names change between a crash and the next start** (the array comes back as `md126`, `sdb` becomes `sdc`). Cleanup finds the array by uuid and its members by serial and model, never by name, and refuses an array with an unrecorded member (Task 4 `TestMdadm_UndoFindsArrayByUUID`, `TestMdadm_UndoRefusesForeignMember`, Task 5 `TestRestoreFromRecovery_ArraysBySerial`).

## Unverified on hardware (to be listed in the PR when the user asks for one)

Real mdadm could not be run in development, so these parts are marked `UNVERIFIED` in code comments and must be checked on a host before release:

- The `mdadm --create` argv: the `--uuid=xxxxxxxx:xxxxxxxx:xxxxxxxx:xxxxxxxx` form, `--bitmap=none`, leaving out `--bitmap` when `--write-journal` is given, and that `--run` with stdin at `/dev/null` never prompts when members carry old signatures.
- The keys of `mdadm --detail --export` and `mdadm --examine --export` (`MD_UUID`, `MD_DEVNAME`), and the exit status and output of `--examine` on a disk with no superblock.
- How long udev takes to create `/dev/md/<name>`, and whether the 32-character name limit holds once mdadm adds the homehost prefix.
- The sysfs attributes and values `md/level`, `raid_disks`, `chunk_size`, `degraded`, `array_state` (`read-auto`), `sync_action`, `sync_completed` (`none`, `a / b`), `layout`, `bitmap/location`, and `dev-*/state` containing `spare` / `journal`; also the raid10 and raid5/6 layout numbers.
- Flush tracing on md: md is bio-based, so the `block_rq_issue`/`block_rq_complete` events of the `Block` collector may never fire for `/dev/mdX`. The phase then shows the existing "ftrace saw 0 Flush commands" note (see the rulings below).
- In-use detection on real LVM, LUKS, multipath, ZFS, btrfs (subvolume `st_dev`) and container root setups.
- Trim phases on md arrays without discard support (raid5/6 by default) fail fio, as on any device without discard. This version has no special handling for it.

## File structure

- `internal/settings/settings.go` (modify): interface, `Variant`, `Disk`, `BenchTarget`, `Undo`, `Lifetime`, `Settler`, `Env`, `Verify`, `All`.
- `internal/settings/perdrive.go` (new): `PerDrive` adapter for drive-scope settings. `writecache.go` keeps its logic.
- `internal/settings/layout.go` (new): layout parameters, validation, the canonical label. `mdadm.go` (new): the backend.
- `internal/inuse/` (new): `inuse.go`, `mountinfo.go`, `stat_linux.go`, `stat_other.go`.
- `internal/rungroup/target.go` (new): `Member`, `GroupVariant`, `MembersOf`. `groupfile.go`, `plan.go`, `recovery.go` (modify).
- `cmd/bbbench/group_undo.go` (new): the undo ledger and failure reports. `group_targets.go` (new): array targets as `DriveInfo`. `group_layout_tui.go` (new): layout screens.
- `cmd/bbbench/group_run.go`, `group_flow.go`, `group_tui.go`, `orchestrator.go`, `orchestrator_diskinfo.go`, `generate_common.go`, `serve_status.go`, `serve_graphs.go`, `serve_groups.go`, `serve_shared.go`, `serve_compare.go`, `serve_browse.go`, `deps.go`, `help.go` (modify).

---

### Task 1: Targets in the data model (drive or array with members)

**Files:**
- Modify: `internal/settings/settings.go` (types only), `internal/rungroup/groupfile.go`, `internal/rungroup/plan.go`, `cmd/bbbench/orchestrator.go` (`DriveInfo`), `cmd/bbbench/serve_status.go` (`DriveStatus`, `newDriveStatuses`), `cmd/bbbench/serve_graphs.go` (`DiskPhaseData`, `generateDiskPhaseData`), `cmd/bbbench/group_tui.go` (`targetsFor`)
- Create: `internal/rungroup/target.go`, `internal/rungroup/target_test.go`
- Test: `internal/rungroup/groupfile_test.go`, `plan_test.go`, `cmd/bbbench/serve_graphs_test.go`, `serve_status_test.go`, `group_tui_test.go`

**Interfaces:**
- Produces (package `settings`): `type Role string` with `RoleData = "data"`, `RoleSpare = "spare"`, `RoleJournal = "journal"`; `type TargetKind string` with `TargetDrive = "drive"`, `TargetArray = "array"`; `type Disk struct{ Target; Role Role }`; `Target` gains `Rotational bool`.
- Produces (package `rungroup`):
  - `type Member struct{ Name, Serial, Model string; Role settings.Role }` (tags `name`, `serial`, `model`, `role`); `func MembersOf(disks []settings.Disk) []Member`.
  - `type GroupVariant struct{ Value string \`json:"value"\`; Disks []Member \`json:"disks"\` }`.
  - `GroupFile` gains `Variants []GroupVariant \`json:"variants,omitempty"\`` (layout groups only). `GroupRun` gains `Teardown *GroupRestore \`json:"teardown,omitempty"\``. `GroupDrive` gains `Kind settings.TargetKind \`json:"kind,omitempty"\`` (`""` for a drive, so drive files do not change; `"array"` for an array), `UUID string \`json:"uuid,omitempty"\`` and `Members []Member \`json:"members,omitempty"\``.
  - `func PlanVariants(vs []Variant) []RunPlan`: one run per variant, `Round` = index, drive order kept as given.
- Produces (package `main`):
  - `DriveInfo` gains `Members []rungroup.Member \`json:"Members,omitempty"\`` and `UUID string \`json:"UUID,omitempty"\``. A `DriveInfo` with members is an array target, and `func (d DriveInfo) IsArray() bool` reports it.
  - `DriveStatus` gains `Kind string \`json:"kind,omitempty"\`` and `Members []MemberStatus \`json:"members,omitempty"\``. `type MemberStatus struct{ rungroup.Member; Config *diskinfo.Info \`json:"config,omitempty"\` }` (the embedded fields flatten in JSON).
  - `DiskPhaseData` gains `Kind string \`json:"kind,omitempty"\`` and `Members []rungroup.Member \`json:"members,omitempty"\``.

- [ ] **Step 1: Write the failing tests**
  - `TestGroupFile_ArrayRoundTrip`: a group with `Variants: [{Value: "raid1 · 2 disks", Disks: [{sdb S1 M data} {sdc S2 M data}]}]` and one run whose drive is `{Device: "md127", Serial: "0a:0b:0c:0d", Model: "raid1", Kind: "array", UUID: "0a:0b:0c:0d", Members: [...]}` with `Teardown: &GroupRestore{Status: RestoreDone}`. Save then `LoadGroup` gives a `reflect.DeepEqual` result.
  - `TestGroupFile_DriveFileHasNoTargetKeys`: the JSON of the group in `TestGroupFile_RoundTrip` contains none of `"variants"`, `"teardown"`, `"kind"`, `"uuid"`, `"members"`.
  - `TestPlanVariants`: `[{raid1 [sdc sdb]} {none [sdb sdc sdd]}]` gives `[{Round 0 [{raid1 [sdc sdb]}]} {Round 1 [{none [sdb sdc sdd]}]}]`.
  - `TestMembersOf`: `Disk{Target{Name: "sdb", Serial: "S1", Model: "M"}, RoleSpare}` gives `Member{"sdb", "S1", "M", "spare"}`.
  - `TestDriveInfo_JSONUnchangedForDrives`: the JSON of a plain `DriveInfo` has no `"Members"` or `"UUID"` key.
  - `TestGenerateDiskPhaseData_ArrayMembers`: a result whose `Drives` hold array `md127` (two data members `sdb`, `sdc`) and drive `sdd`, with one phase that has a job on each. The `md127` entry has `Kind == "array"`, two `Members` with role `data`, `Model`/`Serial` from its `Device`; the `sdd` entry has `Kind == ""` and nil `Members`.
  - `TestNewDriveStatuses_ArrayMembers`: an array `DriveInfo` gives `Kind == "array"` and `Members` `sdb`, `sdc` with their serials and roles; a drive gives `Kind == ""` and no members.
  - `TestTargetsFor_Rotational`: a drive with `Device.Rotational = true` gives `Target.Rotational == true`.
- [ ] **Step 2: Run** `go test ./internal/rungroup ./cmd/bbbench -run 'ArrayRoundTrip|NoTargetKeys|PlanVariants|MembersOf|JSONUnchanged|ArrayMembers|TargetsFor_Rotational'`. Expect FAIL: undefined fields.
- [ ] **Step 3: Implement** the types and fields. `generateDiskPhaseData` copies `Members` from the `DriveInfo` and sets `Kind = "array"` when `IsArray()`. `newDriveStatuses` does the same. `targetsFor` sets `Rotational`.
- [ ] **Step 4: Run** `go test -race ./...` (every existing test passes unchanged) and `make check`. Expect PASS.
- [ ] **Step 5: Commit** `git commit -m "Let run-group targets be drives or arrays with members in group files, run results and status"`.

---

### Task 2: A variant-based Setting interface, with the write cache through a per-drive adapter

**Files:**
- Modify: `internal/settings/settings.go`, `internal/settings/writecache.go`, `internal/settings/settings_test.go`, `internal/settings/writecache_test.go`
- Create: `internal/settings/perdrive.go`, `internal/settings/perdrive_test.go`
- Modify: `internal/rungroup/recovery.go`, `internal/rungroup/recovery_test.go`
- Create: `cmd/bbbench/group_undo.go`, `cmd/bbbench/group_undo_test.go`
- Modify: `cmd/bbbench/group_run.go`, `cmd/bbbench/group_flow.go`, `cmd/bbbench/group_run_test.go`, `cmd/bbbench/group_tui_test.go` (test helpers only)

**Interfaces:**
- Consumes: `settings.Disk`, `settings.TargetKind` (Task 1).
- Produces (package `settings`):
  - `type Lifetime string` with `LifetimeGroup = "group"` (prepared once for every disk of the group, undone when the group ends: drive values) and `LifetimeRun = "run"` (prepared before each run and undone right after it: layouts).
  - `type Variant struct{ Name string; Value Value; Disks []Disk }`. `Name` is unique in the group, `"<group id>_<run n>"`, and is set by the group loop.
  - `type BenchTarget struct{ Kind TargetKind; Target Target; UUID string; Members []Disk; Want string; Err error }`. For a drive, `Target` is the drive itself. For an array, `Target.Name`/`Path` are the md kernel name and node, `Serial` is the uuid, `Model` the level, and the backend sets `Vendor` and `Interface`. `Want` is the `Reading.Value` that verifies the target. `Err` is set when Apply could not make this one target effective.
  - `type Undo struct{ Drive *Target; Original *Reading }` (Task 4 adds `Array`); `func (u Undo) Name() string` returns the drive name.
  - The new `Setting` interface (it replaces the old one):
    ```go
    type Setting interface {
        Name() string
        Kind() Kind
        Scope() Scope
        Input() Input
        Lifetime() Lifetime
        Values() []Value
        Applicable(t Target) (bool, string)        // one disk
        ApplicableVariant(v Variant) (bool, string) // a whole variant
        // Prepare reads what Apply will change and returns what undoes it; it changes nothing.
        Prepare(ctx context.Context, v Variant) ([]Undo, error)
        // Apply makes v effective and returns the targets to benchmark.
        Apply(ctx context.Context, v Variant, prepared []Undo) ([]BenchTarget, error)
        Read(ctx context.Context, t BenchTarget) (Reading, error)
        // Undo puts back one record and checks it; it writes only when something differs.
        Undo(ctx context.Context, u Undo) error
    }
    ```
  - `CommandHinter` becomes `UndoCommand(u Undo) string`.
  - `type DriveOps interface{ Name() string; Kind() Kind; Values() []Value; Applicable(Target) (bool, string); Read(context.Context, Target) (Reading, error); Apply(context.Context, Target, Value) error; Restore(context.Context, Target, Reading) error }`; `type DriveCommandHinter interface{ RestoreCommand(t Target, original Reading) string }`; `func PerDrive(ops DriveOps, settle time.Duration) Setting`.
  - `func Verify(ctx context.Context, s Setting, t BenchTarget, settle time.Duration) (Reading, bool)`: the same loop as today, comparing against `t.Want`.
  - `Env` gains `Settle time.Duration` (default 10 s). `NewWriteCache(env)` returns `PerDrive(newWriteCacheOps(env), env.Settle)`. `newWriteCacheOps(env Env) *writeCache` is used by the existing write-cache tests.
- Produces (package `rungroup`): `func RecoveryOf(setting string, undos []settings.Undo) *Recovery` (drive undos become `Drives` entries); `func (r *Recovery) Undos(targets []settings.Target) (found, missing []settings.Undo)` (entries matched to current targets by serial and model through `Match`; a missing entry keeps its recorded name, serial and model as an unresolved `Target`).
- Produces (package `main`, `group_undo.go`):
  - `type undoLedger struct{ path, setting string; items []settings.Undo }`; `func newUndoLedger(path, setting string, items []settings.Undo) *undoLedger`.
  - `func (l *undoLedger) add(us ...settings.Undo) error`: appends and saves `RecoveryOf(l.setting, l.items)` to `path` atomically, creating the directory.
  - `func (l *undoLedger) undo(ctx context.Context, s settings.Setting, pick func(settings.Undo) bool) (failed []string, err error)`: runs `undoOne` on the picked items in order, drops the ones that succeeded, then saves what is left or removes the file when nothing is left. `err` joins the file errors.
  - `func (l *undoLedger) pending() []settings.Undo`.
  - `func undoOne(parent context.Context, s settings.Setting, u settings.Undo) error`: runs within `restoreTimeout`, recovers a panic in `Undo` or in the report into an error, and calls `reportUndoFailure` on failure.
  - `func reportUndoFailure(s settings.Setting, u settings.Undo, err error)`: for a drive, exactly the text of today's `reportRestoreFailure` (`*** RESTORE FAILED: ...`, `*** Set it back to %q by hand; ...`, `*** Command to set it back by hand: %s` from `UndoCommand`).
  - `func restoreFromRecovery(ctx context.Context, s settings.Setting, rec *rungroup.Recovery, path string, targets map[string]settings.Target) (failed []string)`: a ledger over found and missing entries, undoing the found ones. `failed` lists the recorded names of missing entries and the current names of failed ones, as today. The file keeps only what is not undone.

- [ ] **Step 1: Write the failing tests**
  - `TestPerDrive_Identity`: `NewWriteCache(Env{})` has `Scope() == ScopeDrive`, `Input() == InputValues`, `Kind() == Applied`, `Lifetime() == LifetimeGroup`.
  - `TestPerDrive_ApplicableVariant`: a variant with `sdb` (SATA, hdparm present) and `sdc` (USB) gives `false, "sdc: no controllable volatile write cache on this interface"`.
  - `TestPerDrive_PrepareReadsOriginals`: fake ops with `sdb=enabled`, `sdc=disabled` give two undos whose `Original.Value` are those; ops reading `""` give an error containing `read back empty`.
  - `TestPerDrive_ApplyReportsPerTargetErrors`: ops whose `Apply` fails on `sdc` give two targets, `sdb` with `Err == nil` and `Want == "disabled"`, `sdc` with `Err != nil`, and a nil function error.
  - `TestPerDrive_UndoRestoresAndVerifies`: the restore takes effect, so the result is nil. Ops that never change give `reads "disabled" after restoring, want "enabled"` within a settle time of 50 ms.
  - `TestPerDrive_UndoCommand`: write cache, SATA `sdb`, original `enabled` gives `"hdparm -W1 /dev/sdb"`.
  - `TestVerify` (adapted): a target with `Want: "on"` and a fake whose Read returns old, old, on gives true; one that never changes gives false with the last reading.
  - `TestRecoveryOf_DriveEntries` and `TestRecovery_Undos`: `sdb` was recorded and the same serial and model is now `sdc`, so `found[0].Drive.Name == "sdc"` with its full current `Target`. Serial `S2` has no current target, so `missing[0].Drive.Name == "sdd"`.
  - `TestUndoLedger_MirrorsRecoveryFile`: `add` of two undos writes a file listing both. `undo` with `sdb` failing returns `failed == ["sdb"]` and the file lists only `sdb`. A second `undo` that succeeds removes the file.
  - `TestUndoLedger_PanicInUndoIsAnError`: an `Undo` that panics on `sdb` still lets `sdc` be undone.
- [ ] **Step 2: Run** `go test ./internal/settings ./internal/rungroup ./cmd/bbbench`. Expect FAIL (compile errors).
- [ ] **Step 3: Implement** `settings.go`, `perdrive.go` and `writecache.go`. `PerDrive.Prepare` keeps the messages `read the original %s of %s: %w` and `the %s of %s read back empty`; `runGroup` still prefixes `run group: `. `PerDrive.Apply` never returns a function error for a variant with disks.
- [ ] **Step 4: Port the group loop** in `group_run.go` and `group_flow.go`:
  - Build the per-run `[]settings.Variant` from `PlanRuns`: `Name = g.ID+"_"+n`, `Disks` are the drives' targets with `RoleData`.
  - `LifetimeGroup`: call `Prepare` once with every drive, fill `g.Originals` from the undos, then `ledger.add` before the first `Apply`. The existing-recovery-file refusal comes first, as today.
  - `runOne` calls `Apply` per variant. A target with `Err` gets the note `apply failed: %v` and a plain `Read`. Verification is `settings.Verify(ctx, s, bt, cfg.Settle)`, with the note `setting not effective: reads %q, want %q` (`bt.Want`). Labels are keyed by `bt.Target.Name`.
  - `finishGroup` calls `ledger.undo` on everything.
  - `offerRecovery` calls `restoreFromRecovery(ctx, s, rec, path, targets)` and no longer removes the file itself.
- [ ] **Step 5: Adapt the test helpers only**:
  - `testGroup`, `recoveryFor`, `setupFor` wrap the fakes with `settings.PerDrive(fs, 50*time.Millisecond)`.
  - `panicSetting`, `hinterSetting` and `ifaceSetting` wrap the fake as `DriveOps`.
  - `variantSetting` embeds a `settings.Setting` and overrides `Name`/`Input`.
  - Calls to `restoreFromRecovery` take the path.
  - The reviewer checks `git diff cmd/bbbench/group_run_test.go cmd/bbbench/group_tui_test.go`: no assertion changed.
- [ ] **Step 6: Run** `go test -race ./internal/settings ./internal/rungroup ./cmd/bbbench` (all of `TestRunGroup_*`, `TestRecoveryOffer_*`, `TestRestoreFromRecovery_*`, `TestWriteCache_*` pass) and `make check`.
- [ ] **Step 7: Commit** `git commit -m "Make settings variant-based behind one interface, with the write cache unchanged through a per-drive adapter"`.

---

### Task 3: The in-use disk check (`internal/inuse`)

**Files:**
- Create: `internal/inuse/inuse.go`, `internal/inuse/mountinfo.go`, `internal/inuse/stat_linux.go` (`//go:build linux`), `internal/inuse/stat_other.go` (`//go:build !linux`, returns an error), `internal/inuse/inuse_test.go`

**Interfaces:**
- Produces (package `inuse`):
  - `type Options struct{ Sys, Proc, Udev string; Paths map[string]string; Stat func(path string) (dev, rdev uint64, err error) }`. Defaults: `/sys`, `/proc`, `/run/udev/data`, and `syscall.Stat` (`Dev`, `Rdev`). `Paths` maps a path to what it is, e.g. `"/usr/local/bin/bbbench": "the bbbench binary"`.
  - `type Report struct{ Busy map[string][]string; All string; Warnings []string }`. `Busy` maps a whole-disk kernel name to its reasons. `All` non-empty means no disk can be offered, and says why.
  - `func Scan(opts Options) (*Report, error)`: errors only when `<Proc>/self/mountinfo` cannot be read. `func (r *Report) Reason(disk string) string`: `All` if set, else the reasons joined by `"; "`, `""` when the disk is free. `func DevNumbers(dev uint64) (major, minor uint32)` uses the Linux `dev_t` encoding.
- Rules (they decide the reasons, exact text):
  - A device number `M:m` resolves through `<Sys>/dev/block/M:m`. A partition (it has a `partition` file) resolves to its parent directory. A device with entries in `slaves/` (dm, md) resolves recursively to its leaf disks.
  - Mountinfo: mount point `/` gives `holds the root filesystem (/)`; any other gives `mounted at <mount point> (<device>)`. Octal escapes such as `\040` are decoded.
  - A major-0 mount (btrfs, overlay) whose source starts with `/dev/` resolves through `Stat(source).rdev`. Otherwise a `zfs`, `btrfs` or `bcachefs` mount adds the warning `cannot tell which disks hold <mp> (<fstype> <source>); disks with a ZFS, LVM or md signature are not offered`. If that mount is `/`, any fstype sets `All = "the disk holding the root filesystem could not be determined (<fstype> <source>); no disk is offered"`. Other major-0 fstypes (proc, tmpfs, ...) are ignored.
  - `<Proc>/swaps`: a `partition` entry gives `active swap (<filename>)` (through `rdev`); a `file` entry gives `holds an active swap file (<filename>)` (through `dev`).
  - Each path in `Paths` gives `holds <what> (<path>)` (through `dev`; a major-0 dev is looked up in mountinfo, then by its source).
  - Holders of a disk or of its partitions: `md*` gives `member of <holder> (md RAID)`, `dm-*` gives `used by <holder> (device mapper: LVM, LUKS or multipath)`, anything else gives `used by <holder>`.
  - Udev `<Udev>/b<M:m>` lines `E:ID_FS_TYPE=` on the disk or a partition: `linux_raid_member` gives `has an md RAID superblock (an array that is not assembled; check with mdadm --examine)`, `zfs_member` gives `has a ZFS pool label`, `LVM2_member` gives `is an LVM physical volume`, `crypto_LUKS` gives `is LUKS encrypted`. A missing swaps file or udev directory is not an error.

- [ ] **Step 1: Write the failing tests** with a helper `fakeHost(t)` that builds `sys`, `proc`, `udev` under `t.TempDir()`. It has `disk(name, mm)`, `part(disk, name, mm)`, `holder(dev, h)`, `stack(dm, mm, leaves...)`, `mount(line)`, `swap(line)`, `udev(mm, fstype)`, and a `Stat` fake from a map.
  - `TestScan_MountedPartition`: `sda2` mounted at `/data` gives `Busy["sda"] == ["mounted at /data (sda2)"]`.
  - `TestScan_RootDisk`: `/` on `nvme0n1p2` gives `Reason("nvme0n1") == "holds the root filesystem (/)"`.
  - `TestScan_StackedLVM`: `/home` on `dm-0` with leaves `sdb1` and `sdc` gives both `sdb` and `sdc` the reason `mounted at /home (dm-0)`, plus `used by dm-0 (device mapper: LVM, LUKS or multipath)`.
  - `TestScan_Swap`: `/dev/sdd1 partition` and `/swapfile file` on the filesystem of `sde` give `active swap (/dev/sdd1)` and `holds an active swap file (/swapfile)`.
  - `TestScan_Paths`: `{"/usr/local/bin/bbbench": "the bbbench binary"}` on `sdf` gives `holds the bbbench binary (/usr/local/bin/bbbench)`.
  - `TestScan_MDMember`: `sdg/holders/md127` gives `member of md127 (md RAID)`.
  - `TestScan_UdevSignatures`: a table of the four `ID_FS_TYPE` values and their reasons; `ext4` gives no reason.
  - `TestScan_BtrfsRootBySource`: `0:27 / btrfs /dev/sdh2` gives `sdh` the root reason.
  - `TestScan_RootUnresolved`: `/` is `overlay overlay`, so `All` is set and `Reason("sdz") == All`.
  - `TestScan_ZfsWarns`: `/tank zfs tank` adds a warning starting `cannot tell which disks hold /tank`.
  - `TestScan_EscapedMountPoint`: `/mnt/my\040disk` gives `mounted at /mnt/my disk (sdj1)`.
  - `TestScan_FreeDisk`: `Reason("sdi") == ""`.
  - `TestDevNumbers`: `DevNumbers(0x10302) == (259, 2)`; `DevNumbers(0x100805) == (8, 0x105)` (a minor above 255).
- [ ] **Step 2: Run** `go test ./internal/inuse`. Expect FAIL (the package is missing).
- [ ] **Step 3: Implement** the package. Parse mountinfo in `mountinfo.go`. Order the reasons per disk as found: mounts, swap, paths, holders, signatures.
- [ ] **Step 4: Run** `go test -race ./internal/inuse` and `make check`. Expect PASS.
- [ ] **Step 5: Commit** `git commit -m "Add the in-use disk check that protects busy disks from layout tests"`.

---

### Task 4: The mdadm backend

**Files:**
- Modify: `internal/settings/settings.go` (`ScopeLayout`, `Undo.Array`, `Settler`, `Env` fields, `All`)
- Create: `internal/settings/layout.go`, `internal/settings/layout_test.go`, `internal/settings/mdadm.go`, `internal/settings/mdadm_test.go`

**Interfaces:**
- Consumes: the `Setting` interface (Task 2), `inuse.Scan`/`Report.Reason` (Task 3).
- Produces (package `settings`):
  - `ScopeLayout Scope = "layout"`.
  - `type ArrayUndo struct{ Name, UUID string; Members []Disk }`; `Undo` gains `Array *ArrayUndo`, and `Name()` returns `Array.Name` for an array.
  - `type Settler interface{ WaitSteady(ctx context.Context, ts []BenchTarget, progress func(pct float64)) error }`.
  - `Env` gains `Dev string` (default `/dev`), `Rand io.Reader` (default `crypto/rand.Reader`), `Poll time.Duration` (default 2 s) and `InUse func() (*inuse.Report, error)` (default `inuse.Scan(inuse.Options{})`).
  - `type Layout struct{ Level string; ChunkKiB int; Bitmap bool }`; `const DefaultChunkKiB = 512`; `var Levels = []string{"none", "raid0", "raid1", "raid5", "raid6", "raid10", "linear"}`; `var ChunkChoicesKiB = []int{64, 128, 256, 512, 1024}`.
  - `func MinDataDisks(level string) int`; `func HasChunk(level string) bool` (raid0, raid5, raid6, raid10); `func Redundant(level string) bool` (raid1, raid5, raid6, raid10).
  - `func ValidateLayout(l Layout, disks []Disk) error`; `func LayoutValue(l Layout, disks []Disk) Value` (`ID == Label`); `func ParseLayoutValue(id string) (Layout, error)`; `func ArrayName(variantName string) (string, error)` (`"bbbench_"+variantName`, error when longer than 32 characters).
  - `func NewMdadm(env Env) Setting`: `Name() == "software RAID layout (mdadm)"`, `Applied`, `ScopeLayout`, `InputVariants`, `LifetimeRun`. `Values()` returns one `Value` per entry of `Levels` (ID and Label = the level). It also implements `Settler` and `CommandHinter`.
  - `All(env)` returns `[NewWriteCache(env), NewMdadm(env)]`.
- Canonical value (ID and label):
  - Format `<level> · <roles>[ · <chunk>K][ · bitmap]`. `<roles>` is `N disks` (`1 disk`) when every disk is data, else `N data[ + M spare(s)][ + 1 journal]`. The chunk part appears only when `HasChunk`. `none` is always `none · N disks`.
  - Examples: `raid10 · 4 disks · 512K`, `raid5 · 3 data + 1 spare · 512K · bitmap`, `raid5 · 3 data + 2 spares + 1 journal · 64K`, `raid1 · 2 disks · bitmap`, `linear · 2 disks`, `none · 4 disks`.
- `ValidateLayout` messages (exact, checked in this order):
  - `unknown level %q`
  - `%s is used twice`
  - `none takes data disks only`
  - `%s needs at least %d data disks, has %d`
  - `%s cannot have a spare disk` (when not `Redundant`)
  - `a write journal needs raid5 or raid6`
  - `at most one journal disk`
  - `%s has no redundancy: no write-intent bitmap` (when not `Redundant`)
  - `a write journal and a bitmap cannot be used together`
  - `chunk must be one of 64K, 128K, 256K, 512K, 1024K` (only when `HasChunk`)
  - `mixed media: data disks %s (HDD) and %s (SSD)` (first rotational and first non-rotational data disk; spares and the journal are not counted)
- Behaviour (command lines exact; every command goes through `Env.Run`):
  - `Applicable(t)`: `LookPath("mdadm")` fails gives `false, "mdadm is not installed"`. `ApplicableVariant(v)`: `ParseLayoutValue` then `ValidateLayout`, giving `false, err.Error()`.
  - `Prepare(v)`:
    - `none` gives no undo.
    - Otherwise it validates and calls `Env.InUse()`. A disk with a reason gives `%s is in use: %s`; `All` gives `no disk can be used: %s`.
    - `<Dev>/md/<ArrayName(v.Name)>` existing gives `/dev/md/%s already exists`.
    - The uuid is 16 bytes from `Env.Rand` formatted `%08x:%08x:%08x:%08x`.
    - It returns one `Undo{Array: &ArrayUndo{Name, UUID, Members: v.Disks}}` and runs no command.
  - `Apply(v, prepared)`:
    - `none` returns one `TargetDrive` per disk with `Want = "none"`.
    - Otherwise it runs `mdadm --create /dev/md/<name> --run --metadata=1.2 --name=<name> --uuid=<uuid> --level=<level> --raid-devices=<data>` then:
      - `--chunk=<n>K` if `HasChunk`;
      - for `Redundant` levels without a journal, `--bitmap=internal` or `--bitmap=none`;
      - `--spare-devices=<n>` if any;
      - `--write-journal=<journal path>` if any;
      - then the data paths in variant order, then the spare paths.
    - A command error returns `nil` and an error with the command, the error and its `*exec.ExitError` stderr.
    - Then it resolves the kernel name by reading the link `<Dev>/md/<name>` (polling every `Env.Poll` up to `Env.Settle`). If `<Sys>/block/<md>/md/array_state` is `read-auto`, it runs `mdadm --readwrite /dev/<md>`.
    - It returns one `BenchTarget{Kind: TargetArray, Target: {Name: md, Path: "/dev/"+md, Serial: uuid, Model: level, Vendor: "md", Interface: "md"}, UUID, Members: v.Disks, Want: v.Value.ID}`.
  - `WaitSteady`: for each array it polls `<Sys>/block/<md>/md/sync_action` every `Env.Poll`. A missing file or `idle` means steady. Otherwise `sync_completed` `a / b` reports `100*a/b`, averaged over the arrays; `none` or `delayed` reports 0. It returns `ctx.Err()` when cancelled and has no timeout.
  - `Read(t)`:
    - For a drive target, the value is `none` when neither the disk nor its partitions have holders, else `member of <holder>`.
    - For an array, it reads `md/level`, `raid_disks`, `chunk_size`, `degraded`, `array_state`, `sync_action`, `layout`, `bitmap/location` (missing or `none` means no bitmap) and `dev-*/block`, `dev-*/state` (`spare`, `journal`, otherwise data). It builds the canonical value from what it read and appends, in this order:
      - ` (members differ: want <names> got <names>)` (sorted kernel names with roles compared to `t.Members`);
      - ` (degraded)`;
      - ` (array_state <s>)` when the state is not `clean`, `active` or `active-idle`;
      - ` (sync_action <a>)` when it is not `idle`.
    - `Kernel` is `array_state=<s>`, plus `, layout=<text>` for raid5/6 (`0` left-asymmetric, `1` right-asymmetric, `2` left-symmetric, `3` right-symmetric, else the number) and raid10 (`n<k>`, `f<k>`, `o<k>` from the near, far and offset fields of `md/layout`).
  - `Undo(u)` for an array (algorithm):
    1. Find the md kernel name from the link `<Dev>/md/<name>` and confirm it with `mdadm --detail --export /dev/<md>` (`MD_UUID` equals the uuid, compared case-insensitively). Otherwise scan `<Sys>/block/md*` with the same command.
    2. If found, its `dev-*/block` names must all be among the recorded members' current names; if not, return `array %s has members bbbench did not record (%s); not touched`. Then run `mdadm --stop /dev/<md>`; on failure return an error naming `mdadm --stop`, without wiping anything. Then wait up to `Env.Settle` until `<Sys>/block/<md>/md` is gone or `array_state` is `clear` or `inactive`.
    3. For each member with a path, run `mdadm --examine --export <path>`. If `MD_UUID` equals the uuid, run `mdadm --zero-superblock <path>`. Then run `--examine --export` again: the uuid must be gone. A different uuid is left alone (logged). No superblock means nothing to do.
    4. A member with no current path (not present) gives the error `member S/N %s is not present; its superblock was not wiped`.
  - `UndoCommand(u)`: `mdadm --stop /dev/md/<name>; mdadm --zero-superblock <member paths in order>`.

- [ ] **Step 1: Write the failing tests** with a fake runner (scripted `"name args"` to output or error, and recorded calls), a temporary `Sys` tree and a temporary `Dev` directory. The fake `--create` writes `<Dev>/md/<name>` linking to `../md127` and the md sysfs files.
  - `TestLayoutValue`: the six examples above.
  - `TestParseLayoutValue_RoundTrip`: every example parses back to its `Layout`.
  - `TestValidateLayout`: one table row per message above, plus valid rows (raid5 3 data + 1 spare + bitmap; none with one HDD and one SSD is valid).
  - `TestArrayName`: `"1762790400_1"` gives `"bbbench_1762790400_1"`; a 30-character name gives an error.
  - `TestMdadm_Identity`; `TestMdadm_Applicable` (`"mdadm is not installed"`).
  - `TestMdadm_PrepareRecordsNameAndUUID`: `Rand` returns the bytes `0x00..0x0f`, so the uuid is `"00010203:04050607:08090a0b:0c0d0e0f"`, the name is `bbbench_1762790400_1`, and no command runs.
  - `TestMdadm_PrepareRefusesBusyDisk` (Review Focus 1): `InUse` reports `sdc: mounted at /x`, so the error is `sdc is in use: mounted at /x` and no command runs.
  - `TestMdadm_PrepareRefusesExistingName`; `TestMdadm_PrepareNone` (no undo).
  - `TestMdadm_CreateArgs` (Review Focus 4): an exact argv for raid5 3 data + 1 spare + bitmap at 512K; raid1 without a bitmap (`--bitmap=none`, no `--chunk`); raid0 (`--chunk=512K`, no `--bitmap`); raid6 with a journal (`--write-journal=/dev/nvme0n1`, no `--bitmap`); linear (no chunk, no bitmap).
  - `TestMdadm_ApplyResolvesKernelName`: the returned target is exactly as specified above.
  - `TestMdadm_ApplyReadAutoSwitchesReadWrite` (Review Focus 3): `mdadm --readwrite /dev/md127` is called.
  - `TestMdadm_ApplyCreateFails`: the error contains the stderr `mdadm: cannot open /dev/sdb: Device or resource busy`.
  - `TestMdadm_ApplyNone`.
  - `TestMdadm_WaitSteady`: `resync` with `50 / 200`, then `idle`, reports progress 25 then returns nil. raid0 without `sync_action` returns at once. A cancelled ctx returns `context.Canceled`.
  - `TestMdadm_ReadArray`: a clean raid5 with 3 data + 1 spare, a bitmap at 512K gives `Value == Want` and `Kernel == "array_state=clean, layout=left-symmetric"`. raid10 with layout `0x102` gives `layout=n2`.
  - `TestMdadm_ReadDetectsDegradedAndCheck` (Review Focus 2): `degraded 1` gives the ` (degraded)` suffix; `sync_action check` gives ` (sync_action check)`.
  - `TestMdadm_ReadDetectsUnexpectedBitmap`: `bitmap/location +8` when no bitmap was asked gives a value ending `· bitmap` that differs from `Want`.
  - `TestMdadm_ReadWrongMember`; `TestMdadm_ReadNoneDisk` (`none`; a holder `md5` gives `member of md5`).
  - `TestMdadm_UndoStopsAndWipes`: the calls in order are `--detail --export /dev/md127`, `--stop /dev/md127`, then per member `--examine --export`, `--zero-superblock`, `--examine --export`, and the result is nil.
  - `TestMdadm_UndoFindsArrayByUUID` (Review Focus 5): `/dev/md/<name>` is missing, `md126` reports the uuid, so `md126` is stopped.
  - `TestMdadm_UndoRefusesForeignMember`: `md127` has `dev-sdx` not recorded, so there is no `--stop` and the error contains `not touched`.
  - `TestMdadm_UndoArrayAlreadyStopped`: only the members whose examine shows the uuid are wiped.
  - `TestMdadm_UndoSkipsForeignSuperblock`.
  - `TestMdadm_UndoStopFails`: the error contains `mdadm --stop` and `--zero-superblock` never runs.
  - `TestMdadm_UndoMissingMember`.
  - `TestMdadm_UndoCommand`: `"mdadm --stop /dev/md/bbbench_1_1; mdadm --zero-superblock /dev/sdb /dev/sdc /dev/sdd"`.
- [ ] **Step 2: Run** `go test ./internal/settings -run 'Layout|ArrayName|Mdadm'`. Expect FAIL.
- [ ] **Step 3: Implement** `layout.go` and `mdadm.go`, with an `UNVERIFIED` comment on each item of the hardware list above.
- [ ] **Step 4: Run** `go test -race ./internal/settings ./cmd/bbbench` (the mode TUI still lists only `InputValues` settings, so nothing visible changes) and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Add the mdadm layout setting: build, resync wait, verification and teardown behind the Setting interface"`.

---

### Task 5: Layout groups in the loop: arrays in the recovery file, teardown after every run, the failure policy

**Files:**
- Modify: `internal/rungroup/recovery.go`, `internal/rungroup/recovery_test.go`, `cmd/bbbench/group_run.go`, `cmd/bbbench/group_undo.go`, `cmd/bbbench/group_flow.go`, `cmd/bbbench/serve_status.go` (steps and fields only)
- Create: `cmd/bbbench/group_layout_test.go`

**Interfaces:**
- Consumes: `settings.Undo.Array`, `settings.Settler`, `LifetimeRun` (Task 4); `rungroup.GroupVariant`, `GroupRun.Teardown`, `GroupDrive.Kind/UUID/Members`, `PlanVariants`, `MembersOf` (Task 1); `undoLedger` (Task 2).
- Produces:
  - `rungroup`: `type RecoveryArray struct{ Name, UUID string; Members []RecoveryMember }` (tags `name`, `uuid`, `members`); `type RecoveryMember struct{ Serial, Model, Name string; Role settings.Role }`; `Recovery` gains `Arrays []RecoveryArray \`json:"arrays,omitempty"\`` (a drive-only file is unchanged). `RecoveryOf` and `Undos` handle arrays: members are resolved by serial and model, and an unresolved member keeps its recorded name with an empty `Path`.
  - `groupConfig` gains `Variants []settings.Variant` (layout input; nil for values input) and `MakeDrive func(bt settings.BenchTarget) (DriveInfo, error)` (called for array targets; nil refuses them).
  - Status: `stepBuilding = "building"`, `stepResyncing = "resyncing"`, `stepTearingDown = "tearing-down"`. `GroupStatus` gains `Progress float64 \`json:"progress,omitempty"\`` and `Variant string \`json:"variant,omitempty"\`` (the label of the current layout).
  - `reportUndoFailure` for an array prints `*** TEARDOWN FAILED: array %s (UUID %s; members %s): %v`, `*** Stop it and wipe its members by hand: %s` (`UndoCommand`) and `*** The recovery file %s is kept; bbbench offers the cleanup again at the next start.`
- Loop for a `LifetimeRun` setting (the order is the contract):
  - `runGroup`: plans come from `PlanVariants` over `cfg.Variants`; `g.Variants` comes from `MembersOf`; nothing is prepared at the start. The identity check runs over the union of the disks.
  - `runOne(idx)` with `v = cfg.Variants[idx]` and `v.Name = g.ID+"_"+strconv.Itoa(idx+1)`:
    1. Step `building`; the status drives are the variant's disks.
    2. `Prepare`. On error the run is `not_run` with the note `not built: <err>`.
    3. `ledger.add(undos...)`, which writes the file before the build. On error the group stops.
    4. `Apply`. On error the run is `not_run` with the note `build failed: <err>`.
    5. If the setting is a `Settler`: step `resyncing`, `WaitSteady`, and progress goes to `GroupStatus.Progress`. A cancelled ctx means the run is interrupted; any other error makes the run `not_run` with the note `resync failed: <err>`.
    6. Array targets go through `MakeDrive`; on error the run is `not_run` with the note `cannot benchmark <md>: <err>`. Drive targets take their `DriveInfo` from `cfg.Drives`.
    7. Verify before, `Policy`, `Execute`, verify after, labels: the same code as values. `record()` writes each `GroupDrive` with `Kind`, `UUID`, `Members` for arrays. A run with no target records the variant's disks with the note.
    8. Step `tearing-down`: `ledger.undo` on this run's undos, then `run.Teardown = {done}` or `{failed, names}`, then save. A failure returns `run %d of %d: teardown of %s failed; the group stops so that no further layout is built on disks still assembled`.
  - Every `not_run` path also goes through step 8 before the group moves on. An interrupted run returns at once, and the deferred `finishGroup` undoes whatever the ledger still holds.
  - Resume (`newGroupFile`): with `cfg.Variants` the resumed file must have the same number of `Variants`, the same `Value` and the same disk serials, models and roles per index. Otherwise the error is `run group: cannot resume group %s: layout %d differs (%s)`, and `sameDrivesPerValue` is skipped.
- `offerRecovery`, for a file with `Arrays`:
  - It lists `  array %s (UUID %s): members %s` and asks `Stop these arrays and wipe their members now? [y/n] `.
  - `n` returns the error `not cleaned up: bbbench does not start while arrays it built are left (%s); run it again and accept, or stop them and wipe their members by hand, then remove %s`.
  - A drive-only file behaves as today.
- `printGroupSummary` adds `      members: sdb (data), sdc (spare)` under an array and `  run %d teardown FAILED on: %s` when it failed.

- [ ] **Step 1: Write the failing tests** in `group_layout_test.go`, with a `fakeLayout` (it implements `Setting` and `Settler`, records calls, scripts per-variant Prepare, Apply, Read, Undo and WaitSteady results, and names the produced arrays `md127`, `md126`, ...) and the existing `fakeExec`:
  - `TestRunGroup_LayoutBuildsRunsTearsDown`: two variants (raid1 on sdb,sdc; raid0 on sdb,sdc). Per run the calls are prepare, apply, wait, read, execute, read, undo, in that order. `Execute` gets one `DriveInfo` `md127` with two `Members`, and the labels are keyed `md127`. The group file has `Kind == "array"` with members, `Teardown.Status == "done"` per run, and `Restore.Status == "done"`. The recovery file is gone.
  - `TestRunGroup_LayoutVariantNames`: the prepare calls get `v.Name == "<g.ID>_1"`, then `"<g.ID>_2"`.
  - `TestRunGroup_LayoutRecoveryWrittenBeforeBuild`: at `Apply` time the recovery file has `Arrays[0]` with the name, the uuid and the member serials.
  - `TestRunGroup_LayoutPrepareFailsNotRun` (Review Focus 1): no apply, the run is `not_run` with the note `not built: sdc is in use: mounted at /x`, and the next variant still runs.
  - `TestRunGroup_LayoutBeforeFailsTearsDownContinues`: Read differs from Want on run 1, so run 1 is `not_run` without `Execute`, run 1 is undone before run 2's prepare, and run 2 is `done`.
  - `TestRunGroup_LayoutApplyErrorNotRun`: the undo still runs (a partial build), and the group continues.
  - `TestRunGroup_LayoutAfterFailsInvalid` (Review Focus 2).
  - `TestRunGroup_LayoutTeardownFailureStopsGroup`: the undo fails on run 1, so `runGroup` returns an error, run 2 is never prepared, the recovery file keeps the array, `Restore.Status == "failed"` with `Failed == ["bbbench_<id>_1"]`, and the report contains `TEARDOWN FAILED` and the `UndoCommand`.
  - `TestRunGroup_LayoutInterruptDuringResync`: `WaitSteady` blocks until the ctx is cancelled, so the run is `interrupted`, the undo runs and the recovery file is gone.
  - `TestRunGroup_LayoutResyncStatus`: progress 42 gives `GetStatus().Group.Step == "resyncing"` and `Progress == 42`.
  - `TestRunGroup_LayoutBaselineNone`: a none variant with four disks calls `Execute` with the four `DriveInfo`s, each labelled `none · 4 disks`, with no recovery entry and no undo.
  - `TestRunGroup_LayoutMakeDriveError`: the run is `not_run` with the note, and teardown runs.
  - `TestRunGroup_LayoutResumeRebuilds`: run 1 `done` and run 2 `interrupted`, so only variant 2 is prepared.
  - `TestRunGroup_LayoutResumeRefusesOtherDisks`.
  - `TestRecovery_ArraysRoundTrip` (rungroup): a drive-only `Recovery` has no `"arrays"` key.
  - `TestRestoreFromRecovery_ArraysBySerial` (Review Focus 5): the members recorded as `sdb`, `sdc` are now `sdc`, `sdd`, so the `Undo` gets `Members` with the current names and paths.
  - `TestOfferRecovery_ArraysDeclineRefusesStart`: the output lists the array; `n` gives an error containing `does not start`; `y` undoes and removes the file.
  - `TestPrintGroupSummary_Arrays`.
- [ ] **Step 2: Run** `go test ./internal/rungroup ./cmd/bbbench -run 'Layout|Arrays'`. Expect FAIL.
- [ ] **Step 3: Implement** the loop, the recovery arrays, the reports, the status fields and the offer.
- [ ] **Step 4: Run** `go test -race ./...` (all write-cache tests unchanged) and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Run layout groups: record each array before building it, tear it down after every run and on every exit"`.

---

### Task 6: Array targets in a run

**Files:**
- Create: `cmd/bbbench/group_targets.go`, `cmd/bbbench/group_targets_test.go`
- Modify: `cmd/bbbench/generate_common.go`, `cmd/bbbench/orchestrator.go` (`discoverDrivesWithFioFiles` uses `fioFileName`), `cmd/bbbench/orchestrator_diskinfo.go`, `cmd/bbbench/group_flow.go` (`newGroupConfig`), `cmd/bbbench/serve_groups_test.go` (`newGroupConfig` calls), `cmd/bbbench/orchestrator_fsync_test.go`, `internal/flushtrace/session_test.go`

**Interfaces:**
- Consumes: `settings.BenchTarget` (Tasks 2, 4), `groupConfig.MakeDrive` (Task 5), `DriveInfo.Members/UUID`, `MemberStatus` (Task 1).
- Produces:
  - `func fioFileName(host string, d blockdev.Device) string`: the exact current pattern `{host}_{manufacturer}_{model}_{serial}_{subsystem}_config.fio` sanitized, with `unknown` for an empty vendor. It is used by `generateFioFilesForDrives` and by discovery.
  - `func fioGenerator(cfg *config.Root, eng *templating.Engine, host string) func(blockdev.Device) (string, error)`: renders one device with `generateFioFilesForDrives` into `expandUser(cfg.Fio.Generated.Path)` and returns `filepath.Join(outBase, host, fioFileName(host, d))`.
  - `func arrayDrive(bt settings.BenchTarget, drives map[string]DriveInfo, sysBlock string, gen func(blockdev.Device) (string, error)) (DriveInfo, error)`:
    - `Device{Name, Path, Vendor, Model, Serial}` from `bt.Target`, and `ParentSubsystem` = `bt.Target.Interface`.
    - `Rotational` from the **data** members' `Device.Rotational`. Mixed data members give `mixed media: %s is HDD, %s is SSD`; a member missing from `drives` gives `member %s is not a discovered drive`.
    - `BlockCount` is `<sysBlock>/<md>/size`, `BlockSize` is `queue/logical_block_size`, and `CapacityBytes = BlockCount*512`.
    - `Members = rungroup.MembersOf(bt.Members)`, `UUID = bt.UUID`, `FioFilePath` from `gen`, `FioExists = true`.
  - `newGroupConfig(c *groupChoice, targets map[string]settings.Target, opts runOptions, host string, resume *rungroup.GroupFile, makeDrive func(settings.BenchTarget) (DriveInfo, error)) groupConfig`. For layouts, `Precheck` runs `precheckRun` on the union of the disks (their fio files carry the same workloads).
  - `collectDiskConfig`: for an array drive it collects each member, not the array (`collect(DriveInfo{Device: blockdev.Device{Name, Serial, Model}})`). It sets `DriveStatus.Members[i].Config` on a cloned slice and writes `<member>_diskinfo_<start>.txt`.
  - Flush and write-through need no new branch: `flushtrace.Start("md127", ...)` already takes the `Block` path and `flushGate` reads `md127`'s own `queue/write_cache`. The tests pin this.

- [ ] **Step 1: Write the failing tests:**
  - `TestArrayDrive_FromMembers`: two SSD members, `size` 2000 and `logical_block_size` 4096 in a fake sysfs, give `Rotational == false`, `CapacityBytes == 1024000`, `BlockSize == 4096`, `ParentSubsystem == "md"`, two `Members` with roles, and the `FioFilePath` that `gen` returned.
  - `TestArrayDrive_MixedMediaRefused`.
  - `TestArrayDrive_JournalAndSpareIgnoredForMedia`: HDD data with an SSD journal gives `Rotational == true` and no error.
  - `TestArrayDrive_UnknownMember`.
  - `TestFioFileName_MatchesDiscovery`: the path from `discoverDrivesWithFioFiles` equals `fioFileName` for the same device.
  - `TestFioGenerator_ArrayFile`: with the embedded config and templates, `md127` gives a file containing `filename=/dev/md127` whose name ends `_md_config.fio`.
  - `TestCollectDiskConfig_ArrayMembers`: an array with members `sdb`, `sdc` calls `collect` for `sdb` and `sdc` only, the status has both `Members[i].Config` set, and `sdb_diskinfo_<t>.txt` and `sdc_diskinfo_<t>.txt` exist.
  - `TestFlushGate_ArrayOwnWriteCache`: a fake `sysBlock` with `md127/queue/write_cache = write through` and an fsync phase gives a warning starting `md127 is write through`.
  - `TestSession_MdUsesBlockCollector` (flushtrace): `md127` with `dev` `9:127` enables the block events, like `TestSession_BlockDeviceUsesDevNumbers`.
  - `TestCoreDoesNotNameTheBackend`: reads `../../internal/rungroup/*.go`, `group_run.go`, `group_flow.go`, `group_undo.go`, `group_targets.go` and `serve_*.go` (excluding `_test.go`) and fails on any case-insensitive `mdadm`.
- [ ] **Step 2: Run** `go test ./cmd/bbbench ./internal/flushtrace -run 'ArrayDrive|FioFileName|FioGenerator|ArrayMembers|ArrayOwnWriteCache|MdUsesBlock|NamesTheBackend'`. Expect FAIL.
- [ ] **Step 3: Implement** `group_targets.go`, the `fioFileName` refactor (no output change), the `collectDiskConfig` branch and the `newGroupConfig` parameter. Production `makeDrive` is `func(bt) { return arrayDrive(bt, byName(c.Drives), "", fioGenerator(cfg, eng, host)) }`, built in `runOrchestrator` (Task 7 loads `eng`).
- [ ] **Step 4: Run** `go test -race ./...` and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Benchmark array targets: a fio file for the md device, workload type and disk info from its members"`.

---

### Task 7: The layout TUI and the orchestrator wiring

**Files:**
- Create: `cmd/bbbench/group_layout_tui.go`, `cmd/bbbench/group_layout_tui_test.go`
- Modify: `cmd/bbbench/group_tui.go` (mode list, `groupChoice`), `cmd/bbbench/group_tui_test.go` (`TestMode_ListsSettingsWithDriveCounts`: the variant setting is now listed; an intended change), `cmd/bbbench/group_flow.go` (`resumeChoice`, `inUsePaths`), `cmd/bbbench/orchestrator.go`, `cmd/bbbench/help.go`

**Interfaces:**
- Consumes: `settings.Levels`, `ChunkChoicesKiB`, `ValidateLayout`, `LayoutValue`, `ParseLayoutValue`, `Redundant`, `HasChunk`, `MinDataDisks`, `DefaultChunkKiB` (Task 4); `inuse.Scan`, `Report.Reason` (Task 3); `estimateRun`; `fioGenerator`/`arrayDrive` (Task 6).
- Produces:
  - `groupChoice` gains `Variants []settings.Variant`. For layouts, `Values` is the variants' values in order, `Strategy` is `rungroup.Sequential`, and `Drives` is the union of the disks in order of first use.
  - `func newLayoutModel(s settings.Setting, drives []DriveInfo, targets map[string]settings.Target, busy *inuse.Report, busyErr error) layoutModel`; `func runLayoutSetupTUI(s settings.Setting, drives []DriveInfo, targets map[string]settings.Target, busy *inuse.Report, busyErr error) (*groupChoice, error)`. A disk is grayed when `busyErr != nil` (`in-use check failed: <err>`), when `busy.Reason` is not empty, or when `Applicable` is false.
  - `func inUsePaths(exe, outDir string) map[string]string`: maps `exe` to `"the bbbench binary"`, and the nearest existing ancestor of `outDir` to `"the output directory"`.
  - `resumeChoice` rebuilds the `Variants` from `g.Variants`, matching disks by serial and model. A disk that is now busy gives `cannot resume group %s: %s is in use: %s`. A layout group skips the known-values check (its values are layout strings).
- Screens and keys (decided here):
  - **List**: `a` adds a layout, `b` adds the baseline (`none`, every free disk as data), `Enter` edits the selected layout, `d` deletes it, `c` continues, `q` quits. Continuing with fewer than two layouts shows `Add at least two layouts to compare (the no-array baseline counts as one).`
  - **Editor**: rows are level (`←/→` through `Levels`), chunk (`←/→` through `ChunkChoicesKiB`, shown only when `HasChunk`), bitmap (`Space`, shown only when `Redundant`), then one row per disk. `Space` on a disk cycles unused → data → spare (only when `Redundant`) → journal (only raid5/raid6) → unused. A grayed disk row shows `<name>  <vendor> <model>  <reason>`, and `Space` on it sets the message `<name> is in use: <reason>` (or `<name>: <reason>` when not applicable).
  - The editor shows the live `ValidateLayout` error, or the value label when the layout is valid. `Enter` saves only a valid layout. When data members differ in capacity it warns `members differ in capacity (sdb 4000 GB, sdc 2000 GB): the array uses the smallest`. `Esc` cancels.
  - **Confirm**:
    - Header: `Confirm the layout test: THE DISKS BELOW WILL BE ERASED`.
    - One block per run, `Run <n>: <label>` then `    <name>  <vendor> <model> (S/N <serial>)  <role>`.
    - `Disks erased: <names>` (every disk used: arrays are built on them, and the baseline benchmark overwrites them).
    - `<runs> runs of the full test plan; estimated time <d>, plus the initial resync of each array, which is not included`. The estimate per variant is `estimateRun` of its first data disk; for the baseline it is all its disks.
    - `Each array is stopped and its members wiped after its run, also after an error or Ctrl+C. Nothing is built before you press y.`
    - Keys: `y` starts, `Esc` goes back, `q` quits. `Enter` does nothing here.
- Orchestrator: after `runModeTUI`, a setting whose `Input()` is `InputVariants` gets `busy, berr := inuse.Scan(inuse.Options{Paths: inUsePaths(os.Executable(), outDir)})`, then `runLayoutSetupTUI(setting, drives, targets, busy, berr)` with the filtered discovered drives. The drive selection TUI is skipped. The templates are loaded for `fioGenerator`, and `makeDrive` is passed to `newGroupConfig`. `--dry-run` is still refused.

- [ ] **Step 1: Write the failing tests** (drive the model's `Update` with key messages, as `group_tui_test.go` does):
  - `TestMode_ListsVariantSettings`: the mdadm-like setting appears with its count.
  - `TestLayout_BusyDiskGrayedWithReason`: `sdb` mounted at `/data` shows its reason in the view; `Space` on it leaves it unused with the message `sdb is in use: mounted at /data`.
  - `TestLayout_InUseCheckFailedGraysAll`.
  - `TestLayout_NotApplicableGrayed`: `mdadm is not installed`.
  - `TestLayout_RolesCycle`: raid5 cycles data, spare, journal, unused; raid0 cycles data, unused.
  - `TestLayout_RejectsTooFewDisks`: raid5 with two data disks keeps the editor open and shows `raid5 needs at least 3 data disks, has 2`.
  - `TestLayout_HiddenRows`: raid1 has no chunk row; raid0 has no bitmap row.
  - `TestLayout_BaselineVariant`: `b` with four free disks gives `none · 4 disks`.
  - `TestLayout_DiskInSeveralVariants`.
  - `TestLayout_NeedsTwoVariants`.
  - `TestLayout_CapacityWarning`.
  - `TestLayout_ConfirmListsEveryDiskAndLayout`: the view contains `WILL BE ERASED`, every label, every serial and `resync of each array, which is not included`.
  - `TestLayout_NothingBuiltBeforeConfirm`: the fake setting counts `Prepare`/`Apply`/`Undo` and stays at 0 through every screen; `Enter` on the confirm screen does not start; only `y` produces the choice.
  - `TestLayout_OutcomeVariants`: `Variants` keep the order and roles, `Values` are the labels, `Drives` is the union, `Strategy` is sequential.
  - `TestInUsePaths_OutputDirNotYetCreated`.
  - `TestResume_LayoutGroupRebuildsVariants`: the disks are matched by serial at new names, and a now-busy disk gives the error.
- [ ] **Step 2: Run** `go test ./cmd/bbbench -run 'Layout|Mode_|InUsePaths|Resume_Layout'`. Expect FAIL.
- [ ] **Step 3: Implement** `group_layout_tui.go`, the mode list change and the orchestrator flow. Add one line for layout tests to `help.go`.
- [ ] **Step 4: Run** `go test -race ./...` and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Let the user list layouts with member roles, see busy disks grayed, and confirm the erase once"`.

---

### Task 8: Web UI: members, resync progress, teardown

**Files:**
- Modify: `cmd/bbbench/serve_status.go` (JS), `cmd/bbbench/serve_groups.go` (page JS), `cmd/bbbench/serve_shared.go` (`chartSharedJS`), `cmd/bbbench/serve_graphs.go` and `cmd/bbbench/serve_compare.go` (card subtitle), `cmd/bbbench/serve_browse.go` (run details), `documentation/web-ui.md`
- Test: `cmd/bbbench/serve_status_test.go`, `serve_groups_test.go`, `serve_groups_pages_test.go`

**Interfaces:**
- Consumes: `DriveStatus.Kind/Members`, `GroupStatus.Progress/Variant`, steps (Tasks 1, 5); `GroupFile.Variants`, `GroupRun.Teardown`, `GroupDrive.Kind/UUID/Members`; `DiskPhaseData.Kind/Members`; `DriveInfo.Members` in `/api/results/detail`.
- Produces (JS; escape every inserted string with the page's `esc`/`escG`):
  - Status: `membersHTML(drive)` lists each member's name, model, serial and role, with its interface and SMART health from `config` when present, or `''` for a drive. `groupBannerHTML` shows the step texts `building the array`, `resyncing <round(progress)>%`, `tearing down`, and the `variant` label.
  - Group page: `renderVariants(g)` lists each layout label with its disks and roles. `renderRuns` writes the drive cell of an array as `<md> · array` with a dim member list, and uses the prefix `md:` instead of `write_cache:` for the kernel reading when `d.kind === 'array'`. `renderTeardown(run)` shows `torn down` or `teardown failed on <names>`. For layout groups the page shows `Torn down` / `Teardown failed on: ...` instead of the restore wording.
  - Shared `membersText(d)` gives `members: sdb (data), sdc (data), sdd (spare)` or `''`. It is added to the card subtitle and the modal title in graphs and compare.
  - Run details: `drivesHTML(result)` lists the drives, with members under an array.

- [ ] **Step 1: Write the failing tests:**
  - `TestStatusAPI_CarriesMembersAndProgress`: `/api/status` JSON has `drives[0].kind == "array"`, `drives[0].members[0].role == "data"`, `group.step == "resyncing"`, `group.progress == 42.4`.
  - `TestStatusPage_LayoutScript` (node on the served page): `groupBannerHTML` with `{step: 'resyncing', progress: 42.4}` contains `resyncing 42%`; `membersHTML` escapes a model `<b>x</b>`.
  - `TestHandleGroupDetail_Arrays`: the JSON contains `variants`, a run drive with `kind: "array"` and `members`, and `teardown.status`.
  - `TestGroupPage_ArrayScript` (node): `renderRuns` on an array sample contains `sdb`, `md:` and no `write_cache:`; `renderVariants` lists each label and disk; `renderTeardown` on a failed sample names the array.
  - `TestGraphsPage_MembersSubtitle` (node): `membersText` on an array entry and on a drive entry (`''`).
  - `TestResultDetail_ShowsArrayMembers`: the detail JSON has `drives[0].Members`, and node `drivesHTML` lists them.
- [ ] **Step 2: Run** `go test ./cmd/bbbench -run 'MembersAndProgress|LayoutScript|Group.*Array|MembersSubtitle|ArrayMembers'`. Expect FAIL.
- [ ] **Step 3: Implement** the JS and keep `chartSharedJS` free of backticks. Pages for write-cache groups render as before (the existing page tests pass unchanged).
- [ ] **Step 4: Verify** the dumped status, group, graphs, compare and result pages for a stored layout group: `node --check` each.
- [ ] **Step 5: Docs:** `web-ui.md` describes array targets, members, the resync banner and teardown.
- [ ] **Step 6: Run** `make check`, then **commit** `git commit -m "Show array targets, their members, resync progress and teardown in the web pages"`.

---

### Task 9: Docs, the dependency, the changelog

**Files:**
- Modify: `cmd/bbbench/deps.go`, `cmd/bbbench/deps_test.go`, `README.md`, `CLAUDE.md`, `CHANGELOG.md`, `documentation/flush-latency.md`, `documentation/web-ui.md`

- [ ] **Step 1: Write the failing test:** update `TestCheckDependencies_*` counts for `mdadm`. Run them and expect FAIL.
- [ ] **Step 2: Add** `{"mdadm", []string{"--version"}, false, "building software RAID arrays in a layout setting test", "mdadm"}` to `dependencies`. Run `go test ./cmd/bbbench -run Dependencies` and expect PASS.
- [ ] **Step 3: README:**
  - A "Layout setting test (software RAID)" section opening with **`A layout setting test ERASES every disk it uses: the arrays are built on them and the benchmark writes to them. Their data is not kept and not restored; after the test they carry no array and no md superblock.`**
  - What the section covers: levels, roles and minimum disks; the baseline; which disks are grayed and why; the single `y` confirmation; the resync wait (ETA without it); verification before and after; teardown on every exit; the recovery file now also holding arrays, and the refusal to start when its cleanup is declined; the failure policy; resume.
  - The output files table gains the array fio files (`{host}_md_{level}_{uuid}_md_config.fio` under `fio.generated.path`).
- [ ] **Step 4: CLAUDE.md:**
  - Rewrite the `internal/settings` paragraph for the new interface (`Lifetime`, `Prepare`/`Apply`→targets/`Read`/`Undo`, `PerDrive`, `Settler`, `CommandHinter.UndoCommand`, and how to add a setting: implement `Setting`, or `DriveOps` + `PerDrive` for drive scope, register it in `All`).
  - Add `layout.go`/`mdadm.go` and `internal/inuse`. Add `group_undo.go`, `group_targets.go` and `group_layout_tui.go` to "Run groups in cmd/bbbench". Add `mdadm` under External programs.
  - Extend the safety bullet: a layout test erases its disks, in-use disks are never offered, one confirmation, teardown on every exit, and the recovery file records arrays before they are built.
  - Add a short **"What the hardware-RAID branch plugs into"** note:
    - a controller setting implements `settings.Setting` with a new `Scope` (controller), `InputVariants`, `LifetimeRun`;
    - `Apply` returns `BenchTarget{Kind: TargetArray}` for the virtual drive with its physical disks as `Members` (from `diskinfo.Controller` and the vendor tool, `perccli`/`storcli`/`ssacli`, run through `Env.Run`);
    - `Read` verifies through the vendor tool, and `Undo` deletes the virtual drive;
    - its undo record gets its own list in `rungroup.Recovery` next to `Arrays`;
    - `inuse` applies to the member disks;
    - `arrayDrive`, the labels and the pages are already kind-agnostic, and `TestCoreDoesNotNameTheBackend` keeps it that way;
    - a `Manual` kind (mode set by the user in the BIOS) also needs the multi-invocation groups of the first spec, which `runGroup` refuses today.
- [ ] **Step 5: CHANGELOG** entry under Added (the layout setting, `internal/inuse`, the generalized interface, the recovery file arrays, the web UI). `flush-latency.md` gets one paragraph: on an md array the generic block collector is used on the md device (exact mean, no percentiles); this is not yet verified on hardware, and a phase may show 0 Flushes because md is bio-based.
- [ ] **Step 6: Run** `make check` and the docs audit used before: every flag, endpoint and file name in the README exists in the code; `grep -rn "never change\|only reads" README.md CLAUDE.md documentation` wording matches the new rule.
- [ ] **Step 7: Commit** `git commit -m "Document layout setting tests, the erase warning and what hardware RAID plugs into"`.
