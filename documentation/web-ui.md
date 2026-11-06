# Web UI

The `bbbench serve` command (also started automatically by `orchestrator` during a run) exposes a web interface on `http://0.0.0.0:12345` by default.

## Benchmark Status

The **Status** page shows live progress while `orchestrator` is running.

![Benchmark Status](img/overview_benchmark_status.jpg)

- **Current Status** — running/idle indicator, overall progress bar, current phase number, and completed phase count.
- **Drives** — one card per benchmarked drive showing model, serial, capacity, current status, and active phase name.

Each drive card also shows how the disk is configured, read once when the run starts (read-only: bbbench never changes it):

| Line | Meaning | Source |
|---|---|---|
| Interface | `NVMe (PCIe)`, `NVMe (tcp)`, `SATA`, `SAS`, `USB`, `virtio` | sysfs |
| Link | negotiated link, e.g. `8.0 GT/s PCIe x4`, `6.0 Gbps`; the maximum is shown when known, with **below max** when the link runs slower (bad slot or cable, or the link saving power) | sysfs |
| Power | NVMe power state and APST (`nvme get-feature`), runtime PM, PCIe ASPM policy and link state, SATA link power policy | sysfs, `nvme` |
| Write cache | the kernel's policy (`write back` or `write through`: only write back drives get Flush commands) and the drive's own cache (enabled or disabled) | sysfs, `nvme`, SG_IO |
| SMART | health (`PASSED` / `FAILED`) with temperature, wear, power-on hours and error counts | `smartctl`, else `nvme smart-log` |

Some of these need root, and SMART needs `smartctl` (smartmontools) or, for NVMe, `nvme-cli`. A line that cannot be read is left out, and SMART says why it is unknown.

The same information, in full, is written to a text file per disk in the run's output directory, `<device>_diskinfo_<start time>.txt` (one per run, so a resumed run keeps what the first one saw). It holds the summary, every sysfs value read, and the complete output of each command, including the reports the summary is drawn from: `smartctl -x`, and `nvme id-ctrl`, `nvme id-ns` and `nvme smart-log` for NVMe or `hdparm -I` for SATA. A command that failed (no root, tool not installed) is listed with its error, so the file shows what could not be read.

Each drive's **Phases** list shows its time as it goes: before a phase runs, its expected length (`~5m`, or `length unknown` for a full-disk write); while it runs, the time elapsed against the expected one; once it has finished on that drive, the real time (`took 5m 2s`, with the expected one beside it, finished phases struck through). The header sums the real time spent so far and gives an ETA for what remains. The ETA adjusts as phases finish: the expected length of the remaining phases is scaled by how the finished ones really compared with theirs (fio's setup and ramp-up, a slower drive, ...), and the running phase counts down from that. Phases of unknown length are counted apart, not guessed.

The page auto-refreshes so you can monitor a long run without manually reloading.

## Graphs and Navigation

The **Results** page lets you explore time-series charts from a completed (or in-progress) run.

![Graphs Navigation](img/overview_graphs_navigation.jpg)

The left sidebar provides filters that update the displayed graphs in real time:

| Filter | Description |
|--------|-------------|
| **Graph Type** | Toggle IOPS, Bandwidth (MB/s), and Latency (µs) charts independently. |
| **Drive Type** | Narrow down to HDD or SSD results. |
| **Vendor / Model / Serial** | Select specific hardware identities. |
| **Disk** | Choose which drives to include. |
| **Phase** | Select one or more benchmark phases (e.g. `seqread`, `randread-randread`). |

Each matching combination of disk × phase is rendered as its own graph card. Click a card to maximize it; press **Esc** to return to the grid.

## Comparing Disks on the Same Graph

The **Compare** view overlays multiple disks on a single chart, making it easy to spot performance differences.

![Disk Compare](img/overview_same_graph_disk_compare.jpg)

Select two or more drives in the sidebar, choose a phase, then click **Compare**. Each drive is drawn in a distinct color with a legend. Metric, phase, and time axis are shared so curves are directly comparable.
