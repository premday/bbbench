# Queue, scheduling and multiqueue dump

## Intent

The user, verbatim: "we want to dump all queue and scheduling info from sysfs.
for each block device. and for software raid too. multiqueue mode/status too".
Later additions: "dump all dm_mod parameters from sysfs", and for
`dm_mod`'s `use_blk_mq`, `dm_mq_nr_hw_queues` and `dm_mq_queue_depth`: "these
3 should be visible in the status window and in the dumps".

## Rulings (decided by the controller)

- This extends the per-disk text dump that `diskinfo.CollectDump` already
  writes (`<output>/<device>_diskinfo_<start>.txt`), and adds a compact
  structured summary `Info.Queue` (JSON `queue`, omitempty) for the status
  page and the run data. It reads only: nothing is written to sysfs or
  debugfs, nothing is tuned, the benchmark is unchanged.
- Every block device a run touches gets it: a plain drive, the array node of a
  layout run (md node, dm node) and each member of an array. The array node
  itself is now collected too (until now only its members were), with the
  drive-only reads (SMART, nvme-cli, the drive write cache, topology) skipped:
  an md or dm node has none of them.
- The dump never fabricates: a file of a fixed list that does not exist is
  written as `not present (not on this kernel)`, a file that cannot be read as
  `unreadable: <error>`, binary files are skipped with their size, and
  directories are listed in name order so two dumps compare line by line.
- Bounded: each file is read up to 64 KiB, each device's queue section up to
  4 MiB; what is cut says `truncated`.
- Module parameters are dumped in full (every readable file under
  `/sys/module/<m>/parameters/`, `name = value`, not a curated list), plus
  `version`, `srcversion`, `refcnt`, `initstate` and the `holders/` entries
  when present, for `dm_mod`, `md_mod`, `nvme_core`, `nvme`, `scsi_mod`,
  `sd_mod`, `libata`, the md personalities `raid0`, `raid1`, `raid10`,
  `raid456`, and the dm targets `dm_raid`, `dm_thin_pool`, `dm_mirror`,
  `dm_crypt`, `dm_multipath`. A missing module directory is `not loaded (or
  built in without parameters)`. The module section appears once per dump
  file, even when the file covers several devices (an array node's dump also
  holds its members' queue sections).
- `dm_mod`'s `use_blk_mq`, `dm_mq_nr_hw_queues` and `dm_mq_queue_depth` are
  also in `Info.Queue.DMMod` (JSON `dm_mod`) for every device whenever
  `dm_mod` is loaded, and for a dm node even when it is not (then `loaded:
  false`). `use_blk_mq` exists only on older kernels: a missing file is empty
  in JSON and shown as `n/a (not on this kernel)` on the status page, never as
  a value. The status page shows them as a `dm_mod` row on every card that has
  them (drives, array node, members do not repeat it).

## Contents of the dump, per device

`== Queue and scheduling: <dev> ==`:

- `block/<dev>/{dev,size,ro,removable,hidden,inflight,stat,capability,
  ext_range,range,alignment_offset,discard_alignment,events,events_poll_msecs}`
  (fixed list: missing ones said so).
- every readable file under `block/<dev>/queue/`, recursively (scheduler with
  the active one in brackets, `nr_requests`, `rotational`, `read_ahead_kb`,
  `max_sectors_kb`, `wbt_lat_usec`, `io_poll*`, `iosched/*`, ...). Symlinks are
  shown as `-> target`, never followed.
- blk-mq: `block/<dev>/mq/<n>/` recursively for every hardware queue, the
  number of hardware queues and the CPU map (`hctx0: cpus 0-3`); without
  `mq/` the device is bio-based (md, bio-based dm) and the dump says so.
- device side: SCSI `block/<dev>/device/{queue_depth,queue_type,timeout,
  eh_timeout,state,queue_ramp_up_period,device_blocked,device_busy,
  iorequest_cnt,iodone_cnt,ioerr_cnt}`, the SCSI host
  `class/scsi_host/hostN/{can_queue,cmd_per_lun,sg_tablesize,
  sg_prot_tablesize,nr_hw_queues,host_busy,use_blk_mq,proc_name,state,
  eh_deadline}`, NVMe `class/nvme/nvmeN/{queue_count,sqsize,transport,
  numa_node,kato,cntlid,state,ctrl_loss_tmo,reconnect_delay,
  fast_io_fail_tmo}`.
- md node: every readable file under `block/mdX/md/` recursively (level,
  raid_disks, chunk_size, array_state, sync_*, stripe_cache_*, bitmap/*,
  consistency_policy, dev-*/{state,errors,slot,offset,size,recovery_start}...).
- dm node: every readable file under `block/dm-N/dm/` (name, uuid, suspended,
  use_blk_mq when present).
- md and dm: the `slaves/` and `holders/` entries, then the queue section of
  each slave (a partition's queue is its disk's).
- debugfs: when `/sys/kernel/debug/block/<dev>/` is readable, every file under
  it (hctx*/ state, busy, dispatch, queued, run, sched/, ...), within the same
  bounds; otherwise the dump says debugfs is not mounted or not readable
  (root) and that the values above are the sysfs ones only.

Then once per file `== Kernel module parameters ==`.

## Structured summary `Info.Queue`

`scheduler` (active), `schedulers`, `nr_requests`, `mq` (blk-mq), 
`nr_hw_queues`, `rotational`, `read_ahead_kb`, `max_sectors_kb`,
`wbt_lat_usec`, `rq_affinity`, `nomerges`, `io_poll`, `device_queue_depth`,
for md `md` {`level`, `raid_disks`, `chunk_size`, `stripe_cache_size`,
`sync_action`, `bitmap`}, and `dm_mod` as above. Values are the sysfs strings
("" = not read). The status page shows a `Queue` row (`blk-mq: yes, 4
hardware queues, scheduler none` or `bio-based (no blk-mq), scheduler none`,
then the tunables), an `md` row on the array node and the `dm_mod` row; the
array card shows the node's own configuration above its members, and each
member's row gains a short queue summary.

## Non-goals

No writes, no tuning advice, no benchmark change, no new external program, no
history of queue values over a run.

## Not verifiable without hardware

The exact set of files on a given kernel (the dump lists what is there), the
debugfs blk-mq layout, dm request-based queues and md bio-based queues on a
real array: the tests use fake sysfs and debugfs trees.
