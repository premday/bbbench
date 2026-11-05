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
- **Reading:** it reads `trace_pipe` while the phase runs. A start marker and a
  stop marker written to `trace_marker` bracket the phase (a live `trace_pipe`
  never reports end of file, so the stop marker ends the reader).
- **Pairing:** each `nvme_setup_cmd` is matched to its `nvme_complete_rq` by
  controller, queue and command id, so the pairing is exact even with many
  Flushes in flight. Latency = completion time - setup time, per Flush.
- **Storage:** every Flush is saved as a sample in `<fio log prefix>_flush.log`
  (completion time since tracing started, latency). The summary (mean, p50,
  p99, max) and the graph series are rebuilt from these samples when a run is
  loaded. The series is averaged over fio's polling interval
  (`--log-avg-msec`, stored in the log header) and lines up with the fio
  graphs: time 0 is when the phase starts, a point is stamped at the *start* of
  its window (fio stamps the end; the graphs move its points back one window so
  the first point sits where the ramp-up ends, not one polling interval later),
  and the **ramp-up** (fio's `ramp_time`, stored as `offset_ms`) is shaded
  on the graph. Flushes during the ramp are drawn but left out of the summary,
  as fio leaves the ramp out of its statistics.
- **Other devices:** non-NVMe devices use `block_rq_issue` / `block_rq_complete`
  for requests that are Flushes. Pairing is first-in first-out, so only the mean
  is exact and no percentiles are reported.

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

## bbbench does not configure the device

bbbench only **reads** the drive's state (`queue/write_cache`, tracefs) and never
changes it: it does not enable or disable the write cache, set NVMe features,
or touch queue settings or the I/O scheduler. The Flush numbers describe the
drive as you left it. The only kernel state it creates is its private ftrace
instance, removed when the phase ends.

So before a run, put the device in the state you want to measure, and note it
next to the results: write cache enabled or disabled, kernel (stock or patched),
firmware. To compare setups, change them yourself and run once per setup. The
warning for write-through devices lists the commands, but runs nothing for you.

## Limits

- **Resolution:** the trace text prints timestamps in seconds with six decimals,
  so the resolution is 1 µs. Sub-microsecond differences cannot be seen.
- **Overhead:** tracing adds a little CPU per Flush. The filter runs inside the
  kernel to keep it low.
- **Cause:** the number says how long the drive took, not why. Firmware, a nearly
  full DRAM cache or NAND state all look the same here.
- **Needs** root and a mounted tracefs. Without them the phase still runs and
  only fio's fsync latency is reported.
