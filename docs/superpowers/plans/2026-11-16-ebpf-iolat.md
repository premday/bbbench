# eBPF I/O latency collector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collect the I/O latency series with an in-kernel eBPF aggregation (ftrace kept as fallback), log the mode used, and write each run's output to a `.log` file.

**Architecture:** `iotrace.Start` picks a backend (`ebpf` or `ftrace`) behind one interface; the ftrace backend is today's code. The eBPF backend loads a committed CO-RE object, reads per-CPU log histograms once per window and builds the same `Window`/`Summary` types.

**Tech Stack:** Go 1.24 (pure Go), `github.com/cilium/ebpf`, clang (only for `go generate`), CO-RE/BTF.

**Spec:** `docs/superpowers/specs/2026-11-16-ebpf-iolat-design.md`

## Global Constraints

- `CGO_ENABLED=0` static builds on amd64, arm64, 386; the eBPF backend is built for amd64 and arm64, `386` reports "ebpf not supported on this architecture" and uses ftrace.
- The BPF object is committed; `make check` must pass without clang (`CI=true make check`).
- Gate: `CI=true make check`. Atomic commits stating intent, trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. No PR unless asked; do not push.
- No backticks in the page JS (Go raw strings). Result types (`Summary`, `Window`, `Hist`) and the `_iolat.1.log` format stay readable by old logs.
- bbbench never changes a drive; the eBPF programs only read (tracepoints), nothing is attached after `Close`.

## Review Focus

- Fallback: a kernel without BTF, a verifier failure, no root -> ftrace runs and `mode_note` says why (test with a failing loader).
- Windows: a histogram read racing the window boundary neither drops nor double counts samples (test with a fake reader that changes between reads).
- Start-map full / unmatched completion are counted in `Lost`, never silently.
- Run log: still complete when the run is interrupted (Ctrl+C) and when stdout is not a terminal.
- Old logs without `mode=` read as `ftrace`.

---

### Task 1: Run log file

**Files:**
- Create: `cmd/bbbench/runlog.go`, `cmd/bbbench/runlog_test.go`
- Modify: `cmd/bbbench/orchestrator.go` (`executeRun`: start it after `checkEnvironment`, stop via defer)

**Interfaces:**
- Produces: `startRunLog(outDir, runID string, header string) (stop func(), path string, err error)`; `runLogPath(outDir, runID string) string` = `<outDir>/run_<runID>.log`.

- [ ] **Step 1: Write the failing test** `TestRunLogTeesStdoutStderrAndSlog`: call `startRunLog` on a temp dir with header `"bbbench test"`, print to `os.Stdout`, `os.Stderr` and `slog.Info("hello")`, call `stop`, assert the file starts with the header line and contains all three texts, and that the terminal copies were also written (capture through a pipe replacing `os.Stdout`/`os.Stderr` before the call).
- [ ] **Step 2: Run it, expect FAIL** (`undefined: startRunLog`).
- [ ] **Step 3: Implement** `startRunLog`: replace `os.Stdout`/`os.Stderr` with pipes whose reader goroutines copy to the saved original and to the file (mutex-protected, flushed per write), point `logger` and the slog default at the new stderr, return a `stop` that restores everything, closes the pipes, waits for the goroutines and closes the file (idempotent). Header is the first line, prefixed `# `.
- [ ] **Step 4: Add `TestRunLogCompleteOnInterrupt`:** `stop` called after a cancelled context loses no line written before it.
- [ ] **Step 5: Wire** in `executeRun` (not for `DryRun`); the header carries `buildinfo` version, `os.Args` and, once Task 3 lands, the I/O trace mode request. Run `go test ./cmd/bbbench -run RunLog`, expect PASS.
- [ ] **Step 6: Commit** "Write everything a run prints to run_<id>.log so a run can be read afterwards".

### Task 2: Mode type, flag and log fields

