# I/O latency tracing

Besides fio's own latency, every phase also times each read and write from
kernel trace events, so that the time spent in the drive can be told apart from
the time spent before it. It is **on by default** and needs root and a mounted
tracefs; if tracing is unavailable the phase still runs, a warning is printed
once and the phase carries a note saying why. Use `--no-io-trace` to turn it
off. The terminal prints a line such as
`ftrace device (nvme): read avg 85.0 us p99 120.0 us, write avg 120.0 us p99 210.0 us` for the target (device latency only, with its source, `nvme` or `block`; the rest is in the log and the web UI).

**Collectors (`--io-trace`).** The events can be read two ways, and the summary
line, the `_iolat.1.log` header (`mode=`, and `mode_note=` when a requested
mode was not used) and the run log say which one produced a result.
`ftrace` copies every trace event out of the kernel (per-CPU ring buffers,
read, merged and paired by bbbench): exact per-I/O pairing of NVMe commands and
percentiles, but one reader pipeline that handles roughly 0.3-0.85 M events per
second, so at about 1 M IOPS on several drives most events overrun the ring
buffers. `ebpf` keeps the work in the kernel: a small program stores each
request's (or bio's) start under its pointer, and its completion adds the
latency to a log-bucketed histogram per device, kind and direction;
bbbench reads the histograms once per `--log-avg-msec` window. Nothing is
copied per I/O, so nothing is lost to a ring buffer at any IOPS; the mean and
count are exact, the percentiles keep the same 1.6 % bound, and min and max
are the edges of the lowest and highest bucket used. The eBPF mode times NVMe
namespaces from `nvme_setup_cmd` to `nvme_complete_rq` like ftrace (the two
tracepoints are in the `nvme_core` module's BTF; without them, or with the
module not loaded, the device is timed from `block_rq_issue` to
`block_rq_complete` and the source line says so), needs root, a kernel with
BTF and the block tracepoints, and is built for amd64 and arm64; `auto` (the
default) falls back to ftrace otherwise. Start map overflow (a very deep queue)
is counted and shown as unpaired. Only reads and writes that carry data are
timed (Flushes and discards are not: the Flush phases have their own
trace), a request that completes in several parts is timed once, at its last
part, and the disk is taken from the request queue, so it works with
`queue/iostats` at 0. Each session holds about 18 KB x 2 x 2 histograms per
traced device and CPU (a device timed through requests has a device and a
block-layer series) and a 262144-entry start map, and detaches when its phase
ends. **The eBPF mode has not been run on a real
kernel yet:** if the program is rejected, the run continues on ftrace and the
reason is in the run log.

Every run also writes everything it prints to `<output>/run_<id>.log`. For an md or dm target, which has no request events of its own, it gives the node's bio-level latency and then the members' device mean over all their requests, e.g. `ftrace bio (md0, block_bio_queue -> block_bio_complete): read avg 310.2 us p99 920.0 us, write avg 640.8 us p99 1800.0 us; 4 members traced (block), members' device read avg 95.2 us, write avg 130.4 us`; without the bio tracepoints it says `target bio-based` and gives the members' mean only.

**What is measured** (reads and writes only; Flush, discard and admin commands
are left out, Flush has its own tracing above):
- *Device latency*: for an NVMe namespace `nvme_setup_cmd` to
  `nvme_complete_rq` (paired exactly by controller, queue and command id); for
  every other device (HDDs on SATA or SAS, SSDs, virtio) and NVMe on kernels
  without those tracepoints, `block_rq_issue` to `block_rq_complete`.
- *Block layer latency*: `block_rq_insert` (or the issue, for a request issued
  directly) to `block_rq_complete`. It adds the time in the I/O scheduler and the
  software queues to the device latency.
