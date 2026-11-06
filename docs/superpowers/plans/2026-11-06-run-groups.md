# Run Groups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the orchestrator run the whole test plan once per value of a setting (first: disk write cache policy), applying, verifying and restoring the setting, and show the resulting run group in the web UI.

**Architecture:** A pure `internal/settings` package (how to read, apply and restore a setting, per drive interface) and a pure `internal/rungroup` package (labels, the run plan for the three strategies, group and recovery files, the failure policy). `cmd/bbbench` gets the one-run part of `runOrchestrator` extracted into `executeRun`, a `runGroup` loop around it, a TUI flow to create a group, and web pages and APIs for groups. A run's labels hang on (run, drive).

**Tech Stack:** Go 1.24, bubbletea/bubbles (TUI), the existing raw-string HTML/JS pages with Chart.js, `nvme`, `hdparm`, `sdparm` called through a `Runner`.

**Spec:** `docs/superpowers/specs/2026-11-06-run-groups-design.md`

## Global Constraints

- Only the write cache policy setting is built. No mdadm, hardware RAID, controller mode, power management, or non-interactive group definition. Settings have `Kind()` and `Scope()` so they fit later.
- Volatile settings only: never `--save`, never persistent. bbbench never writes `/sys/block/*/queue/write_cache`.
- Nothing is applied before the confirmation screen. Only selected, applicable drives are touched.
- The value is verified **before and after every run**, per drive, against the drive (kernel value recorded, informational).
- Failure policy: *before* fails on any drive -> the run is not started (`not_run`), the group continues; *after* fails on a drive -> that drive's results are `invalid`, the run is kept. A failed restore is reported prominently with the manual command.
- Every exit path except a hard kill restores the originals (normal end, error, SIGINT/SIGTERM, panic).
- Recovery file: `/var/lib/bbbench/group-recovery.json`, drives keyed by serial and model (name as a hint); restore reads the current value first and writes only if it differs; written before the first apply.
- Group file: `<output>/group_<unix>_group.json` (glob `group_*_group.json`); a run summary gains `group_id` and per-device `labels`. Runs without them are groups of one; no migration.
- An interrupted run is restarted from its first phase on `--resume`; its files are kept and marked interrupted.
- New APIs: `/api/groups`, `/api/groups/detail?id=`, `/api/groups/graphs?id=`; pages `/group?id=`, `/graphs?group=`, `/compare?group=`.
- `nvme`, `hdparm`, `sdparm` are optional dependencies in `cmd/bbbench/deps.go`; a drive whose tool is missing is "not applicable" with the reason.
- Verify JS of changed pages with `node --check` on the **real page served with a stored result** (not an error page), and run the new logic in node.
- Atomic commits per task step as given, message states intent, ending with the `Co-Authored-By` line the harness gives. **No push and no pull request**: those only happen when the user asks.
- `make check` (gofmt, vet, `go test -race`) passes before every commit.

## Review Focus

Spec-implied inputs no task's own happy path covers; each has a test in the named task.

1. A selected drive the setting cannot apply to (USB, virtio, no volatile cache, missing tool) -> excluded from the group with its reason, never benchmarked unlabelled (Task 4, Task 5).
2. SIGINT/SIGTERM in the middle of a run -> originals restored, recovery file removed, group file shows the run `interrupted` (Task 4).
3. Apply "succeeds" but the read-back still shows the old value -> run `not_run`, group continues (Task 4).
4. Restore fails on one drive -> the others are still restored, the failure is reported, the recovery file keeps only the failed drive (Task 4).
5. Device names change between the crash and the next start -> restore matches by serial and model (Task 2, Task 4).

---

### Task 1: The settings package and the write cache setting

**Files:**
- Create: `internal/settings/settings.go`, `internal/settings/writecache.go`, `internal/settings/settings_test.go`, `internal/settings/writecache_test.go`
- Modify: `cmd/bbbench/deps.go` (add `sdparm`), `cmd/bbbench/deps_test.go`

