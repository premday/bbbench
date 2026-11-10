package main

import (
	"bufio"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	distassets "bbbench/dist"
	"bbbench/internal/blockdev"
	"bbbench/internal/config"
	"bbbench/internal/diskinfo"
	"bbbench/internal/flushtrace"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
	"bbbench/internal/templating"
)

// fakeArraySys writes <sys>/<name>/size and queue/logical_block_size.
func fakeArraySys(t *testing.T, name, size, lbs string) string {
	t.Helper()
	sys := t.TempDir()
	q := filepath.Join(sys, name, "queue")
	if err := os.MkdirAll(q, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, name, "size"), []byte(size+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q, "logical_block_size"), []byte(lbs+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sys
}

func memberDisk(name, serial string, role settings.Role) settings.Disk {
	return settings.Disk{Target: settings.Target{Name: name, Path: "/dev/" + name, Serial: serial, Model: "M"}, Role: role}
}

func memberDrives(rot map[string]bool) map[string]DriveInfo {
	out := map[string]DriveInfo{}
	for name, r := range rot {
		out[name] = DriveInfo{Device: blockdev.Device{Name: name, Path: "/dev/" + name, Serial: "S-" + name, Model: "M", Rotational: r}}
	}
	return out
}

func composite(name, iface string, members ...settings.Disk) settings.BenchTarget {
	return settings.BenchTarget{
		Kind:    settings.TargetArray,
		Target:  settings.Target{Name: name, Path: "/dev/" + name, Serial: "0a:0b:0c:0d", Model: "raid1", Vendor: iface, Interface: iface},
		UUID:    "0a:0b:0c:0d",
		Members: members,
	}
}

func TestArrayDrive_FromMembers(t *testing.T) {
	for _, tc := range []struct{ name, iface string }{{"md127", "md"}, {"dm-3", "lvm"}} {
		sys := fakeArraySys(t, tc.name, "2000", "4096")
		bt := composite(tc.name, tc.iface, memberDisk("sdb", "S-sdb", settings.RoleData), memberDisk("sdc", "S-sdc", settings.RoleData))
		var got blockdev.Device
		gen := func(d blockdev.Device) (string, error) {
			got = d
			return "/out/h/" + d.Name + "_config.fio", nil
		}
		di, err := arrayDrive(bt, memberDrives(map[string]bool{"sdb": false, "sdc": false}), sys, gen)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		d := di.Device
		if d.Name != tc.name || d.Path != "/dev/"+tc.name || d.Serial != "0a:0b:0c:0d" || d.Model != "raid1" || d.Vendor != tc.iface {
			t.Errorf("%s: identity = %+v", tc.name, d)
		}
		if d.Rotational || d.CapacityBytes != 1024000 || d.BlockSize != 4096 || d.BlockCount != 2000 || d.ParentSubsystem != tc.iface {
			t.Errorf("%s: device = %+v", tc.name, d)
		}
		if got != d {
			t.Errorf("%s: gen got %+v, want %+v", tc.name, got, d)
		}
		want := []rungroup.Member{{Name: "sdb", Serial: "S-sdb", Model: "M", Role: settings.RoleData}, {Name: "sdc", Serial: "S-sdc", Model: "M", Role: settings.RoleData}}
		if len(di.Members) != 2 || di.Members[0] != want[0] || di.Members[1] != want[1] {
			t.Errorf("%s: members = %+v", tc.name, di.Members)
		}
		if di.UUID != "0a:0b:0c:0d" || di.FioFilePath != "/out/h/"+tc.name+"_config.fio" || !di.FioExists || !di.IsArray() {
			t.Errorf("%s: drive = %+v", tc.name, di)
		}
	}
}

func TestArrayDrive_MixedMediaRefused(t *testing.T) {
	sys := fakeArraySys(t, "md127", "2000", "512")
	bt := composite("md127", "md", memberDisk("sdb", "S-sdb", settings.RoleData), memberDisk("sdc", "S-sdc", settings.RoleData))
	called := false
	gen := func(blockdev.Device) (string, error) { called = true; return "x", nil }
	_, err := arrayDrive(bt, memberDrives(map[string]bool{"sdb": true, "sdc": false}), sys, gen)
	if err == nil || err.Error() != "mixed media: sdb is HDD, sdc is SSD" {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Error("a fio file was generated for a refused target")
	}
}

func TestArrayDrive_JournalAndSpareIgnoredForMedia(t *testing.T) {
	sys := fakeArraySys(t, "md127", "2000", "512")
	bt := composite("md127", "md",
		memberDisk("sdb", "S-sdb", settings.RoleData), memberDisk("sdc", "S-sdc", settings.RoleData),
		memberDisk("nvme0n1", "S-nvme0n1", settings.RoleJournal), memberDisk("sdd", "S-sdd", settings.RoleSpare))
	gen := func(blockdev.Device) (string, error) { return "x", nil }
	di, err := arrayDrive(bt, memberDrives(map[string]bool{"sdb": true, "sdc": true, "nvme0n1": false, "sdd": false}), sys, gen)
	if err != nil {
		t.Fatal(err)
	}
	if !di.Device.Rotational || di.Device.WorkloadType() != "hdd" {
		t.Errorf("device = %+v, want rotational", di.Device)
	}
	if len(di.Members) != 4 || di.Members[2].Role != settings.RoleJournal || di.Members[3].Role != settings.RoleSpare {
		t.Errorf("members = %+v", di.Members)
	}
}

func TestArrayDrive_UnknownMember(t *testing.T) {
	sys := fakeArraySys(t, "md127", "2000", "512")
	bt := composite("md127", "md", memberDisk("sdb", "S-sdb", settings.RoleData), memberDisk("sdx", "S-sdx", settings.RoleData))
	gen := func(blockdev.Device) (string, error) { return "x", nil }
	_, err := arrayDrive(bt, memberDrives(map[string]bool{"sdb": false}), sys, gen)
	if err == nil || err.Error() != "member sdx is not a discovered drive" {
		t.Fatalf("err = %v", err)
	}
}

// fakeDiskSys writes a discoverable disk (model, serial, vendor, subsystem link).
func fakeDiskSys(t *testing.T, sys, name, vendor, model, serial, subsystem string) {
	t.Helper()
	dev := filepath.Join(sys, name, "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"vendor": vendor, "model": model, "serial": serial} {
		if err := os.WriteFile(filepath.Join(dev, f), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../../bus/"+subsystem, filepath.Join(dev, "subsystem")); err != nil {
		t.Fatal(err)
	}
}

func TestFioFileName_MatchesDiscovery(t *testing.T) {
	sys := t.TempDir()
	fakeDiskSys(t, sys, "sda", "ATA ", "Some Model/X", "S N 1", "scsi")
	fakeDiskSys(t, sys, "sdb", "", "M2", "S2", "scsi")
	cfg := &config.Root{}
	cfg.Fio.Generated.Path = t.TempDir()
	drives, err := discoverDrivesWithFioFiles(cfg, "host1", sys, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(drives) != 2 {
		t.Fatalf("drives = %+v", drives)
	}
	for _, d := range drives {
		want := filepath.Join(cfg.Fio.Generated.Path, "host1", fioFileName("host1", d.Device))
		if d.FioFilePath != want {
			t.Errorf("%s: discovery %q, fioFileName %q", d.Device.Name, d.FioFilePath, want)
		}
	}
	// The pattern itself, unchanged: unknown for an empty vendor.
	if got := fioFileName("h", blockdev.Device{Model: "M2", Serial: "S2", ParentSubsystem: "scsi"}); got != "h_unknown_M2_S2_scsi_config.fio" {
		t.Errorf("fioFileName = %q", got)
	}
}

func TestFioGenerator_ArrayFile(t *testing.T) {
	cfg, err := config.LoadFS(distassets.FS, "default.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Fio.Generated.Path = t.TempDir()
	eng, err := templating.DefaultEngine()
	if err != nil {
		t.Fatal(err)
	}
	old := logger
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() { logger = old })
	// The generator prints the paths it writes, like generate.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; devnull.Close() })
	gen := fioGenerator(cfg, eng, "host1")
	for _, tc := range []struct{ name, iface, suffix string }{{"md127", "md", "_md_config.fio"}, {"dm-3", "lvm", "_lvm_config.fio"}} {
		sys := fakeArraySys(t, tc.name, "2000000", "512")
		bt := composite(tc.name, tc.iface, memberDisk("sdb", "S-sdb", settings.RoleData), memberDisk("sdc", "S-sdc", settings.RoleData))
		di, err := arrayDrive(bt, memberDrives(map[string]bool{"sdb": false, "sdc": false}), sys, gen)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if filepath.Dir(di.FioFilePath) != filepath.Join(cfg.Fio.Generated.Path, "host1") || !strings.HasSuffix(di.FioFilePath, tc.suffix) {
			t.Errorf("%s: path = %q", tc.name, di.FioFilePath)
		}
		b, err := os.ReadFile(di.FioFilePath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "filename=/dev/"+tc.name+"\n") {
			t.Errorf("%s: fio file lacks filename=/dev/%s:\n%s", tc.name, tc.name, b)
		}
		// The ssd workloads (members are not rotational) and a parseable file.
		if _, err := parseFioFile(di.FioFilePath); err != nil {
			t.Errorf("%s: parse: %v", tc.name, err)
		}
	}
}

func TestCollectDiskConfig_ArrayMembers(t *testing.T) {
	ResetStatus()
	defer ResetStatus()
	arr := DriveInfo{
		Device:  blockdev.Device{Name: "md127", Serial: "0a:0b", Model: "raid1"},
		Members: []rungroup.Member{{Name: "sdb", Serial: "S1", Model: "M", Role: settings.RoleData}, {Name: "sdc", Serial: "S2", Model: "M", Role: settings.RoleData}},
		UUID:    "0a:0b",
	}
	UpdateStatus(func(s *BenchmarkStatus) { s.Drives = newDriveStatuses([]DriveInfo{arr}, nil) })
	before := GetStatus().Drives[0].Members

	called := make(chan DriveInfo, 4)
	ec := &ExecutionCoordinator{
		drives:    []DriveInfo{arr},
		outputDir: t.TempDir(),
		startTime: time.Unix(1793969936, 0),
		collectDisk: func(d DriveInfo) (diskinfo.Info, string) {
			called <- d
			return diskinfo.Info{Interface: "iface-" + d.Device.Name}, "dump of " + d.Device.Name + "\n"
		},
	}
	ec.collectDiskConfig()

	deadline := time.Now().Add(2 * time.Second)
	for {
		d := GetStatus().Drives[0]
		if len(d.Members) == 2 && d.Members[0].Config != nil && d.Members[1].Config != nil {
			if d.Members[0].Config.Interface != "iface-sdb" || d.Members[1].Config.Interface != "iface-sdc" {
				t.Fatalf("member configs mixed up: %+v %+v", d.Members[0].Config, d.Members[1].Config)
			}
			if d.Config != nil {
				t.Errorf("the composite got a config: %+v", d.Config)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("member configs never arrived: %+v", d)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if before[0].Config != nil || before[1].Config != nil {
		t.Error("the members slice a reader held was changed in place")
	}
	names := map[string]DriveInfo{}
	for i := 0; i < 2; i++ {
		d := <-called
		names[d.Device.Name] = d
	}
	select {
	case d := <-called:
		t.Errorf("collect called for %s too", d.Device.Name)
	default:
	}
	if names["sdb"].Device.Serial != "S1" || names["sdc"].Device.Model != "M" || len(names) != 2 {
		t.Errorf("collect calls = %+v", names)
	}

	for _, dev := range []string{"sdb", "sdc"} {
		path := filepath.Join(ec.outputDir, dev+"_diskinfo_1793969936.txt")
		var b []byte
		for time.Now().Before(deadline) {
			if b, _ = os.ReadFile(path); len(b) > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if string(b) != "dump of "+dev+"\n" {
			t.Errorf("%s: dump file = %q", path, b)
		}
	}
	if _, err := os.Stat(filepath.Join(ec.outputDir, "md127_diskinfo_1793969936.txt")); err == nil {
		t.Error("a dump was written for the composite")
	}
}

// UNVERIFIED on hardware: md and dm are bio-based, so the request events of
// the Block collector may never fire for them. The phase then has a Flush
// summary with Count 0, which the console prints as "none seen" and the
// graphs page explains with "ftrace saw 0 Flush commands" (flushNone).
func TestArrayFlush_NoRequestEventsShowsZeroFlushNote(t *testing.T) {
	sum := &flushtrace.Summary{Source: "block", Count: 0}
	if got := formatFlushSummary(sum); got != "Flush (block): none seen" {
		t.Errorf("console = %q", got)
	}
	r := &BenchmarkResult{
		Drives: []DriveInfo{{
			Device:  blockdev.Device{Name: "md127", Path: "/dev/md127", Model: "raid1", Serial: "0a:0b"},
			Members: []rungroup.Member{{Name: "sdb", Serial: "S1", Model: "M", Role: settings.RoleData}},
		}},
		Phases: []PhaseResult{{
			PhaseNumber: 1, PhaseName: "fsync",
			Jobs:       []JobResult{{Device: "md127", WriteStats: &IOStats{IOPS: 10}}},
			FlushTrace: map[string]*flushtrace.Summary{"md127": sum},
		}},
	}
	data := generateDiskPhaseData(r)
	if len(data) != 1 || data[0].Flush == nil || data[0].Flush.Count != 0 || data[0].Kind != "array" || len(data[0].Members) != 1 {
		t.Fatalf("graph data = %+v", data)
	}
	r.ID = "arr-flush-1"
	globalResultsStore.Clear()
	globalResultsStore.AddResult(r)
	defer globalResultsStore.Clear()
	w := httptest.NewRecorder()
	handleGraphsPage(w, httptest.NewRequest("GET", "/graphs?id=arr-flush-1", nil))
	if page := w.Body.String(); !strings.Contains(page, "'ftrace saw 0 Flush commands'") || !strings.Contains(page, "d.flush.count > 0") {
		t.Error("the graphs page no longer explains a traced phase with 0 Flushes")
	}
}

// The core (the group loop, the targets, the web pages) must not name a
// layout backend: backends live behind settings.Backend.
func TestCoreDoesNotNameTheBackend(t *testing.T) {
	files, err := filepath.Glob("../../internal/rungroup/*.go")
	if err != nil {
		t.Fatal(err)
	}
	serve, err := filepath.Glob("serve_*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "group_run.go", "group_flow.go", "group_undo.go", "group_targets.go", "group_layout_tui.go")
	files = append(files, serve...)
	banned := regexp.MustCompile(`(?i)mdadm|lvm|pvcreate|vgcreate|lvcreate`)
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		fh, err := os.Open(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for n := 1; sc.Scan(); n++ {
			for _, w := range banned.FindAllString(sc.Text(), -1) {
				t.Errorf("%s:%d names the backend: %q", f, n, w)
			}
		}
		if err := sc.Err(); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		fh.Close()
		checked++
	}
	if checked < 7 {
		t.Errorf("only %d files checked", checked)
	}
}
