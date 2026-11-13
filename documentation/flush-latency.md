# What the Flush latency measures

bbbench reports two latencies for the flush workloads (`ssd_fsync_*`):

- **fsync latency** (source: fio): how long the whole `fsync()` call took.
- **Flush latency** (source: ftrace): how long the drive took to complete the
  Flush command the kernel sent it, timed from kernel trace events.

This page explains what a Flush is, where it sits in the Linux stack, what
exactly is timed, and what is and is not included.

## What a Flush is

An NVMe drive acknowledges a write once the data is in its volatile DRAM cache,
not on NAND. A **Flush** command makes the drive write that cache to NAND
before it answers. `fsync()` is how an application asks for that guarantee, so
the Flush latency is the cost of the guarantee. It is the number that matters
for databases, journals and write-ahead logs.

## Where it sits in the stack

bbbench runs fio directly on the raw device (`filename=/dev/nvme0n1`, no
filesystem). `fsync()` is then "write back dirty pages, then flush the device".

```
 fio job: write 4k; fsync(); write 4k; fsync(); ...      (fsync=1)
 ──────────────────────────────────────────────────────────────────  USER SPACE
   fio  ──fsync(fd)──┐                    ▲
                     │                    │  fio "sync latency" = the WHOLE call
 ──────────────────────────────────────────────────────────────────  KERNEL
   syscall / VFS                          │
        │                                 │
   blkdev_fsync()                         │
        ├─ write back dirty page-cache pages   (buffered psync job only;
        │                                       nothing to do for direct=1)
        └─ blkdev_issue_flush()  ──► bio with REQ_PREFLUSH
                  │
   block layer (blk-flush)
        │   queue/write_cache = "write through" ?  ─► flush is DROPPED here,
        │                                              the driver never sees it
        │   queue/write_cache = "write back"    ─► request sent to the driver
        ▼
   NVMe driver
        │  nvme_setup_cmd   ◄══ START  (tracepoint: opcode 0 = Flush, qid > 0)
        │  write command to submission queue, ring doorbell
 ───────┼──────────────────────────────────────────────────────────  PCIe
        ▼                                          ▲
   NVMe controller (drive firmware)                │ completion entry
        queue wait → destage DRAM cache to NAND ───┘ + interrupt
 ───────┼──────────────────────────────────────────────────────────
        ▲
        │  nvme_complete_rq ◄══ STOP   (tracepoint, same ctrl/qid/cmdid)
   NVMe driver completes request, wakes the waiting fsync()
```

## What is measured, and how

- **Source:** the kernel's ftrace tracepoints `nvme:nvme_setup_cmd` and
  `nvme:nvme_complete_rq`, not fio.
- **Setup:** bbbench creates a private ftrace instance
  (`tracefs/instances/bbbench-flush-*`), so global tracing is untouched and
  drives can be traced in parallel. It enables those two events with the
  kernel-side filter `opcode == 0 && qid > 0` (Flush commands only, no admin
  queue).