**Interfaces:**
- Produces (package `settings`):
  - `type Kind string` with `Applied = "applied"`, `Manual = "manual"`; `type Scope string` with `ScopeDrive = "drive"`.
  - `type Target struct{ Name, Path, Serial, Model, Vendor, Interface string }` (`Interface` is `diskinfo.Info.Interface`: `"NVMe (PCIe)"`, `"SATA"`, `"SAS"`, `"USB"`, `"virtio"`).
  - `type Value struct{ ID, Label string }`; `type Reading struct{ Value string \`json:"value"\`; Kernel string \`json:"kernel,omitempty"\`; At time.Time \`json:"at"\` }`.
  - `type Input string` with `InputValues = "values"` (the user ticks values over one common drive selection; drive-scope settings) and `InputVariants = "variants"` (the user lists variants one by one, each with its own disks; layout-scope settings, none exists yet).
  - `type Setting interface { Name() string; Kind() Kind; Scope() Scope; Input() Input; Values() []Value; Applicable(t Target) (bool, string); Read(ctx context.Context, t Target) (Reading, error); Apply(ctx context.Context, t Target, v Value) error; Restore(ctx context.Context, t Target, original Reading) error }`.
  - `type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)`; `type Env struct{ Run Runner; LookPath func(string) (string, error); Sys string; GetWCE func(devPath string) (bool, error) }` (`Sys` default `/sys`, `Run` default exec, `LookPath` default `exec.LookPath`, `GetWCE` default `scsi.GetWCE`).
  - `func NewWriteCache(env Env) Setting` (Name `"disk write cache policy"`, Values `enabled` and `disabled`); `func All(env Env) []Setting` (just the write cache for now).
  - `func Verify(ctx context.Context, s Setting, t Target, want Value, settle time.Duration) (Reading, bool)`: reads every 200 ms until `Reading.Value == want.ID` or `settle` elapses; returns the last reading and whether it matched.

- [ ] **Step 1: Write the failing tests** in `writecache_test.go` with a fake `Runner` (map of `"name args"` -> output) and a temp sysfs:
  - `TestWriteCache_NVMe`: `nvme get-feature /dev/nvme0 -f 6` answering `...Current value:0x000001` reads `enabled`, `0x000000` reads `disabled`; `Apply(disabled)` runs exactly `nvme set-feature /dev/nvme0 -f 6 -V 0` (and `-V 1` for enabled); the controller `/dev/nvme0` comes from `Target.Name` `nvme0n1`; `Reading.Kernel` is the content of `<Sys>/block/nvme0n1/queue/write_cache`.
  - `TestWriteCache_SATA`: `hdparm -W /dev/sdb` answering ` write-caching =  1 (on)` reads `enabled`, `0 (off)` `disabled`; Apply runs `hdparm -W0 /dev/sdb` / `-W1`.
  - `TestWriteCache_SAS`: Read uses `Env.GetWCE("/dev/sdc")`; Apply runs `sdparm --set=WCE=1 /dev/sdc` / `--set=WCE=0` (never `--save`; **check this syntax against sdparm(8) on a real host before release, it is untested here**).
  - `TestWriteCache_ScopeAndInput`: `Scope()==ScopeDrive`, `Kind()==Applied`, `Input()==InputValues`.
  - `TestWriteCache_Applicable`: not applicable, with these exact reasons: interface `USB` or `virtio` -> `"no controllable volatile write cache on this interface"`; NVMe with `LookPath("nvme")` failing -> `"nvme is not installed (nvme-cli)"`; SATA without `hdparm` -> `"hdparm is not installed"`; SAS without `sdparm` -> `"sdparm is not installed"`; applicable otherwise.
  - `TestWriteCache_RestoreWritesOnlyWhenDifferent`: `Restore` with the current value equal to the original runs no set command; different -> one set command.
  - `TestVerify`: a fake setting whose Read returns the old value twice then the new one -> `Verify` returns true; one that never changes -> false after `settle`, with the last reading.
- [ ] **Step 2: Run** `go test ./internal/settings` — expect FAIL (package missing).
- [ ] **Step 3: Implement** `settings.go` (types, `Verify`) and `writecache.go` (`writeCache` struct implementing `Setting`; dispatch on `Target.Interface` prefix `NVMe`, `SATA`, `SAS`/`SCSI`; NVMe feature value bit 0 is the cache enable; hdparm output parsed with a regexp on `write-caching\s*=\s*(\d)`).
- [ ] **Step 4: Add `sdparm`** to the `dependencies` table in `deps.go` (`{"sdparm", []string{"--version"}, false, "changing the write cache of SAS/SCSI drives in a setting test", "sdparm"}`), extend the `hdparm` and `nvme` purposes with the same mention, update `deps_test.go` counts, and make `doctor` print them (it already reads the table).
- [ ] **Step 5: Run** `go test -race ./internal/settings ./cmd/bbbench` and `make check` — expect PASS.
- [ ] **Step 6: Commit** `git commit -m "Add the settings package with the disk write cache policy setting"`.