- *Bio latency* of a bio-based node (an md or dm array target, no request
  queue: `queue/` without `mq/` in sysfs): `block_bio_queue` to
  `block_bio_complete` of the bios submitted to the node itself, so the whole
  time the array takes for an I/O, its members included. Only such a node gets
  it (a request-based device never has the bio events enabled, so nothing is
  counted twice), and it has no block layer latency.

Block events carry no request id, so they are paired by device and start sector,
first in first out; `block_rq_merge` drops the absorbed request. HDDs use this
block path for both metrics: the device latency is issue to complete, the block
layer latency insert to complete. For an array target (md or LVM layout) the
members are traced too, in the same session: they appear as their own device
latency series. md and dm nodes themselves are bio-based and issue no requests:
the target gets its bio latency instead (kind `bio`), paired by device, sector
and length first in first out, through the end sector, so the remainder of a bio
the node splits pairs with the bio as queued and the parts split off are not
counted; bios the node sends to its members carry the members' device numbers
and are filtered out. Without the bio tracepoints the target shows
`no request events seen` while its members are traced. A native NVMe multipath drive may show no block layer series: its block
requests are issued on the hidden path disks (`nvme0c1n1`), not on the head
bbbench traces; its Block-layer card then says `no block-layer events seen`
with that hint, although the Latency card's source line lists the
`block_rq_insert` tracepoint.

**How it is stored.** The latencies go into histograms inside bbbench (relative
error under 2 %), nothing is kept per I/O. Like Flush tracing, they are saved
next to the fio logs as `<output>/<device>_phase<N>_<ts>_log_iolat.1.log`: one
line per `--log-avg-msec` window (end time, kind `device`, `blocklayer` or
`bio`, direction, count, mean, min, max, p50, p99, device), stamped at the window end, plus header lines with the
metadata (window, ramp offset, ring buffer overruns, per-device tracepoints and
pairing; `bio_based=true` for a node without a request queue) and the phase summary (count, mean, min, max, p50, p99 per kind and
direction, computed after the ramp-up). It has up to four lines per window
(device and block layer, reads and writes) for every traced device, so an
8-member array traced for an hour with 1 s windows gives about 130,000 lines
(3600 windows x 4 x 9 devices). The summary and the series (`io_trace`,
`io_trace_series`, `io_trace_notes` in the run summary; `ts_iolat`,
`ts_iolat_members`, `io_trace` in the graph data) are read back from this file
when a run is loaded; the run summary JSON (`<id>_summary.json`) stores
`io_trace_series` again, so it grows with the log.

**How the events are read.** Each session has a private tracefs instance and
reads its binary per-CPU pipes (`per_cpu/cpuN/trace_pipe_raw`): one reader
per CPU decodes the kernel's ring-buffer pages, with every field at the offset
the kernel's own `events/<system>/<event>/format` file gives (no text is
formatted by the kernel or parsed by bbbench), and a merger puts the CPUs back
into time order before the events are paired. The instance uses the `mono`
trace clock (`global` when `mono` is not offered), so that an I/O issued on one
CPU and completed on another compares. When the raw pipes are missing, or a
format file is missing or lacks a field bbbench needs, the session reads the
text `trace_pipe` instead, as before, and says why: the log header carries
`reader=raw` or `reader=text` with `reader_note=`, and the summary JSON
`reader`, `reader_note` and `clock`. An event the merger receives after a newer
one was already paired (a CPU's reader more than 100 ms behind, or more than a
million events queued) is fed anyway and counted as `late` (log header and
summary); a pair it broke shows up as unpaired.

**Lost events.** Every figure carries a `lost events` count: ring buffer
overruns (the reader fell behind) plus events that found no partner (requests
still pending when the phase ended, a command id reused, completions with no
request). It is shown on the cards and the Details page and is never hidden.
On the graphs page a card with losses says which of its ftrace series are
incomplete and why: the ring buffer part makes every ftrace series of the phase
incomplete (their statistics are a sample of the events that arrived), the
unpaired part names each device with a series on that card and those series,
with the share of the events lost when the counts allow it. Large ring buffer
counts are expected on fast drives: an SSD phase runs 3600 s by default, and at
100,000 IOPS with two to four events per request that is about a billion trace
events. When the ring buffer drops events, the terminal and the graphs page
say `raise --trace-buffer-mb or lower the IOPS`; `--no-io-trace` turns the
tracing off.