- **Reading:** it reads the instance's binary per-CPU pipes
  (`per_cpu/cpuN/trace_pipe_raw`, one reader per CPU, fields at the offsets of
  the kernel's format files, merged back into time order) while the phase runs,
  on the instance's `mono` trace clock (`global` when `mono` is missing). A
  start marker written to `trace_marker` sets time zero. At the end tracing is
  switched off and each CPU's pipe is read until it is empty, which proves
  every recorded Flush was delivered. Without the raw pipes, or when a format
  file cannot be used, it reads the text `trace_pipe` as before, where a stop
  marker ends the reader (a live `trace_pipe` never reports end of file). The
  raw reader is verified only against pages built by the tests, not on a real
  kernel (see the README, "I/O latency tracing").
- **Pairing:** each `nvme_setup_cmd` is matched to its `nvme_complete_rq` by
  controller, queue and command id, so the pairing is exact even with many
  Flushes in flight. Latency = completion time - setup time, per Flush.
- **Storage:** the Flush latency is saved like fio's own logs, in
  `<fio log prefix>_flush.1.log`: one line per polling window
  (`--log-avg-msec`), stamped at the end of the window, in fio's columns
  (`time_ms, mean_latency_ns, dir, bs, offset`) followed by `count, min_ns,
  max_ns`. A 5-minute phase is a few hundred lines (about 10 KB), however many
  Flushes it had. Header comments hold the metadata and the whole-phase summary
  (mean, p50, p99, max over every Flush after the ramp-up), computed from the raw
  samples when the phase ends; the individual Flushes are not kept, since exact
  percentiles cannot be rebuilt from windows. The series uses fio's clock
  conventions and lines up with the fio graphs: time 0 is when the phase starts,
  a point is plotted at the *start* of its window (fio stamps the end; the graphs
  move its points back one window so the first point sits where the ramp-up
  ends, not one polling interval later), and the **ramp-up** (fio's
  `ramp_time`, stored as `offset_ms`) is shaded on the graph. Flushes during the
  ramp are left out of the summary and have no points on the graph, as fio
  leaves the ramp out of its statistics and its logs (the log file keeps those
  windows).
- **Other devices:** non-NVMe devices use `block_rq_issue` / `block_rq_complete`
  for requests that are Flushes. Pairing is first-in first-out, so only the mean
  is exact and no percentiles are reported.
- **Reads and writes:** the same private tracefs instance mechanism (`flushtrace.Instance`) carries a sibling session that times every read and write, at the device and in the block layer, in every phase (`<fio log prefix>_iolat.1.log`, `bbbench-iolat-*` instances, `--no-io-trace` to turn it off). It is documented in the README, "I/O latency tracing"; this page covers only the Flush.

## What is included in the number

| Part | In the Flush latency? |
|---|---|
| Driver builds the command, writes it to the submission queue, rings the doorbell | yes |
| Waiting in the drive's queue behind other commands | yes (one reason qd32 differs from qd1) |
| The drive destaging its DRAM cache to NAND | yes, usually the main cost |
| Completion entry, interrupt, driver completion | yes |
| Syscall, writing back the dirty page (psync job), block-layer queuing before the driver | no |

fio's fsync latency is the whole call, so `fsync latency - Flush latency` is
roughly the kernel overhead plus the write-back of dirty pages. That split is
why the ftrace measurement exists: fio cannot see inside a single `fsync()`.

For the two `direct=1` io_uring jobs there is nothing dirty to write back, so
the two numbers should be close. For the buffered `psync` job they differ by the
data write.

## Why the write cache mode matters

If `/sys/block/<dev>/queue/write_cache` is `write through`, the block layer
discards the Flush before it reaches the driver. `fsync()` then returns almost
at once; fio still reports a latency, but there is no Flush to trace, and the
result shows "ftrace saw 0 Flush commands". bbbench reads the mode before such a
phase and prints a warning with the commands to check and enable the cache
(`--skip-write-through` skips the phase instead). On a kernel that honors the
cache state, a drive whose volatile write cache is disabled is `write through`.
`nvme id-ctrl /dev/nvme0 | grep -i vwc` shows whether the drive has a volatile
cache at all, and `nvme set-feature /dev/nvme0 -f 6 -V 1` enables it (capital
`-V`: `-v` is verbose).

## Only a setting test configures the device

Flush tracing and a plain run only **read** the drive's state (`queue/write_cache`,
tracefs): they do not enable or disable the write cache, set NVMe features,
or touch queue settings or the I/O scheduler. The Flush numbers describe the
drive as you left it. The one exception is a *setting test* (see the README):
of the write cache policy, after an explicit confirmation it applies each value,
verifies it before and after each run and restores the original; a *layout*
setting test builds md arrays or LVM volumes on the chosen disks and erases them
(also after one confirmation, with the arrays torn down on every exit). The only kernel state Flush tracing creates is its
private ftrace instance, removed when the phase ends.

So before a run, put the device in the state you want to measure, and note it
next to the results: write cache enabled or disabled, kernel (stock or patched),
firmware. To compare setups, change them yourself and run once per setup, or let a
setting test do it for the write cache. The warning for write-through devices
lists the commands, but runs nothing for you.

## Arrays and logical volumes

In a layout setting test (md array or LVM logical volume) the generic block
collector runs on the md or dm device: the mean is exact and no percentiles are
reported, as for any non-NVMe device. This is not verified on hardware yet: md
and dm devices are bio-based, so the block request events may never fire for
them, and a phase can show 0 Flushes ("ftrace saw 0 Flush commands") even
though the array's member disks received Flushes.

## Limits

- **Resolution:** the trace text prints timestamps in seconds with six decimals,
  so the resolution is 1 µs. Sub-microsecond differences cannot be seen.
- **Overhead:** tracing adds a little CPU per Flush. The filter runs inside the
  kernel to keep it low.
- **Cause:** the number says how long the drive took, not why. Firmware, a nearly
  full DRAM cache or NAND state all look the same here.
- **Needs** root and a mounted tracefs. Without them the phase still runs and
  only fio's fsync latency is reported.
