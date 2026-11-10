# Web UI

The `bbbench serve` command (also started automatically by `orchestrator` during a run) exposes a web interface on `http://0.0.0.0:12345` by default.

## Benchmark Status

The **Status** page shows live progress while `orchestrator` is running.

- **Current Status** — running/idle indicator, overall progress bar, current phase number, and completed phase count.
- **Drives** — one card per benchmarked drive showing model, serial, capacity, current status, and active phase name, then its configuration (below) and its phases.

- **Run group** — during a setting test a banner above the drives reads `Setting test: <setting>, run <i> of <n>, <step>` with the strategy and an ETA that covers the runs still to do (`ETA unknown` when a phase has no known length). Each drive card shows its value and `before`/`after` badges (`pending`, `ok`, `failed`; the reading and the kernel's value are in the tooltip), `invalid` when the drive did not hold the value, and `restoring` while the group restores. Nothing changes outside a run group; the restore outcome is on the group page.
- **Layout groups** (storage layout setting test) — the banner also names the run's layout, `run <i> of <n> (layout <label>)`, and the steps read `building the layout`, `resyncing <x>%` (bbbench waits for the initial resync to finish before measuring) and `tearing down`; the ETA leaves the resync time out. The benchmark target is a composite: an md array (`md127`) or a logical volume (`dm-3`), drawn as `<name> (SSD) · array`. Its card lists the member disks instead of a configuration: name, model, serial and role (`data`, `spare`, `journal`), with each member's interface and SMART health once read (the configuration is read from the members, not from the composite). Drive cards show `tearing down` while the layout is removed.

Each drive card also shows how the disk is configured, read once when the run starts (read-only: reading it changes nothing; bbbench changes a drive's setting only in a setting test, see [Run Groups](#run-groups)):

| Line | Meaning | Source |
|---|---|---|
| Interface | `NVMe (PCIe)`, `NVMe (tcp)`, `SATA`, `SAS`, `USB`, `virtio` | sysfs |
| Slot | where the disk physically sits: the SES enclosure slot or SAS bay for SAS/SATA drives behind an HBA, the SMBIOS system slot (type 9) for NVMe, the kernel's PCI slot table when SMBIOS is unreadable, or the SMBIOS onboard device (type 41) with the SATA port | sysfs, SMBIOS |
| Controller | what it hangs off: NVMe controller, SATA (AHCI), SAS HBA, RAID controller, USB, virtio, with its name, driver, PCI address and IDs, firmware and BIOS version and SAS address. Behind a RAID controller it says whether the disk looks like a **RAID virtual drive** (the physical disks are hidden: benchmark, SMART, temperature and slot are the volume's) or a passthrough disk. | sysfs, `lspci` |
| Link | negotiated link, e.g. `8.0 GT/s PCIe x4`, `6.0 Gbps`; the maximum is shown when known, with **below max** when the link runs slower (bad slot or cable, or the link saving power) | sysfs |
| Power | NVMe power state and APST (`nvme get-feature`), runtime PM, PCIe ASPM policy and link state, SATA link power policy | sysfs, `nvme` |
| Write cache | the kernel's policy (`write back` or `write through`: only write back drives get Flush commands) and the drive's own cache (enabled or disabled) | sysfs, `nvme`, SG_IO |
| SMART | health (`PASSED` / `FAILED`) with temperature, wear, power-on hours and error counts | `smartctl`, else `nvme smart-log` |

Some of these need root (the SMBIOS table is readable by root only; without it the slot falls back to the kernel's PCI slot table, which needs no root, and may be empty), and SMART needs `smartctl` (smartmontools) or, for NVMe, `nvme-cli`. A line that cannot be read is left out, and SMART says why it is unknown.

The same information, in full, is written to a text file per disk in the run's output directory, `<device>_diskinfo_<start time>.txt` (one per run, so a resumed run keeps what the first one saw). It holds the summary, every sysfs value read, and the complete output of each command, including the reports the summary is drawn from: `smartctl -x`, and `nvme id-ctrl`, `nvme id-ns` and `nvme smart-log` for NVMe or `hdparm -I` for SATA. A command that failed (no root, tool not installed) is listed with its error, so the file shows what could not be read.

Each drive's **Phases** list shows its time as it goes: before a phase runs, its expected length (`~5m`, or `length unknown` for a full-disk write); while it runs, the time elapsed against the expected one; once it has finished on that drive, the real time (`took 5m 2s`, with the expected one beside it, finished phases struck through). The header sums the real time spent so far and gives an ETA for what remains. The ETA adjusts as phases finish: the expected length of the remaining phases is scaled by how the finished ones really compared with theirs (fio's setup and ramp-up, a slower drive, ...), and the running phase counts down from that. Phases of unknown length are counted apart, not guessed.

The page auto-refreshes so you can monitor a long run without manually reloading.

## Browsing Results

The **Results** page lists every run found in the output directory (time, ID, mode, drives, phases) with a search box (ID, mode or drive name). Each row links to its **Details**, **Summary**, **Graphs** and **Compare** pages.

The runs of a setting test (run group) are listed under one expandable row for their group, placed where its newest run is: the setting and its values, the strategy, how many runs are listed and their status, and links to the **Group** page and to the group's **Graphs** and **Compare** (`/graphs?group=`, `/compare?group=`). The ▸ button shows or hides its runs. Each member run names its group (a link to `/group?id=`) and the value(s) it ran under; the search box also matches the group ID and values. Single runs are listed as before. `/api/results` carries `group_id` and `setting_values` for member runs only.

- **Details** shows the run's overview, its drives, and its phases with duration and job count. A phase that did not run on a drive (`--skip-write-through`) shows the reason, and a phase that ran with a caveat, such as a write-through drive that never receives a Flush, shows the warning with how to check and fix it. An **Export HTML** button downloads a standalone report. For a run of a run group, a **Run group** card links to the group (and its graphs and compare) and lists each drive's setting, value, the readings before and after the run (with the kernel's value), and whether the result is valid, with the reason when it is not. A run on an array target adds a **Drives** card that lists the target with its member disks and their roles (`/api/results/detail` carries them as `Members` of the drive).
- **Summary** is a table of statistics (min, average, max and standard deviation) per phase, broken down by disk.

## Run Groups

A setting test (run group, see the orchestrator) is served at `/group?id=<group id>`. The page shows the setting, its values, the strategy, host and plan, then a table of runs by drive: the value, the reading before and after the run (with the kernel's own value, for the write cache the `write_cache` of the block device), whether the result is valid, and a link to the run. When a phase of a run carried a write-through warning, the row says so, which explains missing Flush data. Below come the result of restoring the original values and a matrix of the main metrics (IOPS and MB/s: mean, min-max and n) per phase and value, taken over valid results only; a value with no valid result is shown as incomplete. A group that is still running refreshes by itself and appears while it runs.

A layout group (storage layout setting test) adds a **Layouts** card listing each run's layout label with its disks (name, model, serial, role). In the runs table an array target is written `<name> · array` with its members below (`members: sdb (data), sdc (spare)`), the kernel reading is prefixed `layout:` (the array or volume state, such as `array_state=clean`), and each run says `torn down` or `teardown failed on <names>`. The last card is **Teardown** instead of Restore: `Torn down.` or `Teardown failed on: <names>` (the layouts still assembled, to tear down and wipe by hand, see the recovery offer at the next start). Values compare by their label string, so the same layout in two runs is one value of the matrix.

The same data is available as JSON: `/api/groups` (list), `/api/groups/detail?id=` (group file, per run summary, kernel readings, `flush_note` and the matrix; for a layout group also `variants`, each run's `teardown`, and per drive `kind: "array"`, `uuid` and `members`) and `/api/groups/graphs?id=` (the graph data of the member runs together; each entry carries `setting_value` and `run_id`, and `invalid` for results that did not verify; the results of an interrupted run are kept but marked `invalid`, since the setting was not verified after it; runs that were not run contribute nothing).

## Graphs and Navigation

The **Results** page lets you explore time-series charts from a completed (or in-progress) run.

The left sidebar provides filters that update the displayed graphs in real time:

| Filter | Description |
|--------|-------------|
| **Graph Type** | Toggle IOPS, Bandwidth (MB/s), and Latency (µs) charts independently. |
| **Drive Type** | Narrow down to HDD or SSD results. |
| **Vendor / Model / Serial** | Select specific hardware identities. |
| **Disk** | Choose which drives to include. |
| **Phase** | Select one or more benchmark phases (e.g. `seqread`, `randread-randread`). |
| **Setting value** | Run groups only (`/graphs?group=<group id>`, and a member run of a group by its own `/graphs?id=`): the values of the setting (e.g. write cache `enabled`/`disabled`), to show or contrast the variants. |
| **Hide invalid results** | Run groups only (the group, or one of its member runs by id), on by default: results whose drive did not hold its value are left out. Unchecked, they are shown and their cards say `(invalid)`. |

`/graphs?group=<group id>` shows the member runs of a group together (`/api/groups/graphs`); `/graphs?id=<run id>` shows one run; a member run of a group viewed by its own id still carries its label, so it gets the **Setting value** filter and hides invalid results by default, while a plain run shows neither. In a group, a card is titled `disk · value`, and `disk · value · round N` when a value was measured in several runs (parallel-swap), since the same disk appears in each run. A card of an array target adds its members to the subtitle (`members: sdb (data), sdc (data), sdd (spare)`), and so does the enlarged chart's title.

Each matching combination of disk × phase is rendered as its own graph card. Click a card to maximize it; press **Esc** to return to the grid.

**Time axis.** All time-series graphs use one x axis, in seconds from the start of the phase, from 0 to the longest series shown, so cards line up. fio's `ramp_time` is shaded as **ramp-up**: fio logs nothing during it, so the fio curves start where it ends. A point is the average over one polling window (`--log-avg-msec`) and is drawn at the start of its window; hover it to see the window. fio stamps the end of a window, so its points are moved back by one window, which keeps the first point at the end of the ramp-up instead of one interval later.

**Array phases.** For a run on an array target (md or LVM layout) the IOPS and bandwidth charts stack the members (data, spare and journal disks): reads up, writes mirrored down, one area per member, with the array's own fio series as a line outside the stack; hovering lists each member, `Stack total` and the array value. Latency has one line per member and direction and is never stacked. The member values come from the kernel's counters on each member (`/sys/block/<dev>/stat`, sampled once per `--log-avg-msec` window and stored in `<fio log prefix>_members.1.log`, served as `ts_members`), so they include md/LVM overhead (parity, mirroring, read-modify-write, resync) and the stack total can exceed the array value. Member latency is the kernel's per-request time (queue plus service, 1 ms tick resolution, coarse at low load); the array line is fio's. Runs made before this feature have no member series, and the compare page does not draw them. **md state track** (md arrays only): the sampler also stores the array's `sync_action`, `sync_completed`, `sync_speed`, `degraded`, `array_state`, `raid_disks` and each member's state (`/sys/block/<array>/md/`, served as `ts_md` and `md_events`). Windows where `sync_action` is not `idle` get an opaque strip across the top of the plot with the action (resync, recover, check, repair, reshape, frozen) and a light tint; every change between two windows is a vertical marker with its text (`sync_action idle -> check`, `degraded 0 -> 1`, `sdb: in_sync -> removed`), a state present at the start is marked `(at phase start)`, and an unreadable value is not a change. The hover adds `md: <action> <pct>% <speed> MiB/s, degraded <n>`; the card shows a badge (`resync 43% -> 61%`, `degraded`) and `array busy: results include administrative work`, since a sync during a phase (a scheduled check, a rebuild, a reshape; the initial resync is still waited for) adds its own I/O to the numbers. LVM arrays and the compare page do not have it.

**Flush latency** is not measured by fio. It carries a different tag (`ftrace`) and has its own clock from the start of the phase: the mean over each window, with no points during the ramp-up. A drive that never received a Flush shows why instead of an empty chart (see [flush-latency.md](flush-latency.md)).

## Comparing Disks on the Same Graph

The **Compare** view overlays multiple disks on a single chart, making it easy to spot performance differences.

Select two or more drives in the sidebar, choose a phase, then click **Compare**. Each drive is drawn in a distinct color with a legend. Metric, phase, and time axis are shared so curves are directly comparable. Flush latency is a metric there too; it has no read and write sides, so the direction checkboxes do not apply to it.

For a run group (`/compare?group=<group id>`, linked from the group page and the results list), and for a member run of a group by its own `/compare?id=`, the sidebar adds a **Setting value** list and the **Hide invalid results** toggle (on by default; invalid series are labelled `(invalid)` when shown). Each (drive, run) is its own series, named `disk · value`, or `disk · value · round N` when a value was measured in several runs (with the phase after the disk when several phases are selected). An array target's entry in the disk list names its members.
