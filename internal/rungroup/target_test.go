package rungroup

import (
	"reflect"
	"testing"

	"bbbench/internal/settings"
)

func TestPlanVariants(t *testing.T) {
	got := PlanVariants([]Variant{{Value: "raid1", Drives: []string{"sdc", "sdb"}}, {Value: "none", Drives: []string{"sdb", "sdc", "sdd"}}})
	want := []RunPlan{
		{Round: 0, Variants: []Variant{{Value: "raid1", Drives: []string{"sdc", "sdb"}}}},
		{Round: 1, Variants: []Variant{{Value: "none", Drives: []string{"sdb", "sdc", "sdd"}}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestMembersOf(t *testing.T) {
	got := MembersOf([]settings.Disk{{Target: settings.Target{Name: "sdb", Serial: "S1", Model: "M"}, Role: settings.RoleSpare}})
	want := []Member{{Name: "sdb", Serial: "S1", Model: "M", Role: "spare"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}
