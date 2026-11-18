# Flush-latency workloads

The default `ssd` workload list ends with three 5-minute jobs that measure
what a Flush costs: 4k random writes with an `fsync` after every write, so each
write is followed by one device Flush.

| Workload | Engine | I/O | Queue depth |
|---|---|---|---|
| `ssd_fsync_psync_qd1` | `psync` | buffered | 1 |
| `ssd_fsync_uring_qd1` | `io_uring` | direct | 1 |
| `ssd_fsync_uring_qd32` | `io_uring` | direct | 32 |

The orchestrator summary prints `fsync avg: … us` next to the IOPS of these
phases; the full fio JSON in the output directory has the `sync` latency
statistics. That is the latency of the whole `fsync()` call; the part spent in
the drive is measured separately, see [Flush tracing](flush-tracing.md). To compare setups (write cache enabled vs cleared, stock vs patched
kernel), run the workloads once per setup and compare the results.
**A plain run does not configure the device**: it only reads the write cache
mode, so set the drive up yourself before the run and note the setup with the
results. A *setting test* (see [`bbbench orchestrator`](commands.md#bbbench-orchestrator))
can do the comparison for you: it applies each write cache value, verifies it
and restores the original; that is the only mode in which bbbench changes a
drive's setting.

Like the other write workloads they overwrite the device. The `io_uring` jobs
need an fio built with io_uring support and a kernel that has it. To run only
these workloads, generate the job files from a config that lists just them under
`bbbench.workloads.ssd` (`sudo ./bbbench generate --config my.yml`); the
orchestrator has no workload-selection flag and runs whatever was generated.
