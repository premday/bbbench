package rungroup

// RunStatus is the state of one run in a group.
type RunStatus string

const (
	RunPlanned     RunStatus = "planned"
	RunRunning     RunStatus = "running"
	RunDone        RunStatus = "done"
	RunNotRun      RunStatus = "not_run"
	RunInvalid     RunStatus = "invalid"
	RunInterrupted RunStatus = "interrupted"
)

// Finished says whether a run is over for good: done, invalid, or not run
// (its setting did not take before the run). --resume executes the other
// runs (planned, running, interrupted) again and offers a group only while it
// has one of them.
func (s RunStatus) Finished() bool {
	return s == RunDone || s == RunInvalid || s == RunNotRun
}

// Policy applies the failure policy to the per-drive verification results
// taken before and after a run. Any false in before means the run did not
// happen. Otherwise a false or missing entry in after makes the run invalid,
// with the per-drive validity returned. Else the run is done.
func Policy(before, after map[string]bool) (RunStatus, map[string]bool) {
	for _, ok := range before {
		if !ok {
			return RunNotRun, nil
		}
	}
	valid := make(map[string]bool, len(before))
	all := true
	for dev := range before {
		ok, present := after[dev]
		valid[dev] = present && ok
		all = all && valid[dev]
	}
	for dev, ok := range after {
		if _, seen := valid[dev]; !seen {
			valid[dev] = ok
			all = all && ok
		}
	}
	if all {
		return RunDone, nil
	}
	return RunInvalid, valid
}