---

### Task 2: The rungroup package: labels, run plan, files, failure policy

**Files:**
- Create: `internal/rungroup/label.go`, `plan.go`, `groupfile.go`, `recovery.go`, `policy.go` and a `_test.go` for each.

**Interfaces:**
- Consumes: `settings.Reading`.
- Produces (package `rungroup`):
  - `type Label struct{ Setting, Value string; Before, After *settings.Reading; Valid bool; Note string }` (JSON tags `setting,value,before,after,valid,note`).
  - `type DriveRef struct{ Name, Serial, Model, Vendor, Firmware string; CapacityBytes uint64 }`.
  - `type Strategy string` with `Sequential = "sequential"`, `Parallel = "parallel"`, `ParallelSwap = "parallel-swap"`.
  - `type Variant struct{ Value string; Drives []string }` (a value and the drives it applies to, by drive name); `type RunPlan struct{ Round int; Variants []Variant }` (disjoint variants; one for a sequential run, several for a parallel one). A layout-scope setting would later produce one `RunPlan` per variant the user lists; nothing to build now.
  - `func ParallelPossible(drives []DriveRef, nValues int) (bool, string)`; `func PlanRuns(drives []DriveRef, values []string, s Strategy) ([]RunPlan, error)`.
  - `type RunStatus string`: `planned running done not_run invalid interrupted`.
  - `type GroupFile struct{ ID string; Created time.Time; Host string; Plan []string; Setting string; Strategy Strategy; Values []string; Originals map[string]settings.Reading; Runs []GroupRun; Restore GroupRestore }`; `type GroupRun struct{ RunID string; Round int; Status RunStatus; Drives []GroupDrive }`; `type GroupDrive struct{ Device, Serial, Model, Value string; Label Label }`; `type GroupRestore struct{ Status string; Failed []string }` (`Status` `pending|done|failed`). `func (g *GroupFile) Save(dir string) error` (atomic: temp file then rename; name `group_<ID>_group.json`); `func LoadGroup(path string) (*GroupFile, error)`; `func LoadGroups(dir string) ([]*GroupFile, error)` (glob `group_*_group.json`, skips unreadable files).
  - `type Recovery struct{ Setting string; Drives []RecoveryDrive }`; `type RecoveryDrive struct{ Serial, Model, Name string; Original settings.Reading }`; `func (r *Recovery) Save(path string) error`; `func LoadRecovery(path string) (*Recovery, error)` (nil, nil when missing); `func (r *Recovery) Remove(path string) error`; `func (r *Recovery) Match(devices []DriveRef) (found map[string]RecoveryDrive, missing []RecoveryDrive)` (device name -> entry, matched by serial and model, not name).
  - `func Policy(before, after map[string]bool) (RunStatus, map[string]bool)` (device -> ok): any `false` in `before` -> `not_run`; else any `false` or missing in `after` -> `invalid` and the per-drive validity map; else `done`.

- [ ] **Step 1: Write the failing tests:**
  - `TestPlanRuns_Sequential`: 4 drives, values `[enabled disabled]` -> 2 runs (Rounds 0,1), each with one variant covering all 4 drives, values `enabled` then `disabled`.
  - `TestPlanRuns_Parallel`: 4 identical drives d1..d4 -> 1 run with two variants: `enabled` on d1,d2 and `disabled` on d3,d4 (drives ordered by name).
  - `TestPlanRuns_ParallelSwap`: same drives -> 2 runs; run 2 has `disabled` on d1,d2 and `enabled` on d3,d4.
  - `TestPlanRuns_SwapThreeValues`: 3 values, 6 identical drives -> 3 runs, each subset sees each value exactly once (cyclic rotation).
  - `TestParallelPossible`: 3 identical drives and 2 values -> `false` with a reason containing `"not a multiple"`; two models (2+2 drives) with 2 values -> true and each model split evenly; a class of 1 drive with 2 values -> false; different firmware inside a class does not block.
  - `TestGroupFile_RoundTrip` (Save then LoadGroup equal; file name `group_<id>_group.json`; Save leaves no `.tmp`); `TestLoadGroups_SkipsGarbage`.
  - `TestRecovery_RoundTripAndMatch`: matching by serial+model when the device names differ (drive was `sdb`, now `sdc`) -> found; a drive absent -> in `missing`; LoadRecovery of a missing file -> `nil, nil`.
  - `TestPolicy`: table: all ok -> done; before false on one drive -> not_run; after false on one -> invalid with only that drive false; after map missing a drive -> that drive invalid.
