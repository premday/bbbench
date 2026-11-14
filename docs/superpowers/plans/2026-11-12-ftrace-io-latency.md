# ftrace I/O latency and series toggles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** For every phase on a block device, time each read and write from kernel trace events (device latency and block-layer latency) next to fio's latency, say on every chart which tracepoints and which source a series comes from, and let the user show or hide each series with a y scale computed from what is shown.

**Architecture:** The tracefs instance mechanics of `internal/flushtrace` (instance, start/stop markers, reader, stop protocol) become a reusable `flushtrace.Instance`. A new package `internal/iotrace` runs one instance per phase and drive (the drive, or the array node and its members), pairs issue/complete events exactly, and aggregates latencies in the process into one histogram window per `--log-avg-msec` plus a phase histogram after the ramp. The windows and the summary are stored in `<prefix>_iolat.1.log` (same shape rules as the Flush and members logs) and loaded into `PhaseResult.IOTrace`/`IOTraceSeries`, then into the graph data (`ts_iolat`, `io_trace`). The graphs page overlays the ftrace series on the Latency chart, adds a `Device latency (ftrace)` metric, prints a source line on each card, and builds every chart from one dataset function that honours per-series checkboxes.

**Tech Stack:** Go (stdlib), tracefs, Chart.js in the embedded page JS, node for the page-script tests.

**Spec:** `docs/superpowers/specs/2026-11-12-ftrace-io-latency-design.md`

## Global Constraints

