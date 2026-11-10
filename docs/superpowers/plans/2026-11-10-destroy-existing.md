# Destroy an existing RAID configuration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** From the layout list screen the user selects one existing md/LVM configuration, confirms by typing its name, and bbbench tears it down safely so its disks become selectable.

**Architecture:** An optional `settings.Destroyer` provider interface (`Existing`, `Destroy`, `DestroyCommand`); a provider-neutral safety gate in the layout setting backed by machine-readable `inuse` reasons; mdadm and LVM implement it by generalising their `Undo` helpers; the layout TUI gets the `x` flow with injected callbacks.

**Tech Stack:** Go, bubbletea (existing TUI).

**Spec:** `docs/superpowers/specs/2026-11-10-destroy-existing-design.md`

## Global Constraints

- Destroys only the single configuration the user selected, re-found by `Backend`+`ID` and re-checked (disk serial/model) immediately before; refuses when the gate says so; never touches ZFS/btrfs/bcachefs/LUKS/Ceph/DRBD/VMFS, partition tables or plain filesystems.
- Refuses anything mounted, used as swap, holding the bbbench binary or its output, or stacked under/over something that is not part of the selected configuration.
- Top-down, every step verified by reading back; a failure returns what is left and the manual command; success is only reported when the disks are verified free of the configuration's signature and holders.
- Typed-name confirmation (the configuration's name + Enter); Esc cancels; nothing runs before it.
- The core (cmd/bbbench, serve_*.go) never names mdadm/lvm (`TestCoreDoesNotNameTheBackend`).
- Code that writes to a drive lives in `internal/settings` only (CLAUDE.md rule); it only runs after the typed confirmation.
- Needs the tools of the provider (`Available()` false => lists nothing, screen says why).
- Atomic commits with intent in the message, ending `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. No push, no PR.

## Review Focus

- A filesystem mounted on the md device or on an LV of the selected VG is refused (the mount is attributed to the member disks through holders).
- An md array stacked under another md/LV that is not part of the selection is refused.
- A VG spanning a disk the user did not see is refused (all PVs are in `Disks`).
- The array/VG changed between listing and confirmation (uuid, members, serial): refused, not destroyed.
- Unassembled md superblock group: the disk's own superblock uuid is proven before it is zeroed.
- A half-failed teardown (stop ok, zero-superblock fails; lvremove ok, vgremove fails) reports exactly what remains.
- Degraded arrays, arrays with a spare or journal member, inactive arrays, inactive VGs.

---

### Task 1: Machine-readable in-use reasons

**Files:** Modify `internal/inuse/inuse.go` (and `lvm.go`, `mountinfo.go` as needed), `internal/inuse/inuse_test.go`.

**Interfaces:**
- Produces: `type Kind string` with `KindMount, KindSwap, KindPath, KindHolder, KindMD, KindPV, KindSignature`; `type Cause struct{ Kind Kind; Disk string; Via string; Text string }` (`Via` = the device the reason comes from: the holder node `md127`/`dm-3`, the mount point, the VG name, "" when none; `Text` equals the existing reason string); `Report.Causes map[string][]Cause` filled in lockstep with `Busy` (every string in `Busy[d]` has exactly one Cause, same order); helper `func (r *Report) Blockers(disks []string, owned func(Cause) bool) []Cause` returning the causes of those disks for which `owned` is false. `Busy` strings and `Reason` are unchanged byte for byte.
- [ ] Tests: `TestCausesMirrorBusy` (every Busy string has its Cause, for mount, swap, path, md holder, dm holder, LVM PV, signature), `TestBlockersFiltersOwned`, existing tests untouched and green.
- [ ] Commit "Give every in-use reason a machine-readable cause next to its text"

### Task 2: Destroyer interface and the safety gate

**Files:** Create `internal/settings/destroy.go`, `internal/settings/destroy_test.go`; modify `internal/settings/layout.go` (`LayoutSetting` methods) and `internal/settings/backend.go` (add `Existing`, `Destroyer` types and the two `LayoutSetting` methods).

**Interfaces:**
- Consumes: `inuse.Report.Causes/Blockers` (Task 1), `Env.InUse`, the udev settle used after `Undo` in layout.go.
- Produces: `Existing`, `Destroyer` exactly as in the spec; `LayoutSetting.Existing(ctx) ([]Existing, []error)`; `LayoutSetting.Destroy(ctx, e Existing) error`; `func (l *layout) DestroyBlockers(ctx, e Existing) ([]string, error)` (the dry gate check the TUI uses to gray entries: human-readable reasons, empty = allowed; it is exactly steps (1)-(2) of the gate, so Destroy and the picker cannot disagree); the gate's `owned(Cause)` rule: a cause is owned only when it is KindHolder/KindMD/KindPV/KindSignature(md/lvm) whose `Via`/VG is one of `e`'s own composite devices or VG (providers state which in `Existing.Extra["owned"]` as a comma list of node names and VG names; the core only compares strings).
- [ ] Tests with a fake `Destroyer` backend: Existing aggregation with a failing provider (error kept, others listed), gate refusals (mount, swap, path, foreign holder, foreign signature), config changed between listing and Destroy (ID gone; serial differs), provider error returns the manual command, post-check failure (disks still held) returns an error naming what is left, success path order (gate, provider, settle, re-scan).
- [ ] Commit "Add the Destroyer provider interface and a provider-neutral safety gate for destroying an existing configuration"

### Task 3: mdadm provider

**Files:** Modify `internal/settings/mdadm.go` (+ `mdadm_destroy_test.go`).

**Interfaces:** `(*mdadm).Existing(ctx) ([]Existing, error)`, `Destroy(ctx, Existing) error`, `DestroyCommand(Existing) string` per the spec; `Existing.Extra["owned"]` = the md node name; `Existing.Extra["assembled"]` = "yes"/"no"; reuse `findArray`, `run`, `sameUUID`, `mdAttr` and the verification helpers of `Undo`; no `bbbench_` name rule (this feature destroys foreign arrays); refuse an array with a member it cannot find a disk for; for unassembled groups prove each disk's superblock uuid (`mdadm --examine --export`) equals the selected uuid before `--zero-superblock`.
- [ ] Tests (fake `Run`, fake sysfs): listing (assembled raid1/raid5, degraded, spare, inactive, unassembled pair, two arrays), Destroy argv order (`--stop` then `--zero-superblock` per member), verification failure after stop (still assembled), zero-superblock failure on one member (reports remaining, manual command lists the rest), uuid mismatch refuses, unassembled path.
- [ ] Commit "Let the mdadm provider list and destroy existing md arrays"

### Task 4: LVM provider

**Files:** Modify `internal/settings/lvm.go` (+ `lvm_destroy_test.go`).

**Interfaces:** same three methods; `Existing` per volume group (LVs with active state, PVs) and per orphan PV; `Extra["owned"]` = the VG name and its LVs' dm node names; `Disks` = ALL PVs of the VG (resolved to whole disks); `Destroy` of a VG: deactivate and `lvremove` LVs, `vgremove`, `pvremove` each PV, remove the devices-file entry for each PV it removed (reuse `removeLabel`/`removeDevicesEntries` logic generalised so the `bbbench_` rule and the recorded-serial rule are replaced by "PV listed in the selected Existing"), verify each step; orphan PV: `pvremove`. Mark the command lines UNVERIFIED on hardware in a doc comment.
- [ ] Tests (fake `Run`): listing (VG with 2 LVs/2 PVs, inactive VG, orphan PV, VG with PV on a partition), Destroy argv order, vgremove failure leaves PVs and reports, VG with PV outside the selection refused, devices-file handling.
- [ ] Commit "Let the LVM provider list and destroy existing volume groups and orphan physical volumes"

### Task 5: TUI flow and wiring

**Files:** Modify `cmd/bbbench/group_layout_tui.go`, `cmd/bbbench/orchestrator.go` (~line 256, the `runLayoutSetupTUI` call, and the `inuse.Scan` closure ~line 209), `cmd/bbbench/group_layout_tui_test.go`.

**Interfaces:** `runLayoutSetupTUI` gets a `destroy layoutDestroyer` argument: `type layoutDestroyer struct{ List func(ctx) ([]settings.Existing, []error); Blockers func(ctx, settings.Existing) ([]string, error); Destroy func(ctx, settings.Existing) error; Rescan func() (*inuse.Report, error) }` (nil = key `x` disabled, and a one-line hint says so). New steps `layoutStepDestroyPick`, `layoutStepDestroyConfirm`, `layoutStepDestroyRun`. Key `x` on the list screen; picker (up/down, Enter, Esc) with summary, disks and blockers (blocked entries grayed with the first reason and not selectable); confirmation lists `Stack` and every disk under a red "THIS DESTROYS DATA" header and takes the configuration's name typed + Enter (anything else keeps waiting, Esc cancels, wrong name shows "name does not match"); the run step executes `Destroy` as a `tea.Cmd` (spinner, no key except ctrl-c ignored while running), then calls `Rescan` and rebuilds `grayed`/`busy`/`carries`/`warnings` through `newLayoutModel`'s logic (factor it, do not duplicate; layouts already in the list that now use disks that became busy are kept untouched, and the existing per-layout checks at confirm time still apply); success message "<name> destroyed; <disks> are free"; failure shows the error and the manual command. The list screen help line gets "x destroy an existing RAID configuration".
- [ ] Tests (bubbletea model driven by key messages): `x` opens the picker; blocked entries are grayed and not selectable; wrong typed name does nothing; right name calls Destroy once; list refreshes (freed disks no longer grayed); failure keeps the config in the list and shows the manual command; nil destroyer disables `x`; `TestCoreDoesNotNameTheBackend` green.
- [ ] Commit "Add the x action to the layout list screen to destroy a selected existing RAID configuration"

### Task 6: Docs

**Files:** `README.md`, `CHANGELOG.md`, `CLAUDE.md`.

- [ ] State: what `x` does, the typed confirmation, the erase warning first, exactly what is touched (md arrays and their superblocks, LVM LVs/VGs/PVs) and never (ZFS, btrfs, bcachefs, LUKS, Ceph, DRBD, VMFS, partition tables, filesystems), the refusal list (mounted, swap, bbbench/output, foreign stack), that it writes no recovery record, the unverified-on-hardware commands. CLAUDE.md: `Destroyer`, the gate, `inuse.Report.Causes`, the new TUI steps. `make check` passes.
- [ ] Commit "Document destroying an existing RAID configuration from the layout screen"