- [ ] **Step 2: Run** `go test ./internal/rungroup` — expect FAIL.
- [ ] **Step 3: Implement** the files. Drives are classed by (Vendor, Model, CapacityBytes); parallel splits each class evenly in name order; swap rounds are the K cyclic rotations of the subset-to-value assignment.
- [ ] **Step 4: Run** `go test -race ./internal/rungroup` and `make check` — expect PASS.
- [ ] **Step 5: Commit** `git commit -m "Add the rungroup package: run plans, group and recovery files, failure policy"`.

---

### Task 3: Extract `executeRun` and carry labels through a run

**Files:**
- Modify: `cmd/bbbench/orchestrator.go` (extraction), `cmd/bbbench/serve_results.go` (`BenchmarkResult`), `cmd/bbbench/serve_graphs.go` (`DiskPhaseData`), `cmd/bbbench/orchestrator_executor.go` (coordinator `labels`).
- Test: `cmd/bbbench/orchestrator_run_test.go`, `cmd/bbbench/serve_graphs_test.go`.

**Interfaces:**
- Consumes: `rungroup.Label`.
- Produces:
  - `BenchmarkResult` gains `GroupID string \`json:"group_id,omitempty"\`` and `Labels map[string]rungroup.Label \`json:"labels,omitempty"\`` (device -> label).
  - `DiskPhaseData` gains `Setting string \`json:"setting,omitempty"\``, `SettingValue string \`json:"setting_value,omitempty"\``, `RunID string \`json:"run_id,omitempty"\``, `Invalid bool \`json:"invalid,omitempty"\``; `generateDiskPhaseData` fills them from `result.Labels[disk]` and `result.ID`.
  - `type runOptions struct{ Mode, OutDir string; Resume *ExecutionState; DryRun, Verbose, FlushTrace, SkipWriteThrough bool; LogAvgMsec int }`, `type runSpec struct{ Drives []DriveInfo; Labels map[string]rungroup.Label; GroupID string }`, and `func executeRun(ctx context.Context, opts runOptions, spec runSpec) (*BenchmarkResult, error)`: everything from `newExecutionCoordinator` to `saveBenchmarkSummary` (web server start excluded), returning the saved result; the run's result gets `GroupID` and `Labels` from the spec before it is saved.

- [ ] **Step 1: Write the failing tests:** `TestBenchmarkResult_LabelsRoundTrip` (JSON round trip keeps `group_id` and labels; a summary without them loads with an empty `GroupID` and nil labels); `TestGenerateDiskPhaseData_Labels` (a result with a label for `sdb` gives entries for `sdb` with `Setting`, `SettingValue`, `RunID`, and `Invalid` true when `Label.Valid` is false and the label has a value; an unlabelled drive has none of them).
- [ ] **Step 2: Run** `go test ./cmd/bbbench -run 'Labels'` — expect FAIL.
- [ ] **Step 3: Implement** the fields and the `generateDiskPhaseData` change; **then the extraction** in `orchestrator.go`: move the code between building the coordinator and saving the summary into `executeRun`, keep the web server start, signal handling and "stay running" wait in `runOrchestrator`. Behaviour of a plain run must not change.
- [ ] **Step 4: Run** `go test -race ./cmd/bbbench` — all existing orchestrator and serve tests still pass — and `make check`.
- [ ] **Step 5: Commit** `git commit -m "Extract executeRun from runOrchestrator and carry setting labels in run results"`.

---

### Task 4: The group loop: apply, verify, run, verify, restore

**Files:**
- Create: `cmd/bbbench/group_run.go`, `cmd/bbbench/group_run_test.go`
- Modify: `cmd/bbbench/serve_status.go` (status fields), `cmd/bbbench/serve_status_test.go`