**Files:**
- Create: `internal/iotrace/mode.go`, `internal/iotrace/mode_test.go`
- Modify: `internal/iotrace/session.go` (`Options.Mode`, `Summary.Mode`, `Summary.ModeNote`), `internal/iotrace/log.go` (header `mode=`, `mode_note=`), `cmd/bbbench/iolatlog.go` and the flag parsing (`--io-trace=auto|ebpf|ftrace`), `cmd/bbbench/orchestrator_results.go` (`formatIOTraceSummary` prints `[io-trace mode: ...]` once per phase), `cmd/bbbench/serve_graphs.go` (source line).

**Interfaces:**
- Produces: `type Mode string` with `ModeAuto`, `ModeEBPF`, `ModeFtrace`; `ParseMode(string) (Mode, error)`; `Summary.Mode Mode` (never `auto`: the one used), `Summary.ModeNote string` (why a requested mode was not used).

- [ ] **Step 1: Tests:** `TestParseMode` (valid values, error text lists the three), `TestLogModeRoundTrip` (`mode=ebpf mode_note=...` survives `WriteLog`/`ReadLog`, URL-escaped), `TestOldLogModeIsFtrace` (log without `mode=` reads `ModeFtrace`).
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement** the type, the header fields, the flag (default `auto`, rejected values reported before the root check like `--trace-buffer-mb`), the terminal line and the graphs source text (`Summary.Mode` shown next to the source).
- [ ] **Step 4: `CI=true make check`, commit** "Record which I/O trace collector produced a result (mode, and why a fallback happened)".

### Task 3: Backend interface around today's ftrace session

**Files:**
- Modify: `internal/iotrace/session.go`; Create: `internal/iotrace/backend.go`

**Interfaces:**
- Produces: `type backend interface { start(devs []*device, o Options) error; markStart(); stop() (*Result, error); close() }`; `ftraceBackend` = the current `Session` internals; `Start` selects: `ModeFtrace` -> ftrace; `ModeEBPF` -> ebpf or error; `ModeAuto` -> ebpf if `ebpfAvailable()` and it starts, else ftrace with `ModeNote`.

- [ ] **Step 1:** all existing `internal/iotrace` tests must pass unchanged before and after (run them first as the baseline).
- [ ] **Step 2: Write `TestAutoFallsBackToFtrace`** with an injectable `newEBPF` that fails: expect `Summary.Mode == ModeFtrace` and `ModeNote` containing the failure text; `TestForcedEBPFFailureIsError`.
- [ ] **Step 3: Implement** the interface, move the current code behind `ftraceBackend` without behaviour change, add the selection and the note.
- [ ] **Step 4: `CI=true make check`, commit** "Put the I/O trace session behind a backend interface so a collector can be chosen".

### Task 4: BPF program and loader

**Files:**
- Create: `internal/iotrace/ebpf/iolat.bpf.c`, `internal/iotrace/ebpf/gen.go` (`//go:generate`), `internal/iotrace/ebpf/iolat_bpfel.o` (committed), `internal/iotrace/ebpf/load_linux.go`, `load_other.go` (stub), `load_test.go`
- Modify: `go.mod` (`github.com/cilium/ebpf`)

**Interfaces:**
- Produces: `type Program interface { SetDevices([]DevSpec) error; Read() (Snapshot, error); Close() error }`, `Open() (Program, error)`; `DevSpec{Dev uint32; Kind Kind; Role uint8}`; `Snapshot{Hists map[Key][]uint64 /* per-bucket counts */; Min, Max map[Key]uint64; Sum map[Key]uint64; Started, Completed, Lost uint64}`; `Key{Dev uint32; Kind Kind; Dir byte}`. Bucket layout is exactly `iotrace.Hist`'s (`hist.go`): the C file and `hist.go` share the constants through a test that compares the generated bucket bounds.

