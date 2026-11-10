# Software Layout Setting (mdadm, LVM) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a destructive *software layout* setting to setting tests with two backends, mdadm and LVM: the user lists layouts (and optionally the no-array baseline), bbbench builds each md array or logical volume, waits for its resync, verifies it before and after the run, benchmarks its block device, tears it down, and the web pages compare the layouts and show each layout's members.

**Architecture:** The run-group core learns *targets* (a drive, or a composite with members) and a variant-based `settings.Setting` (Prepare, Apply returning targets, Read, Undo, plus a `Lifetime`). The write cache goes through a `PerDrive` adapter so its behaviour does not change. One layout `Setting` (`layout.go`) does what is common to every backend (baseline, in-use re-check, generic validation, udev settle) and dispatches by backend name to a `settings.Backend`: `mdadm.go` first, `lvm.go` second, controller backends in the next branch. A new `internal/inuse` package decides which disks may be erased. One undo ledger mirrors every change to the recovery file, which now holds drive values and built layouts.

**Tech Stack:** Go 1.24 (stdlib only), bubbletea (TUI), the existing raw-string HTML/JS pages, `mdadm`, the lvm2 tools and `udevadm` called through `settings.Env.Run`.

**Spec:** `docs/superpowers/specs/2026-11-10-mdadm-layout-design.md` (binding; the file keeps its first name), built on `docs/superpowers/specs/2026-11-06-run-groups-design.md`.

## Global Constraints