**Interfaces:**
- Consumes: `settings.Setting`, `settings.Verify`, `rungroup.*`, `executeRun`'s `runSpec`/`*BenchmarkResult`.
- Produces:
  - `type groupConfig struct{ Setting settings.Setting; Values []settings.Value; Strategy rungroup.Strategy; Drives []DriveInfo; Targets map[string]settings.Target; OutDir, RecoveryPath, Host string; Settle time.Duration; Resume *rungroup.GroupFile; Execute func(ctx context.Context, spec runSpec) (*BenchmarkResult, error) }` (`Targets` keyed by device name; `Execute` is a closure over `executeRun` and `runOptions` in production; `Resume` is a loaded group file: runs already `done` or `invalid` are skipped, every other run, including `interrupted` and `not_run`, is executed again from its first phase, after a fresh apply and verify).
  - `func runGroup(ctx context.Context, cfg groupConfig) (*rungroup.GroupFile, error)`: writes the recovery file, then per `rungroup.PlanRuns` run: apply each variant's value to that variant's drives -> `settings.Verify` (before) -> `rungroup.Policy` -> `Execute` (labels carry value and the before reading) -> verify (after) -> update labels and the group file (`Save` after every run); at the end, and from a deferred function on error, panic and when `ctx` is cancelled, restore every drive with `Setting.Restore` on a background context, verify the restore, set `GroupFile.Restore`, remove the recovery file when nothing failed. If the recovery file or the group file cannot be written, return before applying anything.
  - `BenchmarkStatus` gains `Group *GroupStatus` (`ID, Setting string; Strategy rungroup.Strategy; Run, Runs int; Step string` with `Step` one of `applying`, `verifying-before`, `running`, `verifying-after`, `restoring`, `done`); `DriveStatus` gains `Setting *DriveSetting` (`Value string; Before, After *settings.Reading; Valid *bool`).
  - `func restoreFromRecovery(ctx context.Context, s settings.Setting, rec *rungroup.Recovery, devices []DriveInfo, targets map[string]settings.Target) (failed []string)`: used at startup when a recovery file exists.

- [ ] **Step 1: Write the failing tests** with a fake `settings.Setting` (records calls, per-drive scripted read-backs) and a fake `Execute` (returns a result whose labels are checked):
  - `TestRunGroup_Sequential`: 2 drives, 2 values -> 2 runs; Execute called twice with the right labels (value per drive, `Before` reading set); both runs `done`; the group file on disk lists both; originals restored (last calls are `Restore`); recovery file removed.
  - `TestRunGroup_Parallel`: 4 drives -> Execute once with two drives per value.
  - `TestRunGroup_BeforeFails` (Review Focus 3): the fake's read-back keeps the old value for `sdb` in run 1 -> run 1 `not_run`, Execute not called for it, run 2 still runs.
  - `TestRunGroup_AfterFails`: after the run `sdb` reads the old value -> run `invalid`, only `sdb`'s label has `Valid=false`.
  - `TestRunGroup_InterruptRestores` (Review Focus 2): cancel `ctx` from inside Execute -> run `interrupted`, originals restored, recovery file removed, group file saved.
  - `TestRunGroup_RestoreFailureOnOneDrive` (Review Focus 4): the fake's Restore fails for `sdb` -> `sdc` still restored, `GroupFile.Restore.Status=="failed"` with `Failed==["sdb"]`, recovery file kept and listing only `sdb`.
  - `TestRunGroup_WritesRecoveryBeforeApply`: the first `Apply` call finds the recovery file already on disk with the originals.
  - `TestRunGroup_UnwritableOutDirFailsBeforeApply`: no `Apply` call happens.
  - `TestRunGroup_ResumeSkipsFinishedRuns`: a `Resume` group file with run 1 `done` and run 2 `interrupted` -> Execute is called only for run 2, the value is applied and verified again first, and run 1's entry in the saved file is unchanged.
  - `func driveRefs(drives []DriveInfo, infos map[string]diskinfo.Info) []rungroup.DriveRef` (Firmware from `Info.Controller.Firmware`, empty when unknown) is covered by `TestDriveRefs`.
  - `TestRestoreFromRecovery_MatchesBySerial` (Review Focus 5): the recovery entry says `sdb`, the drive now appears as `sdc` with the same serial and model -> `Restore` is called on `sdc`; an entry with no matching drive is returned in `failed`.
  - `TestStatus_GroupFields`: during a run the status singleton's `Group.Step` and `Drives[i].Setting` reflect the loop (use a fake Execute that reads `GetStatus()`).
- [ ] **Step 2: Run** `go test ./cmd/bbbench -run 'RunGroup|Recovery|Status_Group'` — expect FAIL.
- [ ] **Step 3: Implement** `group_run.go` and the status fields (update the status through the existing `UpdateStatus` and `markDrive`; copy-on-write for any map or pointer shared with snapshots).
- [ ] **Step 4: Run** `go test -race ./cmd/bbbench` and `make check` — expect PASS.
- [ ] **Step 5: Commit** `git commit -m "Add the run group loop with verification, failure policy and guaranteed restore"`.

