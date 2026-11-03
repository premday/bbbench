package buildinfo

import "testing"

func TestString(t *testing.T) {
	oldV, oldC, oldD := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldV, oldC, oldD })
	Version, Commit, Date = "1.2.3", "abc1234", "2026-01-02T03:04:05Z"
	want := "1.2.3 (commit abc1234, built 2026-01-02T03:04:05Z)"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestDefaultsAreNotEmpty(t *testing.T) {
	if Version == "" || Commit == "" || Date == "" {
		t.Fatal("defaults must not be empty so a plain go build still reports something")
	}
}
