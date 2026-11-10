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
	if len(r.Warnings) != 0 {
		t.Fatalf("a resolved mount must not warn: %v", r.Warnings)
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
