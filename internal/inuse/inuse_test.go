package inuse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type host struct {
	t                *testing.T
	sys, proc, udev  string
	stats            map[string][2]uint64
	pvs              []PV
	pvErr            error
	paths            map[string]string
	nextMountID      int
	mountinfo, swaps []string
}

func fakeHost(t *testing.T) *host {
	t.Helper()
	root := t.TempDir()
	h := &host{t: t, sys: filepath.Join(root, "sys"), proc: filepath.Join(root, "proc"),
		udev: filepath.Join(root, "udev"), stats: map[string][2]uint64{}}
	for _, d := range []string{h.sys + "/block", h.sys + "/class/block", h.sys + "/dev/block", h.proc, h.udev} {
		h.mkdir(d)
	}
	return h
}

func (h *host) mkdir(p string) {
	h.t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) write(p, s string) {
	h.t.Helper()
	h.mkdir(filepath.Dir(p))
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) link(target, name string) {
	h.t.Helper()
	h.mkdir(filepath.Dir(name))
	if err := os.Symlink(target, name); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) disk(name, mm string) {
	h.mkdir(filepath.Join(h.sys, "block", name))
	h.write(filepath.Join(h.sys, "block", name, "dev"), mm+"\n")
	h.link("../../block/"+name, filepath.Join(h.sys, "class/block", name))
	h.link("../../block/"+name, filepath.Join(h.sys, "dev/block", mm))
}

func (h *host) part(disk, name, mm string) {
	d := filepath.Join(h.sys, "block", disk, name)
	h.write(filepath.Join(d, "partition"), "1\n")
	h.write(filepath.Join(d, "dev"), mm+"\n")
	h.link("../../block/"+disk+"/"+name, filepath.Join(h.sys, "class/block", name))
	h.link("../../block/"+disk+"/"+name, filepath.Join(h.sys, "dev/block", mm))
}

// holder records that holder h sits on dev (a disk or "disk/part").
func (h *host) holder(dev, hd string) {
	h.mkdir(filepath.Join(h.sys, "block", dev, "holders", hd))
}

// stack builds a device-mapper style device on top of leaves (names).
func (h *host) stack(dm, mm string, leaves ...string) {
	h.disk(dm, mm)
	for _, l := range leaves {
		h.mkdir(filepath.Join(h.sys, "block", dm, "slaves", l))
	}
}

func (h *host) mount(mm, mp, fstype, source string) {
	h.nextMountID++
	h.mountinfo = append(h.mountinfo, fmt.Sprintf("%d 1 %s / %s rw - %s %s rw", 20+h.nextMountID, mm, mp, fstype, source))
	h.write(filepath.Join(h.proc, "self/mountinfo"), strings.Join(h.mountinfo, "\n")+"\n")
}

func (h *host) swap(line string) {
	if len(h.swaps) == 0 {
		h.swaps = append(h.swaps, "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority")
	}
	h.swaps = append(h.swaps, line)
	h.write(filepath.Join(h.proc, "swaps"), strings.Join(h.swaps, "\n")+"\n")
}

func (h *host) udevType(mm, fstype string) {
	h.write(filepath.Join(h.udev, "b"+mm), "S:disk/x\nE:ID_FS_TYPE="+fstype+"\nE:FOO=bar\n")
}

func (h *host) udevTypeUUID(mm, fstype, uuid string) {
	h.write(filepath.Join(h.udev, "b"+mm), "S:disk/x\nE:ID_FS_TYPE="+fstype+"\nE:ID_FS_UUID="+uuid+"\n")
}

func (h *host) opts() Options {
	if _, err := os.Stat(filepath.Join(h.proc, "self/mountinfo")); err != nil {
		h.write(filepath.Join(h.proc, "self/mountinfo"), "")
	}
	return Options{Sys: h.sys, Proc: h.proc, Udev: h.udev, Paths: h.paths,
		Stat: func(p string) (uint64, uint64, error) {
			v, ok := h.stats[p]
			if !ok {
				return 0, 0, errors.New("no such file")
			}
			return v[0], v[1], nil
		},
		PVs: func() ([]PV, error) { return h.pvs, h.pvErr }}
}

