//go:build linux

package inuse

import "syscall"

func statDev(path string) (dev, rdev uint64, err error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Dev), uint64(st.Rdev), nil //nolint:unconvert // width differs per arch
}
