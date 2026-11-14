//go:build !linux

package inuse

import "errors"

func statDev(path string) (dev, rdev uint64, err error) {
	return 0, 0, errors.New("stat of device numbers is only supported on Linux")
}
