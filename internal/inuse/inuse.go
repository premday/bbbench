// Package inuse decides which disks must not be erased: it finds disks that
// are mounted, used as swap, hold bbbench itself or its output, are held by
// md, device mapper or another stacked device, or carry an md, ZFS, LVM,
// btrfs, bcachefs, LUKS, Ceph BlueStore, DRBD or VMFS signature (from udev).
// Any other filesystem or a partition table does not make a disk busy: it is
// reported in Carries. It only reads; a mount, swap area or path it cannot
// attribute to a disk makes it offer no disk at all.
package inuse

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PV is an LVM physical volume. VG is empty for an orphan PV.
type PV struct{ Device, VG string }

// Options replaces the host for tests. Empty fields use the defaults.
type Options struct {
	Sys, Proc, Udev string // /sys, /proc, /run/udev/data
	// Paths maps a path to what it is, e.g. "/usr/local/bin/bbbench": "the bbbench binary".
	Paths map[string]string
	Stat  func(path string) (dev, rdev uint64, err error)
	PVs   func() ([]PV, error)
}

// Report is the result of Scan.
type Report struct {
	Busy     map[string][]string // whole-disk kernel name -> reasons
	All      string              // non-empty: no disk can be offered, and why
	Warnings []string
	// Carries says what a disk that is not busy still carries, from udev:
	// "gpt partition table, ext4 on sdb1". Such a disk can be offered, and
	// a layout erases it. No udev data means no entry.
	Carries map[string]string
	// Causes has, for every disk in Busy, one Cause per reason, in the same
	// order: the reason in a form a program can act on.
	Causes map[string][]Cause
}

// Kind says what a Cause is.
type Kind string

const (
	KindMount     Kind = "mount"     // a mounted filesystem; Via is the mount point
	KindSwap      Kind = "swap"      // active swap; Via is the swap file or device
	KindPath      Kind = "path"      // bbbench or its output; Via is the path
	KindHolder    Kind = "holder"    // md, dm or another holder; Via is its node name
	KindMD        Kind = "md"        // an md RAID superblock
	KindPV        Kind = "pv"        // an LVM physical volume; Via is the VG, "" when orphan
	KindSignature Kind = "signature" // ZFS, btrfs, bcachefs, LUKS, Ceph, DRBD, VMFS
	KindAll       Kind = "all"       // no disk can be judged (Report.All)
)

// Cause is one reason a disk is busy. Text equals the string in Busy.
type Cause struct {
	Kind Kind
	Disk string
	Via  string
	// Device is the kernel node the mount, swap area or path sits on (md127,
	// dm-3, sda1, sdb; no /dev/), the holder node for KindHolder, "" when it
	// cannot be named or the kind has none.
	Device string
	Text   string
}

