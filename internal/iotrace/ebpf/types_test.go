package ebpf

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func cDefine(t *testing.T, src, name string) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^#define ` + name + ` (\d+)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no #define %s in iolat.bpf.c", name)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func TestSizesMatchTheCSource(t *testing.T) {
	b, err := os.ReadFile("bpf/iolat.bpf.c")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for name, want := range map[string]int{"NBUCKETS": NBuckets, "MAX_DEVS": MaxDevs, "KINDS": Kinds} {
		if got := cDefine(t, src, name); got != want {
			t.Errorf("%s: C has %d, Go has %d", name, got, want)
		}
	}
	if maxSlots != MaxDevs*Kinds*2 {
		t.Errorf("maxSlots %d", maxSlots)
	}
}

// The committed object must be built from the committed source: go generate
// writes the source's hash next to it.
func TestObjectIsBuiltFromTheSource(t *testing.T) {
	src, err := os.ReadFile("bpf/iolat.bpf.c")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("bpf/iolat.bpf.c.sha256")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(src)
	if got := hex.EncodeToString(sum[:]); got != strings.TrimSpace(string(want)) {
		t.Fatalf("bpf/iolat.bpf.c changed (sha256 %s, recorded %s): run go generate ./internal/iotrace/ebpf and commit the object", got, fmt.Sprint(strings.TrimSpace(string(want))))
	}
}
