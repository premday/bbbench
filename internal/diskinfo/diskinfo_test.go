package diskinfo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// put writes files (path -> content) under root.
func put(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func link(t *testing.T, root, target, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
}

// fakeRun answers commands from a map keyed by "name arg arg...".
func fakeRun(answers map[string]string) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if out, ok := answers[name+" "+strings.Join(args, " ")]; ok {
			return []byte(out), nil
		}
		return nil, errors.New("not found: " + name)
	}
}

var spaces = regexp.MustCompile(` +`)

const smartctlNVMe = `{"smartctl":{"messages":[]},"smart_status":{"passed":true},
"temperature":{"current":41},"power_on_time":{"hours":1234},
"nvme_smart_health_information_log":{"percentage_used":3,"media_errors":0}}`

func TestCollect_NVMe(t *testing.T) {
	sys := t.TempDir()
	pci := "devices/pci0000:00/0000:00:01.0/0000:01:00.0"
	put(t, sys, map[string]string{
		"block/nvme0n1/queue/write_cache":    "write back",
		"class/nvme/nvme0/transport":         "pcie",
		pci + "/current_link_speed":          "8.0 GT/s PCIe",
		pci + "/current_link_width":          "4",
		pci + "/max_link_speed":              "16.0 GT/s PCIe",
		pci + "/max_link_width":              "4",
		pci + "/power/control":               "auto",
		pci + "/power/runtime_status":        "active",
		pci + "/link/l1_aspm":                "1",
		pci + "/link/l0s_aspm":               "0",
		"module/pcie_aspm/parameters/policy": "default performance [powersave] powersupersave",
	})
	link(t, sys, filepath.Join(sys, pci), "class/nvme/nvme0/device")

	got := Collect("nvme0n1", Options{Sys: sys, Run: fakeRun(map[string]string{
		"nvme get-feature /dev/nvme0 -f 0x02": "get-feature:0x02 (Power Management), Current value:0x000002",
		"nvme get-feature /dev/nvme0 -f 0x0c": "get-feature:0x0c (Autonomous Power State Transition), Current value:0x000001",
		"nvme get-feature /dev/nvme0 -f 0x06": "get-feature:0x06 (Volatile Write Cache), Current value:0x000000",
		"smartctl -j -H -A /dev/nvme0n1":      smartctlNVMe,
	})})

	if got.Interface != "NVMe (PCIe)" || got.LinkSpeed != "8.0 GT/s PCIe x4" || got.LinkMax != "16.0 GT/s PCIe x4" || !got.LinkBelowMax {
		t.Errorf("link: %+v", got)
	}
	if got.Power.RuntimePM != "auto (active)" || got.Power.ASPM != "policy powersave; L0s off, L1 on" ||
		got.Power.NVMeState != "PS2" || got.Power.APST != "enabled" {
		t.Errorf("power: %+v", got.Power)
	}
	if got.WriteCache.Kernel != "write back" || got.WriteCache.Drive != "disabled" {
		t.Errorf("write cache: %+v", got.WriteCache)
	}
	s := got.Smart
	if s.Status != "PASSED" || s.TempC == nil || *s.TempC != 41 || s.PercentUsed == nil || *s.PercentUsed != 3 || *s.PowerOnHours != 1234 {
		t.Errorf("smart: %+v", s)
	}
}

func TestCollect_NVMeLinkAtMaxIsNotFlagged(t *testing.T) {
	sys := t.TempDir()
	pci := "devices/pci0000:00/0000:01:00.0"
	put(t, sys, map[string]string{
		"block/nvme1n1/queue/write_cache": "write through",
		"class/nvme/nvme1/transport":      "pcie",
		pci + "/current_link_speed":       "16.0 GT/s PCIe",
		pci + "/current_link_width":       "4",
		pci + "/max_link_speed":           "16.0 GT/s PCIe",
		pci + "/max_link_width":           "4",
	})
	link(t, sys, filepath.Join(sys, pci), "class/nvme/nvme1/device")
	got := Collect("nvme1n1", Options{Sys: sys, Run: fakeRun(nil)})
	if got.LinkBelowMax || got.LinkMax != "" || got.LinkSpeed != "16.0 GT/s PCIe x4" {
		t.Errorf("%+v", got)
	}
	// No nvme-cli and no smartctl: nothing breaks, the gaps say why.
	if got.WriteCache.Kernel != "write through" || got.WriteCache.Drive != "" || got.Smart.Status != "" || got.Smart.Detail == "" {
		t.Errorf("%+v", got)
	}
}