- On by default; opt out with `--no-io-trace`. The default is one line (`fs.Bool("no-io-trace", false, ...)`) so it can flip to opt-in later.
- Tracing never fails or slows down a run silently: a failed start is a per-phase note with the reason (`io_trace_notes`), never an error of the phase or the run; overruns and unpaired events are counted and shown.
- A missing tracepoint or a device without request events produces **no** series and a text saying so, never a series of zeros.
- Only reads and writes are counted (rwbs with `R` or `W` and `nr_sector > 0`; NVMe `nvme_cmd_read`/`nvme_cmd_write` on I/O queues `qid > 0`). Discard, flush, write-zeroes and admin commands are ignored; Flush keeps its own session, unchanged.
- No per-I/O storage: latencies go into in-process histograms (relative error under 3 %), one window per `--log-avg-msec`; the phase summary uses the same histogram over completions at or after `offset_ms` (fio's ramp).
- HDDs (SATA/SAS, rotational) get the same metrics as NVMe: the `block_rq_*` path (insert/issue/complete) is the HDD path and is a first-class source, not a fallback. Nothing in the collectors, the summary, the log, the cards, legends or source text assumes NVMe: an `sdX` device shows `block_rq_issue -> block_rq_complete, block_rq_insert -> block_rq_complete`. The histogram covers HDD latencies up to several seconds (and beyond) with the same relative error. Array members (md/LVM over HDDs or NVMe) are traced per member through the same per-device path, so HDD arrays work.
- Private tracefs instance only (`instances/bbbench-iolat-<pid>-<n>`); nothing on a drive or in the global trace settings changes.
- Legend names exactly: `Read (fio)`, `Write (fio)`, `Read device (ftrace)`, `Read block layer (ftrace)`, `Write device (ftrace)`, `Write block layer (ftrace)`, `Array read (fio)`, `Array write (fio)`, members `sdb read (kernel stat)` (role in parentheses after the name as today: `sdc (spare) read (kernel stat)`). New metric: `key: 'iolat'`, label `Device latency (ftrace)`.
- Compare page: not changed (no new series, legend unchanged).
- Page JS lives in Go raw strings: no backticks anywhere in it (`chartSharedJS` and the graphs page). SI units via `siScale`/`fmtScaled`; tooltips use `fmtScaled(v, sc, true)` (two decimals).
- The core (`cmd/bbbench`) never names a layout backend (`TestCoreDoesNotNameTheBackend`).
- `make check` stays green; page code is tested in node (`runPageJS`, `requireNode`).
- Atomic commits, intent in the message, ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. No push, no PR.

## Review Focus

- Array whose node is bio-based (md, dm): the array node has no request events, so it has no ftrace series and its source text says `no request events seen`; the members are still traced and shown. Pinned by `TestSession_ArrayMembers` (Task 3) and `TestGraphsPageIOLatNone` (Task 7).
- High-IOPS phase where the reader cannot keep up: the kernel prints `CPU:N [LOST M EVENTS]`; these are counted, the phase result stays valid and the card says how many events were lost. Pinned by `TestInstance_CountsLostEvents` (Task 1), `TestSession_UnpairedAndOverrun` (Task 3), `TestGraphsPageSourceLine` (Task 6).
- Kernel without `block_rq_insert` (or without the NVMe tracepoints): no block-layer series, the source line names only what was monitored, device latency falls back to `block_rq_issue -> block_rq_complete`. Pinned by `TestSession_NoInsertTracepoint` and `TestSession_NVMeFallsBackToBlock` (Task 3).
- tracefs missing / not root: the phase runs, the Latency and Device latency cards and the run Details page show `no ftrace latency: <reason>`, and the note survives a reload of the summary JSON. Pinned by `TestStartIOTraceNoteOnFailure` (Task 5) and `TestLoadBenchmarkSummary_LoadsIOLat` (Task 4).
- HDD with a long queue (QD 32+, mq-deadline or bfq): requests are inserted, merged and completed out of order, several seconds can pass between insert and complete, and the same sector can be rewritten while an earlier write is pending; pairing by (dev, sector) FIFO must keep every pair right and count none lost. Pinned by `TestSession_HDDLongQueue` (Task 3) and `TestHistHDDRange` (Task 2).
- Old run without `_iolat.1.log`/`io_trace`: pages render as before (no Device latency cards, no overlay, no source line except fio's); only the legend names change. Pinned by `TestGenerateDiskPhaseData_IOTrace` (Task 6) and `TestGraphsPageIOLatNone` (Task 7).

---

### Task 1: Reusable trace instance in flushtrace

No behaviour change for Flush tracing; the existing `flushtrace` tests must pass unchanged.

**Files:**
- Create: `internal/flushtrace/instance.go`, `internal/flushtrace/instance_test.go`
- Modify: `internal/flushtrace/session.go` (Session wraps an Instance), `internal/flushtrace/parse.go` (ParseLine without regexp), `internal/flushtrace/collect_test.go` or a new `parse_test.go`

**Interfaces:**
- Produces:
  - `func FindTracefs(opts Options) (string, error)` (the current `findTracefs`, exported; `TracefsAvailable` unchanged)
  - `func DevNumbers(sysBlock, name string) (maj, min int, err error)` (reads `<sysBlock>/<name>/dev`; sysBlock "" = `/sys/block`)
  - `func HasEvents(root string, events ...string) bool` (each `events/<group>/<name>` exists, e.g. `"block/block_rq_insert"`)
  - `func InstanceName(kind string) string` = `bbbench-<kind>-<pid>-<seq>` (Flush keeps `bbbench-flush-...`)
  - `type Instance struct`; `func OpenInstance(root, name string, bufferKB int, enable []string, filters map[string]string, feed func(Event)) (*Instance, error)`: creates `instances/<name>`, writes `buffer_size_kb`, opens `trace_pipe`, starts the one reader goroutine, writes filters (best effort), enables events, `tracing_on` 1, writes the start marker. On failure cleans up.
  - Reader: start/stop markers are handled by the Instance and **not** passed to `feed`; a raw line containing `[LOST N EVENTS]` adds N to the overrun counter; every other parsed event is passed to `feed` with the instance mutex held.
  - `func (i *Instance) Stop() error` (disable events, stop marker, wait for it or EOF, at most 3 s, `tracing_on` 0, remove the instance); `func (i *Instance) StartNs() int64` (0 = marker not seen); `func (i *Instance) Overrun() int64`; `func (i *Instance) Locked(f func())` (runs f with the reader's mutex).
  - `ParseLine` keeps its signature and results but parses by hand (it now runs for every traced I/O).

- [ ] **Step 1: tests** `TestInstance_FeedsEventsNotMarkers` (fake tracefs as in `session_test.go`: start marker, two events, stop marker -> feed sees the two events, `StartNs` is the marker's time), `TestInstance_CountsLostEvents` (pipe has `CPU:2 [LOST 120 EVENTS]` and `CPU:0 [LOST 3 EVENTS]` -> `Overrun() == 123`, the lines are not events), `TestInstance_CleansUpOnEnableFailure`, `TestInstance_BlockOnlyTracefs` (fake tracefs with only `events/block/*` and an `sdb` in a fake sysfs with `queue/rotational` = 1 and `dev` = `8:16`: the instance opens, block events of 8,16 reach `feed`; no nvme directory is needed anywhere), `TestParseLine_Cases` (task name with spaces and dashes, `<idle>-0`, 6- and 9-digit fractions, irq-flags column variants, header and comment lines false, LOST line false), `BenchmarkParseLine`.
- [ ] **Step 2: run, expect FAIL** `go test ./internal/flushtrace`
- [ ] **Step 3: implement** `instance.go`; rewrite `Session` on top of `OpenInstance` (its collector fed through `feed`, `Summary`/`Samples` read through `Locked`); hand-written `ParseLine` with the same results as the old regexp on every existing test line.
- [ ] **Step 4: run, expect PASS** `go test ./internal/flushtrace` (all old tests unchanged) and `go test -bench ParseLine -run x ./internal/flushtrace` (report the ns/op before and after in the commit message body)
- [ ] **Step 5: commit** "Extract the tracefs instance from Flush tracing so other collectors can share it"

### Task 2: Latency histogram and windows

**Files:**
- Create: `internal/iotrace/hist.go`, `internal/iotrace/window.go`, `internal/iotrace/hist_test.go`, `internal/iotrace/window_test.go`

**Interfaces:**
- Produces:
  - `type Kind string` with `KindDevice Kind = "device"`, `KindBlockLayer Kind = "blocklayer"`; `type Dir string` with `DirRead Dir = "r"`, `DirWrite Dir = "w"`
  - `type Stats struct { Count int64 `json:"count"`; MeanNs float64 `json:"mean_ns"`; MinNs, MaxNs, P50Ns, P99Ns int64 }` (json `min_ns`, `max_ns`, `p50_ns`, `p99_ns`)
  - `type Hist struct` (zero value ready): `Add(ns int64)` (negative = 0), `Count() int64`, `Reset()`, `Stats() Stats`. Count, mean, min and max are exact; p50/p99 use nearest rank and return the bucket's upper edge clamped to [min, max]. Bucket layout: one bucket per ns below 128, then 64 sub-buckets per power of two up to 2^40 ns; larger values go in the last bucket (max stays exact). Relative error at most 1/64.
  - `type Window struct { EndMs int64; Dev string; Kind Kind; Dir Dir; Stats }`
  - unexported `aggregator`: `newAggregator(windowMs, offsetMs int) *aggregator`; `add(dev string, k Kind, d Dir, relNs, latNs int64)`; `finish() ([]Window, map[string]map[Kind]map[Dir]Stats)`. Window index = `relNs / windowNs` (windowMs <= 0 = `flushtrace.DefaultWindowMs`); one open window per (dev, kind, dir), closed when a completion of a later window arrives; a completion of an earlier window than the open one counts in the open one; empty windows are omitted; `EndMs = (k+1)*windowMs`; the phase map only gets completions with `relNs >= offsetMs*1e6`. Windows sorted by EndMs, then dev, kind, dir.

- [ ] **Step 1: tests** `TestHistExactSmall` (values below 128 exact percentiles), `TestHistRelativeError` (100000 log-uniform values 1 µs .. 100 ms from `rand.New(rand.NewPCG(1, 2))`: p50 and p99 within 3 % of exact nearest-rank), `TestHistConstant` (all 100000 ns -> min = max = p50 = p99 = 100000), `TestHistEmpty` (zero Stats, no NaN), `TestHistHuge` (2^42 ns: max exact, p99 <= max), `TestHistHDDRange` (100000 log-uniform values 2 ms .. 10 s, HDD seek and queueing times: p50 and p99 within 3 % of exact; a bimodal set of 100 µs cache hits and 8-15 ms seeks keeps both modes in p50/p99), `TestAggregatorWindows` (two windows of 1000 ms, EndMs 1000 and 2000, counts and means), `TestAggregatorLateEventGoesToOpenWindow`, `TestAggregatorSummarySkipsRamp` (offset 1000: only completions at or after 1 s in the phase stats, all in the windows).
- [ ] **Step 2: run, expect FAIL** `go test ./internal/iotrace`
- [ ] **Step 3: implement**
- [ ] **Step 4: run, expect PASS**
- [ ] **Step 5: commit** "Add the log-bucketed latency histogram and per-window aggregation for I/O tracing"

### Task 3: Collectors and session

**Files:**
- Create: `internal/iotrace/collect.go`, `internal/iotrace/session.go`, `internal/iotrace/text.go`, `internal/iotrace/collect_test.go`, `internal/iotrace/session_test.go`

**Interfaces:**
- Consumes: `flushtrace.FindTracefs`, `DevNumbers`, `HasEvents`, `InstanceName`, `OpenInstance`, `Instance.Stop/StartNs/Overrun/Locked`, `flushtrace.Event` (Task 1); `Hist`, `aggregator`, `Window`, `Stats`, `Kind`, `Dir` (Task 2).
- Produces:
  - Payload parsers (no regexp): `parseBlockRq(payload string, withBytes bool) (blockRq, bool)` with `blockRq{Maj, Min int; RWBS string; Sector uint64; Nr uint32}` (insert, issue and merge carry a bytes field, complete does not; tolerate the optional `ioprio` field and a `(cmd)` containing spaces: sector and nr are read after the first `") "`); `parseNVMeSetup(payload string) (ctrl, disk string, qid, cmdid int, op string, ok bool)` (`op` = the word after `cmd=(`); `parseNVMeDone(payload string) (ctrl string, qid, cmdid int, ok bool)`; `dirOf(rwbs string, nr uint32) (Dir, bool)` (false when nr == 0 or rwbs has `D`, `E` or `N` first; `w` when it contains `W`, `r` when it contains `R`).
  - Pairing (the builder's ruling of the spec's open point): NVMe pending keyed (ctrl, qid, cmdid) with issue time and direction; block pending keyed (maj, min, sector), a FIFO of `{insertNs, issueNs, dir}`: insert pushes; issue sets `issueNs` on the oldest entry without one, else pushes one with `insertNs = 0` (a directly issued request, never queued); complete pops the oldest: block-layer latency = complete - (insertNs, or issueNs when 0); device latency for a `block` source = complete - issueNs. `block_rq_merge` drops the newest entry of its key (absorbed request, not lost). A back-merged request keeps its start sector, so it still pairs. Requeues are not handled. Unpaired = completions of a traced device with no pending entry, plus entries still pending at Stop.
  - `var ErrUnsupported = errors.New("I/O latency tracing unsupported")`
  - `type Options struct { TracefsRoot, SysBlock, InstanceName string; AvgMsec, OffsetMs int; BufferKB int }` (BufferKB 0 = 16384)
  - `type DeviceSummary struct { Name string `json:"name"`; Source string `json:"source,omitempty"` ("nvme" | "block"); Events []string `json:"events,omitempty"` (e.g. `"nvme_setup_cmd -> nvme_complete_rq"`, `"block_rq_insert -> block_rq_complete"`); Pair string `json:"pair,omitempty"` ("ctrl,qid,cmdid" | "dev,sector"); Exact bool `json:"exact"`; BlockLayer bool `json:"block_layer"`; Unpaired int64 `json:"unpaired,omitempty"`; Stats map[Kind]map[Dir]Stats `json:"stats,omitempty"`; Note string `json:"note,omitempty"` (why not traced) }`
  - `type Summary struct { AvgMsec int `json:"avg_msec,omitempty"`; OffsetMs int `json:"offset_ms,omitempty"`; Overrun int64 `json:"overrun,omitempty"`; Devices []DeviceSummary `json:"devices"` (the target first, then members in the given order); Lost int64 `json:"lost"`; Text string `json:"text"` }`; `func (s *Summary) Fill()` sets `Lost` = Overrun + every Unpaired and `Text` = `SourceLine(s)`.
  - `type Result struct { Summary Summary; Windows []Window }`
  - `func Start(devices []string, opts Options) (*Session, error)`: per name: source `nvme` when the name starts with `nvme` and `HasEvents(root, "nvme/nvme_setup_cmd", "nvme/nvme_complete_rq")`, else `block` when `block_rq_issue` and `block_rq_complete` exist; dev numbers from `DevNumbers`; a name that cannot be resolved gets a `DeviceSummary` with `Note` and is not traced; no traceable device -> error wrapping `ErrUnsupported` with the reasons. Enables the union of: nvme setup/complete (nvme devices), `block_rq_issue`, `block_rq_complete`, and `block_rq_insert`, `block_rq_merge` when present (`BlockLayer` = insert present). Filters, best effort: nvme_setup_cmd `(opcode == 1 || opcode == 2) && qid > 0`; block events `dev == <maj<<20|min> || ...` over the traced devices. Completions before the start marker are ignored. Instance name `flushtrace.InstanceName("iolat")` unless Options.InstanceName.
  - `func (s *Session) Stop() (Result, error)` (Result valid even with a cleanup error; `Summary.Fill()` done)
  - `func SourceLine(s *Summary) string`, exact formats: one device `ftrace: <events joined ", "> · pairing: exact (<pair>) · lost events: <n>`; with loss `· lost events: 12 (ring buffer 10, unpaired 2)`; a device without any stats `ftrace: <events> · no request events seen · lost events: 0`; several devices `ftrace: <dev>: <events> · pairing: exact (<pair>); <dev>: no request events seen; <dev>: not traced (<note>) · lost events: <n>`.

- [ ] **Step 1: tests** `TestParseBlockRq` (insert/issue with bytes, complete without, with `none,0,0` ioprio, cmd with spaces, garbage false), `TestParseNVMe` (setup read/write/flush, complete, without `disk=`), `TestDirOf` (`R`, `RA`, `RS`, `WS`, `WFS`, `FWS` -> w; `F` with nr 0, `D`, `DS`, `N` -> false), `TestSession_NVMeReadWrite` (fake tracefs and pipe: start marker; read and write setup/complete on qid 1; an admin qid 0 command and a flush ignored; block insert/issue/complete of the namespace's dev numbers -> device and blocklayer stats per direction with exact values; windows), `TestSession_HDD` (an `sdb` with `queue/rotational` 1, `dev` 8:16, a fake tracefs **without** `events/nvme`: source `block`, pair `dev,sector`, Events `block_rq_issue -> block_rq_complete` and `block_rq_insert -> block_rq_complete`, device and blocklayer stats for reads and writes with latencies of 5-900 ms, `SourceLine` shows the block tracepoints and no nvme name), `TestSession_HDDLongQueue` (32 requests inserted, some back-merged (`block_rq_merge` of the absorbed one), issued in a different order and completed out of order over 3 s, two writes to the same sector pending at once: every pair has the expected latency, FIFO per sector, Unpaired 0, windows span the 3 s), `TestSession_BlockDirectIssue` (no insert for a request -> blocklayer latency from issue), `TestSession_MergeNotLost` (insert A, insert B, merge B, issue/complete A -> Unpaired 0), `TestSession_UnpairedAndOverrun` (a completion without issue plus a LOST line of 10 -> Unpaired 1, Overrun 10, Lost 11), `TestSession_NoInsertTracepoint` (BlockLayer false, Events has one entry, no blocklayer stats), `TestSession_NVMeFallsBackToBlock` (no nvme events dir -> source block), `TestSession_ArrayMembers` (devices md0, sdb, sdc in a fake sysfs, sdb/sdc rotational HDDs, plus a mixed case with an nvme0n1 member: each member gets its own source; only member events -> md0 has empty Stats, members have stats; SourceLine names `md0: no request events seen`), `TestSession_EventsBeforeStartMarkerIgnored`, `TestSession_Unsupported` (no tracefs; no tracepoints; unknown device -> `errors.Is(err, ErrUnsupported)`), `TestSourceLine` (each exact format above), `BenchmarkFeed` (1e5 block issue/complete lines).
- [ ] **Step 2: run, expect FAIL** `go test ./internal/iotrace`
- [ ] **Step 3: implement**
- [ ] **Step 4: run, expect PASS** `go test -race ./internal/iotrace ./internal/flushtrace`
- [ ] **Step 5: commit** "Trace read and write latency at the device and in the block layer from kernel events"

### Task 4: Log format, loader and summary JSON

**Files:**
- Create: `internal/iotrace/log.go`, `internal/iotrace/log_test.go`, `cmd/bbbench/iolatlog.go`, `cmd/bbbench/iolatlog_test.go`
- Modify: `cmd/bbbench/serve_results.go` (`PhaseResult`), `cmd/bbbench/serve.go` (`loadBenchmarkSummary`, next to `applyFlushLog` ~line 322), `cmd/bbbench/orchestrator_executor.go` (`updateLivePhase`, next to `applyFlushLog` ~line 566)

**Interfaces:**
- Consumes: `Result`, `Summary`, `DeviceSummary`, `Window`, `Stats` (Tasks 2-3).
- Produces:
  - `func LogName(prefix string) string` = `prefix + "_iolat.1.log"`; `func WriteLog(w io.Writer, r Result) error`; `func ReadLog(r io.Reader) (Result, error)` (calls `Summary.Fill()`; wrong magic, empty file or a bad data line is an error; unknown `#` lines are ignored).
  - Format (the per-device lines and the trailing `dev` column are the builder's ruling: one file holds the target and its members):
    ```
    # bbbench-iolat-log v1 avg_msec=1000 offset_ms=10000 overrun=0 lost=0
    # device nvme0n1 source=nvme exact=true block_layer=true unpaired=0 pair=ctrl,qid,cmdid events=nvme_setup_cmd->nvme_complete_rq,block_rq_insert->block_rq_complete
    # device md0 note=<text with spaces as _>
    # summary nvme0n1 device r count=312 mean_ns=85000.5 min_ns=60000 max_ns=410000 p50_ns=84000 p99_ns=120000
    # end_ms, kind, dir, count, mean_ns, min_ns, max_ns, p50_ns, p99_ns, dev
    1000, device, r, 312, 85000, 60000, 410000, 84000, 120000, nvme0n1
    ```
    In `events=` the list is comma-separated and ` -> ` is written `->`; the reader restores ` -> `.
  - `type IOLatPoint struct { T float64 `json:"t"`; Kind string `json:"kind"`; Dir string `json:"dir"`; N int64 `json:"n"`; Mean float64 `json:"mean"`; P50 float64 `json:"p50"`; P99 float64 `json:"p99"` }` (latencies in µs, T = window start in s on the trace clock)
  - `func writeIOLatLog(logPrefix string, r iotrace.Result) error`
  - `func loadIOLatLog(logPrefix string) (*iotrace.Summary, map[string][]IOLatPoint, bool)` (device -> points; windows with `EndMs <= OffsetMs` dropped like `loadFlushLog`; `T = (EndMs - avg)/1000`; avg 0 = `defaultLogAvgMsec`)
  - `func applyIOLatLog(phase *PhaseResult, device, logPrefix string)`
  - `PhaseResult.IOTrace map[string]*iotrace.Summary `json:"io_trace,omitempty"``, `PhaseResult.IOTraceSeries map[string]map[string][]IOLatPoint `json:"io_trace_series,omitempty"`` (drive -> traced device -> points), `PhaseResult.IOTraceNotes map[string]string `json:"io_trace_notes,omitempty"`` (drive -> why not traced; only the summary JSON carries it, the loader never clears it)

- [ ] **Step 1: tests** `TestLogRoundTrip` (two devices, an NVMe one and an HDD `sdb` with source `block` and multi-second max latencies, both kinds and directions, a noted device, overrun, events with ` -> ` restored, Text and Lost filled), `TestReadLogRejectsGarbageLine`, `TestReadLogNotIOLat` (a flush log -> error), `TestLogNameSuffix`; `TestIOLatLog_DropsRamp`, `TestIOLatLog_Missing` (ok false), `TestLoadBenchmarkSummary_LoadsIOLat` (summary JSON with `fio_files` and a stored log -> `io_trace` and `io_trace_series`; a second phase without a log keeps its `io_trace_notes` and has no `io_trace`).
- [ ] **Step 2: run, expect FAIL** `go test ./internal/iotrace ./cmd/bbbench -run 'Log|IOLat'`
- [ ] **Step 3: implement** (call `applyIOLatLog` wherever `applyFlushLog` is called)
- [ ] **Step 4: run, expect PASS**
- [ ] **Step 5: commit** "Store the per-window I/O latency next to the fio logs and load it into the run summary"

### Task 5: Record it in every phase

**Files:**
- Modify: `cmd/bbbench/iolatlog.go` (start/stop helper), `cmd/bbbench/orchestrator_executor.go` (`FioResult`, `executePhase` ~line 1030-1093, `updateLivePhase`), `cmd/bbbench/orchestrator.go` (flag ~line 88, `runOptions`, coordinator setup ~line 511), `cmd/bbbench/deps.go` (`checkEnvironment`), `cmd/bbbench/orchestrator_results.go` (terminal summary), `cmd/bbbench/serve_browse.go` (Details page phase list ~line 599), tests in `cmd/bbbench/iolatlog_test.go`, `cmd/bbbench/orchestrator_results_test.go` (or the existing file that tests `formatFlushSummary`), `cmd/bbbench/serve_browse_test.go`

**Interfaces:**
- Consumes: `iotrace.Start/Stop/Options/Summary` (Task 3), `writeIOLatLog`, `applyIOLatLog`, the PhaseResult fields (Task 4), `DriveInfo.Members`, `phaseRampMs`, `ec.avgMsec()`.
- Produces:
  - `FioResult.IOTrace *iotrace.Summary`, `FioResult.IOTraceNote string`
  - `func startIOTrace(drive DriveInfo, logPrefix string, avgMsec, rampMs int) (stop func() *iotrace.Summary, note string)` over `startIOTraceWith(drive DriveInfo, logPrefix string, avgMsec, rampMs int, opts iotrace.Options)`: devices = `drive.Device.Name` then each member's `Name`; on a Start error: note = `"no ftrace latency: " + err.Error()`, logged at Warn, `stop` returns nil; `stop` stops the session, writes the log (Warn on failure), returns the summary, and is idempotent.
  - `executePhase`: when `ec.ioTrace`, start right before fio (after the Flush session and the member sampler), stop after `stopMembers()` and before `stopTrace()`; fill the two FioResult fields. `updateLivePhase` copies `IOTrace` into `PhaseResult.IOTrace`, `IOTraceNote` into `IOTraceNotes`, then `applyIOLatLog`.
  - Flag `--no-io-trace` ("do not trace read and write latency (kernel trace events) during phases"); `runOptions.IOTrace bool` (= `!*noIOTrace`); `ExecutionCoordinator.ioTrace bool`.
  - `checkEnvironment` no longer returns before the I/O check when Flush tracing is off; when `ec.ioTrace` and `flushtrace.TracefsAvailable()` fails it prints once: `  warning: I/O latency cannot be traced: <err>` and `           the phases still run and report fio's latency only`.
  - `func formatIOTraceSummary(s *iotrace.Summary) string` for the target (first) device only: `ftrace device: read avg 85.0 us p99 120.0 us, write avg 30.1 us p99 41.0 us`, plus ` (12 events lost)` when Lost > 0; a target without stats gives `ftrace: no request events seen`. The terminal summary prints it next to the Flush line, and the note when `IOTraceNote` is set.
  - Details page: per device of `phase.io_trace` a `phase-stats` line `<dev>: <text>`; each `io_trace_notes` entry as a warning-styled line like `phase.warnings`.

- [ ] **Step 1: tests** `TestStartIOTraceNoteOnFailure` (TracefsRoot = an empty dir -> note starts with `no ftrace latency:`, `stop()` returns nil and can be called twice), `TestStartIOTraceWritesLog` (fake tracefs with a pre-created instance and pipe, an array drive with two members -> the log exists and names the target and both members), `TestFormatIOTraceSummary` (values, loss suffix, no-events text), `TestUpdateLivePhaseCarriesIOTrace` (a result with a note and one with a stored log -> `IOTraceNotes`, `IOTrace`, `IOTraceSeries`), `TestDetailsPageShowsIOTrace` (page string contains `io_trace_notes` and `io_trace`).
- [ ] **Step 2: run, expect FAIL** `go test ./cmd/bbbench -run 'IOTrace|IOLat|Details'`
- [ ] **Step 3: implement**
- [ ] **Step 4: run, expect PASS** `go test -race ./cmd/bbbench ./internal/...`
- [ ] **Step 5: commit** "Trace read and write latency in every phase unless --no-io-trace, and say why when it cannot"

### Task 6: Graph data, source names in legends, source lines

**Files:**
- Modify: `cmd/bbbench/serve_graphs.go` (`DiskPhaseData` ~line 45-60, `generateDiskPhaseData` ~line 193, page JS: `makeChart`, `memberLabel`, `arrayDatasets`, `makeFlushChart`, `makeChartCard`, `openModal`, modal HTML), `cmd/bbbench/serve_shared.go` (`rampOf` fallback), `internal/flushtrace/text.go` (new), `internal/flushtrace/text_test.go`, `cmd/bbbench/serve_graphs_test.go`

**Interfaces:**
- Consumes: `PhaseResult.IOTrace/IOTraceSeries/IOTraceNotes`, `IOLatPoint`, `iotrace.Summary` (Tasks 3-4).
- Produces:
  - `DiskPhaseData.TSiolat []IOLatPoint `json:"ts_iolat,omitempty"`` (the target device's points), `TSiolatMembers map[string][]IOLatPoint `json:"ts_iolat_members,omitempty"`` (every other traced device), `IOTrace *iotrace.Summary `json:"io_trace,omitempty"``, `IOTraceNote string `json:"io_trace_note,omitempty"``, `FlushSource string `json:"flush_source,omitempty"``
  - `func SourceLine(s *Summary) string` in flushtrace (tracepoints derived from `Source`, which stays `nvme`/`block`): `ftrace: nvme_setup_cmd -> nvme_complete_rq (Flush) · pairing: exact (ctrl,qid,cmdid) · lost events: <n>` and `ftrace: block_rq_issue -> block_rq_complete (Flush) · pairing: first-in first-out · lost events: <n>`
  - Legend and tooltip names per Global Constraints for IOPS, bandwidth and latency (plain, bar fallback, members and array); the line tooltip prints `ctx.dataset.label + ': ' + value`.
  - JS `function sourceLine(d, metric)`: iops/bw `fio`, plus ` · members: kernel stat (/sys/block/<dev>/stat)` when member series exist; lat the same plus ` · ` + `d.io_trace.text` or `d.io_trace_note`; flush `d.flush_source`; iolat `d.io_trace.text` or `d.io_trace_note`. JS `function lossNote(d, metric)`: for lat/iolat with `d.io_trace.lost > 0`: `⚠ <n> trace events lost: the ftrace series are incomplete`; '' otherwise.
  - Card: a `div.src-line` under the subtitle and the loss note in the card's warn element; modal: new `#modal-src` element filled the same way. `rampOf` falls back to `d.io_trace.offset_ms` after `d.flush.offset_ms`.

- [ ] **Step 1: tests** `TestGenerateDiskPhaseData_IOTrace` (array phase: target points in `ts_iolat`, members in `ts_iolat_members`; a phase without trace data marshals without the `ts_iolat`, `io_trace`, `io_trace_note` keys), `TestFlushSourceLine` (both formats), `TestGraphsPageLegendsNameSource` (page contains `'Read (fio)'`, `'Write (fio)'`, `' (kernel stat)'`, `' (fio)'` on the array labels), node `TestGraphsPageSourceLine` (each case of `sourceLine` and `lossNote` above, including an HDD entry whose `io_trace` has source `block`: the line shows `block_rq_issue -> block_rq_complete` and contains no `nvme`), string `TestGraphsPageNoNVMeAssumption` (no page text or label hard-codes an nvme tracepoint name: tracepoint names only come from `io_trace.text` and `flush_source`); update `TestGraphsPageNoMembersUnchanged` and the member-label assertions of `TestGraphsPageMemberGrid` to the new names.
- [ ] **Step 2: run, expect FAIL** `go test ./cmd/bbbench ./internal/flushtrace -run 'Graphs|IOTrace|SourceLine'`
- [ ] **Step 3: implement**
- [ ] **Step 4: run, expect PASS**
- [ ] **Step 5: commit** "Name the source of every chart series and show the monitored tracepoints on each card"

### Task 7: Latency overlay and the Device latency metric

**Files:**
- Modify: `cmd/bbbench/serve_graphs.go` (METRICS ~line 585, `SRC_TITLE`, `srcBadge`, `initFilters` GT list ~line 726, `makeChart`, `makeMembersChart`, new `makeIOLatChart`, `makeChartCard`, `phaseScaleOf` ~line 1536, `renderAll` X_MAX), `cmd/bbbench/serve_shared.go` (`seriesMaxT`: no fio shift for `flush` and `iolat`), `cmd/bbbench/serve_graphs_test.go`

**Interfaces:**
- Consumes: `d.ts_iolat`, `d.ts_iolat_members`, `d.io_trace`, `d.io_trace_note` (Task 6), `memberKey`, `memberStep`, `memberColors`, `rgba`, `toBase`, `phaseScale`.
- Produces:
  - METRICS entry `{ key: 'iolat', label: 'Device latency (ftrace)', unit: 'μs', tsKey: 'ts_iolat', source: 'ftrace', srcTitle: 'Measured from kernel ftrace events (each read and write request, issue to completion), not by fio', has: d => !!d.io_trace || !!d.io_trace_note, sumR/sumW: the target device's `stats.device.r/w.mean_ns / 1000` }`; Flush gets `srcTitle` with today's text; `srcBadge` uses `metric.srcTitle || SRC_TITLE[metric.source]`; GT gains `['iolat', 'Device latency (ftrace)']`.
  - `function iolatPoints(d, pts, kind, dir, stat)` -> `[{x, y}]`, `x = memberKey(p.t, memberStep(d))`, y = `p[stat]` (µs), negative for writes, windows with `n === 0` skipped.
  - `function ftraceDatasets(d, metric)`: lat -> mean of `device` and `blocklayer` per direction (the four lat names of Global Constraints); iolat -> mean and p99 of each kind and direction, labels `Read device mean (ftrace)`, `Read device p99 (ftrace)`, `Read block layer mean (ftrace)`, `Read block layer p99 (ftrace)` and the same for Write, plus per member of `ts_iolat_members` `<name> read device mean (ftrace)` / `<name> write device mean (ftrace)` in `memberColors`. A kind/direction without points yields no dataset. Styles: read `rgba(37,99,235,…)`, write `rgba(220,38,38,…)`; device `borderDash [6,3]`, block layer `[2,2]`, p99 `[8,3,2,3]` at alpha 0.6; no fill.
  - The Latency chart (plain and member) appends `ftraceDatasets(d, metric)`; `makeIOLatChart(canvas, d, metric, yLow, yHigh)` draws them alone (bars of the summary means when there is no series).
  - `function iolatNone(d, metric)`: iolat only: `d.io_trace_note` when there is no `io_trace`; `ftrace saw no read or write requests on <dev>` when the target device has no stats; '' otherwise. The card shows it instead of a chart (like `flushNone`).
  - `function ftraceExtent(d, metric)` -> `{r, w}` over the drawn ftrace datasets; `phaseScaleOf` includes it for lat and iolat; `renderAll` X_MAX includes the ftrace points on lat.

- [ ] **Step 1: tests** node `TestGraphsPageIOLatDatasets` (run on an NVMe entry and on an HDD entry with source `block` and second-scale latencies: same dataset names and the s/ms SI prefix from `siScale`; lat -> exactly the four names, dashes, writes negative, `n: 0` window skipped; iolat -> eight target datasets plus two per member), node `TestGraphsPageIOLatNone` (note text; empty target stats text; an entry without `io_trace` has no ftrace dataset on lat), node `TestGraphsPageIOLatExtent` (a p99 above every mean raises the iolat extent, not the lat one), string `TestGraphsPageIOLatMetric` (`key: 'iolat'`, `'Device latency (ftrace)'`, the GT entry).
- [ ] **Step 2: run, expect FAIL**; **Step 3: implement**; **Step 4: run, expect PASS** `go test ./cmd/bbbench -run Graphs`
- [ ] **Step 5: commit** "Overlay ftrace device and block-layer latency on the Latency chart and add the Device latency graph"

### Task 8: Series visibility toggles

**Files:**
- Modify: `cmd/bbbench/serve_graphs.go` (page JS and CSS), `cmd/bbbench/serve_graphs_test.go`

**Interfaces:**
- Consumes: every dataset builder of Tasks 6-7 (`stackedMemberDatasets`, `memberLatencyDatasets`, `arrayDatasets`, `ftraceDatasets`, the plain and Flush builders), `memberGrid`.
- Produces:
  - Every dataset carries `skey`: `fio.r`, `fio.w` (plain, bar and array lines), `member:<name>` (both directions of a member, kernel stat), `ftrace.<kind>.<stat>.<dir>` (e.g. `ftrace.device.mean.r`), `ftrace.member:<name>` (iolat), `flush`.
  - `const HIDDEN = {}` (metric key -> Set of skeys; page memory only, no storage); `function isShown(metricKey, skey)`; `function setShown(metricKey, skey, on)`.
  - `function chartDatasets(d, metric, mode)` -> `{datasets, stacked, bars}` in the metric's units, only shown series: the single place the datasets of a card are built. Hidden series are not built at all, so the member fill chain (`'origin'` then `'-1'`) runs over the shown members only and `memberGrid(d, metric, shown)` sums the stack totals from shown members only. `makeChart`, `makeMembersChart`, `makeFlushChart`, `makeIOLatChart` take their datasets from it.
  - `function cardSeries(d, metric, mode)` -> `[{skey, label, color}]`, every series the card could draw, shown or not (one entry per skey).
  - `function phaseExtent(phaseData, metric, mode)` -> `{yLow, yHigh}` from `chartDatasets` of each entry (stack totals for stacked charts), `AXIS_HEADROOM` and the current all-zero rules kept; replaces the body of `phaseScaleOf`. Nothing shown -> `{yLow: 0, yHigh: 1}`.
  - `function seriesRow(d, metric, mode)` -> `div.series-row` with one checkbox and colour swatch per `cardSeries` entry; a change calls `setShown`, then every registered `fill()` of the metric (`REFILL[metric.key]`, reset in `renderAll`) and re-opens the modal when it shows that metric (`modalCtx = {d, metric, pname, mode}`, scale from `phaseExtent`); clicks call `stopPropagation` (the card click opens the modal). Shown in every card and the modal; Chart.js's own legend is turned off where the row is shown.

- [ ] **Step 1: tests** node `TestGraphsPageSeriesKeys` (drive lat with io_trace: `fio.r`, `fio.w`, four `ftrace.*.mean.*`; array iops: each `member:<name>` and `fio.r`/`fio.w`), node `TestGraphsPageSeriesToggleScale` (hide `fio.r` and `fio.w` on lat -> `phaseExtent` follows the ftrace means), node `TestGraphsPageSeriesToggleStack` (hide one member -> stack totals and extent drop by its values, the first shown member fills from `'origin'`), node `TestGraphsPageToggleSurvivesRefill` (`setShown` then `chartDatasets` again -> still hidden; other metric unaffected), string `TestGraphsPageSeriesRowStopsPropagation`. Then render a page in headless Chrome if available and say what was and was not checked.
- [ ] **Step 2: run, expect FAIL**; **Step 3: implement**; **Step 4: run, expect PASS** `go test ./cmd/bbbench -run Graphs`
- [ ] **Step 5: commit** "Let each chart series be shown or hidden, with the phase scale computed from the shown series"

### Task 9: Docs

**Files:** `README.md` (flags list: `--no-io-trace`; output files table: `<device>_phase<N>_<ts>_log_iolat.1.log`; summary JSON keys `io_trace`, `io_trace_series`, `io_trace_notes`; a section "I/O latency tracing" next to "Flush tracing"; `/graphs` line), `CHANGELOG.md`, `documentation/web-ui.md` (Graph Type list, Device latency metric, legend names, source line, loss note, series checkboxes), `documentation/flush-latency.md` (pointer to the sibling session), `CLAUDE.md` (`flushtrace.Instance`, package `internal/iotrace`, the log, executor wiring, `ts_iolat`/`io_trace`, the toggles and `chartDatasets` as the single dataset builder)

- [ ] **Step 1:** state what is measured (device: `nvme_setup_cmd -> nvme_complete_rq` or `block_rq_issue -> block_rq_complete`; block layer: `block_rq_insert` (or the issue of a directly issued request) `-> block_rq_complete`), reads and writes only, that HDDs (SATA/SAS) and array members use the block path with the same metrics, pairing keys, in-process histograms (relative error under 2 %), the phase summary after the ramp, on by default with the CPU cost and the loss counter, `--no-io-trace`, that md/dm array nodes are bio-based and usually show `no request events seen` while their members are traced, and **not verified on real hardware**: the tracepoint formats on the user's kernel were taken from kernel sources and tested on hand-written lines only.
- [ ] **Step 2:** `make check` passes. **Step 3: commit** "Document ftrace I/O latency, its log and the series toggles"