- [ ] **Step 1: Write `TestBucketLayoutMatchesHist`** (bucket index function in Go from the C constants equals `Hist.index` for 1 ns .. 10 s samples).
- [ ] **Step 2: Write the C program** (`tp_btf` on `block_rq_insert`, `block_rq_issue`, `block_rq_complete`, `nvme_setup_cmd`, `nvme_complete_rq`, `block_bio_queue`, `block_bio_complete`): start-time hash keyed by request/bio pointer; completion looks up, computes the latency, updates a per-CPU array histogram selected by (dev -> slot via a devices map, kind, direction from `rwbs`/opcode), counters for started, completed, start-map full, no-start. Only devices in the devices map are traced.
- [ ] **Step 3: `go generate` with clang, commit the object;** `Open` loads it with `ebpf.LoadCollectionSpec` + `NewCollection`, attaches the programs it can (missing tracepoints are reported in the error text, `block_rq_insert` missing is allowed), returns `Program`. On non-linux or `386`: `Open` returns an error "ebpf not supported on this platform".
- [ ] **Step 4: Tests without a kernel:** loader error mapping (no BTF, permission), `Snapshot` decoding from byte slices built by hand. A real-load test is behind `BBBENCH_EBPF_TEST=1` and root, skipped otherwise.
- [ ] **Step 5: Freshness test:** `TestObjectNotOlderThanSource` compares the recorded hash of the C file in `gen.go` with the file; `make check` runs it. Commit "Add the eBPF program that times requests in the kernel".

### Task 5: eBPF backend (windows, summary)

**Files:**
- Create: `internal/iotrace/ebpf_backend.go`, `internal/iotrace/ebpf_backend_test.go`

**Interfaces:**
- Consumes: Task 3's `backend`, Task 4's `Program`/`Snapshot`.
- Produces: `ebpfBackend` that, every `windowMs` (aligned to the start marker, monotonic clock), reads the snapshot, subtracts the previous one, builds `Window` entries per (device, kind, direction) with count/mean/min/max/p50/p99 from the histogram, and at `stop` the per-phase `Summary` after `OffsetMs` as the sum of windows; `Result.Lost` = start-map full + no-start; `Overrun` 0; per-device `Unpaired` = no-start.

- [ ] **Step 1: Tests with a fake `Program`:** (a) two windows with known histograms give the expected counts and percentiles within 2 % of an exact computation; (b) a snapshot read twice with the counters growing between reads counts every sample once; (c) `Lost` and `Unpaired` mapping; (d) devices that are not NVMe get `SourceBlock`/bio kinds as in the ftrace result (compare against a ftrace `Result` built from the same synthetic events through `tracefake`, equal counts and means).
- [ ] **Step 2: Implement.** Reuse `Hist` and `Window` from the package; no per-I/O storage.
- [ ] **Step 3: `CI=true make check`, commit** "Build I/O latency windows from the in-kernel histograms".

### Task 6: Selection, logging of what was attached, docs

**Files:**
- Modify: `internal/iotrace/log.go` (`# ebpf attached=<programs> started= completed= lost=`), `cmd/bbbench/serve_graphs.go` (the loss warning for eBPF names the lost counters; no ring-buffer text), README.md, CHANGELOG.md, CLAUDE.md, `--help`.

- [ ] **Step 1: Test** the `# ebpf` header round trip and that `Fill` produces a `KernelNote` when `Started != Completed + inflight` by more than the lost count.
- [ ] **Step 2: Implement and document:** the flag, the mode logging, the run log file, the fallback, the amd64/arm64 limit, "not verified on a real kernel until a run says so".
- [ ] **Step 3: `CI=true make check`, commit** "Document the eBPF I/O trace mode and log what the kernel attached".

### Task 7: Real-server verification (the user, not an agent)

- [ ] Run one high-IOPS phase with `--io-trace=ebpf` and one with `--io-trace=ftrace`; send `run_<id>.log` and the `_iolat.1.log`. Expected: `mode=ebpf`, no ring-buffer overrun, counts near fio's IOPS x duration, bbbench threads near idle in `top -H`. If the program fails to load, the run log has the verifier or BTF message and the run continues on ftrace.
