package util

import (
	"os"
	"strings"
)

// ReadTrim returns the content of a small file (a sysfs value) without
// surrounding white space, "" when it cannot be read.
func ReadTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SysBlockOr returns sysBlock, or /sys/block when it is "" (the default of
// the sysBlock parameters, which tests set to a fake tree).
func SysBlockOr(sysBlock string) string {
	if sysBlock == "" {
		return "/sys/block"
	}
	return sysBlock
}
