package settings

import (
	"context"
	"testing"
	"time"
)

// seq is a Setting whose Read walks through a list of values, then repeats the last.
type seq struct {
	Setting
	vals []string
	n    int
}

func (s *seq) Read(context.Context, Target) (Reading, error) {
	v := s.vals[min(s.n, len(s.vals)-1)]
	s.n++
	return Reading{Value: v, At: time.Now()}, nil
}

func TestVerify(t *testing.T) {
	s := &seq{vals: []string{"enabled", "enabled", "disabled"}}
	r, ok := Verify(context.Background(), s, Target{}, Value{ID: "disabled"}, 5*time.Second)
	if !ok || r.Value != "disabled" || s.n != 3 {
		t.Errorf("%+v %v %d", r, ok, s.n)
	}
	s = &seq{vals: []string{"enabled"}}
	start := time.Now()
	r, ok = Verify(context.Background(), s, Target{}, Value{ID: "disabled"}, 500*time.Millisecond)
	if ok || r.Value != "enabled" || time.Since(start) < 500*time.Millisecond {
		t.Errorf("%+v %v %v", r, ok, time.Since(start))
	}
}
