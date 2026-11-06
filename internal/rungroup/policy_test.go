package rungroup

import (
	"reflect"
	"testing"
)

func TestPolicy(t *testing.T) {
	tests := []struct {
		name          string
		before, after map[string]bool
		status        RunStatus
		valid         map[string]bool
	}{
		{"all ok", map[string]bool{"a": true, "b": true}, map[string]bool{"a": true, "b": true}, RunDone, nil},
		{"before false", map[string]bool{"a": true, "b": false}, map[string]bool{"a": true, "b": true}, RunNotRun, nil},
		{"after false", map[string]bool{"a": true, "b": true}, map[string]bool{"a": true, "b": false}, RunInvalid, map[string]bool{"a": true, "b": false}},
		{"after missing", map[string]bool{"a": true, "b": true}, map[string]bool{"a": true}, RunInvalid, map[string]bool{"a": true, "b": false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, v := Policy(tc.before, tc.after)
			if s != tc.status || !reflect.DeepEqual(v, tc.valid) {
				t.Errorf("got %s %v", s, v)
			}
		})
	}
}