- Not built: hardware RAID (the next branch), ZFS, btrfs, bcache, partitioning, any filesystem on the layout (fio runs on the raw md or dm device), LVM thin pools, caches, VDO, snapshots, integrity, `--assume-clean` / `--nosync`, a RAID 10 near/far/offset choice (each backend's default layout is used and read back), resilver or degraded-mode benchmarks.
- Setting name `software layout`. Every layout value starts with its backend name (`mdadm`, `lvm`), except the baseline `none · N disks`, which belongs to no backend. One group may mix backends and the baseline.
- mdadm parameters are exactly: level (`raid0`, `raid1`, `raid5`, `raid6`, `raid10`, `linear`), member disks each with a role (`data`, `spare`, `journal`), chunk size (default `512K`) and write-intent bitmap. Minimum data disks: raid0 2, raid1 2, raid5 3, raid6 4, raid10 4, linear 2.
- LVM parameters are exactly: type (`linear`, `striped`, `mirror`, `raid1`, `raid5`, `raid6`, `raid10`), member disks (data only), stripes (`striped` only, 2 up to the disks, default all) and stripe size (`striped`, `raid5`, `raid6`, `raid10`; 64K to 1024K, default `64K`). Minimum disks: linear 1, striped 2, mirror 2, raid1 2, raid5 3, raid6 5, raid10 4 and even.
- The baseline `none` takes 1 or more data disks.
- Layouts have no parallel or swap strategy. Variants run one after the other, and a disk may appear in several variants.
- Nothing is built before the one global confirmation, which is a single key and not a typed phrase. In-use disks (including LVM PVs, in a VG or not, and disks under an active LV) are grayed with the reason and cannot be selected. When bbbench is unsure, the disk is not selectable.
- Layout name: `bbbench_<group id>_<n>` (n is the 1-based run), at most 32 characters. mdadm creates `/dev/md/<name>` and never reuses an existing `/dev/mdN`; LVM names both the VG and its one LV `<name>`, and refuses a VG that already exists.
- Wait for the resync with visible progress and no timeout. Ctrl+C aborts with the normal teardown. The ETA says the resync time is not included.
- Teardown (mdadm: stop the array, zero the superblock of every member; LVM: `lvremove`, `vgremove`, `pvremove` on every member) runs on every exit path: normal end, error, Ctrl+C, SIGTERM/SIGHUP, panic, and ends with `udevadm settle` when `udevadm` is installed. The recovery file `/var/lib/bbbench/group-recovery.json` records each layout (backend, name, uuid when known, member serials and models, backend `Extra`) **before it is built**. Cleanup touches only recorded layouts that still match their members. If the user declines the cleanup, bbbench refuses to start.
- Failure policy (both backends): a failed *before* verification makes the run `not_run`, the layout is torn down and the next layout runs. A failed *after* verification makes the run `invalid`. A failed teardown stops the group, keeps the recovery file and prints the manual commands.
- The run-group core never names a backend or its tools (`mdadm`, `lvm`, `pvcreate`, `vgcreate`, `lvcreate`) and never branches on a backend name: `internal/rungroup`, `cmd/bbbench/group_run.go`, `group_flow.go`, `group_undo.go`, `group_targets.go`, `group_layout_tui.go` and every `serve_*.go` (enforced by `TestCoreDoesNotNameTheBackend`, Task 8, extended to the TUI in Task 9). `internal/settings/layout.go` names no backend either: backends are passed to `NewLayout` in `All`.
- A plain single run and the write-cache setting test are unchanged in behaviour and output. Every existing test keeps its assertions, and every new JSON field is `omitempty`, so the files they write are byte-identical.
- `internal/inuse` is used only by the layout selection and by the layout re-check before a build. Single runs and the write-cache flow never call it.
- Pure Go, no new module dependency. Every command goes through the injectable runner, so no test needs root or disks. `mdadm`, `lvm` and `udevadm` are optional dependencies; a backend whose tool is missing is shown unavailable with the reason.
- For every changed page, run `node --check` on the real page served for a stored result (not an error page), and run the new JS functions in node.
- Run `make check` (gofmt, vet, `go test -race`) before every commit. One commit per task, the message states the intent and ends with the `Co-Authored-By` line the harness gives. **No push, no pull request.**

## Review Focus

Inputs the spec implies but no happy path covers; each has a test in the task named.

1. **A selected disk becomes busy after the confirmation** (it is mounted, added to swap or made an LVM PV before its layout's turn). The build is refused for that layout, the run is `not_run` with the reason, and nothing is created, whatever the backend (Task 4 `TestLayout_PrepareRefusesBusyDisk`, Task 6 `TestRunGroup_LayoutPrepareFailsNotRun`).
2. **The same disks go from one layout to the next** (mdadm raid1 on sdb,sdc, then lvm striped on sdb,sdc). udev still reports the signature bbbench has just wiped (`linux_raid_member`, `LVM2_member`), so the next layout's in-use re-check would refuse it. Every successful teardown ends with `udevadm settle` (Task 4 `TestLayout_UndoSettlesUdev`), and the backend tests pin that the wipe itself is verified (Task 5 `TestMdadm_UndoStopsAndWipes`, Task 7 `TestLVM_UndoRemovesAll`).
3. **The layout changes during the benchmark**: the distro's periodic md `check` starts, a member fails, or an LVM raid LV reports a health status or falls out of sync. The after-verification reading differs and the run is `invalid` (Task 5 `TestMdadm_ReadDetectsDegradedAndCheck`, Task 7 `TestLVM_ReadDetectsHealthAndSync`, Task 6 `TestRunGroup_LayoutAfterFailsInvalid`).
4. **The resync never ends**: an md array starts `read-auto` (`md_mod start_ro=1`), or a raid LV is created inactive. The no-timeout wait would hang. mdadm Apply switches the array to read-write, and LVM Apply verifies the LV is active before returning it (Task 5 `TestMdadm_ApplyReadAutoSwitchesReadWrite`, Task 7 `TestLVM_ApplyRefusesInactiveLV`).
5. **Names change between a crash and the next start** (the array comes back as `md126`, `sdb` becomes `sdc`). Cleanup finds the md array by uuid, the VG by its name, and the members by serial and model, never by kernel name, and refuses a layout with an unrecorded member (Task 5 `TestMdadm_UndoFindsArrayByUUID`, `TestMdadm_UndoRefusesForeignMember`, Task 7 `TestLVM_UndoRefusesForeignPV`, Task 6 `TestRestoreFromRecovery_ArraysBySerial`).

## Unverified on hardware (to be listed in the PR when the user asks for one)

Real mdadm and lvm2 could not be run in development, so these parts are marked `UNVERIFIED` in code comments and must be checked on a host before release:

- The `mdadm --create` argv: the `--uuid=xxxxxxxx:xxxxxxxx:xxxxxxxx:xxxxxxxx` form, `--bitmap=none`, leaving out `--bitmap` when `--write-journal` is given, and that `--run` with stdin at `/dev/null` never prompts when members carry old signatures.
- The keys of `mdadm --detail --export` and `mdadm --examine --export` (`MD_UUID`, `MD_DEVNAME`), and the exit status and output of `--examine` on a disk with no superblock.
- How long udev takes to create `/dev/md/<name>`, and whether the 32-character name limit holds once mdadm adds the homehost prefix.
- The sysfs attributes and values `md/level`, `raid_disks`, `chunk_size`, `degraded`, `array_state` (`read-auto`), `sync_action`, `sync_completed` (`none`, `a / b`), `layout`, `bitmap/location`, and `dev-*/state` containing `spare` / `journal`; also the raid10 and raid5/6 layout numbers.
- The `lvcreate` argv per type: `--extents 100%FREE` for raid and mirror LVs (metadata sub-LVs), `--mirrorlog core` for `mirror`, the raid6 minimum of 3 data stripes (5 disks), `--stripes` for raid10 being the number of mirrored pairs, and that `--yes` never leaves a prompt when members carry old signatures.
- The JSON report fields of `lvs`/`vgs`/`pvs` (`--reportformat json`): `segtype` values (`raid5` or `raid5_ls`), `stripes` of a raid LV (data stripes or all images), `stripe_size` units with `--units k --nosuffix`, `sync_percent` for a mirror, `lv_health_status`, and the exit status of `pvs` and `vgs` on a disk or VG that does not exist.
- The LVM devices file (`lvmdevices`, `use_devicesfile=1`): whether `pvcreate` adds an entry, and whether `pvremove` already removes it; also whether `--autobackup n` leaves files under `/etc/lvm/archive`.
- Whether `udevadm settle` is enough for `ID_FS_TYPE` to drop after `mdadm --zero-superblock` and `pvremove` on a whole disk (inotify watch).
- Flush tracing on md and dm: both are bio-based for these layouts, so the `block_rq_issue`/`block_rq_complete` events of the `Block` collector may never fire for `/dev/mdX` or `/dev/dm-N`. The phase then shows the existing "ftrace saw 0 Flush commands" note (see the rulings below).
- In-use detection on real LVM, LUKS, multipath, ZFS, btrfs (subvolume `st_dev`) and container root setups.
- Trim phases on arrays and LVs without discard support (md raid5/6 by default) fail fio, as on any device without discard. This version has no special handling for it.

## File structure

- `internal/settings/settings.go` (modify): interface, `Variant`, `Disk`, `BenchTarget`, `Undo`, `Lifetime`, `Settler`, `Env`, `Verify`, `All`.
- `internal/settings/perdrive.go` (new): `PerDrive` adapter for drive-scope settings. `writecache.go` keeps its logic.
- `internal/settings/backend.go` (new): the `Backend` interface, `Layout`, `Option`, `LayoutSetting`. `layout.go` (new): the software-layout setting (baseline, generic validation, in-use re-check, dispatch, udev settle). `mdadm.go` and `lvm.go` (new): the backends.
- `internal/inuse/` (new): `inuse.go`, `mountinfo.go`, `lvm.go` (the `pvs` listing), `stat_linux.go`, `stat_other.go`.
- `internal/rungroup/target.go` (new): `Member`, `GroupVariant`, `MembersOf`. `groupfile.go`, `plan.go`, `recovery.go` (modify).
- `cmd/bbbench/group_undo.go` (new): the undo ledger and failure reports. `group_targets.go` (new): composite targets as `DriveInfo`. `group_layout_tui.go` (new): layout screens.
- `cmd/bbbench/group_run.go`, `group_flow.go`, `group_tui.go`, `orchestrator.go`, `orchestrator_diskinfo.go`, `generate_common.go`, `serve_status.go`, `serve_graphs.go`, `serve_groups.go`, `serve_shared.go`, `serve_compare.go`, `serve_browse.go`, `deps.go`, `help.go` (modify).

---

### Task 1: Targets in the data model (drive or composite with members)

**Files:**
- Modify: `internal/settings/settings.go` (types only), `internal/rungroup/groupfile.go`, `internal/rungroup/plan.go`, `cmd/bbbench/orchestrator.go` (`DriveInfo`), `cmd/bbbench/serve_status.go` (`DriveStatus`, `newDriveStatuses`), `cmd/bbbench/serve_graphs.go` (`DiskPhaseData`, `generateDiskPhaseData`), `cmd/bbbench/group_tui.go` (`targetsFor`)
- Create: `internal/rungroup/target.go`, `internal/rungroup/target_test.go`
- Test: `internal/rungroup/groupfile_test.go`, `plan_test.go`, `cmd/bbbench/serve_graphs_test.go`, `serve_status_test.go`, `group_tui_test.go`

**Interfaces:**
- Produces (package `settings`): `type Role string` with `RoleData = "data"`, `RoleSpare = "spare"`, `RoleJournal = "journal"`; `type TargetKind string` with `TargetDrive = "drive"`, `TargetArray = "array"` (every composite: an md array, an LV, later a controller's virtual drive); `type Disk struct{ Target; Role Role }`; `Target` gains `Rotational bool`.
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
  - `TestGroupFile_ArrayRoundTrip`: a group with `Variants: [{Value: "mdadm raid1 · 2 disks", Disks: [{sdb S1 M data} {sdc S2 M data}]}]` and one run whose drive is `{Device: "md127", Serial: "0a:0b:0c:0d", Model: "raid1", Kind: "array", UUID: "0a:0b:0c:0d", Members: [...]}` with `Teardown: &GroupRestore{Status: RestoreDone}`. Save then `LoadGroup` gives a `reflect.DeepEqual` result.
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
  - `type BenchTarget struct{ Kind TargetKind; Target Target; UUID string; Members []Disk; Want string; Err error }` (Task 4 adds `Backend`). For a drive, `Target` is the drive itself. For a composite, `Target.Name`/`Path` are its kernel name and node (`md127`, `dm-3`), `Serial` is its uuid, `Model` the level, and the backend sets `Vendor` and `Interface`. `Want` is the `Reading.Value` that verifies the target. `Err` is set when Apply could not make this one target effective.
  - `type Undo struct{ Drive *Target; Original *Reading }` (Task 4 adds `Array`); `func (u Undo) Name() string` returns the drive name.
  - `type Settler interface{ WaitSteady(ctx context.Context, ts []BenchTarget, progress func(pct float64)) error }` (implemented by the layout setting in Task 4; the group loop uses it in Task 6).
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
- Create: `internal/inuse/inuse.go`, `internal/inuse/mountinfo.go`, `internal/inuse/lvm.go`, `internal/inuse/stat_linux.go` (`//go:build linux`), `internal/inuse/stat_other.go` (`//go:build !linux`, returns an error), `internal/inuse/inuse_test.go`

**Interfaces:**
- Produces (package `inuse`):
  - `type PV struct{ Device, VG string }` (`Device` is the PV path, `VG` empty for an orphan PV).
  - `type Options struct{ Sys, Proc, Udev string; Paths map[string]string; Stat func(path string) (dev, rdev uint64, err error); PVs func() ([]PV, error) }`. Defaults: `/sys`, `/proc`, `/run/udev/data`, `syscall.Stat` (`Dev`, `Rdev`), and `listPVs`: when `lvm` is on `PATH` it runs `lvm pvs --reportformat json -o pv_name,vg_name` and parses `report[0].pv[]`; when it is not, it returns nil, nil (the udev signature still covers PVs). `Paths` maps a path to what it is, e.g. `"/usr/local/bin/bbbench": "the bbbench binary"`.
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
  - LVM objects: each `PV` resolves through `Stat(Device).rdev` to its whole disk and gives `is an LVM physical volume of volume group <vg>`, or `is an LVM physical volume (no volume group)` for an orphan. On a disk that has this reason, the udev `LVM2_member` reason is not added again. An error from `PVs` adds the warning `cannot list LVM physical volumes: <err>; disks with an LVM signature are not offered` and is not an error of `Scan`. (An active LV on a disk already gives its `dm-*` holder reason.)

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
  - `TestScan_LVMPhysicalVolumes`: `PVs` returns `{/dev/sdk1 vg0}` and `{/dev/sdl ""}`, with udev `LVM2_member` on `sdl`. `Reason("sdk") == "is an LVM physical volume of volume group vg0"`, and `Busy["sdl"] == ["is an LVM physical volume (no volume group)"]` (no duplicate).
  - `TestScan_PVsFail`: `PVs` returns an error, so `Scan` succeeds with the warning starting `cannot list LVM physical volumes`, and a udev `LVM2_member` disk keeps its signature reason.
  - `TestScan_FreeDisk`: `Reason("sdi") == ""`.
  - `TestDevNumbers`: `DevNumbers(0x10302) == (259, 2)`; `DevNumbers(0x100805) == (8, 0x105)` (a minor above 255).
- [ ] **Step 2: Run** `go test ./internal/inuse`. Expect FAIL (the package is missing).
- [ ] **Step 3: Implement** the package. Parse mountinfo in `mountinfo.go`, the `pvs` JSON in `lvm.go`. Order the reasons per disk as found: mounts, swap, paths, holders, LVM PVs, signatures.
- [ ] **Step 4: Run** `go test -race ./internal/inuse` and `make check`. Expect PASS.
- [ ] **Step 5: Commit** `git commit -m "Add the in-use disk check that protects busy disks from layout tests"`.

---

### Task 4: The software-layout setting and the layout `Backend` interface

**Files:**
- Modify: `internal/settings/settings.go` (`ScopeLayout`, `Undo.Array`, `BenchTarget.Backend`, `Env` fields)
- Create: `internal/settings/backend.go`, `internal/settings/layout.go`, `internal/settings/layout_test.go` (with a `fakeBackend`)

**Interfaces:**
- Consumes: the `Setting` interface, `Variant`, `BenchTarget`, `Undo`, `Settler` (Task 2); `inuse.Scan`/`Report.Reason` (Task 3).
- Produces (package `settings`):
  - `ScopeLayout Scope = "layout"`. `BenchTarget` gains `Backend string` (`""` for a drive target).
  - `type ArrayUndo struct{ Backend, Name, UUID string; Members []Disk; Extra map[string]string }` (`UUID` empty when the backend has none before the build; `Extra` is owned by the backend, stored by the core and never read by it). `Undo` gains `Array *ArrayUndo`, and `Name()` returns `Array.Name` for a layout.
  - `Env` gains `Dev string` (default `/dev`), `Rand io.Reader` (default `crypto/rand.Reader`), `Poll time.Duration` (default 2 s) and `InUse func() (*inuse.Report, error)` (default `inuse.Scan(inuse.Options{})`).
  - `const LevelNone = "none"`. `type Option struct{ Key, Label string; Choices []string; Default string }`. `type Layout struct{ Backend, Level string; Options map[string]string }` (the baseline is `Layout{Level: LevelNone}`).
  - The plug-in point (`backend.go`); mdadm (Task 5), LVM (Task 7) and the controller backends of the next branch implement it:
    ```go
    type Backend interface {
        Name() string              // first word of every value; recorded on targets and undo records
        Available() (bool, string) // false with the reason when its tool is missing
        Levels() []string
        Roles(level string) []Role                    // RoleData first
        Options(level string, dataDisks int) []Option // the editor rows for this level, in order
        Validate(l Layout, disks []Disk) error        // backend rules, after the generic ones
        Label(l Layout, disks []Disk) string          // the canonical value, starts with Name()+" "
        Parse(value string) (Layout, error)           // inverse of Label, every option set
        Prepare(ctx context.Context, v Variant, l Layout, name string) (*ArrayUndo, error) // changes nothing
        Apply(ctx context.Context, v Variant, l Layout, rec *ArrayUndo) ([]BenchTarget, error)
        WaitSteady(ctx context.Context, ts []BenchTarget, progress func(pct float64)) error
        Read(ctx context.Context, t BenchTarget) (Reading, error) // composite targets only
        Undo(ctx context.Context, u ArrayUndo) error             // tears down and verifies
        UndoCommand(u ArrayUndo) string
    }
    type LayoutSetting interface {
        Setting
        Settler
        CommandHinter
        Backends() []Backend
        ValidateLayout(l Layout, disks []Disk) error
        LayoutValue(l Layout, disks []Disk) (Value, error) // validates; ID == Label
        ParseLayout(id string) (Layout, error)
    }
    ```
  - `func NewLayout(env Env, backends ...Backend) LayoutSetting`; `func WithDefaults(b Backend, l Layout, dataDisks int) Layout` (drops options not listed for the level, sets missing ones and ones no longer among the choices to the default); `func ArrayName(variantName string) (string, error)` (`"bbbench_"+variantName`, error when longer than 32 characters).
- Behaviour of the layout setting (`layout.go`; it names no backend):
  - `Name() == "software layout"`, `Applied`, `ScopeLayout`, `InputVariants`, `LifetimeRun`. `Values()` is `none`, then `<backend> <level>` for every backend and level in order.
  - `Applicable(t)`: true when any backend is available; otherwise false with `<name>: <reason>` per backend joined by `"; "`.
  - `ValidateLayout` messages (exact, checked in this order): `unknown backend %q`; `%s is not available: %s`; `unknown %s level %q` (`a layout needs a backend` when `Backend == ""` and the level is not `none`); `%s is used twice`; `none takes data disks only`; `none needs at least 1 disk`; `%s cannot have a %s disk` (a role not in `Roles(level)`); `%s has no %s option` (level, key); `%s is not set` (label); `%s must be one of %s` (label, choices joined by `", "`); then `b.Validate`; then `mixed media: data disks %s (HDD) and %s (SSD)` (first rotational and first non-rotational data disk).
  - `LayoutValue`: the baseline label is `none · N disks` (`none · 1 disk`). A backend label that does not start with `<name> ` gives `backend %s labels %q without its name`. `ParseLayout`: `none · …` is the baseline, else the first word picks the backend (`unknown backend %q`).
  - `ApplicableVariant(v)`: `ParseLayout`, `ValidateLayout`, and `LayoutValue(...).ID` must equal `v.Value.ID`, else `layout %q does not match its disks (%q)`.
  - `Prepare(v)`: `ApplicableVariant`, then `Env.InUse()` (error: `cannot check which disks are in use: %v`; `All`: `no disk can be used: %s`; a disk: `%s is in use: %s`). This also applies to the baseline, whose benchmark overwrites the disks; the baseline then returns no undo. Otherwise `ArrayName(v.Name)` and `b.Prepare`; the setting sets `rec.Backend = b.Name()` and returns one `Undo{Array: rec}`.
  - `Apply(v, prepared)`: the baseline returns one `TargetDrive` per disk with `Want = "none"`. Otherwise `b.Apply(ctx, v, l, prepared[0].Array)`, and the setting sets `Backend` on every target.
  - `WaitSteady(ts)`: hands the targets of each backend to that backend's `WaitSteady`; drive targets are steady.
  - `Read(t)`: a drive target (`Backend == ""`) reads `none` when neither the disk nor its partitions have holders under `<Sys>/block`, else `member of <holder>`. Otherwise it dispatches on `t.Backend` (`unknown backend %q`).
  - `Undo(u)`: dispatches on `u.Array.Backend` (`unknown backend %q; nothing torn down`). After a successful backend `Undo`, when `LookPath("udevadm")` succeeds, it runs `udevadm settle --timeout=<Settle in whole seconds, at least 1>`; a settle error is logged, not returned. `UndoCommand(u)` dispatches.

- [ ] **Step 1: Write the failing tests** with a `fakeBackend` (name, levels, roles, options, availability, scripted results, recorded calls) and a fake `Env.Run`:
  - `TestLayout_Identity`; `TestLayout_ValuesListBackends`: backends `aa` (levels `x`, `y`) and `bb` (`z`) give `none`, `aa x`, `aa y`, `bb z`.
  - `TestLayout_Applicable`: both unavailable give `false, "aa: tool a missing; bb: tool b missing"`; one available gives true.
  - `TestValidateLayout_Generic`: one table row per message above, in that order, plus a valid baseline with one HDD and one SSD (mixed media is checked for layouts only).
  - `TestLayoutValue_Baseline` (`none · 4 disks`, `none · 1 disk`); `TestLayoutValue_RequiresBackendPrefix`; `TestParseLayout_Dispatch` (baseline, `aa x …`, `unknown backend "zz"`).
  - `TestLayout_ApplicableVariantMismatch`: a value for three disks on a variant with two.
  - `TestLayout_PrepareRefusesBusyDisk` (Review Focus 1): `InUse` reports `sdc: mounted at /x`, so the error is `sdc is in use: mounted at /x` and the backend's `Prepare` is never called. `TestLayout_PrepareBaselineChecksInUse`: the same for a `none` variant. `TestLayout_PrepareNothingUsable`: `All` set gives `no disk can be used: …`.
  - `TestLayout_PrepareRecordsBackend`: variant name `1762790400_1` gives `Undo.Array.Backend == "aa"` and the name `bbbench_1762790400_1` passed to the backend. `TestArrayName`: a 30-character variant name gives an error.
  - `TestLayout_ApplyBaseline` (drive targets, `Want == "none"`, `Backend == ""`); `TestLayout_ApplySetsBackend`.
  - `TestLayout_ReadNoneDisk`: `none`; a holder `md5` gives `member of md5`; a partition holder `dm-0` gives `member of dm-0`.
  - `TestLayout_DispatchByBackend`: `Read`, `WaitSteady`, `Undo` and `UndoCommand` reach `bb` for a `bb` target or record; an unknown backend gives the errors above.
  - `TestLayout_UndoSettlesUdev` (Review Focus 2): with `Settle` 10 s the last call after a successful undo is `udevadm settle --timeout=10`; a failed backend undo runs no settle; a missing `udevadm` runs nothing and returns nil.
  - `TestWithDefaults`: changing the level drops an option the new level does not list and adds the new level's defaults; a value no longer among the choices is reset.
- [ ] **Step 2: Run** `go test ./internal/settings -run 'Layout|ArrayName|WithDefaults'`. Expect FAIL (undefined).
- [ ] **Step 3: Implement** `backend.go` and `layout.go`. `All` is not changed yet: no backend exists.
- [ ] **Step 4: Run** `go test -race ./internal/settings` and `make check`. Expect PASS.
- [ ] **Step 5: Commit** `git commit -m "Add the software layout setting that dispatches to pluggable backends, with the baseline and the in-use re-check"`.

---

### Task 5: The mdadm backend

**Files:**
- Modify: `internal/settings/settings.go` (`All`)
- Create: `internal/settings/mdadm.go`, `internal/settings/mdadm_test.go`

**Interfaces:**
- Consumes: `Backend`, `Layout`, `Option`, `ArrayUndo`, `NewLayout`, the `Env` fields (Task 4).
- Produces (package `settings`): `func NewMdadm(env Env) Backend` with `Name() == "mdadm"`. `All(env)` returns `[NewWriteCache(env), NewLayout(env, NewMdadm(env))]`.
- Parameters:
  - `Available`: `LookPath("mdadm")` fails gives `false, "mdadm is not installed"`.
  - `Levels`: `raid0`, `raid1`, `raid5`, `raid6`, `raid10`, `linear`. `Roles`: raid0 and linear `[data]`; raid1 and raid10 `[data, spare]`; raid5 and raid6 `[data, spare, journal]`.
  - `Options(level, n)`: `{Key: "chunk", Label: "chunk", Choices: 64K 128K 256K 512K 1024K, Default: "512K"}` for raid0, raid5, raid6, raid10; then `{Key: "bitmap", Label: "write-intent bitmap", Choices: no yes, Default: "no"}` for raid1, raid5, raid6, raid10.
  - `Validate` messages (exact, in order): `%s needs at least %d data disks, has %d` (raid0 2, raid1 2, raid5 3, raid6 4, raid10 4, linear 2); `at most one journal disk`; `a write journal and a bitmap cannot be used together`.
  - `Label`: `mdadm <level> · <roles>[ · <chunk>][ · bitmap]`. `<roles>` is `N disks` (`1 disk`) when every disk is data, else `N data[ + M spare(s)][ + 1 journal]`. Examples: `mdadm raid10 · 4 disks · 512K`, `mdadm raid5 · 3 data + 1 spare · 512K · bitmap`, `mdadm raid5 · 3 data + 2 spares + 1 journal · 64K`, `mdadm raid1 · 2 disks · bitmap`, `mdadm linear · 2 disks`.
- Behaviour (command lines exact; every command goes through `Env.Run`):
  - `Prepare(v, l, name)`: `<Dev>/md/<name>` existing gives `/dev/md/%s already exists`. The uuid is 16 bytes from `Env.Rand` formatted `%08x:%08x:%08x:%08x`. It returns `&ArrayUndo{Name, UUID, Members: v.Disks}` and runs no command.
  - `Apply(v, l, rec)`: it runs `mdadm --create /dev/md/<name> --run --metadata=1.2 --name=<name> --uuid=<uuid> --level=<level> --raid-devices=<data>` then:
    - `--chunk=<n>K` when the level has a chunk;
    - for raid1, raid5, raid6, raid10 without a journal, `--bitmap=internal` or `--bitmap=none`;
    - `--spare-devices=<n>` if any;
    - `--write-journal=<journal path>` if any;
    - then the data paths in variant order, then the spare paths.
    - A command error returns `nil` and an error with the command, the error and its `*exec.ExitError` stderr.
    - Then it resolves the kernel name by reading the link `<Dev>/md/<name>` (polling every `Env.Poll` up to `Env.Settle`). If `<Sys>/block/<md>/md/array_state` is `read-auto`, it runs `mdadm --readwrite /dev/<md>`.
    - It returns one `BenchTarget{Kind: TargetArray, Target: {Name: md, Path: "/dev/"+md, Serial: uuid, Model: level, Vendor: "md", Interface: "md"}, UUID, Members: v.Disks, Want: v.Value.ID}`.
  - `WaitSteady`: for each array it polls `<Sys>/block/<md>/md/sync_action` every `Env.Poll`. A missing file or `idle` means steady. Otherwise `sync_completed` `a / b` reports `100*a/b`, averaged over the arrays; `none` or `delayed` reports 0. It returns `ctx.Err()` when cancelled and has no timeout.
  - `Read(t)`: it reads `md/level`, `raid_disks`, `chunk_size`, `degraded`, `array_state`, `sync_action`, `layout`, `bitmap/location` (missing or `none` means no bitmap) and `dev-*/block`, `dev-*/state` (`spare`, `journal`, otherwise data). It builds the label from what it read and appends, in this order:
    - ` (members differ: want <names> got <names>)` (sorted kernel names with roles compared to `t.Members`);
    - ` (degraded)`;
    - ` (array_state <s>)` when the state is not `clean`, `active` or `active-idle`;
    - ` (sync_action <a>)` when it is not `idle`.
    - `Kernel` is `array_state=<s>`, plus `, layout=<text>` for raid5/6 (`0` left-asymmetric, `1` right-asymmetric, `2` left-symmetric, `3` right-symmetric, else the number) and raid10 (`n<k>`, `f<k>`, `o<k>` from the near, far and offset fields of `md/layout`).
  - `Undo(u)` (algorithm):
    1. Find the md kernel name from the link `<Dev>/md/<name>` and confirm it with `mdadm --detail --export /dev/<md>` (`MD_UUID` equals the uuid, compared case-insensitively). Otherwise scan `<Sys>/block/md*` with the same command.
    2. If found, its `dev-*/block` names must all be among the recorded members' current names; if not, return `array %s has members bbbench did not record (%s); not touched`. Then run `mdadm --stop /dev/<md>`; on failure return an error naming `mdadm --stop`, without wiping anything. Then wait up to `Env.Settle` until `<Sys>/block/<md>/md` is gone or `array_state` is `clear` or `inactive`.
    3. For each member with a path, run `mdadm --examine --export <path>`. If `MD_UUID` equals the uuid, run `mdadm --zero-superblock <path>`. Then run `--examine --export` again: the uuid must be gone. A different uuid is left alone (logged). No superblock means nothing to do.
    4. A member with no current path (not present) gives the error `member S/N %s is not present; its superblock was not wiped`.
  - `UndoCommand(u)`: `mdadm --stop /dev/md/<name>; mdadm --zero-superblock <member paths in order>`.

- [ ] **Step 1: Write the failing tests** with a fake runner (scripted `"name args"` to output or error, and recorded calls), a temporary `Sys` tree and a temporary `Dev` directory. The fake `--create` writes `<Dev>/md/<name>` linking to `../md127` and the md sysfs files.
  - `TestMdadm_Label`: the five examples above. `TestMdadm_ParseRoundTrip`: every example parses back to its `Layout`.
  - `TestMdadm_Validate`: one row per message above, plus valid rows (raid5 3 data + 1 spare + bitmap).
  - `TestMdadm_ThroughLayout`: `NewLayout(env, NewMdadm(env)).ValidateLayout` gives `raid0 cannot have a spare disk`, `raid1 cannot have a journal disk`, `raid0 has no bitmap option`, `chunk must be one of 64K, 128K, 256K, 512K, 1024K`.
  - `TestMdadm_Identity` (name, levels, roles, options per level); `TestMdadm_Available` (`"mdadm is not installed"`); `TestAll_RegistersLayout`: `All(env)[1].Name() == "software layout"` and its backends are `["mdadm"]`.
  - `TestMdadm_PrepareRecordsNameAndUUID`: `Rand` returns the bytes `0x00..0x0f`, so the uuid is `"00010203:04050607:08090a0b:0c0d0e0f"` and no command runs. `TestMdadm_PrepareRefusesExistingName`.
  - `TestMdadm_CreateArgs`: an exact argv for raid5 3 data + 1 spare + bitmap at 512K; raid1 without a bitmap (`--bitmap=none`, no `--chunk`); raid0 (`--chunk=512K`, no `--bitmap`); raid6 with a journal (`--write-journal=/dev/nvme0n1`, no `--bitmap`); linear (no chunk, no bitmap). (mdadm adds an internal bitmap on its own to large redundant arrays, so `--bitmap=none` must be explicit.)
  - `TestMdadm_ApplyResolvesKernelName`: the returned target is exactly as specified above.
  - `TestMdadm_ApplyReadAutoSwitchesReadWrite` (Review Focus 4): `mdadm --readwrite /dev/md127` is called.
  - `TestMdadm_ApplyCreateFails`: the error contains the stderr `mdadm: cannot open /dev/sdb: Device or resource busy`.
  - `TestMdadm_WaitSteady`: `resync` with `50 / 200`, then `idle`, reports progress 25 then returns nil. raid0 without `sync_action` returns at once. A cancelled ctx returns `context.Canceled`.
  - `TestMdadm_ReadArray`: a clean raid5 with 3 data + 1 spare, a bitmap at 512K gives `Value == Want` and `Kernel == "array_state=clean, layout=left-symmetric"`. raid10 with layout `0x102` gives `layout=n2`.
  - `TestMdadm_ReadDetectsDegradedAndCheck` (Review Focus 3): `degraded 1` gives the ` (degraded)` suffix; `sync_action check` gives ` (sync_action check)`.
  - `TestMdadm_ReadDetectsUnexpectedBitmap`: `bitmap/location +8` when no bitmap was asked gives a value ending `· bitmap` that differs from `Want`. `TestMdadm_ReadWrongMember`.
  - `TestMdadm_UndoStopsAndWipes` (Review Focus 2): the calls in order are `--detail --export /dev/md127`, `--stop /dev/md127`, then per member `--examine --export`, `--zero-superblock`, `--examine --export`, and the result is nil.
  - `TestMdadm_UndoFindsArrayByUUID` (Review Focus 5): `/dev/md/<name>` is missing, `md126` reports the uuid, so `md126` is stopped.
  - `TestMdadm_UndoRefusesForeignMember` (Review Focus 5): `md127` has `dev-sdx` not recorded, so there is no `--stop` and the error contains `not touched`.
  - `TestMdadm_UndoArrayAlreadyStopped`: only the members whose examine shows the uuid are wiped. `TestMdadm_UndoSkipsForeignSuperblock`.
  - `TestMdadm_UndoStopFails`: the error contains `mdadm --stop` and `--zero-superblock` never runs. `TestMdadm_UndoMissingMember`.
  - `TestMdadm_UndoCommand`: `"mdadm --stop /dev/md/bbbench_1_1; mdadm --zero-superblock /dev/sdb /dev/sdc /dev/sdd"`.
- [ ] **Step 2: Run** `go test ./internal/settings -run 'Mdadm|All_'`. Expect FAIL.
- [ ] **Step 3: Implement** `mdadm.go`, with an `UNVERIFIED` comment on each mdadm item of the hardware list above, and register it in `All`.
- [ ] **Step 4: Run** `go test -race ./internal/settings ./cmd/bbbench` (the mode TUI still lists only `InputValues` settings, so nothing visible changes) and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Add the mdadm layout backend: build, resync wait, verification and teardown behind the Backend interface"`.

---

### Task 6: Layout groups in the loop: layouts in the recovery file, teardown after every run, the failure policy

**Files:**
- Modify: `internal/rungroup/recovery.go`, `internal/rungroup/recovery_test.go`, `cmd/bbbench/group_run.go`, `cmd/bbbench/group_undo.go`, `cmd/bbbench/group_flow.go`, `cmd/bbbench/serve_status.go` (steps and fields only)
- Create: `cmd/bbbench/group_layout_test.go`

**Interfaces:**
- Consumes: `settings.Undo.Array` (`ArrayUndo`), `BenchTarget.Backend`, `settings.Settler`, `LifetimeRun` (Tasks 2, 4); `rungroup.GroupVariant`, `GroupRun.Teardown`, `GroupDrive.Kind/UUID/Members`, `PlanVariants`, `MembersOf` (Task 1); `undoLedger` (Task 2).
- Produces:
  - `rungroup`: `type RecoveryArray struct{ Backend, Name, UUID string; Members []RecoveryMember; Extra map[string]string }` (tags `backend`, `name`, `uuid,omitempty`, `members`, `extra,omitempty`); `type RecoveryMember struct{ Serial, Model, Name string; Role settings.Role }`; `Recovery` gains `Arrays []RecoveryArray \`json:"arrays,omitempty"\`` (a drive-only file is unchanged). `RecoveryOf` and `Undos` handle layouts: members are resolved by serial and model, and an unresolved member keeps its recorded name with an empty `Path`. `Backend` and `Extra` are copied as they are.
  - `groupConfig` gains `Variants []settings.Variant` (layout input; nil for values input) and `MakeDrive func(bt settings.BenchTarget) (DriveInfo, error)` (called for composite targets; nil refuses them).
  - Status: `stepBuilding = "building"`, `stepResyncing = "resyncing"`, `stepTearingDown = "tearing-down"`. `GroupStatus` gains `Progress float64 \`json:"progress,omitempty"\`` and `Variant string \`json:"variant,omitempty"\`` (the label of the current layout).
  - `func layoutDesc(a settings.ArrayUndo) string`: `<backend> <name>`, plus ` (UUID <uuid>)` when the uuid is set (`mdadm bbbench_1_1 (UUID 0a:…)`, `lvm bbbench_1_2`). Every message below uses it, so the core prints the backend as data and never branches on it.
  - `reportUndoFailure` for a layout prints `*** TEARDOWN FAILED: %s; members %s: %v` (`layoutDesc`), `*** Tear it down and wipe its members by hand: %s` (`UndoCommand`) and `*** The recovery file %s is kept; bbbench offers the cleanup again at the next start.`
- Loop for a `LifetimeRun` setting (the order is the contract):
  - `runGroup`: plans come from `PlanVariants` over `cfg.Variants`; `g.Variants` comes from `MembersOf`; nothing is prepared at the start. The identity check runs over the union of the disks.
  - `runOne(idx)` with `v = cfg.Variants[idx]` and `v.Name = g.ID+"_"+strconv.Itoa(idx+1)`:
    1. Step `building`; the status drives are the variant's disks.
    2. `Prepare`. On error the run is `not_run` with the note `not built: <err>`.
    3. `ledger.add(undos...)`, which writes the file before the build. On error the group stops.
    4. `Apply`. On error the run is `not_run` with the note `build failed: <err>`.
    5. If the setting is a `Settler`: step `resyncing`, `WaitSteady`, and progress goes to `GroupStatus.Progress`. A cancelled ctx means the run is interrupted; any other error makes the run `not_run` with the note `resync failed: <err>`.
    6. Composite targets go through `MakeDrive`; on error the run is `not_run` with the note `cannot benchmark <name>: <err>`. Drive targets take their `DriveInfo` from `cfg.Drives`.
    7. Verify before, `Policy`, `Execute`, verify after, labels: the same code as values. `record()` writes each `GroupDrive` with `Kind`, `UUID`, `Members` for composites. A run with no target records the variant's disks with the note.
    8. Step `tearing-down`: `ledger.undo` on this run's undos, then `run.Teardown = {done}` or `{failed, names}`, then save. A failure returns `run %d of %d: teardown of %s failed; the group stops so that no further layout is built on disks still assembled`.
  - Every `not_run` path also goes through step 8 before the group moves on. An interrupted run returns at once, and the deferred `finishGroup` undoes whatever the ledger still holds.
  - Resume (`newGroupFile`): with `cfg.Variants` the resumed file must have the same number of `Variants`, the same `Value` and the same disk serials, models and roles per index. Otherwise the error is `run group: cannot resume group %s: layout %d differs (%s)`, and `sameDrivesPerValue` is skipped.
- `offerRecovery`, for a file with `Arrays`:
  - It lists `  %s: members %s` (`layoutDesc`, then `sdb (data), sdc (spare)`) and asks `Tear down these layouts and wipe their members now? [y/n] `.
  - `n` returns the error `not cleaned up: bbbench does not start while layouts it built are left (%s); run it again and accept, or tear them down and wipe their members by hand, then remove %s`.
  - A drive-only file behaves as today.
- `printGroupSummary` adds `      members: sdb (data), sdc (spare)` under a composite and `  run %d teardown FAILED on: %s` when it failed.

- [ ] **Step 1: Write the failing tests** in `group_layout_test.go`, with a `fakeLayout` (it implements `Setting`, `Settler` and `CommandHinter`, records calls, scripts per-variant Prepare, Apply, Read, Undo and WaitSteady results, and names the produced composites `md127`, `md126`, ...) and the existing `fakeExec`:
  - `TestRunGroup_LayoutBuildsRunsTearsDown`: two variants (`mdadm raid1` on sdb,sdc; `mdadm raid0` on sdb,sdc). Per run the calls are prepare, apply, wait, read, execute, read, undo, in that order. `Execute` gets one `DriveInfo` `md127` with two `Members`, and the labels are keyed `md127`. The group file has `Kind == "array"` with members, `Teardown.Status == "done"` per run, and `Restore.Status == "done"`. The recovery file is gone.
  - `TestRunGroup_LayoutMixedBackends`: variants `mdadm raid1 …`, `lvm raid1 …` and `none · 2 disks` run in that order; the records carry backends `mdadm` and `lvm`, and the group file holds the three labels (the core treats the backend as data).
  - `TestRunGroup_LayoutVariantNames`: the prepare calls get `v.Name == "<g.ID>_1"`, then `"<g.ID>_2"`.
  - `TestRunGroup_LayoutRecoveryWrittenBeforeBuild`: at `Apply` time the recovery file has `Arrays[0]` with the backend, the name, the uuid and the member serials.
  - `TestRunGroup_LayoutPrepareFailsNotRun` (Review Focus 1): no apply, the run is `not_run` with the note `not built: sdc is in use: mounted at /x`, and the next variant still runs.
  - `TestRunGroup_LayoutBeforeFailsTearsDownContinues`: Read differs from Want on run 1, so run 1 is `not_run` without `Execute`, run 1 is undone before run 2's prepare, and run 2 is `done`.
  - `TestRunGroup_LayoutApplyErrorNotRun`: the undo still runs (a partial build), and the group continues.
  - `TestRunGroup_LayoutAfterFailsInvalid` (Review Focus 3).
  - `TestRunGroup_LayoutTeardownFailureStopsGroup`: the undo fails on run 1, so `runGroup` returns an error, run 2 is never prepared, the recovery file keeps the layout, `Restore.Status == "failed"` with `Failed == ["bbbench_<id>_1"]`, and the report contains `TEARDOWN FAILED`, `mdadm bbbench_<id>_1 (UUID` and the `UndoCommand`.
  - `TestRunGroup_LayoutInterruptDuringResync`: `WaitSteady` blocks until the ctx is cancelled, so the run is `interrupted`, the undo runs and the recovery file is gone.
  - `TestRunGroup_LayoutResyncStatus`: progress 42 gives `GetStatus().Group.Step == "resyncing"` and `Progress == 42`.
  - `TestRunGroup_LayoutBaselineNone`: a none variant with four disks calls `Execute` with the four `DriveInfo`s, each labelled `none · 4 disks`, with no recovery entry and no undo.
  - `TestRunGroup_LayoutMakeDriveError`: the run is `not_run` with the note, and teardown runs.
  - `TestRunGroup_LayoutResumeRebuilds`: run 1 `done` and run 2 `interrupted`, so only variant 2 is prepared.
  - `TestRunGroup_LayoutResumeRefusesOtherDisks`.
  - `TestRecovery_ArraysRoundTrip` (rungroup): a record with backend `lvm`, no uuid and `Extra` survives `RecoveryOf` → JSON → `Undos` unchanged; a drive-only `Recovery` has no `"arrays"` key.
  - `TestRestoreFromRecovery_ArraysBySerial` (Review Focus 5): the members recorded as `sdb`, `sdc` are now `sdc`, `sdd`, so the `Undo` gets `Members` with the current names and paths.
  - `TestOfferRecovery_ArraysDeclineRefusesStart`: the output lists `mdadm bbbench_1_1 (UUID …): members …` and `lvm bbbench_1_2: members …`; `n` gives an error containing `does not start`; `y` undoes both and removes the file.
  - `TestPrintGroupSummary_Arrays`.
- [ ] **Step 2: Run** `go test ./internal/rungroup ./cmd/bbbench -run 'Layout|Arrays'`. Expect FAIL.
- [ ] **Step 3: Implement** the loop, the recovery records, the reports, the status fields and the offer.
- [ ] **Step 4: Run** `go test -race ./...` (all write-cache tests unchanged) and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Run layout groups: record each layout before building it, tear it down after every run and on every exit"`.

---

### Task 7: The LVM backend

**Files:**
- Modify: `internal/settings/settings.go` (`All`), `internal/rungroup/recovery_test.go`
- Create: `internal/settings/lvm.go`, `internal/settings/lvm_test.go`

**Interfaces:**
- Consumes: `Backend`, `Layout`, `Option`, `ArrayUndo`, `NewLayout`, the `Env` fields (Task 4); `RecoveryOf`/`Undos` with `Arrays` (Task 6).
- Produces (package `settings`): `func NewLVM(env Env) Backend` with `Name() == "lvm"`. `All(env)` returns `[NewWriteCache(env), NewLayout(env, NewMdadm(env), NewLVM(env))]`.
- Every lvm2 command runs as `lvm <command> ...` (one binary to look up; `pvcreate` and the others are links to it). Reports use `--reportformat json --units k --nosuffix` and parse `report[0].lv[]`, `.vg[]` or `.pv[]`. A `pvs` or `vgs` that exits non-zero on the object it was asked about means the object does not exist.
- Parameters:
  - `Available`: `LookPath("lvm")` fails gives `false, "the lvm2 tools are not installed"`.
  - `Levels`: `linear`, `striped`, `mirror`, `raid1`, `raid5`, `raid6`, `raid10`. `Roles`: `[data]` for every level.
  - `Options(level, n)`: `{Key: "stripes", Label: "stripes", Choices: "2" … "<n>" (just "2" when n < 2), Default: "<max(n,2)>"}` for striped; then `{Key: "stripesize", Label: "stripe size", Choices: 64K 128K 256K 512K 1024K, Default: "64K"}` for striped, raid5, raid6, raid10.
  - `Validate` messages (exact, in order): `%s needs at least %d disks, has %d` (linear 1, striped 2, mirror 2, raid1 2, raid5 3, raid6 5, raid10 4); `raid10 needs an even number of disks, has %d`.
  - `Label`: `lvm <type> · N disks[ · <s> stripes][ · <size> stripe]` (`1 disk`); the stripe count appears only for striped when it is less than N. Examples: `lvm linear · 1 disk`, `lvm striped · 4 disks · 64K stripe`, `lvm striped · 4 disks · 2 stripes · 128K stripe`, `lvm mirror · 2 disks`, `lvm raid1 · 3 disks`, `lvm raid5 · 3 disks · 64K stripe`, `lvm raid10 · 4 disks · 256K stripe`. `Parse` without a stripe count sets `stripes` to N.
- Behaviour (command lines exact; `<name>` is the VG name and the LV name):
  - `Prepare(v, l, name)`: `lvm vgs --reportformat json -o vg_name <name>` succeeding gives `volume group %s already exists`. Then `lvm lvmdevices`: when it succeeds (the host uses a devices file), the serials of the members whose path is not on a `DEVNAME=` line go to `Extra["devicesfile_added"]` (comma-separated; `pvcreate` will add them). It returns `&ArrayUndo{Name, Members: v.Disks, Extra}` with no uuid and changes nothing.
  - `Apply(v, l, rec)`:
    1. `lvm pvcreate --yes <paths>`; 2. `lvm vgcreate --yes --autobackup n <name> <paths>`;
    3. `lvm lvcreate --yes --autobackup n --activate y --name <name> --extents 100%FREE <type args> <name> <paths>`, with `<type args>`:
       - linear `--type linear`; striped `--type striped --stripes <s> --stripesize <k>k`;
       - mirror `--type mirror --mirrors <N-1> --mirrorlog core`; raid1 `--type raid1 --mirrors <N-1>`;
       - raid5 `--type raid5 --stripes <N-1> --stripesize <k>k`; raid6 `--type raid6 --stripes <N-2> --stripesize <k>k`; raid10 `--type raid10 --stripes <N/2> --mirrors 1 --stripesize <k>k`.
    4. A command error returns `nil` and an error with the command, the error and its stderr; what was built is left to `Undo`.
    5. It resolves the kernel name by reading the link `<Dev>/<name>/<name>` (`../dm-3`; polling every `Env.Poll` up to `Env.Settle`), then reads `lvm lvs --reportformat json --units k --nosuffix -o lv_uuid,lv_attr <name>/<name>`. An `lv_attr` whose fifth character is not `a` gives `logical volume %s/%s is not active (lv_attr %s)`.
    6. It returns one `BenchTarget{Kind: TargetArray, Target: {Name: dm, Path: "/dev/"+dm, Serial: lv_uuid, Model: type, Vendor: "lvm", Interface: "lvm"}, UUID: lv_uuid, Members: v.Disks, Want: v.Value.ID}`.
  - `WaitSteady`: for mirror and raid targets (by `Target.Model`) it polls `lvm lvs --reportformat json --units k --nosuffix --select lv_uuid=<uuid> -o sync_percent` every `Env.Poll` until `100.00`, reporting the average; linear and striped targets are steady at once (no command). It returns `ctx.Err()` when cancelled and has no timeout.
  - `Read(t)`: `lvm lvs --reportformat json --units k --nosuffix --select lv_uuid=<uuid> -o vg_name,lv_name,segtype,stripes,stripe_size,lv_attr,sync_percent,lv_health_status` (no row gives `logical volume %s not found`) and `lvm pvs --reportformat json --select vg_name=<vg> -o pv_name`. The type is the `segtype` (`raid5_*` reads as raid5, `raid6_*` as raid6), N the number of PVs, the stripe count from `stripes` for striped, the stripe size from `stripe_size`. It builds the label from what it read and appends, in this order:
    - ` (members differ: want <names> got <names>)` (sorted PV kernel names against `t.Members`);
    - ` (not active)` when the fifth `lv_attr` character is not `a`;
    - ` (health <s>)` when `lv_health_status` is not empty;
    - ` (syncing <p>%)` for mirror and raid when `sync_percent` is below 100.
    - `Kernel` is `segtype=<s>, lv_attr=<attr>`.
  - `Undo(u)` (algorithm):
    1. `lvm vgs --reportformat json -o vg_name <name>`. If the VG exists, `lvm pvs --reportformat json --select vg_name=<name> -o pv_name`: every PV's kernel name must be among the recorded members' current names, else `volume group %s has physical volumes bbbench did not record (%s); not touched`. Then `lvm lvremove --yes --autobackup n <name>` (on failure an error naming `lvremove`, nothing else is run), `lvm vgremove --yes --autobackup n <name>` (on failure an error naming `vgremove`), and `vgs` again: the VG must be gone (`volume group %s still exists after vgremove`).
    2. For each member with a path: `lvm pvs --reportformat json -o pv_name,vg_name <path>`. Not a PV: nothing to do. A PV of another VG: left alone, logged. An orphan PV (a recorded member was not a PV before the build: Prepare's in-use check refuses PVs): `lvm pvremove --yes <path>`, then `pvs` again must fail (`%s still carries an LVM label after pvremove`).
    3. For each serial in `Extra["devicesfile_added"]` whose path `lvm lvmdevices` still lists: `lvm lvmdevices --deldev <path>`.
    4. A member with no current path gives `member S/N %s is not present; its LVM label was not removed`.
  - `UndoCommand(u)`: `lvremove -y <name>; vgremove -y <name>; pvremove -y <member paths in order>`.

- [ ] **Step 1: Write the failing tests** with the fake runner of Task 5 (scripted JSON reports), a temporary `Dev` directory where the fake `lvcreate` writes `<Dev>/<name>/<name>` linking to `../dm-3`:
  - `TestLVM_Identity`, `TestLVM_Available` (`"the lvm2 tools are not installed"`), `TestLVM_Options` (striped with 4 disks: stripes `2 3 4` default `4`, then stripe size default `64K`; raid5: stripe size only; linear, mirror, raid1: none).
  - `TestLVM_Validate`: raid6 with 4 disks gives `raid6 needs at least 5 disks, has 4`; raid10 with 5 gives the even message; linear with 1 disk is valid.
  - `TestLVM_LabelAndParse`: the seven examples round trip.
  - `TestLVM_PrepareRefusesExistingVG`; `TestLVM_PrepareRecordsDevicesFile`: `lvmdevices` lists `/dev/sdb` only, so `Extra["devicesfile_added"] == "S2,S3"`; `lvmdevices` failing gives no `Extra`; no command other than `vgs` and `lvmdevices` runs.
  - `TestLVM_CreateArgs`: the exact `pvcreate`, `vgcreate` argv, and one `lvcreate` argv per type (striped with 2 of 4 stripes at 128K; raid10 with 4 disks gives `--stripes 2 --mirrors 1`).
  - `TestLVM_ApplyResolvesDM`: the returned target is exactly as specified above.
  - `TestLVM_ApplyRefusesInactiveLV` (Review Focus 4): `lv_attr` `rwi---r---` gives the `is not active` error.
  - `TestLVM_ApplyCreateFails`: the error contains the `lvcreate` stderr `Insufficient free space`.
  - `TestLVM_WaitSteady`: raid1 at `25.00` then `100.00` reports 25 then returns nil; striped runs no command; a cancelled ctx returns `context.Canceled`.
  - `TestLVM_ReadLV`: a clean raid5 on 3 PVs at 64K gives `Value == Want` and `Kernel == "segtype=raid5, lv_attr=rwi-a-r---"`.
  - `TestLVM_ReadDetectsHealthAndSync` (Review Focus 3): `lv_health_status` `refresh needed` gives ` (health refresh needed)`; `sync_percent` `87.50` gives ` (syncing 87.5%)`. `TestLVM_ReadWrongMembers`.
  - `TestLVM_UndoRemovesAll` (Review Focus 2): the calls in order are `vgs`, `pvs --select`, `lvremove`, `vgremove`, `vgs`, then per member `pvs`, `pvremove`, `pvs`, and the result is nil.
  - `TestLVM_UndoRefusesForeignPV` (Review Focus 5): the VG also has `/dev/sdx`, so there is no `lvremove` and the error contains `not touched`.
  - `TestLVM_UndoAfterPartialBuild`: no VG, `sdb` an orphan PV, `sdc` not a PV, so only `pvremove --yes /dev/sdb` runs.
  - `TestLVM_UndoLeavesOtherVGsPV`; `TestLVM_UndoLvremoveFails` (the error names `lvremove`; no `vgremove`, no `pvremove`); `TestLVM_UndoDevicesFile` (`lvmdevices --deldev /dev/sdc`); `TestLVM_UndoMissingMember`.
  - `TestLVM_UndoCommand`: `"lvremove -y bbbench_1_1; vgremove -y bbbench_1_1; pvremove -y /dev/sdb /dev/sdc"`.
  - `TestAll_RegistersLayout` (updated): the backends are `["mdadm", "lvm"]`. `TestLayout_MdadmAndLVMValues`: `ParseLayout` of an mdadm and an lvm raid5 value reaches each backend, and the two values differ.
  - `TestRecovery_LVMRecordRoundTrip` (rungroup): a record from `NewLVM`'s shape (backend `lvm`, no uuid, `Extra`) resolves its members by serial at new names.
- [ ] **Step 2: Run** `go test ./internal/settings ./internal/rungroup -run 'LVM|All_|MdadmAndLVM'`. Expect FAIL.
- [ ] **Step 3: Implement** `lvm.go`, with an `UNVERIFIED` comment on each LVM item of the hardware list above, and register it in `All`.
- [ ] **Step 4: Run** `go test -race ./...` and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Add the LVM layout backend: linear, striped, mirror and raid LVs built, verified and torn down behind the Backend interface"`.

---

### Task 8: Composite targets in a run

**Files:**
- Create: `cmd/bbbench/group_targets.go`, `cmd/bbbench/group_targets_test.go`
- Modify: `cmd/bbbench/generate_common.go`, `cmd/bbbench/orchestrator.go` (`discoverDrivesWithFioFiles` uses `fioFileName`), `cmd/bbbench/orchestrator_diskinfo.go`, `cmd/bbbench/group_flow.go` (`newGroupConfig`), `cmd/bbbench/serve_groups_test.go` (`newGroupConfig` calls), `cmd/bbbench/orchestrator_fsync_test.go`, `internal/flushtrace/session_test.go`

**Interfaces:**
- Consumes: `settings.BenchTarget` (Tasks 2, 4), `groupConfig.MakeDrive` (Task 6), `DriveInfo.Members/UUID`, `MemberStatus` (Task 1).
- Produces:
  - `func fioFileName(host string, d blockdev.Device) string`: the exact current pattern `{host}_{manufacturer}_{model}_{serial}_{subsystem}_config.fio` sanitized, with `unknown` for an empty vendor. It is used by `generateFioFilesForDrives` and by discovery.
  - `func fioGenerator(cfg *config.Root, eng *templating.Engine, host string) func(blockdev.Device) (string, error)`: renders one device with `generateFioFilesForDrives` into `expandUser(cfg.Fio.Generated.Path)` and returns `filepath.Join(outBase, host, fioFileName(host, d))`.
  - `func arrayDrive(bt settings.BenchTarget, drives map[string]DriveInfo, sysBlock string, gen func(blockdev.Device) (string, error)) (DriveInfo, error)` (no branch on the backend):
    - `Device{Name, Path, Vendor, Model, Serial}` from `bt.Target`, and `ParentSubsystem` = `bt.Target.Interface`.
    - `Rotational` from the **data** members' `Device.Rotational`. Mixed data members give `mixed media: %s is HDD, %s is SSD`; a member missing from `drives` gives `member %s is not a discovered drive`.
    - `BlockCount` is `<sysBlock>/<name>/size`, `BlockSize` is `queue/logical_block_size`, and `CapacityBytes = BlockCount*512`.
    - `Members = rungroup.MembersOf(bt.Members)`, `UUID = bt.UUID`, `FioFilePath` from `gen`, `FioExists = true`.
  - `newGroupConfig(c *groupChoice, targets map[string]settings.Target, opts runOptions, host string, resume *rungroup.GroupFile, makeDrive func(settings.BenchTarget) (DriveInfo, error)) groupConfig`. For layouts, `Precheck` runs `precheckRun` on the union of the disks (their fio files carry the same workloads).
  - `collectDiskConfig`: for a composite drive it collects each member, not the composite (`collect(DriveInfo{Device: blockdev.Device{Name, Serial, Model}})`). It sets `DriveStatus.Members[i].Config` on a cloned slice and writes `<member>_diskinfo_<start>.txt`.
  - Flush and write-through need no new branch: `flushtrace.Start("md127", ...)` and `flushtrace.Start("dm-3", ...)` already take the `Block` path, and `flushGate` reads the device's own `queue/write_cache`. The tests pin this.

- [ ] **Step 1: Write the failing tests:**
  - `TestArrayDrive_FromMembers`: two SSD members, `size` 2000 and `logical_block_size` 4096 in a fake sysfs, give `Rotational == false`, `CapacityBytes == 1024000`, `BlockSize == 4096`, `ParentSubsystem == "md"`, two `Members` with roles, and the `FioFilePath` that `gen` returned. The same for an LVM target `dm-3` gives `ParentSubsystem == "lvm"`.
  - `TestArrayDrive_MixedMediaRefused`; `TestArrayDrive_JournalAndSpareIgnoredForMedia` (HDD data with an SSD journal gives `Rotational == true` and no error); `TestArrayDrive_UnknownMember`.
  - `TestFioFileName_MatchesDiscovery`: the path from `discoverDrivesWithFioFiles` equals `fioFileName` for the same device.
  - `TestFioGenerator_ArrayFile`: with the embedded config and templates, `md127` gives a file containing `filename=/dev/md127` whose name ends `_md_config.fio`, and `dm-3` (vendor and interface `lvm`) one containing `filename=/dev/dm-3` ending `_lvm_config.fio`.
  - `TestCollectDiskConfig_ArrayMembers`: a composite with members `sdb`, `sdc` calls `collect` for `sdb` and `sdc` only, the status has both `Members[i].Config` set, and `sdb_diskinfo_<t>.txt` and `sdc_diskinfo_<t>.txt` exist.
  - `TestFlushGate_ArrayOwnWriteCache`: a fake `sysBlock` with `md127/queue/write_cache = write through` and an fsync phase gives a warning starting `md127 is write through`.
  - `TestSession_MdUsesBlockCollector` (flushtrace): `md127` with `dev` `9:127` enables the block events, like `TestSession_BlockDeviceUsesDevNumbers`; `TestSession_DmUsesBlockCollector`: `dm-3` with `dev` `253:3` the same.
  - `TestCoreDoesNotNameTheBackend`: reads `../../internal/rungroup/*.go`, `group_run.go`, `group_flow.go`, `group_undo.go`, `group_targets.go` and `serve_*.go` (excluding `_test.go`) and fails on any case-insensitive `mdadm`, `lvm`, `pvcreate`, `vgcreate` or `lvcreate`, naming the file, the line and the word.
- [ ] **Step 2: Run** `go test ./cmd/bbbench ./internal/flushtrace -run 'ArrayDrive|FioFileName|FioGenerator|ArrayMembers|ArrayOwnWriteCache|MdUsesBlock|DmUsesBlock|NameTheBackend'`. Expect FAIL.
- [ ] **Step 3: Implement** `group_targets.go`, the `fioFileName` refactor (no output change), the `collectDiskConfig` branch and the `newGroupConfig` parameter. Production `makeDrive` is `func(bt) { return arrayDrive(bt, byName(c.Drives), "", fioGenerator(cfg, eng, host)) }`, built in `runOrchestrator` (Task 9 loads `eng`).
- [ ] **Step 4: Run** `go test -race ./...` and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Benchmark composite targets: a fio file for the md or dm device, workload type and disk info from its members"`.

---

### Task 9: The layout TUI and the orchestrator wiring

**Files:**
- Create: `cmd/bbbench/group_layout_tui.go`, `cmd/bbbench/group_layout_tui_test.go`
- Modify: `cmd/bbbench/group_tui.go` (mode list, `groupChoice`), `cmd/bbbench/group_tui_test.go` (`TestMode_ListsSettingsWithDriveCounts`: the variant setting is now listed; an intended change), `cmd/bbbench/group_flow.go` (`resumeChoice`, `inUsePaths`), `cmd/bbbench/group_targets_test.go` (`TestCoreDoesNotNameTheBackend` also reads `group_layout_tui.go`), `cmd/bbbench/orchestrator.go`, `cmd/bbbench/help.go`

**Interfaces:**
- Consumes: `settings.LayoutSetting` (`Backends`, `ValidateLayout`, `LayoutValue`, `ParseLayout`), `Backend.Name/Available/Levels/Roles/Options`, `WithDefaults`, `LevelNone` (Task 4); `NewMdadm`, `NewLVM` (Tasks 5, 7) in tests only; `inuse.Scan`, `Report.Reason` (Task 3); `estimateRun`; `fioGenerator`/`arrayDrive` (Task 8).
- Produces:
  - `groupChoice` gains `Variants []settings.Variant`. For layouts, `Values` is the variants' values in order, `Strategy` is `rungroup.Sequential`, and `Drives` is the union of the disks in order of first use.
  - `func newLayoutModel(s settings.LayoutSetting, drives []DriveInfo, targets map[string]settings.Target, busy *inuse.Report, busyErr error) layoutModel`; `func runLayoutSetupTUI(s settings.LayoutSetting, drives []DriveInfo, targets map[string]settings.Target, busy *inuse.Report, busyErr error) (*groupChoice, error)`. A disk is grayed when `busyErr != nil` (`in-use check failed: <err>`), when `busy.Reason` is not empty, or when `Applicable` is false. The model only knows the backends through the interface: no backend name or option key appears in it.
  - `func inUsePaths(exe, outDir string) map[string]string`: maps `exe` to `"the bbbench binary"`, and the nearest existing ancestor of `outDir` to `"the output directory"`.
  - `resumeChoice` rebuilds the `Variants` from `g.Variants`, matching disks by serial and model, and parses each value with `ParseLayout`. A disk that is now busy gives `cannot resume group %s: %s is in use: %s`. A layout group skips the known-values check (its values are layout strings).
- Screens and keys (decided here):
  - **List**: `a` adds a layout, `b` adds the baseline (`none`, every free disk as data), `Enter` edits the selected layout, `d` deletes it, `c` continues, `q` quits. Continuing with fewer than two layouts shows `Add at least two layouts to compare (the no-array baseline counts as one).`
  - **Backend** (after `a`, only when more than one backend is available): one row per backend in `Backends()` order; an unavailable one is grayed with its reason and cannot be chosen. `↑/↓` and `Enter` choose, `Esc` goes back. With one available backend this screen is skipped; with none, `a` shows `no layout backend is available: <reasons>`.
  - **Editor** (title `Layout <n> (<backend>)`): rows are level (`←/→` through `Levels()`), then one row per `Options(level, data disks)` entry (`←/→` through its `Choices`; a `no`/`yes` option also toggles with `Space`), then one row per disk. `Space` on a disk cycles unused → each role of `Roles(level)` in order → unused. Every change of level or of the data disks re-applies `WithDefaults`. A grayed disk row shows `<name>  <vendor> <model>  <reason>`, and `Space` on it sets the message `<name> is in use: <reason>` (or `<name>: <reason>` when not applicable).
  - The editor shows the live `ValidateLayout` error, or the value label when the layout is valid. `Enter` saves only a valid layout. When data members differ in capacity it warns `members differ in capacity (sdb 4000 GB, sdc 2000 GB): the layout uses the smallest`. `Esc` cancels.
  - **Confirm**:
    - Header: `Confirm the layout test: THE DISKS BELOW WILL BE ERASED`.
    - One block per run, `Run <n>: <label>` then `    <name>  <vendor> <model> (S/N <serial>)  <role>`.
    - `Disks erased: <names>` (every disk used: layouts are built on them, and the baseline benchmark overwrites them).
    - `<runs> runs of the full test plan; estimated time <d>, plus the initial resync of each layout, which is not included`. The estimate per variant is `estimateRun` of its first data disk; for the baseline it is all its disks.
    - `Each layout is torn down and its members wiped after its run, also after an error or Ctrl+C. Nothing is built before you press y.`
    - Keys: `y` starts, `Esc` goes back, `q` quits. `Enter` does nothing here.
- Orchestrator: after `runModeTUI`, a setting whose `Input()` is `InputVariants` (asserted to `settings.LayoutSetting`) gets `busy, berr := inuse.Scan(inuse.Options{Paths: inUsePaths(os.Executable(), outDir)})`, then `runLayoutSetupTUI(setting, drives, targets, busy, berr)` with the filtered discovered drives. The drive selection TUI is skipped. The templates are loaded for `fioGenerator`, and `makeDrive` is passed to `newGroupConfig`. `--dry-run` is still refused.

- [ ] **Step 1: Write the failing tests** (drive the model's `Update` with key messages, as `group_tui_test.go` does; the setting is `settings.NewLayout(env, settings.NewMdadm(env), settings.NewLVM(env))` with a fake `LookPath`):
  - `TestMode_ListsVariantSettings`: `software layout` appears with its count.
  - `TestLayout_BackendPickerFirst`: with both tools, `a` shows `mdadm` and `lvm`; choosing `lvm` opens `Layout 1 (lvm)` with LVM levels.
  - `TestLayout_SingleBackendSkipsPicker`: without `lvm`, `a` opens `Layout 1 (mdadm)` at once. `TestLayout_UnavailableBackendGrayed`: with both listed and `lvm` missing, the picker shows `the lvm2 tools are not installed` and `Enter` on it does nothing. `TestLayout_NoBackend`.
  - `TestLayout_BusyDiskGrayedWithReason`: `sdb` mounted at `/data` shows its reason in the view; `Space` on it leaves it unused with the message `sdb is in use: mounted at /data`. `TestLayout_InUseCheckFailedGraysAll`.
  - `TestLayout_RolesCycle`: mdadm raid5 cycles data, spare, journal, unused; mdadm raid0 cycles data, unused; lvm raid5 cycles data, unused.
  - `TestLayout_RejectsTooFewDisks`: mdadm raid5 with two data disks keeps the editor open and shows `raid5 needs at least 3 data disks, has 2`.
  - `TestLayout_OptionRowsPerBackend`: mdadm raid1 has no chunk row; mdadm raid0 no bitmap row; lvm striped has `stripes` and `stripe size`; lvm raid1 has no option row.
  - `TestLayout_StripesFollowDisks`: lvm striped with 4 disks defaults to 4 stripes (label without a stripe count); choosing 2 gives `· 2 stripes ·`; removing two disks resets the stripes to 2.
  - `TestLayout_BaselineVariant`: `b` with four free disks gives `none · 4 disks`.
  - `TestLayout_DiskInSeveralVariants` (one mdadm and one lvm layout on the same disks); `TestLayout_NeedsTwoVariants`; `TestLayout_CapacityWarning`.
  - `TestLayout_ConfirmListsEveryDiskAndLayout`: the view contains `WILL BE ERASED`, every label, every serial and `resync of each layout, which is not included`.
  - `TestLayout_NothingBuiltBeforeConfirm`: a fake runner counts every command and stays at 0 through every screen; `Enter` on the confirm screen does not start; only `y` produces the choice.
  - `TestLayout_OutcomeVariants`: `Variants` keep the order and roles, `Values` are the labels, `Drives` is the union, `Strategy` is sequential.
  - `TestInUsePaths_OutputDirNotYetCreated`.
  - `TestResume_LayoutGroupRebuildsVariants`: the disks are matched by serial at new names, and a now-busy disk gives the error.
  - `TestCoreDoesNotNameTheBackend` (extended): `group_layout_tui.go` is read too.
- [ ] **Step 2: Run** `go test ./cmd/bbbench -run 'Layout|Mode_|InUsePaths|Resume_Layout|NameTheBackend'`. Expect FAIL.
- [ ] **Step 3: Implement** `group_layout_tui.go`, the mode list change and the orchestrator flow. Add one line for layout tests to `help.go`.
- [ ] **Step 4: Run** `go test -race ./...` and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Let the user pick a backend, list layouts with their options and member roles, see busy disks grayed, and confirm the erase once"`.

---

### Task 10: Web UI: members, resync progress, teardown

**Files:**
- Modify: `cmd/bbbench/serve_status.go` (JS), `cmd/bbbench/serve_groups.go` (page JS), `cmd/bbbench/serve_shared.go` (`chartSharedJS`), `cmd/bbbench/serve_graphs.go` and `cmd/bbbench/serve_compare.go` (card subtitle), `cmd/bbbench/serve_browse.go` (run details), `documentation/web-ui.md`
- Test: `cmd/bbbench/serve_status_test.go`, `serve_groups_test.go`, `serve_groups_pages_test.go`

**Interfaces:**
- Consumes: `DriveStatus.Kind/Members`, `GroupStatus.Progress/Variant`, steps (Tasks 1, 6); `GroupFile.Variants`, `GroupRun.Teardown`, `GroupDrive.Kind/UUID/Members`; `DiskPhaseData.Kind/Members`; `DriveInfo.Members` in `/api/results/detail`.
- Produces (JS; escape every inserted string with the page's `esc`/`escG`; no backend name in any page, the label carries it):
  - Status: `membersHTML(drive)` lists each member's name, model, serial and role, with its interface and SMART health from `config` when present, or `''` for a drive. `groupBannerHTML` shows the step texts `building the layout`, `resyncing <round(progress)>%`, `tearing down`, and the `variant` label.
  - Group page: `renderVariants(g)` lists each layout label with its disks and roles. `renderRuns` writes the drive cell of a composite as `<name> · array` with a dim member list, and uses the prefix `layout:` instead of `write_cache:` for the kernel reading when `d.kind === 'array'`. `renderTeardown(run)` shows `torn down` or `teardown failed on <names>`. For layout groups the page shows `Torn down` / `Teardown failed on: ...` instead of the restore wording.
  - Shared `membersText(d)` gives `members: sdb (data), sdc (data), sdd (spare)` or `''`. It is added to the card subtitle and the modal title in graphs and compare.
  - Run details: `drivesHTML(result)` lists the drives, with members under a composite.

- [ ] **Step 1: Write the failing tests:**
  - `TestStatusAPI_CarriesMembersAndProgress`: `/api/status` JSON has `drives[0].kind == "array"`, `drives[0].members[0].role == "data"`, `group.step == "resyncing"`, `group.progress == 42.4`.
  - `TestStatusPage_LayoutScript` (node on the served page): `groupBannerHTML` with `{step: 'resyncing', progress: 42.4}` contains `resyncing 42%`; `membersHTML` escapes a model `<b>x</b>`.
  - `TestHandleGroupDetail_Arrays`: the JSON contains `variants`, a run drive with `kind: "array"` and `members`, and `teardown.status`.
  - `TestGroupPage_ArrayScript` (node): `renderRuns` on an mdadm sample and on an lvm sample (`dm-3`) contains the member names, `layout:` and no `write_cache:`; `renderVariants` lists each label and disk; `renderTeardown` on a failed sample names the layout.
  - `TestGraphsPage_MembersSubtitle` (node): `membersText` on a composite entry and on a drive entry (`''`).
  - `TestResultDetail_ShowsArrayMembers`: the detail JSON has `drives[0].Members`, and node `drivesHTML` lists them.
- [ ] **Step 2: Run** `go test ./cmd/bbbench -run 'MembersAndProgress|LayoutScript|Group.*Array|MembersSubtitle|ArrayMembers'`. Expect FAIL.
- [ ] **Step 3: Implement** the JS and keep `chartSharedJS` free of backticks. Pages for write-cache groups render as before (the existing page tests pass unchanged).
- [ ] **Step 4: Verify** the dumped status, group, graphs, compare and result pages for a stored layout group with an mdadm and an lvm run: `node --check` each.
- [ ] **Step 5: Docs:** `web-ui.md` describes composite targets (md arrays and LVs), members, the resync banner and teardown.
- [ ] **Step 6: Run** `make check`, then **commit** `git commit -m "Show layout targets, their members, resync progress and teardown in the web pages"`.

---

### Task 11: Docs, the dependencies, the changelog

**Files:**
- Modify: `cmd/bbbench/deps.go`, `cmd/bbbench/deps_test.go`, `README.md`, `CLAUDE.md`, `CHANGELOG.md`, `documentation/flush-latency.md`, `documentation/web-ui.md`

- [ ] **Step 1: Write the failing test:** update `TestCheckDependencies_*` counts for `mdadm`, `lvm` and `udevadm`. Run them and expect FAIL.
- [ ] **Step 2: Add** to `dependencies`, all optional:
  - `{"mdadm", []string{"--version"}, false, "building software RAID arrays in a layout setting test (mdadm backend)", "mdadm"}`
  - `{"lvm", []string{"version"}, false, "building logical volumes in a layout setting test (LVM backend), and listing LVM physical volumes so that they are never offered", "lvm2"}`
  - `{"udevadm", []string{"--version"}, false, "waiting for udev after a layout teardown", "udev (systemd)"}`
  Run `go test ./cmd/bbbench -run Dependencies` and expect PASS.
- [ ] **Step 3: README:**
  - A "Layout setting test (mdadm, LVM)" section opening with **`A layout setting test ERASES every disk it uses: the layouts are built on them and the benchmark writes to them. Their data is not kept and not restored; after the test they carry no array, no md superblock and no LVM label.`**
  - What the section covers: the backends and how one is chosen; per backend the levels, options, roles and minimum disks; the baseline; which disks are grayed and why (including LVM PVs); the single `y` confirmation; the resync wait (ETA without it); verification before and after; teardown on every exit; the recovery file now also holding layouts, and the refusal to start when its cleanup is declined; the failure policy; resume.
  - The output files table gains the layout fio files (`{host}_md_{level}_{uuid}_md_config.fio` and `{host}_lvm_{type}_{lv uuid}_lvm_config.fio` under `fio.generated.path`).
- [ ] **Step 4: CLAUDE.md:**
  - Rewrite the `internal/settings` paragraph for the new interface (`Lifetime`, `Prepare`/`Apply`→targets/`Read`/`Undo`, `PerDrive`, `Settler`, `CommandHinter.UndoCommand`, and how to add a setting: implement `Setting`, or `DriveOps` + `PerDrive` for drive scope, register it in `All`; how to add a layout backend: implement `settings.Backend`, pass it to `NewLayout` in `All`).
  - Add `backend.go`/`layout.go`/`mdadm.go`/`lvm.go` and `internal/inuse`. Add `group_undo.go`, `group_targets.go` and `group_layout_tui.go` to "Run groups in cmd/bbbench". Add `mdadm`, `lvm`, `udevadm` under External programs.
  - Extend the safety bullet: a layout test erases its disks, in-use disks (LVM PVs included) are never offered, one confirmation, teardown on every exit, and the recovery file records layouts before they are built.
  - Add a **"What the hardware-RAID branch plugs into"** note, from the spec's last section:
    - a controller backend (`perccli`/`storcli`, `ssacli`) implements `settings.Backend`, every method listed in Task 4, and is passed to `NewLayout` in `All`; nothing else needs a case for it;
    - its levels are the controller's RAID levels and JBOD/passthrough, its `Options` the controller's (stripe size, read and write cache policy), its `Roles` data and hot spare;
    - `Apply` creates the virtual drive and returns it as `BenchTarget{Kind: TargetArray}` with the physical disks as `Members` (from `diskinfo.Controller` and the vendor tool, run through `Env.Run`); `Read` verifies it through the vendor tool; `Undo` deletes it; the controller and virtual-drive ids go in `ArrayUndo.Extra`, which the recovery file stores as it is;
    - `inuse` applies to the physical disks;
    - a controller mode the user sets in the BIOS (RAID vs HBA) is a separate `Manual` setting and needs the multi-invocation groups of the first spec, which `runGroup` refuses today;
    - the core (`internal/rungroup`, the group loop, the undo ledger, the recovery offer, `arrayDrive`, the layout TUI, the pages) never names a backend and never branches on one; `TestCoreDoesNotNameTheBackend` lists the names to keep out, and the next branch adds its tools to it.
- [ ] **Step 5: CHANGELOG** entry under Added (the layout setting with the mdadm and LVM backends, `internal/inuse`, the generalized interface, the recovery file layouts, the web UI). `flush-latency.md` gets one paragraph: on an md array or an LV the generic block collector is used on the md or dm device (exact mean, no percentiles); this is not yet verified on hardware, and a phase may show 0 Flushes because md and dm are bio-based.
- [ ] **Step 6: Run** `make check` and the docs audit used before: every flag, endpoint and file name in the README exists in the code; `grep -rn "never change\|only reads" README.md CLAUDE.md documentation` wording matches the new rule.
- [ ] **Step 7: Commit** `git commit -m "Document layout setting tests with the mdadm and LVM backends, the erase warning and what hardware RAID plugs into"`.
