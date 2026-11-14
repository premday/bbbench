# ftrace latency of an array's own node (bio level) — design addendum

Date: 2026-11-12. Branch `run_groups_mdadm`. Addendum to
`2026-11-12-ftrace-io-latency-design.md`. Requested by the user: "the
ftraces metrics are only for drives. do it to when it makes sense on the raid
device".

## Why

md and most dm nodes (LVM included) are bio-based: they have no blk-mq
request queue, so `block_rq_insert/issue/complete` never fire for them. Today
the array node of a run gets no ftrace latency (its card says "no request
events seen") and its Flush session sees 0 Flushes; only the members, which
are request-based, are traced. The latency of the node exists at the bio
level: from the bio submitted to the node to its completion.

## What

- **Kind `bio`**: latency from `block_bio_queue` to `block_bio_complete` of
  the bios submitted to the node, per direction (read, write; discards,
  secure erases, write zeroes and pure Flushes excluded, as for requests).
  Payloads (kernel `TP_printk`): `block_bio_queue` `"%d,%d %s %llu + %u [%s]"`
  (dev, rwbs, sector, nr_sector, comm), `block_bio_complete`
  `"%d,%d %s %llu + %u [%d]"` (…, error).
- **Pairing**: by (dev, sector, nr_sectors), first-in first-out, not exact.
  A bio the node splits (`bio_split` + `bio_chain`) completes in several
  events: its remainder completes last with an advanced sector and the same
  end sector, so a completion pairs by (dev, end sector) with the oldest
  pending bio of that end that starts at or before it; a completion that lies
  inside a pending bio (a split part) is not counted, neither as a latency
  nor as unpaired. Bios remapped or cloned to lower devices produce their own
  events on the lower dev, which the dev filter drops.
- **When**: only for a traced device that has no request events by
  construction: bio-based = `<sys>/block/<dev>/queue/` exists and
  `<sys>/block/<dev>/mq/` does not (the queue dump's notion), and not an NVMe
  namespace with the NVMe tracepoints (a native multipath head is bio-based
  but its commands are traced on the controller). Request-based devices keep
  their device and block-layer series and get no bio series, so nothing is
  counted twice. A bio-based device on a kernel without the bio tracepoints
  is traced as today (no events, the card says so).
- **Flush on a bio-based node**: the Flush session uses `block_bio_queue` ->
  `block_bio_complete` with rwbs containing F and 0 sectors (source `bio`),
  first-in first-out per device; the node's own Flush latency, its members'
  flushes included. `Summary.Source` = `bio`, `Events`, `SourceLine` and the
  console summary say so from the data.
- **Cost and safety**: the same private-instance session (one per target,
  node + members); the bio events are enabled only when a traced device is
  bio-based, with a dev filter on exactly those devices; unpaired bios count
  in the loss totals; tracing never fails the phase.

## Stored data

- `_iolat.1.log`: the device header gets `source=bio`, `bio_based=true`,
  `pair=dev,sector,nr_sectors`, `events=block_bio_queue->block_bio_complete`;
  window lines with kind `bio` (the kind column exists). Old logs read as
  before; old readers ignore the new header field.
- `io_trace`: `devices[].bio_based`, stats per kind (`bio`), `text_bio`
  (SourceLineOf the bio kind); `block_layer_note` of a bio-based node points at
  the bio series. `ts_iolat` of the target carries the kind `bio` points.
- Flush: `flush.source` = `bio`, `flush.events` =
  `block_bio_queue -> block_bio_complete`.

## Display

- Device latency (ftrace) card of an array/dm target: `Array read bio mean
  (ftrace)` / `Array read bio p99 (ftrace)` (and write) in the array's fio
  colours, dashed like the device series, next to the members' device lines;
  series keys `ftrace.bio.<stat>.<dir>`. The source line names
  `block_bio_queue -> block_bio_complete` for the node.
- Block-layer latency card: members only, plus a note from the node's
  `block_layer_note` ("bio-based … bio-level latency on the Device latency
  card").
- Latency card: the node's bio mean as a dashed line `Array read bio mean
  (ftrace)`.
- Flush card: the node's Flush series with its source line.
- Badge tooltips (`ftraceRef`) and the README table list the bio pair for
  bio-based nodes. Toggles, y extent, SI units, HDD seconds and `HIDDEN` work
  for the new series.

## Not verified on hardware

The bio payload formats on the user's kernel, whether `BIO_TRACE_COMPLETION`
makes `block_bio_complete` fire for the node's own bio on md, dm and LVM
nodes (and with which sector after a split or a dm remap), and the
queue/complete event counts on real arrays. Tests use hand-written lines and
fake tracefs trees.