**Cost and opt-out.** Tracing every read and write costs CPU, and at very high
IOPS it can disturb the numbers it measures; this is why `--no-io-trace` exists.
The kernel allocates the trace ring buffer per CPU. Concurrent sessions share a
budget, `--trace-buffer-mb` (default 1024 MiB): every drive running a phase at
the same time has one I/O latency session and, in a phase that fsyncs, one
Flush session, each counting as one session. Each CPU of an I/O session gets
budget / (sessions x CPUs), at least 1 MiB and at most 64 MiB; of a Flush
session the same, at most 8 MiB. The kernel memory taken is sessions x CPUs x
the per-CPU size: at most the budget, except at the 1 MiB floor, where it is
exceeded rather than buffers shrunk further (one drive on 16 CPUs: 16 MiB per
CPU, 256 MiB; one drive on 64 CPUs: 16 MiB per CPU, 1 GiB; one drive on 2 CPUs: 64 MiB per CPU, 128 MiB; 12 drives on 64
CPUs: the floor, 768 MiB in total, twice that in an fsync phase;
`--trace-buffer-mb 4096` with one drive on 64 CPUs: 64 MiB per CPU, 4 GiB).
The run prints the size it uses, e.g. `ftrace buffers: 256 MiB budget
(--trace-buffer-mb), 16 MiB per CPU for up to 2 concurrent sessions on 8
CPUs: 256 MiB of kernel memory`.
Like Flush tracing it uses a private tracefs instance
(`instances/bbbench-iolat-*`), removed when the phase ends. Instances left by a
killed or crashed bbbench (`bbbench-iolat-*` and `bbbench-flush-*`) keep their
events enabled and cost CPU on every I/O until they are removed: every start of
`orchestrator` removes them, whatever its flags (also with `--no-io-trace` and
when no phase fsyncs), and so does every trace session that opens; `doctor`
lists those it finds. An instance is stale when its pid is not running, read
from `/proc` of bbbench's own PID namespace: a bbbench running in another
container that shares the tracefs looks dead, and its instance is switched off
and removed.

**Not verified on hardware.** The feature is tested against hand-written trace
lines, hand-built ring-buffer pages and a fake tracefs tree and has not been
run end to end on a real kernel. Nothing of the raw reader has run on a real
kernel: it is verified only against pages and format files the tests build
from the kernel sources (`internal/tracefake`). Unverified there: the page and
record encoding (page header, commit word and its missed-events flags, the
record types, absolute time stamps, whether a discarded event's time delta
counts, as in libtraceevent), the format files of a real kernel (the NVMe
layout in the fixtures is remembered, not copied), that the Go netpoller can
park on `trace_pipe_raw` (its poll with `buffer_percent` 0) and that a read
deadline wakes it, that a pipe reads empty once tracing is off and drained,
the `mono` clock in the instance, and the 100 ms hold of the merge against a
reader's real wake-up delay. The fixture layouts cannot make the decoder right
by accident: it reads every offset from the format files, and refuses a format
it cannot use (the text pipe is then read). With the raw reader the payload
grammar of the text lines is not involved; it still matters on the text
fallback. Unverified on the text path: the payload formats of the tracepoints
beyond the Flush lines already tested (`block_rq_insert`, `block_rq_merge`, the
read opcode names, a `block_rq_complete` line with the I/O priority field,
`block_bio_queue` and `block_bio_complete`, taken from the kernel's `TP_printk`
as `maj,min rwbs sector + nr [comm|error]`). Unverified on both: whether `block_bio_complete` fires
for the node's own bios on md, dm and LVM nodes (`BIO_TRACE_COMPLETION`) and
with which sector after a split or a dm remap, and the queue and complete event
counts on real arrays; the `ctrl_id` filter field
of `nvme_setup_cmd` and `nvme_complete_rq` (if the kernel rejects it the filter
is lost and bbbench filters in user space: correct but a higher event rate); the
NVMe native multipath sysfs layout (`<head>/multipath/nvme<s>c<c>n<n>`: without
it the controller filter is left out); how requeues, front merges that move the
start sector and partial completions are paired (they are counted as unpaired or
understate the block layer latency, see the package comment in
`internal/iotrace/session.go`); the cost of the per-CPU ring buffers and the
1024 MiB default budget with its 1-64 MiB per-CPU range; and the effect of
tracing on the measured I/O. Run it first on a test machine, and use `--no-io-trace` if it
disturbs a measurement.

