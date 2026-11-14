package main

import (
	"slices"
	"testing"

	"bbbench/internal/blockdev"
)

func TestDeviceNameOrder(t *testing.T) {
	want := []string{"nvme0n1", "nvme1n1", "nvme2n1", "nvme10n1", "sda", "sdb", "sdz", "sdaa", "sdab"}
	got := slices.Clone(want)
	slices.Reverse(got)
	d := make([]DriveInfo, len(got))
	for i, n := range got {
		d[i] = DriveInfo{Device: blockdev.Device{Name: n}}
	}
	var names []string
	for _, x := range sortedDrives(d) {
		names = append(names, x.Device.Name)
	}
	if !slices.Equal(names, want) {
		t.Errorf("order = %v\nwant    %v", names, want)
	}
	if d[0].Device.Name != "sdab" {
		t.Errorf("the argument was reordered: %v", d[0].Device.Name)
	}
	if !deviceNameLess("md9", "md10") || deviceNameLess("md10", "md9") || deviceNameLess("sda", "sda") {
		t.Error("md9 < md10, irreflexive")
	}
}
