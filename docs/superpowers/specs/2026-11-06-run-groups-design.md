# Run groups: iterate the test plan over configurations

Status: design, not built. Branch `run_groups`.
Items marked **(call)** are defaults chosen while writing this, not decisions
the user made; they are listed again under "Open points".

## Intent

bbbench runs a test plan (the workload phases of `dist/default.yml`) on a set
of drives. A recurring need is to run the **same plan under several
configurations** and compare: drive write cache on and off, a RAID layout (see
the `raid_testing` branch), power management, and ideas not yet had.

Today the user does this by hand: configure the drives as A, run the
orchestrator, configure them as B, run it again. Nothing in the results says
which run had which setting, so the user has to remember, and comparing means
picking runs one by one.

This design adds an organisation layer above a run, the **run group**, and a
way for bbbench to create one: apply a setting to the drives, run the plan,
apply the next value, run again, and keep every run labelled with the setting
it ran under, verified, and visible in the web UI.

## Vocabulary

- **Setting**: a thing that can be changed on a drive, with a known way to read,
  apply and restore it. First one: *disk write cache policy*. Later: RAID layout
  (software, mdadm), power management.
- **Value**: one choice of a setting (`write cache: enabled`, `disabled`).
- **Run**: one execution of the whole test plan, as today (one coordinator, one
  `run_<unix>_summary.json`).
- **Run group**: the set of runs of one plan that differ by the value of one
  setting. A normal run is a group of one with no setting.
- **Label**: the value a drive carried during a run. It belongs to the pair
  (run, drive), not to the run: with the parallel option a run holds drives
  with different values.

## Goals

1. Create a group from the orchestrator: choose *single run* or *setting test*.
2. For a setting test, offer the available settings (write cache policy first)
   and the values to test.
3. bbbench applies each value to the selected drives, runs the full plan, moves
   on to the next value, and puts every drive back as it found it.
4. The value is **verified before and after each run**, per drive, against the
   drive itself (not assumed). Both readings are stored.
5. When selected drives are identical (same vendor, model, capacity), offer to
   run values **in parallel** on different drives. Optionally a **swap round**
   that rotates the values over the drives, to cancel drive-to-drive variance.
   Not the default.
6. **Everything is visible in the web UI**: live status, the group, every run's
   per-drive value, both verification readings, invalid or not-run runs, the
   restore result, and comparison of the variants.
7. Old runs keep working and appear as groups of one.

## Non-goals (first version)

- The RAID layout setting (it uses the same mechanism later; the mdadm build and
  teardown code is in `raid_testing`) and power management.
- Combining two settings in one group (a matrix); one setting per group.
- Verification between phases; it is per run.
- Changing a setting while a run is in progress.
- Creating groups without the TUI (flags or a file). The mechanism should not
  prevent it; building it is left to the builder if cheap.

## Anti-goals (a failure even if it technically works)

- A drive left with a changed setting after bbbench exits, including after a
  crash or a signal.
- Results measured under a setting that was not in effect being shown as valid.
- Existing runs, pages or files breaking.
- Reconfiguring a drive the user did not select or confirm.

## Creating a group (orchestrator TUI)

Order: drive selection (as today) -> **mode** -> setting -> values -> strategy
-> confirmation -> run.

1. **Mode**: *single run* or *setting test*. Single run is today's flow,
   unchanged.
2. **Setting**: a list of available settings, each shown with the number of
   selected drives it applies to, and the reason for those it does not.
3. **Values**: the user ticks the values to test. For write cache both
   `enabled` and `disabled` start ticked **(call)**.
4. **Strategy**, offered only when it is possible:
   - *one value after the other* (default): every selected drive gets the same
     value in a run; one run per value.
   - *parallel*: identical drives are divided among the values, all values run
     at the same time in one run.
   - *parallel with swap round*: parallel, then the values are rotated over the
     drive subsets and the plan runs again, so each value is measured on every
     subset. Not the default.
5. **Confirmation**: a summary of what will be changed on which drives, the
   number of runs, the estimated time, and the statement that the original
   values will be restored. Nothing is applied before the user confirms.

Parallel rules **(call)**: drives are classed by (vendor, model, capacity).
Parallel is offered only if every selected drive belongs to a class whose size
is a multiple of the number of values, and each class is divided evenly among
the values. Otherwise the TUI says why it is not offered. With K values and
subsets S1..SK, the swap rounds are the K cyclic rotations (each subset sees
each value once); for two values this is the simple swap. Different firmware in
one class is shown as a warning, not a blocker.

## The setting abstraction

A setting provides, per drive:

- `Applicable(drive)`: yes, or the reason it is not (for write cache: a RAID
  virtual drive, USB, virtio, a drive with no volatile cache).
- `Values(drives)`: the values it can offer.
- `Read(drive)`: the current value, from the drive itself.
- `Apply(drive, value)` and `Restore(drive, original)`.
- `Verify(drive, value)`: reads back and returns the readings to store.

**Write cache policy** per interface (read-only commands to check, volatile
settings only, never "save"):

| Interface | Read | Apply |
|---|---|---|
| NVMe | `nvme get-feature /dev/nvmeN -f 6` | `nvme set-feature /dev/nvmeN -f 6 -V 0\|1` |
| SATA | `hdparm -W /dev/sdX` | `hdparm -W0\|1 /dev/sdX` |
| SAS / SCSI | MODE SENSE caching page (`scsi.GetWCE`, already in the repo) | `sdparm --set WCE=0\|1` |

