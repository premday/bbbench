package settings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake answers commands from a map keyed by "name arg arg..." and records
// every command run.
type fake struct {
	answers map[string]string
	ran     []string
}

func (f *fake) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	f.ran = append(f.ran, key)
	if isSet(key) {
		return nil, nil
	}
	if out, ok := f.answers[key]; ok {
		return []byte(out), nil
	}
	return nil, errors.New("not found: " + key)
}

func isSet(c string) bool {
	return strings.Contains(c, "set-feature") || strings.Contains(c, "-W0") || strings.Contains(c, "-W1") || strings.Contains(c, "--set")
}

// sets lists the commands run that change something.
func (f *fake) sets() []string {
	var s []string
	for _, c := range f.ran {
		if isSet(c) {
			s = append(s, c)
		}
	}
	return s
}

func lookAll(string) (string, error) { return "/usr/bin/x", nil }

func lookNone(string) (string, error) { return "", errors.New("not found") }

func sysWith(t *testing.T, name, kernel string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "block", name, "queue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "write_cache"), []byte(kernel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func find(vs []Value, id string) Value {
	for _, v := range vs {
		if v.ID == id {
			return v
		}
	}
	return Value{}
}

func TestWriteCache_NVMe(t *testing.T) {
	f := &fake{answers: map[string]string{"nvme get-feature /dev/nvme0 -f 6": "get-feature:0x06 (Volatile Write Cache), Current value:0x000001"}}
	s := NewWriteCache(Env{Run: f.run, LookPath: lookAll, Sys: sysWith(t, "nvme0n1", "write back")})
	tg := Target{Name: "nvme0n1", Path: "/dev/nvme0n1", Interface: "NVMe (PCIe)"}
	r, err := s.Read(context.Background(), tg)
	if err != nil || r.Value != "enabled" || r.Kernel != "write back" || r.At.IsZero() {
		t.Fatalf("%+v %v", r, err)
	}
	f.answers["nvme get-feature /dev/nvme0 -f 6"] = "get-feature:0x06 (Volatile Write Cache), Current value:0x000000"
	if r, _ = s.Read(context.Background(), tg); r.Value != "disabled" {
		t.Errorf("%+v", r)
	}
	vals := s.Values()
	if err := s.Apply(context.Background(), tg, find(vals, "disabled")); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), tg, find(vals, "enabled")); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.sets(), "|")
	if got != "nvme set-feature /dev/nvme0 -f 6 -V 0|nvme set-feature /dev/nvme0 -f 6 -V 1" {
		t.Errorf("%s", got)
	}
}

func TestWriteCache_SATA(t *testing.T) {
	f := &fake{answers: map[string]string{"hdparm -W /dev/sdb": "\n/dev/sdb:\n write-caching =  1 (on)\n"}}
	s := NewWriteCache(Env{Run: f.run, LookPath: lookAll, Sys: sysWith(t, "sdb", "write back")})
	tg := Target{Name: "sdb", Path: "/dev/sdb", Interface: "SATA"}
	r, err := s.Read(context.Background(), tg)
	if err != nil || r.Value != "enabled" {
		t.Fatalf("%+v %v", r, err)
	}
	f.answers["hdparm -W /dev/sdb"] = " write-caching =  0 (off)"
	if r, _ = s.Read(context.Background(), tg); r.Value != "disabled" {
		t.Errorf("%+v", r)
	}
	vals := s.Values()
	_ = s.Apply(context.Background(), tg, find(vals, "disabled"))
	_ = s.Apply(context.Background(), tg, find(vals, "enabled"))
	if got := strings.Join(f.sets(), "|"); got != "hdparm -W0 /dev/sdb|hdparm -W1 /dev/sdb" {
		t.Errorf("%s", got)
	}
}