func (h *host) scan() *Report {
	h.t.Helper()
	r, err := Scan(h.opts())
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// dev builds a dev_t for major:minor (small numbers).
func dev(major, minor uint64) uint64 { return major<<8 | minor }

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestScan_MountedPartition(t *testing.T) {
	h := fakeHost(t)
	h.disk("sda", "8:0")
	h.part("sda", "sda2", "8:2")
	h.mount("8:2", "/data", "ext4", "/dev/sda2")
	eq(t, h.scan().Busy["sda"], []string{"mounted at /data (sda2)"})
}

func TestScan_RootDisk(t *testing.T) {
	h := fakeHost(t)
	h.disk("nvme0n1", "259:0")
	h.part("nvme0n1", "nvme0n1p2", "259:2")
	h.mount("259:2", "/", "ext4", "/dev/nvme0n1p2")
	if got := h.scan().Reason("nvme0n1"); got != "holds the root filesystem (/)" {
		t.Fatal(got)
	}
}

func TestScan_StackedLVM(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdb", "8:16")
	h.part("sdb", "sdb1", "8:17")
	h.disk("sdc", "8:32")
	h.stack("dm-0", "253:0", "sdb1", "sdc")
	h.holder("sdb/sdb1", "dm-0")
	h.holder("sdc", "dm-0")
	h.mount("253:0", "/home", "ext4", "/dev/mapper/vg-home")
	r := h.scan()
	want := []string{"mounted at /home (dm-0)", "used by dm-0 (device mapper: LVM, LUKS or multipath)"}
	eq(t, r.Busy["sdb"], want)
	eq(t, r.Busy["sdc"], want)
}

func TestScan_Swap(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdd", "8:48")
	h.part("sdd", "sdd1", "8:49")
	h.disk("sde", "8:64")
	h.stats["/dev/sdd1"] = [2]uint64{dev(0, 5), dev(8, 49)}
	h.stats["/swapfile"] = [2]uint64{dev(8, 64), 0}
	h.swap("/dev/sdd1\tpartition\t100\t0\t-2")
	h.swap("/swapfile\tfile\t100\t0\t-3")
	r := h.scan()
	eq(t, r.Busy["sdd"], []string{"active swap (/dev/sdd1)"})
	eq(t, r.Busy["sde"], []string{"holds an active swap file (/swapfile)"})
}

func TestScan_Paths(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdf", "8:80")
	h.stats["/usr/local/bin/bbbench"] = [2]uint64{dev(8, 80), 0}
	h.paths = map[string]string{"/usr/local/bin/bbbench": "the bbbench binary"}
	eq(t, h.scan().Busy["sdf"], []string{"holds the bbbench binary (/usr/local/bin/bbbench)"})
}

func TestScan_PathsOnBtrfs(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdf", "8:80")
	h.mount("0:30", "/srv", "btrfs", "/dev/sdf")
	h.stats["/dev/sdf"] = [2]uint64{dev(0, 5), dev(8, 80)}
	h.stats["/srv/out"] = [2]uint64{dev(0, 30), 0}
	h.paths = map[string]string{"/srv/out": "the output directory"}
	got := h.scan().Busy["sdf"]
	if got[len(got)-1] != "holds the output directory (/srv/out)" {
		t.Fatal(got)
	}
}

func TestScan_MDMember(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdg", "8:96")
	h.holder("sdg", "md127")
	eq(t, h.scan().Busy["sdg"], []string{"member of md127 (md RAID)"})
}

func TestScan_OtherHolder(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdg", "8:96")
	h.holder("sdg", "nbd0")
	eq(t, h.scan().Busy["sdg"], []string{"used by nbd0"})
}

func TestScan_UdevSignatures(t *testing.T) {
	cases := []struct{ fs, want string }{
		{"linux_raid_member", "has an md RAID superblock (an array that is not assembled; check with mdadm --examine)"},
		{"zfs_member", "has a ZFS pool label"},
		{"LVM2_member", "is an LVM physical volume"},
		{"crypto_LUKS", "is LUKS encrypted"},
		{"ceph_bluestore", "has a Ceph BlueStore label (an OSD of a Ceph cluster; check with ceph-volume lvm list)"},
		{"drbd", "has DRBD metadata (a backing device of a DRBD replicated volume)"},
		{"VMFS_volume_member", "is a VMware VMFS volume member"},
		{"ext4", ""},
	}
	for _, c := range cases {
		t.Run(c.fs, func(t *testing.T) {
			h := fakeHost(t)
			h.disk("sdh", "8:112")
			h.part("sdh", "sdh1", "8:113")
			h.udevType("8:113", c.fs)
			if got := h.scan().Reason("sdh"); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestScan_UdevOnWholeDisk(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdh", "8:112")
	h.udevType("8:112", "zfs_member")
	if got := h.scan().Reason("sdh"); got != "has a ZFS pool label" {
		t.Fatal(got)
	}
}

func TestScan_BtrfsRootBySource(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdh", "8:112")
	h.part("sdh", "sdh2", "8:114")
	h.mount("0:27", "/", "btrfs", "/dev/sdh2")
	h.stats["/dev/sdh2"] = [2]uint64{dev(0, 5), dev(8, 114)}
	r := h.scan()
	if r.All != "" {
		t.Fatal(r.All)
	}
	if got := r.Reason("sdh"); got != "holds the root filesystem (/)" {
		t.Fatal(got)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "may span more disks") {
		t.Fatalf("a btrfs mount must warn about other members: %v", r.Warnings)
	}
}

func TestScan_RootUnresolved(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdz", "8:240")
	h.mount("0:40", "/", "overlay", "overlay")
	r := h.scan()
	want := "the disk holding the root filesystem could not be determined (overlay overlay); no disk is offered"
	if r.All != want {
		t.Fatal(r.All)
	}
	if r.Reason("sdz") != want {
		t.Fatal(r.Reason("sdz"))
	}
}

func TestScan_ZfsWarns(t *testing.T) {
	h := fakeHost(t)
	h.disk("sda", "8:0")
	h.mount("0:41", "/tank", "zfs", "tank")
	h.mount("0:42", "/proc", "proc", "proc")
	r := h.scan()
	if len(r.Warnings) != 1 || !strings.HasPrefix(r.Warnings[0], "cannot tell which disks hold /tank") {
		t.Fatal(r.Warnings)
	}
	if r.Warnings[0] != "cannot tell which disks hold /tank (zfs tank); disks with a ZFS, LVM or md signature are not offered" {
		t.Fatal(r.Warnings[0])
	}
	if r.All != "" {
		t.Fatal(r.All)
	}
}

func TestScan_EscapedMountPoint(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdj", "8:144")
	h.part("sdj", "sdj1", "8:145")
	h.mount("8:145", `/mnt/my\040disk`, "ext4", "/dev/sdj1")
	eq(t, h.scan().Busy["sdj"], []string{"mounted at /mnt/my disk (sdj1)"})
}

func TestScan_LVMPhysicalVolumes(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdk", "8:160")
	h.part("sdk", "sdk1", "8:161")
	h.disk("sdl", "8:176")
	h.udevType("8:176", "LVM2_member")
	h.stats["/dev/sdk1"] = [2]uint64{dev(0, 5), dev(8, 161)}
	h.stats["/dev/sdl"] = [2]uint64{dev(0, 5), dev(8, 176)}
	h.pvs = []PV{{"/dev/sdk1", "vg0"}, {"/dev/sdl", ""}}
	r := h.scan()
	if got := r.Reason("sdk"); got != "is an LVM physical volume of volume group vg0" {
		t.Fatal(got)
	}
	eq(t, r.Busy["sdl"], []string{"is an LVM physical volume (no volume group)"})
}

func TestScan_PVsFail(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdk", "8:160")
	h.udevType("8:160", "LVM2_member")
	h.pvErr = errors.New("boom")
	r := h.scan()
	if len(r.Warnings) != 1 || !strings.HasPrefix(r.Warnings[0], "cannot list LVM physical volumes: boom; ") {
		t.Fatal(r.Warnings)
	}
	eq(t, r.Busy["sdk"], []string{"is an LVM physical volume"})
	if !r.PVsIncomplete || !strings.HasPrefix(r.Warnings[0], WarnCannotListPVs+": ") {
		t.Errorf("PVsIncomplete %v, warning %q", r.PVsIncomplete, r.Warnings[0])
	}
	h.pvErr = nil
	if h.scan().PVsIncomplete {
		t.Error("PVsIncomplete set on a scan that listed the PVs")
	}
}

// A PV that cannot be placed on a disk leaves the PV list incomplete.
func TestScan_PVUnplacedIsIncomplete(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdk", "8:160")
	h.pvs = []PV{{"/dev/mapper/gone", "vg0"}}
	if r := h.scan(); !r.PVsIncomplete {
		t.Errorf("PVsIncomplete not set: %v", r.Warnings)
	}
}

func TestScan_FreeDisk(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdi", "8:128")
	if got := h.scan().Reason("sdi"); got != "" {
		t.Fatal(got)
	}
}

func TestScan_NoMountinfo(t *testing.T) {
	h := fakeHost(t)
	o := h.opts()
	os.Remove(filepath.Join(h.proc, "self/mountinfo"))
	if _, err := Scan(o); err == nil {
		t.Fatal("want an error")
	}
}

func TestDevNumbers(t *testing.T) {
	if ma, mi := DevNumbers(0x10302); ma != 259 || mi != 2 {
		t.Fatal(ma, mi)
	}
	if ma, mi := DevNumbers(0x100805); ma != 8 || mi != 0x105 {
		t.Fatal(ma, mi)
	}
}

func TestParsePVs(t *testing.T) {
	got, err := parsePVs([]byte(`{"report":[{"pv":[{"pv_name":"/dev/sda1","vg_name":"vg0"},{"pv_name":"/dev/sdb","vg_name":""}]}]}`))
	if err != nil || len(got) != 2 || got[0] != (PV{"/dev/sda1", "vg0"}) || got[1] != (PV{"/dev/sdb", ""}) {
		t.Fatal(got, err)
	}
	if _, err := parsePVs([]byte("nope")); err == nil {
		t.Fatal("want an error")
	}
}

func TestScan_BtrfsMemberSignature(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdh", "8:112")
	h.disk("sdm", "8:192") // second member, not named in the mount source
	h.udevType("8:112", "btrfs")
	h.udevType("8:192", "btrfs")
	h.mount("0:27", "/pool", "btrfs", "/dev/sdh")
	h.stats["/dev/sdh"] = [2]uint64{dev(0, 5), dev(8, 112)}
	r := h.scan()
	want := "has a btrfs filesystem label (member of a btrfs filesystem; check with btrfs filesystem show)"
	if got := r.Busy["sdm"]; len(got) != 1 || got[0] != want {
		t.Fatal(got)
	}
	if len(r.Warnings) != 1 || strings.Contains(r.Warnings[0], "ZFS, LVM or md") {
		t.Fatal(r.Warnings)
	}
}

func TestScan_BtrfsUnresolvedWarnText(t *testing.T) {
	h := fakeHost(t)
	h.mount("0:27", "/pool", "btrfs", "UUID=abc")
	r := h.scan()
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "btrfs signature") && !strings.Contains(r.Warnings[0], "btrfs, ZFS") {
		t.Fatal(r.Warnings)
	}
}

func TestScan_BcachefsMemberSignature(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdm", "8:192")
	h.udevType("8:192", "bcachefs")
	if got := h.scan().Reason("sdm"); !strings.HasPrefix(got, "has a bcachefs filesystem label") {
		t.Fatal(got)
	}
}

func TestScan_RootUnknownToSysfs(t *testing.T) {
	h := fakeHost(t)
	h.disk("sda", "8:0")
	h.mount("8:99", "/", "ext4", "/dev/sda99")
	r := h.scan()
	if !strings.HasPrefix(r.All, "the disk holding the root filesystem could not be determined") {
		t.Fatal(r.All)
	}
}

func TestScan_SwapUnattributed(t *testing.T) {
	want := func(f string) string {
		return "an active swap area (" + f + ") could not be attributed to a disk; no disk can be offered"
	}
	t.Run("stat fails", func(t *testing.T) {
		h := fakeHost(t)
		h.disk("sdd", "8:48")
		h.swap("/dev/gone\tpartition\t100\t0\t-2")
		if r := h.scan(); r.All != want("/dev/gone") || r.Reason("sdd") != r.All {
			t.Fatal(r.All)
		}
	})
	t.Run("partition not in sysfs", func(t *testing.T) {
		h := fakeHost(t)
		h.disk("sdd", "8:48")
		h.stats["/dev/x"] = [2]uint64{dev(0, 5), dev(8, 200)}
		h.swap("/dev/x\tpartition\t100\t0\t-2")
		if r := h.scan(); r.All != want("/dev/x") {
			t.Fatal(r.All)
		}
	})
	t.Run("file on unknown fs", func(t *testing.T) {
		h := fakeHost(t)
		h.stats["/swapfile"] = [2]uint64{dev(0, 77), 0}
		h.swap("/swapfile\tfile\t100\t0\t-3")
		if r := h.scan(); r.All != want("/swapfile") {
			t.Fatal(r.All)
		}
	})
}

func TestScan_SwapEscapedFilename(t *testing.T) {
	h := fakeHost(t)
	h.disk("sde", "8:64")
	h.stats["/my swap"] = [2]uint64{dev(8, 64), 0}
	h.swap(`/my\040swap` + "\tfile\t100\t0\t-3")
	eq(t, h.scan().Busy["sde"], []string{"holds an active swap file (/my swap)"})
}

func TestScan_SwapAndPathsNegative(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdd", "8:48")
	h.disk("sde", "8:64")
	h.stats["/dev/sdd"] = [2]uint64{dev(0, 5), dev(8, 48)}
	h.stats["/bin/x"] = [2]uint64{dev(8, 48), 0}
	h.swap("/dev/sdd\tpartition\t100\t0\t-2")
	h.paths = map[string]string{"/bin/x": "the bbbench binary"}
	r := h.scan()
	if r.All != "" || r.Reason("sde") != "" {
		t.Fatal(r.All, r.Busy)
	}
	if len(r.Busy["sdd"]) != 2 {
		t.Fatal(r.Busy)
	}
}

func TestScan_PathsUnattributed(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdf", "8:80")
	h.paths = map[string]string{"/out": "the output directory"}
	r := h.scan() // Stat fails for /out
	if !strings.HasPrefix(r.All, "the output directory (/out) could not be attributed to a disk") ||
		!strings.HasSuffix(r.All, "no disk can be offered") {
		t.Fatal(r.All)
	}
	h2 := fakeHost(t)
	h2.disk("sdf", "8:80")
	h2.stats["/out"] = [2]uint64{dev(0, 77), 0}
	h2.paths = map[string]string{"/out": "the output directory"}
	if r := h2.scan(); r.All != "the output directory (/out) could not be attributed to a disk; no disk can be offered" {
		t.Fatal(r.All)
	}
}

func TestScan_PVStatFails(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdk", "8:160")
	h.pvs = []PV{{"/dev/missing", "vg0"}}
	r := h.scan()
	if len(r.Warnings) != 1 || !strings.HasPrefix(r.Warnings[0], "cannot tell which disk holds the LVM physical volume /dev/missing") {
		t.Fatal(r.Warnings)
	}
}

func TestScan_UnknownSlave(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdb", "8:16")
	h.stack("dm-0", "253:0", "sdb1", "ghost") // ghost is not in class/block
	h.part("sdb", "sdb1", "8:17")
	h.mount("253:0", "/home", "ext4", "/dev/mapper/x")
	r := h.scan()
	if !strings.Contains(r.All, "ghost") {
		t.Fatal(r.All)
	}
	for _, reasons := range r.Busy {
		_ = reasons
	}
	if _, ok := r.Busy["ghost"]; ok {
		t.Fatal("slave name must not become a disk")
	}
}

func TestScan_MissingUdevAndSwaps(t *testing.T) {
	h := fakeHost(t)
	h.disk("sda", "8:0")
	os.RemoveAll(h.udev)
	r := h.scan()
	if r.All != "" || r.Reason("sda") != "" {
		t.Fatal(r.All, r.Busy)
	}
}

func fakeLVM(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lvm"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestListPVs(t *testing.T) {
	t.Run("no lvm", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if pvs, err := listPVs(); pvs != nil || err != nil {
			t.Fatal(pvs, err)
		}
	})
	t.Run("non-zero exit", func(t *testing.T) {
		fakeLVM(t, "exit 5")
		if _, err := listPVs(); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("output", func(t *testing.T) {
		fakeLVM(t, `echo '{"report":[{"pv":[{"pv_name":"/dev/sda1","vg_name":"vg0"}]}]}'`)
		pvs, err := listPVs()
		if err != nil || len(pvs) != 1 || pvs[0] != (PV{"/dev/sda1", "vg0"}) {
			t.Fatal(pvs, err)
		}
	})
}

// TestScan_Carries: an unmounted filesystem or a partition table does not
// make a disk busy, but the report says what the disk carries.
func TestScan_Carries(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdb", "8:16")
	h.write(filepath.Join(h.udev, "b8:16"), "E:ID_PART_TABLE_TYPE=gpt\n")
	h.part("sdb", "sdb1", "8:17")
	h.udevType("8:17", "ext4")
	h.part("sdb", "sdb2", "8:18")
	h.udevType("8:18", "swap")
	h.disk("sdc", "8:32")
	h.udevType("8:32", "xfs")
	h.disk("sdd", "8:48") // no udev data: nothing to say
	h.disk("sde", "8:64")
	h.part("sde", "sde1", "8:65")
	h.udevType("8:65", "zfs_member") // busy, said by Busy, not Carries
	r := h.scan()
	for _, d := range []string{"sdb", "sdc", "sdd"} {
		if why := r.Reason(d); why != "" {
			t.Errorf("%s busy: %s", d, why)
		}
	}
	want := map[string]string{"sdb": "gpt partition table, ext4 on sdb1, swap on sdb2", "sdc": "xfs on sdc"}
	if fmt.Sprint(r.Carries) != fmt.Sprint(want) {
		t.Errorf("carries %v\nwant    %v", r.Carries, want)
	}
}

func TestCausesMirrorBusy(t *testing.T) {
	h := fakeHost(t)
	h.disk("sda", "8:0")
	h.part("sda", "sda2", "8:2")
	h.mount("8:2", "/data", "ext4", "/dev/sda2")
	h.disk("sdd", "8:48")
	h.part("sdd", "sdd1", "8:49")
	h.stats["/dev/sdd1"] = [2]uint64{dev(0, 5), dev(8, 49)}
	h.swap("/dev/sdd1\tpartition\t100\t0\t-2")
	h.disk("sdf", "8:80")
	h.stats["/usr/local/bin/bbbench"] = [2]uint64{dev(8, 80), 0}
	h.paths = map[string]string{"/usr/local/bin/bbbench": "the bbbench binary"}
	h.disk("sdg", "8:96")
	h.holder("sdg", "md127")
	h.disk("sdb", "8:16")
	h.stack("dm-0", "253:0", "sdb")
	h.holder("sdb", "dm-0")
	h.disk("sdk", "8:160")
	h.part("sdk", "sdk1", "8:161")
	h.stats["/dev/sdk1"] = [2]uint64{dev(0, 5), dev(8, 161)}
	h.pvs = []PV{{"/dev/sdk1", "vg0"}}
	h.udevType("8:161", "LVM2_member") // also signature, deduped against the PV
	h.disk("sdh", "8:112")
	h.part("sdh", "sdh1", "8:113")
	h.udevTypeUUID("8:113", "linux_raid_member", "a1b2c3d4-0000-1111-2222-333344445555")
	h.disk("sdi", "8:128")
	h.udevType("8:128", "zfs_member")
	r := h.scan()

	want := map[string][]Cause{
		"sda": {{KindMount, "sda", "/data", "sda2", "mounted at /data (sda2)"}},
		"sdd": {{KindSwap, "sdd", "/dev/sdd1", "sdd1", "active swap (/dev/sdd1)"}},
		"sdf": {{KindPath, "sdf", "/usr/local/bin/bbbench", "sdf", "holds the bbbench binary (/usr/local/bin/bbbench)"}},
		"sdg": {{KindHolder, "sdg", "md127", "md127", "member of md127 (md RAID)"}},
		"sdb": {{KindHolder, "sdb", "dm-0", "dm-0", "used by dm-0 (device mapper: LVM, LUKS or multipath)"}},
		"sdk": {{KindPV, "sdk", "vg0", "sdk1", "is an LVM physical volume of volume group vg0"}},
		"sdh": {{KindMD, "sdh", "a1b2c3d4-0000-1111-2222-333344445555", "sdh1", "has an md RAID superblock (an array that is not assembled; check with mdadm --examine)"}},
		"sdi": {{KindSignature, "sdi", "", "sdi", "has a ZFS pool label"}},
	}
	for disk, w := range want {
		got := r.Causes[disk]
		if len(got) != len(w) {
			t.Fatalf("%s: causes %v want %v", disk, got, w)
		}
		for i := range w {
			if got[i] != w[i] {
				t.Errorf("%s[%d]: %+v want %+v", disk, i, got[i], w[i])
			}
		}
	}
	for disk, reasons := range r.Busy {
		if len(r.Causes[disk]) != len(reasons) {
			t.Fatalf("%s: %d causes for %d reasons", disk, len(r.Causes[disk]), len(reasons))
		}
		for i, text := range reasons {
			if r.Causes[disk][i].Text != text || r.Causes[disk][i].Disk != disk {
				t.Errorf("%s[%d]: %+v does not mirror %q", disk, i, r.Causes[disk][i], text)
			}
		}
	}
}

func TestBlockersFiltersOwned(t *testing.T) {
	r := &Report{Causes: map[string][]Cause{
		"sda": {{Kind: KindHolder, Disk: "sda", Via: "md127", Text: "a"}, {Kind: KindMount, Disk: "sda", Via: "/x", Text: "b"}},
		"sdb": {{Kind: KindHolder, Disk: "sdb", Via: "md127", Text: "c"}},
		"sdc": {{Kind: KindSwap, Disk: "sdc", Text: "d"}},
	}}
	owned := func(c Cause) bool { return c.Kind == KindHolder && c.Via == "md127" }
	got := r.Blockers([]string{"sdb", "sda", "sdz"}, owned)
	if len(got) != 1 || got[0].Text != "b" {
		t.Fatalf("got %+v", got)
	}
	if got := r.Blockers([]string{"sda", "sdb"}, func(Cause) bool { return true }); len(got) != 0 {
		t.Fatalf("all owned: %+v", got)
	}
	r.All = "no disk can be offered"
	got = r.Blockers([]string{"sdb"}, func(Cause) bool { return true })
	if len(got) != 1 || got[0].Kind != KindAll || got[0].Text != "no disk can be offered" {
		t.Fatalf("All case: %+v", got)
	}
}

func TestCausesDevice(t *testing.T) {
	h := fakeHost(t)
	h.disk("sda", "8:0")
	h.part("sda", "sda1", "8:1")
	h.part("sda", "sda2", "8:2")
	h.disk("sdb", "8:16")
	h.stack("md127", "9:127", "sda1", "sdb")
	h.stack("md128", "9:128", "sda2")
	h.holder("sda/sda1", "md127")
	h.holder("sdb", "md127")
	h.holder("sda/sda2", "md128")
	h.mount("9:127", "/", "ext4", "/dev/md127")
	h.mount("9:128", "/srv", "ext4", "/dev/md128")
	h.udevType("8:1", "linux_raid_member")
	h.disk("sdc", "8:32")
	h.stack("dm-3", "253:3", "sdc")
	h.stats["/dev/mapper/vg-swap"] = [2]uint64{dev(0, 5), dev(253, 3)}
	h.swap("/dev/mapper/vg-swap\tpartition\t100\t0\t-2")
	r := h.scan()

	for _, d := range []string{"sda", "sdb"} {
		found := false
		for _, c := range r.Causes[d] {
			if c.Kind == KindMount && c.Via == "/" && c.Device == "md127" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no root mount cause on md127: %+v", d, r.Causes[d])
		}
	}
	var got []string
	for _, c := range r.Causes["sda"] {
		got = append(got, string(c.Kind)+":"+c.Via+":"+c.Device)
	}
	want := []string{"mount:/:md127", "mount:/srv:md128", "holder:md127:md127", "holder:md128:md128", "md::sda1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sda causes %v want %v", got, want)
	}
	c := r.Causes["sdc"]
	if len(c) == 0 || c[0].Kind != KindSwap || c[0].Device != "dm-3" || c[0].Via != "/dev/mapper/vg-swap" {
		t.Fatalf("sdc: %+v", c)
	}
}

func TestBlockersAllWinsAndDedupes(t *testing.T) {
	r := &Report{All: "x", Causes: map[string][]Cause{"sda": {{Kind: KindSwap, Disk: "sda"}}}}
	got := r.Blockers([]string{"sda"}, func(Cause) bool { return false })
	if len(got) != 1 || got[0].Kind != KindAll {
		t.Fatalf("%+v", got)
	}
	r.All = ""
	got = r.Blockers([]string{"sda", "sda"}, func(Cause) bool { return false })
	if len(got) != 1 {
		t.Fatalf("repeated disk repeated causes: %+v", got)
	}
}

// Two partitions with the same kind of signature are two causes (each with
// its node and uuid), while Reason still names the reason once.
func TestCausesKeepEveryNode(t *testing.T) {
	h := fakeHost(t)
	h.disk("sdb", "8:16")
	h.part("sdb", "sdb1", "8:17")
	h.part("sdb", "sdb2", "8:18")
	h.udevTypeUUID("8:17", "linux_raid_member", "uuid-a")
	h.udevTypeUUID("8:18", "linux_raid_member", "uuid-b")
	h.disk("sdc", "8:32")
	h.part("sdc", "sdc1", "8:33")
	h.part("sdc", "sdc2", "8:34")
	h.udevType("8:33", "LVM2_member")
	h.udevType("8:34", "crypto_LUKS")
	r := h.scan()
	var got []string
	for _, c := range append(r.Causes["sdb"], r.Causes["sdc"]...) {
		got = append(got, string(c.Kind)+":"+c.Via+":"+c.Device)
	}
	want := []string{"md:uuid-a:sdb1", "md:uuid-b:sdb2", "pv::sdc1", "signature::sdc2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("causes %v want %v", got, want)
	}
	if got := r.Reason("sdb"); got != "has an md RAID superblock (an array that is not assembled; check with mdadm --examine)" {
		t.Errorf("reason %q", got)
	}
	if len(r.Busy["sdb"]) != len(r.Causes["sdb"]) {
		t.Errorf("busy %v causes %v", r.Busy["sdb"], r.Causes["sdb"])
	}
}
