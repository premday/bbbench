package rungroup

import (
	"reflect"
	"strings"
	"testing"
)

func drives(names ...string) []DriveRef {
	var d []DriveRef
	for _, n := range names {
		d = append(d, DriveRef{Name: n, Model: "M", Vendor: "V", CapacityBytes: 100, Serial: "S" + n})
	}
	return d
}

func TestPlanRuns_Sequential(t *testing.T) {
	runs, err := PlanRuns(drives("d1", "d2", "d3", "d4"), []string{"enabled", "disabled"}, Sequential)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	for i, want := range []string{"enabled", "disabled"} {
		r := runs[i]
		if r.Round != i || len(r.Variants) != 1 || r.Variants[0].Value != want ||
			!reflect.DeepEqual(r.Variants[0].Drives, []string{"d1", "d2", "d3", "d4"}) {
			t.Errorf("run %d = %+v", i, r)
		}
	}
}

func TestPlanRuns_Parallel(t *testing.T) {
	runs, err := PlanRuns(drives("d4", "d2", "d3", "d1"), []string{"enabled", "disabled"}, Parallel)
	if err != nil {
		t.Fatal(err)
	}
	want := []RunPlan{{Round: 0, Variants: []Variant{
		{Value: "enabled", Drives: []string{"d1", "d2"}},
		{Value: "disabled", Drives: []string{"d3", "d4"}},
	}}}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("got %+v", runs)
	}
}

func TestPlanRuns_ParallelSwap(t *testing.T) {
	runs, err := PlanRuns(drives("d1", "d2", "d3", "d4"), []string{"enabled", "disabled"}, ParallelSwap)
	if err != nil {
		t.Fatal(err)
	}
	want := []RunPlan{
		{Round: 0, Variants: []Variant{
			{Value: "enabled", Drives: []string{"d1", "d2"}},
			{Value: "disabled", Drives: []string{"d3", "d4"}}}},
		{Round: 1, Variants: []Variant{
			{Value: "disabled", Drives: []string{"d1", "d2"}},
			{Value: "enabled", Drives: []string{"d3", "d4"}}}},
	}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("got %+v", runs)
	}
}

func TestPlanRuns_SwapThreeValues(t *testing.T) {
	vals := []string{"a", "b", "c"}
	runs, err := PlanRuns(drives("d1", "d2", "d3", "d4", "d5", "d6"), vals, ParallelSwap)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %d", len(runs))
	}
	seen := map[string]map[string]bool{}
	for _, r := range runs {
		if len(r.Variants) != 3 {
			t.Fatalf("variants = %d", len(r.Variants))
		}
		for _, v := range r.Variants {
			if len(v.Drives) != 2 {
				t.Errorf("variant size %d", len(v.Drives))
			}
			k := strings.Join(v.Drives, ",")
			if seen[k] == nil {
				seen[k] = map[string]bool{}
			}
			if seen[k][v.Value] {
				t.Errorf("subset %s sees %s twice", k, v.Value)
			}
			seen[k][v.Value] = true
		}
	}
	if len(seen) != 3 {
		t.Errorf("subsets = %d", len(seen))
	}
}

func TestParallelPossible(t *testing.T) {
	ok, why := ParallelPossible(drives("d1", "d2", "d3"), 2)
	if ok || !strings.Contains(why, "not a multiple") {
		t.Errorf("3 drives: %v %q", ok, why)
	}
	two := append(drives("a1", "a2"), drives("b1", "b2")...)
	two[2].Model, two[3].Model = "N", "N"
	if ok, why := ParallelPossible(two, 2); !ok {
		t.Errorf("two models: %q", why)
	}
	runs, err := PlanRuns(two, []string{"x", "y"}, Parallel)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range runs[0].Variants {
		if len(v.Drives) != 2 {
			t.Errorf("variant %+v", v)
		}
		// each model split evenly: one drive of each model per variant
		if v.Drives[0][0] == v.Drives[1][0] {
			t.Errorf("model not split evenly: %+v", v)
		}
	}
	if ok, _ := ParallelPossible(append(drives("d1", "d2"), DriveRef{Name: "z", Model: "Q"}), 2); ok {
		t.Error("class of 1 should fail")
	}
	fw := drives("d1", "d2")
	fw[0].Firmware, fw[1].Firmware = "1", "2"
	if ok, why := ParallelPossible(fw, 2); !ok {
		t.Errorf("firmware blocked: %q", why)
	}
	if _, err := PlanRuns(drives("d1", "d2", "d3"), []string{"x", "y"}, Parallel); err == nil {
		t.Error("expected error")
	}
}
