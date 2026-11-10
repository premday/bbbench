# Destroy an existing RAID configuration (storage layout screen)

## Intent

Disks that already belong to a RAID configuration (an assembled md array, an
md superblock without an array, an LVM volume group, an orphan physical
volume) are grayed out in the layout screens and can never be offered. The
user, who owns the machine, wants to free such disks from the same screen
where a layout is added, without leaving bbbench: pick one specific existing
configuration, see exactly what goes, confirm, and have it torn down properly.

## Decisions (user)

- It lives on the layout list screen, at the same level as "add a layout".
- The user **must select** the configuration to destroy, one at a time. There is
  no "destroy everything" and nothing is destroyed implicitly.
- "Properly" = top-down, each step verified by reading back, not a blind wipe.

## Goals / non-goals / anti-goals

- Goals: list what each provider finds (assembled arrays, unassembled md
  superblocks, volume groups, orphan PVs) with its members and what sits on it;
  destroy the selected one; re-scan so the freed disks become selectable.
- Non-goals: ZFS, btrfs, bcachefs, LUKS, Ceph, DRBD, VMFS, partition tables and
  plain filesystems are never touched (their disks stay grayed with the reason).
  Hardware RAID is the next branch (it implements the same interface).
- Anti-goals (failure even if it "works"): destroying anything mounted, used as
  swap, holding bbbench or its output, or stacked under something that is not
  part of the selected configuration; destroying a configuration other than the
  one shown to the user; a half-torn-down configuration reported as success.

## Design

- **Interface** (`internal/settings/destroy.go`, optional, so a provider that has
  nothing to destroy need not implement it):
  `type Existing struct{ Backend, ID, Name, Summary string; Disks []Target;
  Stack []string; Extra map[string]string }` (`ID` is the provider's stable key,
  the md uuid or the VG name; `Disks` carry kernel name, path, serial, model;
  `Stack` is the human-readable list of what is destroyed with it, top first, e.g.
  `lv data/home`, `md127 (raid5, clean)`, `pv /dev/sdb`) and
  `type Destroyer interface{ Existing(ctx) ([]Existing, error);
  Destroy(ctx, Existing) error; DestroyCommand(Existing) string }`.
  `LayoutSetting` gets `Existing(ctx) ([]Existing, []error)` (all providers that
  implement `Destroyer`, per-provider errors kept) and
  `Destroy(ctx, e Existing) error` (dispatch + the core safety gate below).
- **Core safety gate** (in the layout setting, provider-neutral, runs before the
  provider is called): (1) re-list `Existing` and find the selected one again by
  `Backend`+`ID`; refuse if it is gone or its disks (serial, model) differ from
  what the user saw; (2) re-scan `inuse` and refuse when any of these holds for
  the configuration's disks, the composite devices on them (md/dm nodes) or
  anything stacked above those: a mount, swap, the bbbench binary or its output
  directory, a holder or a signature that does not belong to the selected
  configuration itself (another md, a dm device that is not one of its LVs,
  LUKS, ZFS, ...); the md/LVM signatures and holders that make up the
  configuration are expected and ignored; (3) call the provider; (4) wait for
  udev; (5) re-scan and require that the disks no longer carry the
  configuration's signature and are no longer held; otherwise return an error
  that says what is left and the manual command (`DestroyCommand`).
  To support (2) `inuse.Report` gets machine-readable reasons (kind: mount, swap,
  path, holder, md, lvm-pv, other signature; and the device they come from) next
  to the existing `Busy` strings, which keep their text.
- **mdadm provider**: `Existing` = assembled arrays (`/sys/block/md*/md`, uuid from
  `mdadm --detail --export`, members from `md/dev-*`) plus disks that carry an md
  superblock but belong to no assembled array (grouped by uuid via
  `mdadm --examine --export`); `Destroy` of an assembled array: find it by uuid,
  refuse unknown members, `mdadm --stop`, `mdadm --zero-superblock` on each
  member, verify gone (reuse the helpers of `Undo`); of an unassembled group:
  `--zero-superblock` on each disk after proving the disk's superblock uuid is
  the selected one.
- **LVM provider**: `Existing` = volume groups (LVs, PVs, active or not) plus
  orphan PVs; `Destroy` of a VG: deactivate and `lvremove` its LVs, `vgremove`,
  `pvremove` each PV, drop the devices-file entry of each PV it removed, verify
  each step; an orphan PV: `pvremove` (reuse the helpers of `Undo`, without its
  `bbbench_` name rule, which is exactly what this feature must not require).
  A VG that has a PV on a disk that is not among the selected configuration's
  disks (a VG spanning disks the user did not see) is refused: its `Disks` are
  all of its PVs.
- **TUI** (`group_layout_tui.go`): on the list screen the key `x` ("destroy an
  existing RAID configuration") opens a picker of `Existing` entries (per
  provider, with summary and disks; entries the safety gate would refuse are
  grayed with the reason, from a dry gate check). Enter on one opens the
  confirmation screen: everything in `Stack` and every disk is listed under a
  red "THIS DESTROYS DATA" header and the user confirms with a single `y` (user's decision, like the layout
  confirmation: Enter does nothing, anything else cancels); Esc cancels.
  Then the teardown runs (the screen shows progress and the provider's error with
  its manual command on failure), the in-use report and the grayed disks are
  recomputed, and the list screen shows "<name> destroyed; sdb, sdc are free".
  The screens never name a provider.
- **No recovery record**: nothing is built, so there is nothing to restore; the
  action writes nothing to the recovery file. What was destroyed and the result
  is printed to the log (`logger.Info` with names and disks).
- Roots: needs root like the rest of layout tests; `Available()` of a provider
  whose tool is missing means it lists nothing and the screen says so.

## Tests

Fake tools and fake sysfs like the existing provider tests: listing (assembled,
unassembled, degraded, spare, nested md-on-LV, VG with LVs, orphan PV); Destroy
steps in order with argv checked; every refusal in the gate (mounted FS on the
md, mounted LV, swap on an LV, output dir on the array, extra partition mounted
on a member, stacked foreign holder, VG with a PV outside the selection, array
whose uuid changed between listing and destroy, member serial changed); a failed
step reports what is left plus the manual command; TUI flow (picker grays blocked
entries, wrong typed name does nothing, right name runs, list refreshes);
`TestCoreDoesNotNameTheBackend` stays green.

## Docs

README (a section under the layout setting: what the key does, the erase
warning, what is and is not touched, the typed confirmation), CHANGELOG, CLAUDE.md
(Destroyer, the gate, the inuse reasons), `documentation/web-ui.md` is unaffected.
Unverified-on-hardware list: the new mdadm/LVM commands.
