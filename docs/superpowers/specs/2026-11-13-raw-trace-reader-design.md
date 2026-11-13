# Raw per-CPU trace reader and configurable trace buffers — design

Date: 2026-11-13. Branch `run_groups_mdadm`. Addendum to
`2026-11-12-ftrace-io-latency-design.md`. Requested by the user: items 3 and 5
of the loss follow-up list ("read faster", "raise the buffer limits and make
the size configurable").

## Why

A user saw `1,296,525,622 trace events lost` on a 3600 s phase at about
100k IOPS. Each traced request is 2 to 4 events. Today one goroutine per
session reads the instance's text `trace_pipe`: the kernel formats every event
as text (single-threaded, in the reader's read() call), then bbbench parses the
line by hand and feeds it under a mutex taken for each event. That reader falls
behind and the per-CPU ring buffers overrun.

Two changes:

- **(3) Read the binary per-CPU pipes.** `per_cpu/cpuN/trace_pipe_raw` returns
  ring-buffer pages as the kernel stored them. There is no kernel text
  formatting and no text parsing, and each CPU gets its own reader goroutine.
- **(5) Bigger and configurable buffers.** `--trace-buffer-mb` replaces the
  fixed 256 MiB budget. The per-CPU cap goes from 16 MiB to 64 MiB.

Nothing here has run on a real kernel. The raw path is verified only against
pages that the tests build by hand from the documented layout (the tests in
this repo run unprivileged, and tracefs is root-only).

## 1. Decoding

### What a page is

A read of `trace_pipe_raw` returns one sub-buffer ("page") of the ring buffer:

```
u64   time_stamp     page base time, in the instance's trace clock
long  commit         data length (low 27 bits) + flags (bit 31 missed events, bit 30 count stored)
data  events...      until commit length
[long missed count]  at data[length] when bit 30 is set
```

Each event starts with a u32 header: `type_len` in the low 5 bits and
`time_delta` in the high 27 bits (little-endian bit field):

| type_len | meaning | size | time |
|---|---|---|---|
| 0 | data, length in the next u32 (`array[0]`, which counts itself); data after it | 4 + array[0], rounded up to 4 | ts += delta |
| 1..28 | data of `type_len * 4` bytes right after the header | 4 + 4·type_len | ts += delta |
| 29 | padding. With delta 0 it is the end of the page (stop). Otherwise a discarded event of length `array[0]` | 4 + array[0] | ts += delta (as libtraceevent's kbuffer does) |
| 30 | time extend | 8 | ts += array[0]<<27 + delta |
| 31 | absolute time stamp (59 bits; top bits from the current ts, as `rb_fix_abs_ts`) | 8 | ts = value |

Data events carry the trace entry: the common fields (`common_type` u16,
`common_flags`, `common_preempt_count`, `common_pid`), then the event's own
fields.

### Layouts come from the kernel, not from memory

At session start the decoder reads these files from the tracefs root:

- `events/header_page`: offset and size of `timestamp`, `commit` and `data`. The
  page size is data offset + data size. The read size is the larger of that and
  `os.Getpagesize()`. The commit word size is `sizeof(long)`.
- `events/header_event`: the `type_len`/`time_delta` widths (5/27 bits) and the
  type numbers (padding 29, time_extend 30, time_stamp 31, data max 28). If they
  differ, raw reading is not used.
- `events/<system>/<event>/format` for every enabled event, plus
  `events/ftrace/print/format` for the trace_marker writes (the start and stop
  markers): `ID`, and each field's offset, size and signedness.

Each event gets a decode plan from the format file. The plan holds the fields
the collectors use:

| event | fields used |
|---|---|
| `block_rq_insert/issue/merge/complete`, `block_bio_queue/complete` | `dev` (dev_t, kernel encoding major<<20 \| minor), `sector`, `nr_sector`, `rwbs` (char array, NUL-terminated) |
| `nvme_setup_cmd` | `ctrl_id`, `qid`, `cid`, `opcode`, `disk` (char array; optional, older kernels lack it) |
| `nvme_complete_rq` | `ctrl_id`, `qid`, `cid` |
| `ftrace/print` | `buf` (flexible array to the end of the event) |

Integer fields of size 1, 2, 4 or 8 are read in host byte order and
sign-extended when the format says signed. If a format file is missing or
unparsable, or a field the plan needs is missing or has an unsupported size,
**the raw path is not used for that session**. The session falls back to the
text pipe and keeps the reason (`ReaderNote`). Decoding one event wrongly is
never an option. Field order and sizes do change between kernels: `rwbs` is 10
bytes on 7.0 and was 8 before. Reading the offsets from the format files is
what makes that safe.

This **removes the "payload format unverified" risk for the raw path**. The
text payload grammar (`TP_printk` output parsed by hand) is not involved. The
binary layout is described by the kernel itself. What is still unverified is
the page and event header encoding: it is taken from `ring_buffer.c` and
libtraceevent's kbuffer, and tested only against pages the tests build.

The decoder turns each data event into the same `flushtrace.Event` the text
path makes, with the decoded values in `Event.F` (`Fields`, `F.Decoded` true)
and `Payload` empty (for a print event, `Payload` is the marker text). Small
strings (`rwbs`, `disk`, the controller name `nvme<ctrl_id>`) are interned per
decoder, so a decoded event allocates nothing. The collectors take their
values from `F` when `F.Decoded`, else they parse `Payload` as today. Their
pairing logic is the same code on both paths.

The **text path stays**. It is the fallback when there is no
`per_cpu/*/trace_pipe_raw`, when a format does not parse, when the clock is
not in ns, or when a raw pipe cannot be polled. It is also the reference that
the tests compare the raw path against. Timestamps are more precise on the raw
path: the text pipe prints µs and the raw pages carry ns.

## 2. Ordering

The text pipe gave one globally time-ordered stream (the kernel merges the
CPUs in `trace_find_next_entry`, lowest CPU first on a tie). The collectors
depend on that order. A request issued on CPU A and completed on CPU B must
arrive issue first, or the block FIFO and the bio pairing count it unpaired.

**Clock.** Merging needs timestamps that compare across CPUs. The private
instance's `trace_clock` is set to `mono` (ktime_get_mono_fast_ns: global, in
ns, NMI-safe). If `mono` is not offered, `global` is used. The instance-level
file leaves the system clock alone. The clock is set before `buffer_size_kb`
(changing the clock resets the buffer). This applies to the text path too,
which used the default `local` clock until now. `local` is per-CPU and not
guaranteed to compare across CPUs, so the change helps that path as well.
When neither clock can be set and the clock in effect is not an ns clock
(`counter`, `uptime`, `x86-tsc`), the raw path is not used: its timestamps
would not be ns. Event time = page `time_stamp` + the deltas, in ns.

**Decision: a bounded k-way merge, not order-tolerant collectors.**
Order-tolerant collectors would have to park completions in every collector
(the iotrace block FIFO, the bio spans, the NVMe map, the Flush collectors). It
would also change their semantics against the text reference. A merge in front
of the unchanged collectors keeps one pairing implementation and makes the
equivalence test meaningful.

The merger (one goroutine per session) keeps a FIFO of decoded batches per
CPU. Every batch is one page, already in time order within its CPU. It emits
events in global time order up to a **watermark** W:

- A CPU that has **finished** (drained at stop, or EOF) does not limit W.
- A CPU with **queued events** does not limit W. Its next event is its queue
  head.
- A **busy** CPU with an empty queue (it last read data and is decoding or
  reading again) limits W to the last timestamp it delivered. Per-CPU streams
  are monotonic, so nothing older can come from it.
- An **idle** CPU (its last read found the buffer empty) limits W to the
  larger of its last timestamp and `aged`. `aged` is the newest timestamp of
  every batch that reached the merger at least **H = 100 ms** ago (wall
  clock). Idle means its buffer was empty at the last read. An event it
  stores later is delivered after an `irq_work` wake-up plus a goroutine
  wake-up. H covers that, including the Go netpoller's worst case of about
  10 ms when every P is busy. A merge-only timer re-evaluates when the oldest
  held batch ages past H. The readers never sleep-poll.
- **Cap**: when more than 1 M events are queued (about 128 MB), the oldest are
  emitted regardless of W. This bounds memory when one CPU's reader stays
  behind.

Ties go to the lower CPU first, like the kernel. An event whose timestamp is
older than one already emitted is **late**: it was held back beyond the
watermark (a CPU behind by more than H, or a forced emit under the cap). It is
still fed, in arrival order, and counted in `Late`
(`Instance.Late()`, `iotrace.Summary.Late`, the log header `late=`). A late
event that breaks a pair shows up in `Unpaired`, and so in `Lost`, through the
collectors' existing accounting. `Late` itself is not added to `Lost`, because
a late event that still paired correctly would then be counted as lost when it
was not. The merger never reorders events it has already emitted.

Tests: cross-CPU pairing in both CPU orders (issue on the lower and on the
higher CPU), the busy watermark, the idle hold with an injected clock, the cap
and late counting.

## 3. Overrun accounting

The text pipe reported `CPU:N [LOST n EVENTS]`. The kernel prints the per-CPU
`overrun - last_overrun` delta of the page it swapped in. A raw page carries
the same quantity (`cpu_buffer->lost_events`). It is flagged in the commit
word: `RB_MISSED_EVENTS` (bit 31) means events were lost before this page, and
`RB_MISSED_STORED` (bit 30) means the count is stored as a `long` right after
the data. A flagged page adds the stored count to `Overrun`, or 1 when there
was no room to store it (then the count is a lower bound, which the docs say).
`Instance.Overrun` keeps its meaning (events the kernel dropped) on both paths.

## 4. Concurrency and stop

- **Readers**: one goroutine per `per_cpu/cpuN` (`trace_pipe_raw`, opened
  `O_NONBLOCK`, through the Go netpoller). Each one reads a page, decodes it
  without any shared lock into a pooled batch, and sends it to the merger. On
  `EAGAIN` it tells the merger it is idle and parks in the netpoller. The pipe
  supports poll: `tracing_buffers_poll` -> `ring_buffer_poll_wait` with the
  instance's `buffer_percent`. The private instance sets
  `buffer_percent` to 0, so poll reports readable as soon as the buffer holds
  anything. Otherwise an idle CPU with data below the watermark would look
  empty to the merger, and the default of 50 % would delay it without bound.
- **Merger**: the single feeder of the collector. It takes the instance mutex
  **once per emitted run of events**, not once per event. `Locked`, `StartNs`
  and `Overrun` keep their contracts. The start marker (`ftrace/print`,
  `bbbench-start`) is an event in the merged stream, so it is placed in
  time order like on the text path.
- **Stop**: disable the events, write the stop marker, switch `tracing_on`
  off. From then on nothing more is written. Then each reader is told to
  drain: a flag plus `SetReadDeadline(now)` wakes it from the netpoller. With
  the flag set, the reader keeps reading without parking until a read returns
  `EAGAIN` (empty) or 0 (EOF of a plain file in the tests). Because tracing is
  off, empty means every event this CPU stored has been delivered. This
  per-CPU drain replaces the stop marker's proof (the marker could only prove
  delivery for the single merged stream). When every reader has finished, the
  merger emits everything left in order and exits. Stop waits for that (3 s,
  Close 0.5 s). Then it closes the files. A reader still blocked is woken by
  the close (`ErrFileClosing`), and the merger exits once every reader has
  said done. No goroutine or fd outlives Stop/Close. A test checks the
  goroutine and fd counts.
- **Pollability**: if a raw pipe on a real tracefs is not pollable
  (`SetReadDeadline` returns `ErrNoDeadline` and it is not a regular file),
  the session falls back to the text path. A non-pollable pipe could not be
  drained without blocking or sleeping. The splice path is not used.

## 5. Page size

Reads are of the sub-buffer size (header_page) and at least
`os.Getpagesize()`. The kernel returns one page per read, zero-filled after
the commit length. `buffer_size_kb` stays per CPU.

## 6. Fake tracefs for tests

A new package, `internal/tracefake`, builds what the kernel would show:

- format files generated from C-style field lists with natural alignment
  (`field:unsigned short common_type;	offset:0;	size:2;	signed:0;`), for the
  block, bio, NVMe and print events. The layouts are those of the kernel's
  `TP_STRUCT__entry` (block from 7.0's `include/trace/events/block.h`, NVMe
  from `drivers/nvme/host/trace.h` as remembered: the decoder reads offsets
  from the files, so a wrong fixture layout cannot make the decoder right by
  accident). Also `header_page` and `header_event`.
- a page encoder: events per CPU into pages, with small and large data
  events, time extends, absolute time stamps, padding, and the missed-events
  flags and count.
- `FromText` (its own Sscanf-based reading of the kernel's text lines,
  independent of the production parser), so the existing iotrace session
  tests can also run through the raw path.

A randomized test generates one event stream (block and bio requests, NVMe
commands, markers, some lost events), spreads it over CPUs, writes it both as
text lines and as raw pages, and checks that both paths give identical
results: pairing, windows, unpaired counts, overrun.

A benchmark compares the two on the same synthetic stream: the raw decoder +
merge + feed against ParseLine + feed.

## Part B: `--trace-buffer-mb`

- `--trace-buffer-mb N` on the orchestrator (default 256). It is the total
  that the concurrent sessions share, as today. N < 1 or a non-number is
  rejected at startup.
- The per-CPU size is budget / (sessions × CPUs), at least 1 MiB (floor) and at
  most 64 MiB (was 16 MiB). Total kernel memory = sessions × CPUs × per-CPU
  size. That is at most the budget, except at the 1 MiB floor, where it is
  sessions × CPUs MiB. The flag help and the README say so. The Flush session
  takes the same helper, capped at its own default (it sees only Flushes).
- The run summary prints the chosen per-CPU size when tracing is on:
  `ftrace buffers: <budget> MiB budget, <n> MiB per CPU for up to <s>
  concurrent sessions on <c> CPUs`.
- When ring-buffer loss is reported, the terminal summary and the graphs
  page's loss note add the hint: `raise --trace-buffer-mb or lower the IOPS`.
  The hint lives in cmd/bbbench. The iotrace package does not name the
  flags of a command.