func TestCollect_SATA(t *testing.T) {
	sys := t.TempDir()
	real := "devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/block/sda"
	put(t, sys, map[string]string{
		real + "/queue/write_cache":                          "write back",
		real + "/device/power/control":                       "on",
		"class/ata_link/link1/sata_spd":                      "6.0 Gbps",
		"class/scsi_host/host0/link_power_management_policy": "med_power_with_dipm",
	})
	link(t, sys, filepath.Join(sys, real), "block/sda")

	got := Collect("sda", Options{Sys: sys,
		DriveWCE: func(dev string) (bool, error) { return dev == "/dev/sda", nil },
		Run: fakeRun(map[string]string{"smartctl -j -H -A /dev/sda": `{"smart_status":{"passed":false},
"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":8}},{"id":197,"raw":{"value":2}},{"id":9,"raw":{"value":100}}]}}`}),
	})
	if got.Interface != "SATA" || got.LinkSpeed != "6.0 Gbps" {
		t.Errorf("link: %+v", got)
	}
	if got.Power.RuntimePM != "on" || got.Power.LinkPolicy != "med_power_with_dipm" {
		t.Errorf("power: %+v", got.Power)
	}
	if got.WriteCache.Drive != "enabled" || got.WriteCache.Kernel != "write back" {
		t.Errorf("write cache: %+v", got.WriteCache)
	}
	if got.Smart.Status != "FAILED" || *got.Smart.Reallocated != 8 || *got.Smart.Pending != 2 {
		t.Errorf("smart: %+v", got.Smart)
	}
}

func TestCollect_SASAndUSB(t *testing.T) {
	sys := t.TempDir()
	sas := "devices/pci0000:00/0000:00:02.0/host2/port-2:0/end_device-2:0/target2:0:0/2:0:0:0/block/sdb"
	put(t, sys, map[string]string{
		sas + "/queue/write_cache":                  "write back",
		"class/sas_phy/phy-2:0/negotiated_linkrate": "6.0 Gbit",
		"class/sas_phy/phy-2:0/maximum_linkrate":    "12.0 Gbit",
	})
	link(t, sys, filepath.Join(sys, sas), "block/sdb")
	got := Collect("sdb", Options{Sys: sys, Run: fakeRun(nil), DriveWCE: func(string) (bool, error) { return false, errors.New("x") }})
	if got.Interface != "SAS" || got.LinkSpeed != "6.0 Gbit" || got.LinkMax != "12.0 Gbit" || !got.LinkBelowMax || got.WriteCache.Drive != "" {
		t.Errorf("sas: %+v", got)
	}

	usb := "devices/pci0000:00/0000:00:14.0/usb2/2-1/2-1:1.0/host6/target6:0:0/6:0:0:0/block/sdc"
	put(t, sys, map[string]string{usb + "/queue/write_cache": "write through", "devices/pci0000:00/0000:00:14.0/usb2/2-1/speed": "5000"})
	link(t, sys, filepath.Join(sys, usb), "block/sdc")
	got = Collect("sdc", Options{Sys: sys, Run: fakeRun(nil), DriveWCE: func(string) (bool, error) { return false, errors.New("x") }})
	if got.Interface != "USB" || got.LinkSpeed != "5000 Mb/s" {
		t.Errorf("usb: %+v", got)
	}
}

func TestParseSmartctl(t *testing.T) {
	if _, ok := parseSmartctl([]byte("not json")); ok {
		t.Error("garbage must not parse")
	}
	// A device smartctl cannot read keeps smartctl's own explanation.
	s, ok := parseSmartctl([]byte(`{"smartctl":{"messages":[{"string":"Permission denied"}]}}`))
	if !ok || s.Status != "" || s.Detail != "Permission denied" {
		t.Errorf("%+v ok=%v", s, ok)
	}
}

func TestParseNVMeSmartLog(t *testing.T) {
	s, ok := parseNVMeSmartLog([]byte(`{"critical_warning":0,"temperature":314,"percent_used":7,"media_errors":0,"power_on_hours":50}`))
	if !ok || s.Status != "PASSED" || *s.TempC != 41 || *s.PercentUsed != 7 || s.Source != "nvme smart-log" {
		t.Errorf("%+v ok=%v", s, ok)
	}
	s, _ = parseNVMeSmartLog([]byte(`{"critical_warning":4}`))
	if s.Status != "FAILED" {
		t.Errorf("critical warning must fail: %+v", s)
	}
	if _, ok := parseNVMeSmartLog([]byte(`{}`)); ok {
		t.Error("no critical_warning field is not a health report")
	}
}

// Without smartmontools an NVMe disk falls back to nvme-cli.
func TestCollect_NVMeSmartFallback(t *testing.T) {
	sys := t.TempDir()
	put(t, sys, map[string]string{"block/nvme0n1/queue/write_cache": "write back"})
	got := Collect("nvme0n1", Options{Sys: sys, Run: fakeRun(map[string]string{
		"nvme smart-log /dev/nvme0 -o json": `{"critical_warning":0,"temperature":300,"percent_used":1}`,
	})})
	if got.Smart.Status != "PASSED" || got.Smart.Source != "nvme smart-log" {
		t.Errorf("%+v", got.Smart)
	}
}