---

### Task 5: The TUI flow and the orchestrator wiring

**Files:**
- Create: `cmd/bbbench/group_tui.go`, `cmd/bbbench/group_tui_test.go`
- Modify: `cmd/bbbench/orchestrator.go` (the mode question before drive selection; the setup flow after it; startup recovery check), `cmd/bbbench/help.go`, `README.md`

**Interfaces:**
- Consumes: `settings.All`, `settings.Setting` (`Input()`), `rungroup.ParallelPossible`, `diskinfo.Collect` (for `Target.Interface`).
- Produces:
  - `type modeChoice struct{ Setting settings.Setting }` (`Setting == nil` means *single run*) and `func runModeTUI(available []settings.Setting, drives []DriveInfo, targets map[string]settings.Target) (modeChoice, error)`: asks *single run* or *setting test*, then lists the settings whose `Input()` is `InputValues` (the only form built) with the number of **discovered** drives each applies to and, for the others, the reason. It runs **before** drive selection.
  - `type groupChoice struct{ Setting settings.Setting; Values []settings.Value; Strategy rungroup.Strategy; Drives []DriveInfo; Excluded map[string]string }` (`Excluded`: device -> reason) and `func runGroupSetupTUI(setting settings.Setting, selected []DriveInfo, targets map[string]settings.Target, infos map[string]diskinfo.Info) (*groupChoice, error)`: after drive selection, steps `values -> strategy -> confirm`.
  - `func infoFor(drives []DriveInfo) map[string]diskinfo.Info` (one `diskinfo.Collect(name, diskinfo.Options{})` per drive, in parallel) and `func targetsFor(drives []DriveInfo, infos map[string]diskinfo.Info) map[string]settings.Target` (`Interface` from the info).

