# Per-member metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** For an array run, chart each member disk's IOPS, bandwidth and latency (from `/sys/block/<dev>/stat`) stacked under, or beside, the array's own line.

**Architecture:** A sampler (`internal/diskstat`) reads the members' stat counters once per fio log window while a phase runs and stores raw per-window deltas in `<prefix>_members.1.log`. The graph data loads that log into `ts_members` (the same path as the Flush log), and the graphs page stacks IOPS and bandwidth per member and draws latency as lines.

**Tech Stack:** Go (stdlib), Chart.js in the embedded page JS.

**Spec:** `docs/superpowers/specs/2026-11-10-per-member-metrics-design.md`

## Global Constraints

- Source of the numbers is `/sys/block/<name>/stat` only (user's decision); no tracing for this.
- IOPS and bandwidth: stacked per member, array **not** in the stack (own stack key, drawn as a line). Latency: one line per member and direction, never stacked or summed.
- Hover lists each member's value, the stack total, then the array's value.
- Read is drawn up, write mirrored down (existing convention); bandwidth in MB/s, latency in µs.
- A sampling failure never fails or invalidates a phase. Runs without the log render exactly as today. Not on the compare page.
- The core (cmd/bbbench, serve_*.go) never names mdadm/lvm (`TestCoreDoesNotNameTheBackend`).
- Atomic commits, intent in the message, ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. No push, no PR.

## Review Focus

- A member whose stat file disappears mid-phase: windows already taken are kept, the member's later points are absent, no panic.
- A counter that goes backwards (device reset): that window's delta is treated as 0, never negative.
- A window with zero I/O: IOPS 0 and latency 0 (no divide by zero), and zero-latency points are not drawn as a latency of 0.
- Phase with ramp-up: windows ending inside the ramp are dropped on load, like the Flush series.
- Array with a spare or journal disk: it is sampled and shown (idle band), labelled with its role.
- Two phases in parallel on different arrays: independent samplers, no shared state.

---

### Task 1: Sampler

**Files:**
- Create: `internal/diskstat/diskstat.go`, `internal/diskstat/diskstat_test.go`

**Interfaces:**
- Produces:
  - `type Counters struct{ ReadIOs, ReadSectors, ReadTicksMs, WriteIOs, WriteSectors, WriteTicksMs uint64 }`
  - `func ReadCounters(sysBlock, name string) (Counters, error)` (fields 0,2,3,4,6,7 of `<sysBlock>/<name>/stat`; sysBlock "" = /sys/block)
  - `type Window struct{ EndMs, SpanMs int64; Device string; D Counters }` where `D` is the delta of the window, backwards counter = 0
  - `type Sampler`; `func NewSampler(names []string, sysBlock string) *Sampler`; `func (s *Sampler) Sample(now time.Time)` (first call sets time zero and the baseline; each later call appends one Window per device, EndMs = ms since the first call); `func (s *Sampler) Run(ctx context.Context, every time.Duration)` (calls Sample at start and every `every` until ctx is done, plus one last Sample on stop); `func (s *Sampler) Windows() []Window` (safe after Run returned)
  - `func (w Window) Rates() (readIOPS, writeIOPS, readMBps, writeMBps, readLatUs, writeLatUs float64)`: per second over SpanMs, "MB/s" is MiB/s like the page's other bandwidth values: sectors*512/1048576 per second; latency µs = ticks*1000/ios, 0 when ios == 0.

- [ ] **Step 1: tests** `TestReadCounters` (fake sysfs tree, fields parsed), `TestSampleDeltas` (stepping `time.Time`, two devices, EndMs and SpanMs, deltas), `TestCounterBackwardsIsZero`, `TestMissingDeviceKeepsWindows` (file removed after the 2nd sample: earlier windows kept, no later ones for it, others continue), `TestRatesZeroIO` (all zero, no NaN), `TestRatesValues` (known deltas -> exact IOPS, MiB/s, µs).
- [ ] **Step 2: run, expect FAIL** `go test ./internal/diskstat`
- [ ] **Step 3: implement** per the signatures above; `Run` uses a ticker and does a final `Sample` when ctx ends.
- [ ] **Step 4: run, expect PASS**
- [ ] **Step 5: commit** "Add the sysfs disk counter sampler used for per-member metrics"

### Task 2: Members log

**Files:**
- Create: `internal/diskstat/log.go`, `internal/diskstat/log_test.go`

**Interfaces:**
- Consumes: `Window`, `Counters` (Task 1).
- Produces:
  - `type Meta struct{ AvgMsec, OffsetMs int; Members []MemberMeta; Array string }`, `type MemberMeta struct{ Name, Role string }`
  - `func LogName(prefix string) string` = `prefix + "_members.1.log"`
  - `func WriteLog(w io.Writer, m Meta, ws []Window) error`; `func ReadLog(r io.Reader) (Meta, []Window, error)`
  - Format: `#` header lines `avg_msec N`, `offset_ms N`, `array <name>`, `device <name> <role>`; then one line per window and device `end_ms, span_ms, device, read_ios, write_ios, read_sectors, write_sectors, read_ticks_ms, write_ticks_ms`.

- [ ] **Step 1: tests** `TestLogRoundTrip`, `TestReadLogRejectsGarbageLine` (error, not panic), `TestLogNameSuffix`.
- [ ] **Step 2: run, expect FAIL**; **Step 3: implement**; **Step 4: run, expect PASS**
- [ ] **Step 5: commit** "Store per-member counter windows next to the fio logs"

### Task 3: Record in the phase and load into the graph data

**Files:**
- Create: `cmd/bbbench/memberslog.go`, `cmd/bbbench/memberslog_test.go`
- Modify: `cmd/bbbench/orchestrator_executor.go` (start/stop next to the Flush trace in `executePhase`, ~line 1030; apply next to `applyFlushLog` ~line 566), `cmd/bbbench/serve.go` (~line 322, apply on load), `cmd/bbbench/serve_results.go` (`PhaseResult.MemberSeries`), `cmd/bbbench/serve_graphs.go` (`DiskPhaseData.TSmembers`, filled near `dp.TSflush`)

**Interfaces:**
- Consumes: `diskstat.NewSampler/Run/Windows/Rates`, `diskstat.WriteLog/ReadLog/LogName`; `DriveInfo.Members`, `phaseRampMs`, `ec.avgMsec()`.
- Produces:
  - `type MemberSeries struct { Role string `json:"role,omitempty"`; IOPS, BW, Lat []TimePoint }` with json `iops`, `bw`, `lat` (R = read, W = write; MB/s; µs)
  - `PhaseResult.MemberSeries map[string]map[string]MemberSeries` (device -> member -> series), json `member_series,omitempty`
  - `DiskPhaseData.TSmembers map[string]MemberSeries` json `ts_members,omitempty`
  - `func loadMembersLog(logPrefix string) (map[string]MemberSeries, bool)`: windows ending inside the ramp are dropped; points stamped at the window start (`(EndMs-SpanMs)/1000`, same as the Flush series); latency points with 0 for a direction stay 0 (the JS skips them)
  - `func applyMembersLog(phase *PhaseResult, device, logPrefix string)`
  - `func startMemberSampler(drive DriveInfo, logPrefix string, avgMsec, rampMs int) (stop func())`: nil-safe no-op for a non-array drive; names are the members' kernel names; `stop` cancels, writes the log (warn on failure) and is idempotent.

- [ ] **Step 1: tests** `TestLoadMembersLogDropsRamp`, `TestLoadMembersLogMissing` (ok false), `TestMemberSeriesInGraphData` (array phase has `ts_members`, a drive phase does not; assert on the marshalled JSON), `TestStartMemberSamplerNoopForDrive`, `TestStartMemberSamplerWritesLog` (fake sysfs via a `sysBlock` parameter on an inner helper, short interval).
- [ ] **Step 2: run, expect FAIL**; **Step 3: implement** (wire `executePhase` so the sampler starts right before fio is launched and `stop()` runs where `stopTrace()` runs, and `applyMembersLog` is called wherever `applyFlushLog` is); **Step 4: run** `go test ./cmd/bbbench ./internal/...` expect PASS
- [ ] **Step 5: commit** "Sample the members of an array during each phase and load the series into the graph data"

### Task 4: Charts

**Files:**
- Modify: `cmd/bbbench/serve_graphs.go` (`makeChart`, the per-phase Y-scale block ~line 1020), `cmd/bbbench/serve_shared.go` (a shared colour-per-member helper if it is also useful there; no backticks in it), `cmd/bbbench/serve_graphs_test.go`

**Interfaces:**
- Consumes: `d.ts_members` (Task 3), existing `rampOf`, `fioShift`, `windowOf`, `fmtTick`, `X_MAX`.
- Produces: when `d.ts_members` has members and the metric is `iops` or `bw`: one filled line dataset per member and direction with `stack: 'members'`, read positive, write negative, scale `y.stacked = true`; the array's read/write series as non-filled datasets with `stack: 'array'`; tooltip (`mode: 'index', intersect: false`) lists each member's value, then `Stack total`, then `Array`. Metric `lat`: one line per member and direction (no stack) plus the array lines. Y scale for a phase includes the stack totals per x (sum of members' read, sum of members' write). Colours: stable per member name, distinct from the Read/Write colours of the array line.

- [ ] **Step 1: tests** (page-string style, as the existing tests) `TestGraphsPageStacksMembers` (contains `stack: 'members'`, `stack: 'array'`, `Stack total`), `TestGraphsPageLatencyNotStacked` (the latency branch has no `stack:` for members), `TestGraphsPageNoMembersUnchanged` (the old Read/Write dataset code still present).
- [ ] **Step 2: run, expect FAIL**; **Step 3: implement**; **Step 4: run, expect PASS**. Then render a page in a browser or with a headless check if available and say what was and was not checked (the Chart.js behaviour of an unstacked line next to a stack under a stacked scale is UNVERIFIED until seen; if it does not work, draw the array line in a second axis-less overlay with the same scale).
- [ ] **Step 5: commit** "Stack per-member IOPS and bandwidth under the array line and draw per-member latency as lines"

### Task 5: Docs

**Files:** `README.md` (graphs section), `CHANGELOG.md`, `documentation/web-ui.md`, `CLAUDE.md` (new package, the log, `ts_members`)

- [ ] **Step 1:** state what the charts show; that the numbers are the kernel's counters on each member (they include md overhead such as parity and mirroring, so the stack total can exceed the array value); latency is the kernel's per-request time (queue plus service, 1 ms tick resolution, coarse at low load); the array line is fio's; only runs made from this version have it; not on compare.
- [ ] **Step 2:** `make check` passes. **Step 3: commit** "Document the per-member charts and the members log"

---

## Addendum tasks: md state and administrative operations

Spec: the "Addendum" section of the spec. Global constraints as above, plus: the core never names a provider (`diskstat` reads the sysfs `md` directory only when present); unreadable values are empty, never an error; an old members log (without the new lines) still loads and renders; the old reader semantics for the existing lines are unchanged.

### Task 6: Read and store the md state

**Files:** Modify `internal/diskstat/diskstat.go`, `internal/diskstat/log.go`, their tests.

**Interfaces:**
- Produces: `type MDState struct{ Action string; Completed, Total uint64; SpeedKiBps uint64; Degraded int; ArrayState string; RaidDisks int; Members map[string]string }`; `func ReadMD(sysBlock, array string, members []string) (MDState, bool)` (ok false when `<sysBlock>/<array>/md` does not exist; `sync_completed` `none` = 0/0; members read from `md/dev-<name>/state`; an unreadable file leaves that field empty/zero); `type MDWindow struct{ EndMs, SpanMs int64; S MDState }`; `Sampler` gets `func (s *Sampler) WithArray(name string, members []string) *Sampler` and records one `MDWindow` per `Sample` after the first when the array has an md directory; `func (s *Sampler) MDWindows() []MDWindow`. Log: `WriteLog(w, m Meta, ws []Window)` becomes `WriteLog(w, m Meta, ws []Window, md []MDWindow)` and `ReadLog` returns them too, with the `md,` and `mdmember,` line formats of the spec; every existing caller is updated.
- [ ] Tests: `TestReadMDParses` (fake sysfs: sync_action check, sync_completed `1000 / 4000`, sync_speed, degraded 1, array_state, per-member states), `TestReadMDNoDirIsNotOK`, `TestReadMDUnreadableFieldIsEmpty`, `TestSamplerRecordsMDWindows`, `TestLogRoundTripWithMD`, `TestReadLogOldFormatHasNoMD` (a log without the new lines), `TestReadLogRejectsGarbageMDLine`.
- [ ] Commit "Sample the md sysfs state of an array next to the member counters"

### Task 7: Load into the graph data

**Files:** Modify `cmd/bbbench/memberslog.go`, `cmd/bbbench/serve_results.go`, `cmd/bbbench/serve_graphs.go` (data only), tests.

**Interfaces:**
- Produces: `type MDPoint struct{ T float64 `json:"t"`; Action string `json:"action"`; Pct float64 `json:"pct"`; SpeedKiBps uint64 `json:"speed"`; Degraded int `json:"degraded"`; State string `json:"state"` }`, `type MDEvent struct{ T float64 `json:"t"`; Text string `json:"text"` }`, `PhaseResult.MDSeries map[string]MDInfo` (device -> `{Points []MDPoint; Events []MDEvent}`), `DiskPhaseData.TSmd []MDPoint` (`ts_md,omitempty`) and `MDEvents []MDEvent` (`md_events,omitempty`). `startMemberSampler` calls `WithArray(drive.Device.Name, member names)` and writes the md windows; `loadMembersLog` returns them with the same ramp-window drop and window-start stamping as the members; events are derived from consecutive windows (action, array_state, degraded, raid_disks, each member's state; first window event when not idle or degraded). Event text format: `sync_action idle -> check`, `degraded 0 -> 1`, `array_state clean -> active`, `sdb: in_sync -> faulty`, `raid_disks 4 -> 5`; first-window: `sync_action check (at phase start)`, `degraded 1 (at phase start)`.
- [ ] Tests: `TestMDEventsFromWindows` (each change kind, none when stable, first-window events), `TestLoadMembersLogMD`, `TestMDJSONOmittedWithoutMD`, `TestStartMemberSamplerRecordsMD`.
- [ ] Commit "Load the md state and its changes into the graph data of an array phase"

### Task 8: Show it on the charts

**Files:** Modify `cmd/bbbench/serve_graphs.go` (JS), `cmd/bbbench/serve_graphs_test.go`.

**Interfaces:** Consumes `d.ts_md`, `d.md_events`. For an array phase with md data, on IOPS, bandwidth and latency charts: a Chart.js plugin shades windows whose action is not `idle`, labelled with the action (colour distinct from the ramp-up shading); events drawn as vertical markers with their text; hover adds `md: <action> <pct>% <speed> MiB/s, degraded <n>` for the hovered window (the tooltip already uses a shared x grid, keyed with the same `memberKey`); the card header badge `resync 43% -> 61%` / `degraded` and the line "array busy: results include administrative work". No md data = no change.
- [ ] Tests (node on the page's own JS as in `TestGraphsPageMemberGrid`, plus string tests): `TestGraphsPageMDBands` (band windows, label), `TestGraphsPageMDBadge` (progress first -> last, degraded only), `TestGraphsPageNoMDUnchanged`. Check once in headless Chrome and say what was and was not checked.
- [ ] Commit "Shade md sync activity and mark administrative events on the array charts"

### Task 9: Docs

- [ ] README (graphs and the layout section), CHANGELOG, `documentation/web-ui.md`, CLAUDE.md: what the md track shows and where it comes from (sysfs `md` directory), the event kinds, that a resync during a phase means the numbers include it, that the md provider still waits for the initial resync, not verified on real hardware. `make check` passes. Commit "Document the md state track on the array charts".