// Blockers returns the causes, for the given disks in order, that owned
// reports false for. When Report.All is set no disk can be judged, so it
// returns a single KindAll cause and the caller cannot proceed.
func (r *Report) Blockers(disks []string, owned func(Cause) bool) []Cause {
	if r.All != "" {
		return []Cause{{Kind: KindAll, Text: r.All}}
	}
	var out []Cause
	seen := map[string]bool{}
	for _, d := range disks {
		if seen[d] {
			continue
		}
		seen[d] = true
		for _, c := range r.Causes[d] {
			if !owned(c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// Reason is why the disk may not be erased, "" when it is free.
func (r *Report) Reason(disk string) string {
	if r.All != "" {
		return r.All
	}
	return strings.Join(r.Busy[disk], "; ")
}

// DevNumbers splits a Linux dev_t into major and minor.
func DevNumbers(dev uint64) (major, minor uint32) {
	major = uint32(((dev >> 8) & 0xfff) | ((dev >> 32) &^ 0xfff))
	minor = uint32((dev & 0xff) | ((dev >> 12) &^ 0xff))
	return
}

type scanner struct {
	o      Options
	r      *Report
	mounts []mount
}

func (s *scanner) add(disk, reason string, kind Kind, via, device string) {
	for _, have := range s.r.Busy[disk] {
		if have == reason {
			return
		}
	}
	s.r.Busy[disk] = append(s.r.Busy[disk], reason)
	if s.r.Causes == nil {
		s.r.Causes = map[string][]Cause{}
	}
	s.r.Causes[disk] = append(s.r.Causes[disk], Cause{Kind: kind, Disk: disk, Via: via, Device: device, Text: reason})
}

// setAll marks that no disk can be offered; the first reason wins.
func (s *scanner) setAll(reason string) {
	if s.r.All == "" {
		s.r.All = reason
	}
}

func (s *scanner) warn(msg string) { s.r.Warnings = append(s.r.Warnings, msg) }

// resolve maps "M:m" to the kernel name of that device and to the whole
// disks (leaves) under it. ok is false when sysfs does not know the device.
func (s *scanner) resolve(mm string) (name string, leaves []string, ok bool) {
	dir, err := filepath.EvalSymlinks(filepath.Join(s.o.Sys, "dev/block", mm))
	if err != nil {
		return "", nil, false
	}
	name = filepath.Base(dir)
	return name, s.leaves(dir, map[string]bool{}), true
}

func (s *scanner) leaves(dir string, seen map[string]bool) []string {
	if seen[dir] {
		return nil
	}
	seen[dir] = true
	if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
		dir = filepath.Dir(dir)
	}
	slaves, _ := os.ReadDir(filepath.Join(dir, "slaves"))
	if len(slaves) == 0 {
		return []string{filepath.Base(dir)}
	}
	var out []string
	for _, sl := range slaves {
		sub, err := filepath.EvalSymlinks(filepath.Join(s.o.Sys, "class/block", sl.Name()))
		if err != nil {
			// A slave sysfs does not know cannot be attributed to a disk:
			// naming it would put a partition among the disks, so refuse all.
			s.setAll(fmt.Sprintf("the stacked device %s has a member (%s) that could not be resolved to a disk; no disk is offered", filepath.Base(dir), sl.Name()))
			continue
		}
		out = append(out, s.leaves(sub, seen)...)
	}
	return out
}

// sourceRdev finds the device of a major-0 filesystem (btrfs, overlay...)
// through the mount source, when that is a /dev node.
func (s *scanner) sourceRdev(source string) (uint64, bool) {
	if !strings.HasPrefix(source, "/dev/") {
		return 0, false
	}
	_, rdev, err := s.o.Stat(source)
	if err != nil || rdev == 0 {
		return 0, false
	}
	return rdev, true
}

func mmOf(dev uint64) string {
	ma, mi := DevNumbers(dev)
	return fmt.Sprintf("%d:%d", ma, mi)
}

// devLeaves resolves a st_dev (a filesystem's device) to (name, leaves).
func (s *scanner) devLeaves(dev uint64) (string, []string, bool) {
	ma, _ := DevNumbers(dev)
	if ma != 0 {
		return s.resolve(mmOf(dev))
	}
	for _, m := range s.mounts {
		if m.MajMin != mmOf(dev) {
			continue
		}
		if rdev, ok := s.sourceRdev(m.Source); ok {
			if name, leaves, ok := s.resolve(mmOf(rdev)); ok {
				return name, leaves, true
			}
		}
	}
	return "", nil, false
}

// Scan inspects the host. It fails only when mountinfo cannot be read.
func Scan(opts Options) (*Report, error) {
	s := &scanner{o: opts, r: &Report{Busy: map[string][]string{}}}
	if s.o.Sys == "" {
		s.o.Sys = "/sys"
	}
	if s.o.Proc == "" {
		s.o.Proc = "/proc"
	}
	if s.o.Udev == "" {
		s.o.Udev = "/run/udev/data"
	}
	if s.o.Stat == nil {
		s.o.Stat = statDev
	}
	if s.o.PVs == nil {
		s.o.PVs = listPVs
	}
	mounts, err := readMountinfo(filepath.Join(s.o.Proc, "self/mountinfo"))
	if err != nil {
		return nil, err
	}
	s.mounts = mounts

	s.scanMounts()
	s.scanSwaps()
	s.scanPaths()
	s.scanHolders()
	s.scanPVs()
	s.scanSignatures()
	return s.r, nil
}

func (s *scanner) scanMounts() {
	for _, m := range s.mounts {
		major := strings.SplitN(m.MajMin, ":", 2)[0]
		var name string
		var leaves []string
		var ok bool
		if major == "0" {
			if rdev, found := s.sourceRdev(m.Source); found {
				name, leaves, ok = s.resolve(mmOf(rdev))
			}
			if ok {
				// Only the disk named in the source is known; the other
				// members of a multi-device filesystem are caught by
				// their signature.
				switch m.FSType {
				case "btrfs", "bcachefs":
					s.warn(fmt.Sprintf("%s %s (%s) may span more disks than the one named; disks with a %s signature are not offered", m.FSType, m.Point, m.Source, m.FSType))
				}
			} else {
				switch m.FSType {
				case "btrfs":
					s.warn(fmt.Sprintf("cannot tell which disks hold %s (%s %s); disks with a btrfs, ZFS, LVM or md signature are not offered", m.Point, m.FSType, m.Source))
				case "zfs", "bcachefs":
					s.warn(fmt.Sprintf("cannot tell which disks hold %s (%s %s); disks with a ZFS, LVM or md signature are not offered", m.Point, m.FSType, m.Source))
				}
				if m.Point == "/" {
					s.setAll(fmt.Sprintf("the disk holding the root filesystem could not be determined (%s %s); no disk is offered", m.FSType, m.Source))
				}
				continue
			}
		} else {
			name, leaves, ok = s.resolve(m.MajMin)
			if !ok {
				if m.Point == "/" {
					s.setAll(fmt.Sprintf("the disk holding the root filesystem could not be determined (%s %s); no disk is offered", m.FSType, m.Source))
				}
				continue
			}
		}
		reason := fmt.Sprintf("mounted at %s (%s)", m.Point, name)
		if m.Point == "/" {
			reason = "holds the root filesystem (/)"
		}
		for _, l := range leaves {
			s.add(l, reason, KindMount, m.Point, name)
		}
	}
}

func (s *scanner) scanSwaps() {
	data, err := os.ReadFile(filepath.Join(s.o.Proc, "swaps"))
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[min(1, len(lines)):] {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		file := decodeOctal(f[0])
		dev, rdev, err := s.o.Stat(file)
		unattributed := fmt.Sprintf("an active swap area (%s) could not be attributed to a disk; no disk can be offered", file)
		if err != nil {
			s.setAll(unattributed)
			continue
		}
		switch f[1] {
		case "partition":
			if name, leaves, ok := s.resolve(mmOf(rdev)); ok {
				for _, l := range leaves {
					s.add(l, fmt.Sprintf("active swap (%s)", file), KindSwap, file, name)
				}
			} else {
				s.setAll(unattributed)
			}
		case "file":
			if name, leaves, ok := s.devLeaves(dev); ok {
				for _, l := range leaves {
					s.add(l, fmt.Sprintf("holds an active swap file (%s)", file), KindSwap, file, name)
				}
			} else {
				s.setAll(unattributed)
			}
		}
	}
}

func (s *scanner) scanPaths() {
	paths := make([]string, 0, len(s.o.Paths))
	for p := range s.o.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		dev, _, err := s.o.Stat(p)
		if err != nil {
			s.setAll(fmt.Sprintf("%s (%s) could not be attributed to a disk (%v); no disk can be offered", s.o.Paths[p], p, err))
			continue
		}
		name, leaves, ok := s.devLeaves(dev)
		if !ok {
			s.setAll(fmt.Sprintf("%s (%s) could not be attributed to a disk; no disk can be offered", s.o.Paths[p], p))
			continue
		}
		for _, l := range leaves {
			s.add(l, fmt.Sprintf("holds %s (%s)", s.o.Paths[p], p), KindPath, p, name)
		}
	}
}

// diskDirs lists every whole device in /sys/block and, for each, the
// directories of the disk itself and of its partitions (name -> M:m).
type devEntry struct{ dir, mm string }

func (s *scanner) disks() map[string][]devEntry {
	out := map[string][]devEntry{}
	entries, _ := os.ReadDir(filepath.Join(s.o.Sys, "block"))
	for _, e := range entries {
		dir := filepath.Join(s.o.Sys, "block", e.Name())
		list := []devEntry{{dir, readTrim(filepath.Join(dir, "dev"))}}
		subs, _ := os.ReadDir(dir)
		for _, sub := range subs {
			sd := filepath.Join(dir, sub.Name())
			if _, err := os.Stat(filepath.Join(sd, "partition")); err == nil {
				list = append(list, devEntry{sd, readTrim(filepath.Join(sd, "dev"))})
			}
		}
		out[e.Name()] = list
	}
	return out
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *scanner) scanHolders() {
	disks := s.disks()
	for _, name := range sortedKeys(disks) {
		for _, d := range disks[name] {
			hs, _ := os.ReadDir(filepath.Join(d.dir, "holders"))
			for _, h := range hs {
				switch {
				case strings.HasPrefix(h.Name(), "md"):
					s.add(name, fmt.Sprintf("member of %s (md RAID)", h.Name()), KindHolder, h.Name(), h.Name())
				case strings.HasPrefix(h.Name(), "dm-"):
					s.add(name, fmt.Sprintf("used by %s (device mapper: LVM, LUKS or multipath)", h.Name()), KindHolder, h.Name(), h.Name())
				default:
					s.add(name, fmt.Sprintf("used by %s", h.Name()), KindHolder, h.Name(), h.Name())
				}
			}
		}
	}
}

func (s *scanner) scanPVs() {
	pvs, err := s.o.PVs()
	if err != nil {
		s.warn(fmt.Sprintf("cannot list LVM physical volumes: %v; disks with an LVM signature are not offered", err))
		return
	}
	for _, pv := range pvs {
		_, rdev, err := s.o.Stat(pv.Device)
		if err != nil {
			s.warn(fmt.Sprintf("cannot tell which disk holds the LVM physical volume %s: %v", pv.Device, err))
			continue
		}
		_, leaves, ok := s.resolve(mmOf(rdev))
		if !ok {
			s.warn(fmt.Sprintf("cannot tell which disk holds the LVM physical volume %s", pv.Device))
			continue
		}
		reason := "is an LVM physical volume (no volume group)"
		if pv.VG != "" {
			reason = fmt.Sprintf("is an LVM physical volume of volume group %s", pv.VG)
		}
		for _, l := range leaves {
			s.add(l, reason, KindPV, pv.VG, "")
		}
	}
}

func (s *scanner) scanSignatures() {
	disks := s.disks()
	for _, name := range sortedKeys(disks) {
		hasPV := false
		for _, have := range s.r.Busy[name] {
			if strings.HasPrefix(have, "is an LVM physical volume") {
				hasPV = true
			}
		}
		var carries []string
		for i, d := range disks[name] {
			if d.mm == "" {
				continue
			}
			udev := filepath.Join(s.o.Udev, "b"+d.mm)
			if i == 0 {
				if pt := udevProp(udev, "ID_PART_TABLE_TYPE"); pt != "" {
					carries = append(carries, pt+" partition table")
				}
			}
			fs := udevProp(udev, "ID_FS_TYPE")
			switch fs {
			case "linux_raid_member":
				s.add(name, "has an md RAID superblock (an array that is not assembled; check with mdadm --examine)", KindMD, "", "")
			case "zfs_member":
				s.add(name, "has a ZFS pool label", KindSignature, "", "")
			case "LVM2_member":
				if !hasPV {
					s.add(name, "is an LVM physical volume", KindPV, "", "")
				}
			case "btrfs":
				s.add(name, "has a btrfs filesystem label (member of a btrfs filesystem; check with btrfs filesystem show)", KindSignature, "", "")
			case "bcachefs":
				s.add(name, "has a bcachefs filesystem label (member of a bcachefs filesystem)", KindSignature, "", "")
			case "crypto_LUKS":
				s.add(name, "is LUKS encrypted", KindSignature, "", "")
			case "ceph_bluestore":
				s.add(name, "has a Ceph BlueStore label (an OSD of a Ceph cluster; check with ceph-volume lvm list)", KindSignature, "", "")
			case "drbd":
				s.add(name, "has DRBD metadata (a backing device of a DRBD replicated volume)", KindSignature, "", "")
			case "VMFS_volume_member":
				s.add(name, "is a VMware VMFS volume member", KindSignature, "", "")
			case "":
			default:
				carries = append(carries, fs+" on "+filepath.Base(d.dir))
			}
		}
		if len(carries) > 0 && len(s.r.Busy[name]) == 0 {
			if s.r.Carries == nil {
				s.r.Carries = map[string]string{}
			}
			s.r.Carries[name] = strings.Join(carries, ", ")
		}
	}
}

// udevProp is the property key of a udev database entry, "" when it or the
// entry is missing.
func udevProp(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "E:"+key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
