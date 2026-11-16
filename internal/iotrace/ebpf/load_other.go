//go:build !(linux && (amd64 || arm64))

package ebpf

import (
	"fmt"
	"runtime"
)

// Open reports that this platform has no eBPF collector.
func Open() (Program, error) {
	return nil, fmt.Errorf("%w on %s/%s", ErrUnsupported, runtime.GOOS, runtime.GOARCH)
}