The kernel's `queue/write_cache` is read back too and stored next to the drive
reading. It is informational for the verification **(call)**: the drive-level
value decides validity, but if the kernel does not follow (a stock kernel does
not flip it), the run carries a warning, because the Flush-latency phases depend
on it. Applying waits a bounded time for the reading to settle before declaring
a failure.

This reverses the documented rule that bbbench never changes a drive's
configuration. After this change the rule is: it changes it only in setting-test
mode, after an explicit confirmation, and always restores it. README,
`flush-latency.md`, `web-ui.md`, CLAUDE.md and the warning texts must say so.

## Verification and failure policy

Per drive, per run:

- **Before**: after applying, before the first phase.
- **After**: after the last phase, before the next value is applied or the
  originals are restored.

Both are stored with the readings and the time.

Failure policy **(call)**:

- *Before* fails on any drive of a run: the run is **not started**. It is shown
  as "not run: setting not effective", with the readings, and the group continues
  with the next run.
- *After* fails on a drive: the run is kept and that drive's results are flagged
  **invalid**. Other drives of the run stay valid.
- Failing to restore is reported prominently (UI and console) with the exact
  command to restore by hand; it never silently passes.

## Data model

New file per group in the output directory, `group_<unix>_group.json`:

```
id, created, host, plan {workload names}, setting {name},
strategy ("sequential" | "parallel" | "parallel-swap"),
values [..],
originals { drive: value },                 // what to restore
runs [ { run_id, round, status: planned|running|done|not_run|invalid|interrupted,
         drives [ { device, value, verified_before {value,kernel,at}, verified_after {...}, valid, note } ] } ],
restore { status: pending|done|failed, per-drive results }
```

Each run's summary (`run_<unix>_summary.json`) gains `group_id` and a per-drive
`setting` {name, value, verified readings}, so a run stays self-describing if the
group file is lost. Runs without these fields are groups of one, computed when
loading (no migration).

A **recovery file** (`.bbbench-group-recovery.json`) records the original value of
every drive **before the first apply** and is removed after a successful
restore. If it exists at startup, the orchestrator offers to restore before
doing anything else **(call)**.

`--resume` of a group continues at the first run that is not done, reusing the
per-run state file for the partial run.

## Orchestrator control flow

One orchestrator process owns the whole group. A loop is added above the
existing per-run machinery; the run itself (coordinator, phases, fio, summary)
is reused unchanged:

```
confirm -> write recovery file
for each run in the plan:
    apply values -> verify before (failure policy)
    create coordinator, execute the plan, save the run summary
    verify after
restore originals (also on error, SIGINT/SIGTERM, panic) -> verify restore -> remove recovery file
write the group file (after every run, so a crash keeps what is done)
```

Restore runs from a deferred function and from the signal handler, so every exit
path except a hard kill restores; a hard kill leaves the recovery file.

## Web UI (all of it, first version)

- **Status tab**: a group banner (setting, run i of n, strategy); per drive card
  the current value, verification badges (before and after: pending, ok, failed
  with readings), and during restore its state. The ETA covers the remaining
  runs, not only the current one.
- **Results list**: runs grouped under their group (expandable), showing the
  setting and values; single runs as before.
- **Group page** (`/group?id=`): setting, values, strategy, host, plan; a table
  run x drive with value, both verification readings, validity and a link to the
  run; the restore result; a small matrix of the main metrics per phase and per
  value.
- **Graphs and compare for a group** (`/graphs?group=`, `/compare?group=`): the
  data of the member runs together, with a new filter dimension **setting
  value**, so cards and overlays contrast the variants. Invalid results are
  hidden by default with a toggle to show them, flagged **(call)**.
- **Run details**: shows its group and each drive's value and verification.
- **API**: `/api/groups`, `/api/groups/detail?id=`, `/api/groups/graphs?id=`.

## Safety

- Nothing is applied before the confirmation screen.
- Only drives the user selected are touched. Drives in use (mounted, swap, md/dm
  holders) are refused once the in-use protection of `raid_testing` is merged;
  until then the confirmation screen shows them.
- Restore is verified, not assumed.

## Testing

- Fake sysfs and a fake command runner for each setting source (NVMe, SATA, SAS),
  including a failing apply, a failing read-back, and a drive that does not follow.
- Unit tests for: the parallel split and swap rotations, per-drive labels, the
  verification state machine and failure policy, the recovery file lifecycle,
  group-of-one for old runs.
- Node checks (`node --check` plus logic runs) of the new page scripts against a
  stored group.
- Not testable without hardware, to be listed in the PR: real `nvme set-feature`,
  `hdparm -W` and `sdparm` behaviour, and a kernel that follows the cache state.

## Open points

1. Failure policy (above) is a default; the user has not confirmed it.
2. Crash recovery: offering to restore at the next start vs. a separate command.
3. "Identical" = vendor, model, capacity; firmware only warned about.
4. Swap rounds for more than two values use cyclic rotation.
5. Invalid results hidden by default in graphs.
6. Whether a non-interactive way to define a group is wanted soon.

## Left to the builder

File and function names, the TUI layout of the new screens, how the group file
is written atomically, how the status singleton is extended, the exact wording
of messages and the style of the new pages (follow the existing ones).
