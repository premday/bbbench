package rungroup

import (
	"fmt"
	"sort"
)

// DriveRef identifies a drive for planning.
type DriveRef struct {
	Name, Serial, Model, Vendor, Firmware string
	CapacityBytes                         uint64
}

// Strategy says how values are spread over drives.
type Strategy string

const (
	Sequential   Strategy = "sequential"
	Parallel     Strategy = "parallel"
	ParallelSwap Strategy = "parallel-swap"
)

// Variant is a value and the drives (by name) it applies to.
type Variant struct {
	Value  string
	Drives []string
}

// RunPlan is one run: disjoint variants (one when sequential).
type RunPlan struct {
	Round    int
	Variants []Variant
}

type class struct {
	vendor, model string
	capacity      uint64
}

// subsets splits every class evenly, in name order, into n subsets.
func subsets(drives []DriveRef, n int) ([][]string, error) {
	if n < 1 {
		return nil, fmt.Errorf("no values")
	}
	classes := map[class][]string{}
	for _, d := range drives {
		k := class{d.Vendor, d.Model, d.CapacityBytes}
		classes[k] = append(classes[k], d.Name)
	}
	keys := make([]class, 0, len(classes))
	for k := range classes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.vendor != b.vendor {
			return a.vendor < b.vendor
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.capacity < b.capacity
	})
	out := make([][]string, n)
	for _, k := range keys {
		names := classes[k]
		sort.Strings(names)
		desc := fmt.Sprintf("%s %s (%d bytes)", k.vendor, k.model, k.capacity)
		if len(names) < n {
			return nil, fmt.Errorf("drive class %s has %d drive(s), fewer than the %d values", desc, len(names), n)
		}
		if len(names)%n != 0 {
			return nil, fmt.Errorf("drive class %s has %d drives, not a multiple of %d values", desc, len(names), n)
		}
		per := len(names) / n
		for i := 0; i < n; i++ {
			out[i] = append(out[i], names[i*per:(i+1)*per]...)
		}
	}
	for i := range out {
		sort.Strings(out[i])
	}
	return out, nil
}

// ParallelPossible reports whether the drives can be split evenly over
// nValues, class by class; when not, the string says why.
func ParallelPossible(drives []DriveRef, nValues int) (bool, string) {
	if _, err := subsets(drives, nValues); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// PlanRuns builds the runs for the values under the strategy.
func PlanRuns(drives []DriveRef, values []string, s Strategy) ([]RunPlan, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("no values")
	}
	switch s {
	case Sequential:
		names := make([]string, 0, len(drives))
		for _, d := range drives {
			names = append(names, d.Name)
		}
		sort.Strings(names)
		var runs []RunPlan
		for i, v := range values {
			runs = append(runs, RunPlan{Round: i, Variants: []Variant{{Value: v, Drives: append([]string(nil), names...)}}})
		}
		return runs, nil
	case Parallel, ParallelSwap:
		n := len(values)
		subs, err := subsets(drives, n)
		if err != nil {
			return nil, err
		}
		rounds := 1
		if s == ParallelSwap {
			rounds = n
		}
		var runs []RunPlan
		for r := 0; r < rounds; r++ {
			p := RunPlan{Round: r}
			for k, sub := range subs {
				p.Variants = append(p.Variants, Variant{Value: values[(k+r)%n], Drives: sub})
			}
			runs = append(runs, p)
		}
		return runs, nil
	}
	return nil, fmt.Errorf("unknown strategy %q", s)
}

// PlanVariants builds one run per variant, in the order given; each variant
// keeps its drive order.
func PlanVariants(vs []Variant) []RunPlan {
	runs := make([]RunPlan, 0, len(vs))
	for i, v := range vs {
		runs = append(runs, RunPlan{Round: i, Variants: []Variant{{Value: v.Value, Drives: append([]string(nil), v.Drives...)}}})
	}
	return runs
}