func TestWriteCache_SAS(t *testing.T) {
	f := &fake{}
	wce := true
	s := NewWriteCache(Env{Run: f.run, LookPath: lookAll, Sys: sysWith(t, "sdc", "write back"),
		GetWCE: func(p string) (bool, error) {
			if p != "/dev/sdc" {
				t.Errorf("path %s", p)
			}
			return wce, nil
		}})
	tg := Target{Name: "sdc", Path: "/dev/sdc", Interface: "SAS"}
	r, err := s.Read(context.Background(), tg)
	if err != nil || r.Value != "enabled" {
		t.Fatalf("%+v %v", r, err)
	}
	wce = false
	if r, _ = s.Read(context.Background(), tg); r.Value != "disabled" {
		t.Errorf("%+v", r)
	}
	f.answers = map[string]string{"sdparm --set=WCE=0 /dev/sdc": "", "sdparm --set=WCE=1 /dev/sdc": ""}
	vals := s.Values()
	_ = s.Apply(context.Background(), tg, find(vals, "disabled"))
	_ = s.Apply(context.Background(), tg, find(vals, "enabled"))
	if got := strings.Join(f.sets(), "|"); got != "sdparm --set=WCE=0 /dev/sdc|sdparm --set=WCE=1 /dev/sdc" {
		t.Errorf("%s", got)
	}
	for _, c := range f.ran {
		if strings.Contains(c, "--save") {
			t.Errorf("must never save: %s", c)
		}
	}
}

func TestWriteCache_ScopeAndInput(t *testing.T) {
	s := NewWriteCache(Env{})
	if s.Scope() != ScopeDrive || s.Kind() != Applied || s.Input() != InputValues || s.Name() != "disk write cache policy" {
		t.Errorf("%s %s %s %s", s.Name(), s.Scope(), s.Kind(), s.Input())
	}
	if v := s.Values(); len(v) != 2 || v[0].ID != "enabled" || v[1].ID != "disabled" {
		t.Errorf("%+v", v)
	}
	if len(All(Env{})) != 1 {
		t.Error("All")
	}
}

func TestWriteCache_Applicable(t *testing.T) {
	const noCtl = "no controllable volatile write cache on this interface"
	only := func(bins ...string) func(string) (string, error) {
		return func(b string) (string, error) {
			for _, x := range bins {
				if x == b {
					return "/usr/bin/" + b, nil
				}
			}
			return "", errors.New("nf")
		}
	}
	cases := []struct {
		iface  string
		look   func(string) (string, error)
		ok     bool
		reason string
	}{
		{"USB", lookAll, false, noCtl},
		{"virtio", lookAll, false, noCtl},
		{"NVMe (PCIe)", lookNone, false, "nvme is not installed (nvme-cli)"},
		{"SATA", only("nvme", "sdparm"), false, "hdparm is not installed"},
		{"SAS", only("nvme", "hdparm"), false, "sdparm is not installed"},
		{"NVMe (PCIe)", only("nvme"), true, ""},
		{"SATA", only("hdparm"), true, ""},
		{"SAS", only("sdparm"), true, ""},
	}
	for _, c := range cases {
		s := NewWriteCache(Env{LookPath: c.look})
		ok, why := s.Applicable(Target{Name: "x", Interface: c.iface})
		if ok != c.ok || why != c.reason {
			t.Errorf("%s: got %v %q", c.iface, ok, why)
		}
	}
}

func TestWriteCache_RestoreWritesOnlyWhenDifferent(t *testing.T) {
	f := &fake{answers: map[string]string{"hdparm -W /dev/sdb": " write-caching =  1 (on)"}}
	s := NewWriteCache(Env{Run: f.run, LookPath: lookAll, Sys: sysWith(t, "sdb", "write back")})
	tg := Target{Name: "sdb", Path: "/dev/sdb", Interface: "SATA"}
	if err := s.Restore(context.Background(), tg, Reading{Value: "enabled"}); err != nil {
		t.Fatal(err)
	}
	if len(f.sets()) != 0 {
		t.Errorf("wrote: %v", f.sets())
	}
	if err := s.Restore(context.Background(), tg, Reading{Value: "disabled"}); err != nil {
		t.Fatal(err)
	}
	if got := f.sets(); len(got) != 1 || got[0] != "hdparm -W0 /dev/sdb" {
		t.Errorf("%v", got)
	}
}
