package settings

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOps is a DriveOps keeping one value per drive name.
type fakeOps struct {
	mu       sync.Mutex
	cur      map[string]string
	applyErr map[string]bool
	frozen   bool // Apply and Restore change nothing
}

func (f *fakeOps) Name() string                     { return "fake" }
func (f *fakeOps) Kind() Kind                       { return Applied }
func (f *fakeOps) Values() []Value                  { return []Value{{ID: enabled}, {ID: disabled}} }
func (f *fakeOps) Applicable(Target) (bool, string) { return true, "" }

func (f *fakeOps) Read(_ context.Context, t Target) (Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Reading{Value: f.cur[t.Name], At: time.Now()}, nil
}

func (f *fakeOps) Apply(_ context.Context, t Target, v Value) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr[t.Name] {
		return errors.New("apply failed on " + t.Name)
	}
	if !f.frozen {
		f.cur[t.Name] = v.ID
	}
	return nil
}

func (f *fakeOps) Restore(_ context.Context, t Target, original Reading) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.frozen {
		f.cur[t.Name] = original.Value
	}
	return nil
}

func disks(ts ...Target) []Disk {
	var out []Disk
	for _, t := range ts {
		out = append(out, Disk{Target: t, Role: RoleData})
	}
	return out
}

var (
	sataB = Target{Name: "sdb", Path: "/dev/sdb", Serial: "S1", Model: "M", Interface: "SATA"}
	usbC  = Target{Name: "sdc", Path: "/dev/sdc", Serial: "S2", Model: "M", Interface: "USB"}
)

func TestPerDrive_Identity(t *testing.T) {
	s := NewWriteCache(Env{})
	if s.Scope() != ScopeDrive || s.Input() != InputValues || s.Kind() != Applied || s.Lifetime() != LifetimeGroup {
		t.Errorf("%s %s %s %s", s.Scope(), s.Input(), s.Kind(), s.Lifetime())
	}
}

func TestPerDrive_ApplicableVariant(t *testing.T) {
	s := NewWriteCache(Env{LookPath: lookAll})
	ok, why := s.ApplicableVariant(Variant{Value: Value{ID: enabled}, Disks: disks(sataB, usbC)})
	if ok || why != "sdc: no controllable volatile write cache on this interface" {
		t.Errorf("%v %q", ok, why)
	}
	if ok, why := s.ApplicableVariant(Variant{Value: Value{ID: enabled}, Disks: disks(sataB)}); !ok || why != "" {
		t.Errorf("sdb alone: %v %q", ok, why)
	}
}

func TestPerDrive_PrepareReadsOriginals(t *testing.T) {
	ops := &fakeOps{cur: map[string]string{"sdb": enabled, "sdc": disabled}}
	s := PerDrive(ops, 50*time.Millisecond)
	undos, err := s.Prepare(context.Background(), Variant{Disks: disks(sataB, usbC)})
	if err != nil || len(undos) != 2 {
		t.Fatalf("%+v %v", undos, err)
	}
	for i, want := range []string{enabled, disabled} {
		if undos[i].Original == nil || undos[i].Original.Value != want || undos[i].Drive == nil {
			t.Errorf("undo %d = %+v, want original %q", i, undos[i], want)
		}
	}
	if undos[0].Name() != "sdb" || undos[1].Name() != "sdc" || undos[1].Drive.Serial != "S2" {
		t.Errorf("names %q %q, drive %+v", undos[0].Name(), undos[1].Name(), undos[1].Drive)
	}
	empty := PerDrive(&fakeOps{cur: map[string]string{}}, 50*time.Millisecond)
	if _, err := empty.Prepare(context.Background(), Variant{Disks: disks(sataB)}); err == nil || !strings.Contains(err.Error(), "read back empty") {
		t.Errorf("err = %v, want read back empty", err)
	}
}

func TestPerDrive_ApplyReportsPerTargetErrors(t *testing.T) {
	ops := &fakeOps{cur: map[string]string{"sdb": enabled, "sdc": enabled}, applyErr: map[string]bool{"sdc": true}}
	s := PerDrive(ops, 50*time.Millisecond)
	v := Variant{Name: "1_1", Value: Value{ID: disabled}, Disks: disks(sataB, usbC)}
	ts, err := s.Apply(context.Background(), v, nil)
	if err != nil || len(ts) != 2 {
		t.Fatalf("%+v %v", ts, err)
	}
	if ts[0].Target.Name != "sdb" || ts[0].Err != nil || ts[0].Want != disabled || ts[0].Kind != TargetDrive {
		t.Errorf("sdb target %+v", ts[0])
	}
	if ts[1].Target.Name != "sdc" || ts[1].Err == nil {
		t.Errorf("sdc target %+v, want an error", ts[1])
	}
	if r, err := s.Read(context.Background(), ts[0]); err != nil || r.Value != disabled {
		t.Errorf("read %+v %v", r, err)
	}
}

func TestPerDrive_UndoRestoresAndVerifies(t *testing.T) {
	ops := &fakeOps{cur: map[string]string{"sdb": disabled}}
	s := PerDrive(ops, 50*time.Millisecond)
	tg := sataB
	u := Undo{Drive: &tg, Original: &Reading{Value: enabled}}
	if err := s.Undo(context.Background(), u); err != nil || ops.cur["sdb"] != enabled {
		t.Fatalf("%v, sdb = %q", err, ops.cur["sdb"])
	}
	ops = &fakeOps{cur: map[string]string{"sdb": disabled}, frozen: true}
	s = PerDrive(ops, 50*time.Millisecond)
	start := time.Now()
	err := s.Undo(context.Background(), u)
	if err == nil || !strings.Contains(err.Error(), `reads "disabled" after restoring, want "enabled"`) {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Errorf("verify took %v", d)
	}
}

func TestPerDrive_UndoCommand(t *testing.T) {
	s := NewWriteCache(Env{LookPath: lookAll})
	h, ok := s.(CommandHinter)
	if !ok {
		t.Fatal("write cache is not a CommandHinter")
	}
	tg := sataB
	if c := h.UndoCommand(Undo{Drive: &tg, Original: &Reading{Value: enabled}}); c != "hdparm -W1 /dev/sdb" {
		t.Errorf("%q", c)
	}
	if c := PerDrive(&fakeOps{}, 0).(CommandHinter).UndoCommand(Undo{Drive: &tg, Original: &Reading{Value: enabled}}); c != "" {
		t.Errorf("ops without a hinter: %q", c)
	}
}
