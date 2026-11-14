package main

import (
	"slices"
	"strings"
)

// deviceNameLess orders kernel device names the way a person reads them:
// runs of digits compare as numbers, so sda < sdb < sdz < sdaa and nvme0n1 <
// nvme1n1 < nvme2n1 < nvme10n1 (a plain string sort puts nvme10n1 first).
func deviceNameLess(a, b string) bool {
	// sd, vd, hd and xvd disks are lettered a..z, aa..az, ...: a longer letter
	// suffix comes later (sdz < sdaa).
	if pa, ra, ok := letterSuffix(a); ok {
		if pb, rb, ok := letterSuffix(b); ok && pa == pb && ra != rb {
			if len(ra) != len(rb) {
				return len(ra) < len(rb)
			}
			return ra < rb
		}
	}
	for a != "" && b != "" {
		da, db := isDigit(a[0]), isDigit(b[0])
		if da && db {
			na, ra := splitDigits(a)
			nb, rb := splitDigits(b)
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = ra, rb
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

// letterSuffix splits a name like sdaa into its family ("sd") and letters
// ("aa"); ok is false unless the whole name is a family plus letters.
func letterSuffix(n string) (family, letters string, ok bool) {
	for _, f := range []string{"xvd", "sd", "vd", "hd"} {
		if rest, found := strings.CutPrefix(n, f); found && rest != "" && strings.Trim(rest, "abcdefghijklmnopqrstuvwxyz") == "" {
			return f, rest, true
		}
	}
	return "", "", false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// splitDigits returns the leading digits of s without leading zeros (so
// "007" and "7" compare equal; a name that differs only there falls back to
// plain order below) and the rest.
func splitDigits(s string) (digits, rest string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return strings.TrimLeft(s[:i], "0"), s[i:]
}

// sortedDrives returns a copy of drives ordered by device name
// (deviceNameLess); the argument is left as it is.
func sortedDrives(drives []DriveInfo) []DriveInfo {
	out := slices.Clone(drives)
	slices.SortStableFunc(out, func(a, b DriveInfo) int {
		switch {
		case deviceNameLess(a.Device.Name, b.Device.Name):
			return -1
		case deviceNameLess(b.Device.Name, a.Device.Name):
			return 1
		}
		return strings.Compare(a.Device.Name, b.Device.Name)
	})
	return out
}