// The dump holds the summary, every sysfs value read and the full output of
// each command, including the identify and SMART reports only the dump runs.
func TestCollectDump_NVMe(t *testing.T) {
	sys := t.TempDir()
	put(t, sys, map[string]string{
		"block/nvme0n1/queue/write_cache": "write back",
		"class/nvme/nvme0/transport":      "pcie",
	})
	_, dump := CollectDump("nvme0n1", Options{Sys: sys, Label: "nvme0n1 - Acme X1, S/N 42", Run: fakeRun(map[string]string{
		"nvme get-feature /dev/nvme0 -f 0x06": "get-feature:0x06 (Volatile Write Cache), Current value:0x000001",
		"smartctl -j -H -A /dev/nvme0n1":      smartctlNVMe,
		"smartctl -x /dev/nvme0n1":            "FULL SMARTCTL REPORT",
		"nvme id-ctrl /dev/nvme0":             "vwc : 0x1",
		"nvme id-ns /dev/nvme0n1":             "nsze : 0x100",
		"nvme smart-log /dev/nvme0":           "critical_warning : 0",
	})})
	dump = spaces.ReplaceAllString(dump, " ") // the summary aligns its columns
	for _, want := range []string{
		"bbbench disk configuration: nvme0n1 - Acme X1, S/N 42", "never changes the drive",
		"== Summary ==", "Interface: NVMe (PCIe)", "Kernel cache: write back", "Drive cache: enabled", "SMART: PASSED (smartctl)", "Temperature: 41 C",
		"== sysfs ==", "block/nvme0n1/queue/write_cache = write back",
		"== smartctl -x /dev/nvme0n1 ==\nFULL SMARTCTL REPORT", "== nvme id-ctrl /dev/nvme0 ==\nvwc : 0x1",
		"== nvme id-ns /dev/nvme0n1 ==", "== nvme smart-log /dev/nvme0 ==",
		"== nvme get-feature /dev/nvme0 -f 0x02 ==\n(no output", // a command that failed is still listed
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q:\n%s", want, dump)
		}
	}
	// The summary-only path does not run the extra reports.
	if _, d := CollectDump("nvme0n1", Options{Sys: sys, Run: fakeRun(nil)}); !strings.Contains(d, "== Summary ==") {
		t.Error("a dump is produced even when every command fails")
	}
}

func TestCollectDump_SATARunsHdparm(t *testing.T) {
	sys := t.TempDir()
	real := "devices/pci0000:00/ata2/host1/target1:0:0/1:0:0:0/block/sdb"
	put(t, sys, map[string]string{real + "/queue/write_cache": "write back"})
	link(t, sys, filepath.Join(sys, real), "block/sdb")
	_, dump := CollectDump("sdb", Options{Sys: sys, DriveWCE: func(string) (bool, error) { return true, nil },
		Run: fakeRun(map[string]string{"hdparm -I /dev/sdb": "ATA device identify", "smartctl -x /dev/sdb": "ATA smart"})})
	if !strings.Contains(dump, "== hdparm -I /dev/sdb ==\nATA device identify") || strings.Contains(dump, "nvme id-ctrl") {
		t.Errorf("dump:\n%s", dump)
	}
}

func TestCollect_DoesNotRunExtraReports(t *testing.T) {
	sys := t.TempDir()
	put(t, sys, map[string]string{"block/nvme0n1/queue/write_cache": "write back"})
	var ran []string
	Collect("nvme0n1", Options{Sys: sys, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil, errors.New("x")
	}})
	for _, r := range ran {
		if strings.Contains(r, "id-ctrl") || strings.Contains(r, "smartctl -x") {
			t.Errorf("Collect ran the dump-only command %q", r)
		}
	}
}

func TestFormat_NoStatusHasNoBareSource(t *testing.T) {
	out := Format(Info{Interface: "SATA", Smart: Smart{Source: "smartctl", Detail: "Permission denied"}})
	if strings.Contains(out, "SMART:") || !strings.Contains(out, "SMART note:") || strings.Contains(out, "(smartctl)") {
		t.Errorf("%s", out)
	}
	if got := Format(Info{}); got != "(nothing could be read)\n" {
		t.Errorf("empty = %q", got)
	}
}

// A PCI device without link attributes (no current_link_speed) used to panic
// in the speed comparison, which would have taken the orchestrator down.
func TestCollect_NVMeWithoutLinkAttributes(t *testing.T) {
	sys := t.TempDir()
	pci := "devices/pci0000:00/0000:01:00.0"
	put(t, sys, map[string]string{
		"block/nvme0n1/queue/write_cache": "write back",
		"class/nvme/nvme0/transport":      "pcie",
		pci + "/vendor":                   "0x144d",
	})
	link(t, sys, filepath.Join(sys, pci), "class/nvme/nvme0/device")
	got := Collect("nvme0n1", Options{Sys: sys, Run: fakeRun(nil)})
	if got.Interface != "NVMe (PCIe)" || got.LinkSpeed != "" || got.LinkBelowMax {
		t.Errorf("%+v", got)
	}
	if gts("") != 0 || gts("  ") != 0 || gts("8.0 GT/s PCIe") != 8 {
		t.Error("gts")
	}
}