**In the web UI.** The Latency chart draws the traced mean latency as dashed
lines next to fio's (device and block layer, reads and writes), a card says
which tracepoints each series comes from, and two graph types of their own
show the mean and p99 per direction and each member's mean: **Device latency
(ftrace)** the device latency, **Block-layer latency (ftrace)** the block layer
latency. The members' ftrace series appear only on those graphs, not on the
Latency chart. A bio-based array target adds its own bio series: `Array read bio
mean (ftrace)` and `p99` (and write) in the array's colours on the Device
latency card next to its members' lines, the bio mean as a dashed line on the
Latency chart; its Block-layer card draws the members only, with a note that
the node has no block layer. A drive without block layer series (kernel without
`block_rq_insert`, a native NVMe multipath head, a bio-based target whose
members have none) gets a Block-layer card that says why; a run with no block
layer data at all has no Block-layer card. Every series has a checkbox to show or hide it; the y scale follows the
shown series, and the choice is kept in the page only until it is reloaded. The
compare page does not draw this data. See
[documentation/web-ui.md](documentation/web-ui.md).

**Tracepoints behind each ftrace metric.**

| Metric (graph type) | Kernel tracepoints (trace events) and the pair the latency spans |
|---|---|
| Flush latency | Device Flush commands. NVMe: `nvme_setup_cmd` -> `nvme_complete_rq` (flush opcode, I/O queues, paired exactly by controller, queue, command id). Any other device, and NVMe without those tracepoints: `block_rq_issue` -> `block_rq_complete` (rwbs `F`, 0 sectors, first-in first-out). A bio-based md/dm node: `block_bio_queue` -> `block_bio_complete` (its flush bios: rwbs `F`, 0 sectors, first-in first-out) |
| Device latency (ftrace) | Each read and write. NVMe: `nvme_setup_cmd` -> `nvme_complete_rq` (read and write commands, exact). Any other device (SATA, SAS, HDDs, array members), and NVMe without those tracepoints: `block_rq_issue` -> `block_rq_complete` (first-in first-out per sector). A bio-based md/dm node (array target): `block_bio_queue` -> `block_bio_complete` (its read and write bios, first-in first-out by device, sector and length) |
| Block-layer latency (ftrace) | Every request-based device, NVMe included: `block_rq_insert` -> `block_rq_complete` (a request issued without an insert counts from `block_rq_issue`; merges via `block_rq_merge`), so it adds the I/O scheduler and software queues. None on a bio-based md/dm node |
| Latency chart, dashed ftrace lines | Both of the previous: the Device latency and the Block-layer latency means, and a bio-based node's bio mean |

The same text is the tooltip of each metric's source badge (the `ftrace` badge in the graph-type list, the section headers and the card titles; the Latency badge says `fio` and its tooltip adds the dashed lines). The tooltip and the section header then also list the tracepoints actually monitored in the shown runs, from the data (`io_trace.devices[].events`, `flush.events` in the run summary JSON), so they match reality when a kernel lacks one.