- [ ] **Step 1: Write the failing tests** driving the models' `Update` with key messages (as `orchestrator_tui_test.go` does):
  - `TestMode_SingleRunReturnsNilSetting`; `TestMode_ListsSettingsWithDriveCounts` (the write cache entry shows the count of discovered drives it applies to; a setting whose `Input()` is `InputVariants` is not listed).
  - `TestSetup_ValuesBothTickedByDefault` (write cache enabled and disabled start ticked); `TestSetup_NeedsTwoValues` (unticking one value blocks Enter with a message).
  - `TestSetup_ExcludedDrivesListedWithReason` (Review Focus 1): a USB drive among the selection -> not in `Drives`, in `Excluded` with the reason, and shown on the confirmation view; if no selected drive is applicable the flow stops with that explanation.
  - `TestSetup_StrategyOptions`: sequential always offered and default; parallel offered only when `ParallelPossible`; `parallel-swap` offered only together with parallel and never preselected; when not possible the view shows the reason.
  - `TestSetup_ConfirmShowsRunCountAndEstimate`: the confirmation view states the number of runs for the chosen strategy (2 for sequential with two values, 1 for parallel, 2 for parallel-swap) and an estimated total time (runs x the plan's known phase lengths, with a note when some phases have an unknown length); `TestSetup_FirmwareMismatchWarns`: identical-model drives with different firmware strings get a warning on the strategy and confirm screens, and parallel stays offered.
  - `TestSetup_NothingAppliedBeforeConfirm`: the models never call the setting (fake setting counts calls) until the final confirm key.
- [ ] **Step 2: Run** `go test ./cmd/bbbench -run 'Setup|Mode'` — expect FAIL.
- [ ] **Step 3: Implement** `group_tui.go`. In `runOrchestrator`: after discovery and filtering, call `runModeTUI`; `Setting == nil` -> today's single-run path unchanged (drive selection, each drive gets the plan for its type, everything at once); otherwise run the drive selection TUI, then `runGroupSetupTUI`, then build `groupConfig` (recovery path `/var/lib/bbbench/group-recovery.json`, `Settle` 10 s, `Execute` closing over `executeRun`) and call `runGroup`, then print a per-run summary table. At startup, before the TUI, if `rungroup.LoadRecovery` finds a file, offer (y/n) to restore it using `restoreFromRecovery` and report failures. With `--resume`, if the output directory holds a group file with runs that are not finished, restore first (as above), then continue that group via `groupConfig.Resume` with the same setting, values, strategy and drives (matched by serial and model), skipping both TUIs.
- [ ] **Step 4: Run** `go test -race ./cmd/bbbench` and `make check` — expect PASS.
- [ ] **Step 5: README and help**: document *single run / setting test*, what each asks for (drive-scope settings: drives and values; a layout setting would ask for variants with their disks, later), the strategies (parallel and swap, not default), the recovery file and that bbbench now changes drive settings only in setting-test mode after confirmation.
- [ ] **Step 6: Commit** `git commit -m "Offer single run or setting test in the orchestrator and run the group"`.

---

### Task 6: Groups in the web store: loading, APIs and the group page

**Files:**
- Create: `cmd/bbbench/serve_groups.go`, `cmd/bbbench/serve_groups_test.go`
- Modify: `cmd/bbbench/serve.go` (routes, load groups next to summaries), `cmd/bbbench/serve_results.go` (store), `documentation/web-ui.md`

**Interfaces:**
- Consumes: `rungroup.GroupFile`, `rungroup.LoadGroups`, `globalResultsStore`, `generateDiskPhaseData`.
- Produces:
  - `type GroupStore` with `AddGroup(*rungroup.GroupFile)`, `GetGroup(id string) (*rungroup.GroupFile, bool)`, `ListGroups() []*rungroup.GroupFile` (newest first), `Clear()`; `var globalGroupStore`.
  - `func loadGroups(outputDir string) error` (called where summaries are loaded; also registered live by `runGroup` after each group-file save).
  - Handlers: `handleGroupsList` (`/api/groups`: id, created, setting, strategy, values, runs count, per-status counts), `handleGroupDetail` (`/api/groups/detail?id=`: the group file plus each run's summary headline), `handleGroupGraphs` (`/api/groups/graphs?id=`: the union of `generateDiskPhaseData` of the member runs; entries of `not_run` runs are absent, `invalid` ones carry `invalid:true`), `handleGroupPage` (`/group?id=`).
  - The group page: header (setting, values, strategy, host, plan), a table run x drive (value, before and after readings with the kernel value, valid or invalid, link to `/result?id=`), the kernel's `write_cache` per run with a note when the run's phases carry a write-through warning (so missing Flush data is explained), the restore result, and a matrix of the main metrics per phase and value (mean, min-max, n over valid results; a value with no valid result shown as incomplete).

- [ ] **Step 1: Write the failing tests:** `TestGroupStore` (add/get/list order); `TestHandleGroupsList` and `TestHandleGroupDetail` (JSON fields, including per run `kernel` and `flush_note` true when any phase of the run has a warning; 404 for an unknown id; 400 without id); `TestHandleGroupGraphs_UnionAndInvalid` (two member runs with labels -> entries from both with `setting_value`, `run_id`; an `invalid` drive flagged; a `not_run` run contributes nothing); `TestGroupMatrix` (the aggregation function `groupMatrix(entries []DiskPhaseData) []MatrixRow`: mean/min/max/n per (phase, value) over valid entries only; a value with only invalid entries gives `n=0` and is marked incomplete).
- [ ] **Step 2: Run** `go test ./cmd/bbbench -run 'Group'` — expect FAIL.
- [ ] **Step 3: Implement** the store, loader, handlers and the page (raw-string HTML like the existing pages; use the shared nav; escape every inserted string).
- [ ] **Step 4: Verify the page:** dump `/group?id=` for a stored group (not an error page), `node --check` its script, and run the matrix/readings rendering function in node on sample data.
- [ ] **Step 5: Docs:** `web-ui.md` gets the group page description.
- [ ] **Step 6: Run** `make check`; **Commit** `git commit -m "Load run groups and serve the group page and APIs"`.

---

### Task 7: Status tab: live group state

**Files:**
- Modify: `cmd/bbbench/serve_status.go` (page JS and CSS), `cmd/bbbench/serve_status_test.go`, `documentation/web-ui.md`

**Interfaces:**
- Consumes: `BenchmarkStatus.Group`, `DriveStatus.Setting` from Task 4.

- [ ] **Step 1: Write the failing test** `TestStatusAPI_CarriesGroup`: with `Group` and a drive `Setting` set through `UpdateStatus`/`markDrive`, `/api/status` JSON contains `group.step`, `group.run`, `group.runs` and the drive's `setting.value`, `setting.before`, `setting.valid`.
- [ ] **Step 2: Run** it — expect FAIL until the snapshot copies the new fields (copy `Group` by value and the drive pointer fields as the snapshot already does for `Config`).
- [ ] **Step 3: Implement the page:** a banner above the drives (`Setting test: <setting>, run <i> of <n>, <step>` plus the strategy); on each drive card the value, and before/after badges (`pending`, `ok`, `failed` with the reading in a tooltip), `restoring` and the restore result at the end; the overall ETA multiplies by the remaining runs (use the existing per-run ETA and the number of runs left).
- [ ] **Step 4: Verify** with the dumped page: `node --check`, and run the badge and banner functions in node on a failed-before and an after-ok sample, including HTML escaping.
- [ ] **Step 5: Run** `make check`; docs line in `web-ui.md`; **Commit** `git commit -m "Show the run group and per-drive setting verification on the status tab"`.

---

### Task 8: Graphs, compare, results list and run details by setting

**Files:**
- Modify: `cmd/bbbench/serve_graphs.go`, `cmd/bbbench/serve_compare.go`, `cmd/bbbench/serve_shared.go` (shared helpers), `cmd/bbbench/serve_browse.go`, `cmd/bbbench/serve_groups.go`, tests, `documentation/web-ui.md`

**Interfaces:**
- Consumes: `DiskPhaseData.Setting/SettingValue/RunID/Invalid` (Task 3), `/api/groups/graphs` (Task 6).
- Produces: `/graphs?group=<id>` and `/compare?group=<id>` read `/api/groups/graphs`; a sidebar filter dimension `setting` (the value labels), a "hide invalid results" toggle on (hidden) by default, series named `disk · value · round N` when a group has several runs of one value; the results list shows a run's group as a link with its value, and groups as expandable rows; the run details page shows the group and each drive's value and verification readings.

- [ ] **Step 1: Write the failing tests:** `TestGraphsPage_GroupParam` (the page for `?group=` is served for a stored group and contains the data URL `/api/groups/graphs?id=`; 404 for an unknown group); `TestBrowseList_Groups` (`/api/results` entries carry `group_id`; the group API lists the group); `TestResultDetail_ShowsLabels` (the detail JSON includes the labels).
- [ ] **Step 2: Run** `go test ./cmd/bbbench -run 'GroupParam|BrowseList_Groups|ShowsLabels'` — expect FAIL.
- [ ] **Step 3: Implement:** server side routes and the data URL switch; JS: add the `setting` entry to `DATA_DIMS`/`DIM_F` only when entries carry `setting_value`, the invalid toggle (filters `d.invalid`), series labels in `seriesLabel`, the results list grouping and the details block (escape all text).
- [ ] **Step 4: Verify** the dumped graphs, compare, results and result-details pages for a stored group: `node --check` each, and run in node: the dimension appears only for group data, invalid entries are filtered by default and shown with the toggle, `seriesLabel` adds value and round.
- [ ] **Step 5: Run** `make check`; **Commit** `git commit -m "Select and overlay runs by setting value in graphs, compare, the results list and run details"`.

---

### Task 9: Docs, the safety rule and the changelog

**Files:**
- Modify: `README.md`, `CLAUDE.md`, `CHANGELOG.md`, `documentation/flush-latency.md`, `documentation/web-ui.md`, `cmd/bbbench/orchestrator_skip.go` (warning text)

- [ ] **Step 1: Find every statement that bbbench never changes drive configuration** (`grep -rn "never change\|never configures\|only reads" README.md CLAUDE.md documentation cmd internal`) and reword each: it changes a drive's setting only in setting-test mode, after an explicit confirmation, and restores it; the test in `orchestrator_fsync_test.go` that asserts the warning text must be updated with the new wording.
- [ ] **Step 2: CLAUDE.md:** describe `internal/settings` (add a setting = implement `Setting`, register in `All`), `internal/rungroup`, `executeRun`, `runGroup`, the group and recovery file formats and paths, and that the first version has only the write cache setting with room for mdadm and controller-mode settings (scope, kind, targets).
- [ ] **Step 3: CHANGELOG** entry under Added; README "Output files" table gains `group_<unix>_group.json` and the recovery file location.
- [ ] **Step 4: Run** `make check` and the docs audit used before (every flag in README exists in code, every endpoint listed, every file name in the tables matches the code) — no drift.
- [ ] **Step 5: Commit** `git commit -m "Document setting tests and the new rule for changing drive settings"`.
