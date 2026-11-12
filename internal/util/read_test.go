package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadTrim(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v")
	if err := os.WriteFile(p, []byte("  write back\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadTrim(p); got != "write back" {
		t.Errorf("ReadTrim = %q", got)
	}
	if got := ReadTrim(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing file = %q", got)
	}
}

func TestSysBlockOr(t *testing.T) {
	if got := SysBlockOr(""); got != "/sys/block" {
		t.Errorf("default = %q", got)
	}
	if got := SysBlockOr("/tmp/sys"); got != "/tmp/sys" {
		t.Errorf("override = %q", got)
	}
}
