package diskinfo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errNo = errors.New("no")

// dmModParams is the dm_mod parameter set the user reads by hand, plus one
// more, as a kernel that still has use_blk_mq shows them.
var dmModParams = map[string]string{
	"module/dm_mod/parameters/use_blk_mq":             "Y",
	"module/dm_mod/parameters/dm_mq_nr_hw_queues":     "0",
	"module/dm_mod/parameters/dm_mq_queue_depth":      "2048",
	"module/dm_mod/parameters/reserved_bio_based_ios": "16",
	"module/dm_mod/initstate":                         "live",
	"module/dm_mod/refcnt":                            "3",
}

// nvmeQueueTree is an NVMe namespace with two hardware queues and no
// scheduler.
func nvmeQueueTree(t *testing.T, sys string) {
	t.Helper()
	put(t, sys, map[string]string{
		"block/nvme0n1/dev":                      "259:0",
		"block/nvme0n1/size":                     "1000215216",
		"block/nvme0n1/inflight":                 "       0        0",
		"block/nvme0n1/stat":                     "1 2 3 4",
		"block/nvme0n1/queue/write_cache":        "write back",
		"block/nvme0n1/queue/scheduler":          "[none] mq-deadline kyber",
		"block/nvme0n1/queue/nr_requests":        "1023",
		"block/nvme0n1/queue/rotational":         "0",
		"block/nvme0n1/queue/read_ahead_kb":      "128",
		"block/nvme0n1/queue/max_sectors_kb":     "1280",
		"block/nvme0n1/queue/max_hw_sectors_kb":  "2048",
		"block/nvme0n1/queue/wbt_lat_usec":       "2000",
		"block/nvme0n1/queue/rq_affinity":        "1",
		"block/nvme0n1/queue/nomerges":           "0",
		"block/nvme0n1/queue/io_poll":            "0",
		"block/nvme0n1/mq/0/cpu_list":            "0, 1",
		"block/nvme0n1/mq/0/nr_tags":             "1023",
		"block/nvme0n1/mq/0/nr_reserved_tags":    "0",
		"block/nvme0n1/mq/1/cpu_list":            "2, 3",
		"block/nvme0n1/mq/1/nr_tags":             "1023",
		"class/nvme/nvme0/transport":             "pcie",
		"class/nvme/nvme0/queue_count":           "5",
		"class/nvme/nvme0/sqsize":                "1023",
		"class/nvme/nvme0/numa_node":             "0",
		"module/nvme_core/parameters/io_timeout": "30",
		"module/nvme_core/parameters/multipath":  "Y",
		"module/nvme/parameters/poll_queues":     "0",
	})
}

func TestQueue_NVMeMultiqueue(t *testing.T) {
	sys := t.TempDir()
	nvmeQueueTree(t, sys)
	put(t, sys, dmModParams)
	info, dump := CollectDump("nvme0n1", Options{Sys: sys, Debug: filepath.Join(sys, "nodebug"), Run: fakeRun(nil)})
	q := info.Queue
	if q == nil {
		t.Fatal("no queue summary")
	}
	if q.Scheduler != "none" || strings.Join(q.Schedulers, ",") != "none,mq-deadline,kyber" || !q.MQ || q.NrHWQueues != 2 ||
		q.NrRequests != "1023" || q.Rotational != "0" || q.ReadAheadKB != "128" || q.MaxSectorsKB != "1280" ||
		q.WBTLatUsec != "2000" || q.RqAffinity != "1" || q.NoMerges != "0" || q.IOPoll != "0" ||
		q.NVMeQueueCount != "5" || q.NVMeSQSize != "1023" || q.MD != nil {
		t.Errorf("queue = %+v", q)
	}
	if d := q.DMMod; d == nil || !d.Loaded || d.UseBlkMQ != "Y" || d.MQNrHWQueues != "0" || d.MQQueueDepth != "2048" {
		t.Errorf("dm_mod = %+v", q.DMMod)
	}
	for _, want := range []string{
		"== Queue and scheduling: nvme0n1 ==",
		"block/nvme0n1/queue/scheduler = [none] mq-deadline kyber",
		"block/nvme0n1/queue/max_hw_sectors_kb = 2048",
		"block/nvme0n1/removable = not present (not on this kernel)",
		"blk-mq: yes, 2 hardware queues",
		"hctx0: cpus 0, 1", "hctx1: cpus 2, 3",
		"block/nvme0n1/mq/1/nr_tags = 1023",
		"class/nvme/nvme0/queue_count = 5", "class/nvme/nvme0/kato = not present (not on this kernel)",
		"debugfs: " + filepath.Join(sys, "nodebug", "block", "nvme0n1") + " is not readable",
		"these are the sysfs values only",
		"== Kernel module parameters ==",
		"module/nvme_core/parameters/io_timeout = 30", "module/nvme_core/parameters/multipath = Y",
		"module/nvme/parameters/poll_queues = 0",
		"module/dm_mod/parameters/use_blk_mq = Y",
		"module/dm_mod/parameters/dm_mq_nr_hw_queues = 0",
		"module/dm_mod/parameters/dm_mq_queue_depth = 2048",
		"module/dm_mod/parameters/reserved_bio_based_ios = 16",
		"module/dm_mod/refcnt = 3", "module/dm_mod/initstate = live",
		"module/md_mod: not loaded (or built in without parameters)",
		"module/dm_crypt: not loaded",
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q:\n%s", want, dump)
		}
	}
	// Each file once, in name order.
	if strings.Count(dump, "block/nvme0n1/queue/nr_requests = ") != 1 {
		t.Error("nr_requests listed more than once")
	}
	if strings.Index(dump, "queue/max_hw_sectors_kb") > strings.Index(dump, "queue/max_sectors_kb") {
		t.Error("queue files are not in name order")
	}
	if strings.Count(dump, "== Kernel module parameters ==") != 1 {
		t.Error("module section must appear once")
	}
	sum := spaces.ReplaceAllString(dump, " ")
	if !strings.Contains(sum, "Queue: blk-mq, 2 hardware queues, scheduler none") ||
		!strings.Contains(sum, "dm_mod: use_blk_mq Y, dm_mq_nr_hw_queues 0, dm_mq_queue_depth 2048") {
		t.Errorf("summary:\n%s", dump)
	}
	b, _ := json.Marshal(info)
	if !strings.Contains(string(b), `"queue":{"scheduler":"none"`) || !strings.Contains(string(b), `"dm_mod":{"loaded":true,"use_blk_mq":"Y"`) {
		t.Errorf("json: %s", b)
	}
}

// A SATA disk with mq-deadline: its iosched tunables, the SCSI device and host
// queue facts. A kernel without use_blk_mq says so, never a value.
func TestQueue_SATAWithIOSched(t *testing.T) {
	sys := t.TempDir()
	real := "devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/block/sda"
	put(t, sys, map[string]string{
		real + "/queue/write_cache":                     "write back",
		real + "/queue/scheduler":                       "[mq-deadline] none",
		real + "/queue/rotational":                      "1",
		real + "/queue/iosched/read_expire":             "500",
		real + "/queue/iosched/write_expire":            "5000",
		real + "/queue/iosched/fifo_batch":              "16",
		real + "/mq/0/cpu_list":                         "0, 1, 2, 3",
		real + "/device/queue_depth":                    "32",
		real + "/device/queue_type":                     "simple",
		real + "/device/timeout":                        "30",
		"class/scsi_host/host0/can_queue":               "32",
		"class/scsi_host/host0/cmd_per_lun":             "0",
		"module/dm_mod/parameters/dm_mq_queue_depth":    "2048",
		"module/dm_mod/parameters/dm_mq_nr_hw_queues":   "1",
		"module/scsi_mod/parameters/scsi_logging_level": "0",
	})
	link(t, sys, filepath.Join(sys, real), "block/sda")
	info, dump := CollectDump("sda", Options{Sys: sys, Debug: t.TempDir(), Run: fakeRun(nil),
		DriveWCE: func(string) (bool, error) { return true, nil }})
	q := info.Queue
	if q == nil || q.Scheduler != "mq-deadline" || q.Rotational != "1" || q.DeviceQueueDepth != "32" || q.NrHWQueues != 1 || !q.MQ {
		t.Fatalf("queue = %+v", q)
	}
	if d := q.DMMod; d == nil || d.UseBlkMQ != "" || d.MQNrHWQueues != "1" {
		t.Errorf("dm_mod = %+v", q.DMMod)
	}
	for _, want := range []string{
		"block/sda/queue/iosched/fifo_batch = 16", "block/sda/queue/iosched/read_expire = 500",
		"block/sda/device/queue_depth = 32", "block/sda/device/queue_type = simple",
		"class/scsi_host/host0/can_queue = 32", "class/scsi_host/host0/nr_hw_queues = not present",
		"module/scsi_mod/parameters/scsi_logging_level = 0",
		"module/dm_mod/parameters/use_blk_mq = not present (not on this kernel)",
		"module/dm_mod/parameters/dm_mq_queue_depth = 2048",
		"is not readable",
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q:\n%s", want, dump)
		}
	}
	if strings.Contains(spaces.ReplaceAllString(dump, " "), "use_blk_mq Y") {
		t.Error("a missing use_blk_mq must not read as a value")
	}
}

// mdTree is an md raid1 over sdb and sdc (two SATA disks with their own
// queues), bio-based.
func mdTree(t *testing.T, sys string) {
	t.Helper()
	files := map[string]string{
		"block/md127/queue/scheduler":             "none",
		"block/md127/queue/rotational":            "0",
		"block/md127/queue/write_cache":           "write back",
		"block/md127/md/level":                    "raid1",
		"block/md127/md/raid_disks":               "2",
		"block/md127/md/chunk_size":               "0",
		"block/md127/md/array_state":              "clean",
		"block/md127/md/sync_action":              "idle",
		"block/md127/md/degraded":                 "0",
		"block/md127/md/bitmap/location":          "+8",
		"block/md127/md/bitmap/chunksize":         "67108864",
		"block/md127/md/dev-sdb/state":            "in_sync",
		"block/md127/md/dev-sdb/slot":             "0",
		"block/md127/md/dev-sdb/errors":           "0",
		"block/md127/md/dev-sdc/state":            "in_sync",
		"block/md127/md/dev-sdc/slot":             "1",
		"block/sdb/queue/scheduler":               "[mq-deadline] none",
		"block/sdb/mq/0/cpu_list":                 "0",
		"block/sdc/queue/scheduler":               "mq-deadline [bfq] none",
		"block/sdc/mq/0/cpu_list":                 "0",
		"module/md_mod/parameters/start_ro":       "0",
		"module/md_mod/parameters/create_on_open": "Y",
		"module/raid1/initstate":                  "live",
	}
	put(t, sys, files)
	for _, d := range []string{"sdb", "sdc"} {
		link(t, sys, filepath.Join(sys, "block", d), "block/md127/slaves/"+d)
		link(t, sys, filepath.Join(sys, "block", d), "block/md127/md/dev-"+d+"/block")
	}
	link(t, sys, "dev-sdb", "block/md127/md/rd0")
	if err := os.MkdirAll(filepath.Join(sys, "block/md127/holders"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestQueue_MDNodeAndMembers(t *testing.T) {
	sys := t.TempDir()
	mdTree(t, sys)
	var ran []string
	o := Options{Sys: sys, Debug: t.TempDir(), Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name)
		return nil, errNo
	}, DriveWCE: func(string) (bool, error) { t.Error("an md node has no drive cache to query"); return false, errNo }}
	info, dump := CollectDump("md127", o)
	if len(ran) > 0 {
		t.Errorf("commands run on an md node: %v", ran)
	}
	q := info.Queue
	if q == nil || q.MQ || q.NrHWQueues != 0 || q.Scheduler != "none" {
		t.Fatalf("queue = %+v", q)
	}
	if m := q.MD; m == nil || m.Level != "raid1" || m.RaidDisks != "2" || m.ChunkSize != "0" || m.SyncAction != "idle" || m.Bitmap != "+8" {
		t.Errorf("md = %+v", q.MD)
	}
	if q.DMMod != nil {
		t.Errorf("dm_mod is not loaded here: %+v", q.DMMod)
	}
	if info.Interface != "md" || info.Smart.Detail != "" {
		t.Errorf("info = %+v", info)
	}
	for _, want := range []string{
		"== Queue and scheduling: md127 ==",
		"blk-mq: no (bio-based: no mq/ directory)",
		"block/md127/md/level = raid1", "block/md127/md/bitmap/location = +8",
		"block/md127/md/dev-sdb/state = in_sync", "block/md127/md/dev-sdc/slot = 1",
		"block/md127/md/dev-sdb/block -> ", "block/md127/md/rd0 -> dev-sdb",
		"slaves: sdb, sdc", "holders: (none)",
		"== Queue and scheduling: sdb (member of md127) ==", "block/sdb/queue/scheduler = [mq-deadline] none",
		"== Queue and scheduling: sdc (member of md127) ==", "block/sdc/queue/scheduler = mq-deadline [bfq] none",
		"module/md_mod/parameters/create_on_open = Y", "module/raid1/initstate = live",
		"module/dm_mod: not loaded",
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q:\n%s", want, dump)
		}
	}
	if strings.Count(dump, "== Kernel module parameters ==") != 1 {
		t.Error("module section once per file, even with members")
	}
	if strings.Contains(dump, "smartctl") || strings.Contains(dump, "SMBIOS") {
		t.Errorf("drive-only sections on an md node:\n%s", dump)
	}
	if !strings.Contains(spaces.ReplaceAllString(dump, " "), "md: raid1, 2 disks, chunk 0, sync idle, bitmap +8") {
		t.Errorf("summary:\n%s", dump)
	}
}

// A dm node: dm/ attributes, slaves (a partition: its disk's queue), and the
// dm_mod row even when dm_mod has no sysfs directory.
func TestQueue_DMNode(t *testing.T) {
	sys := t.TempDir()
	put(t, sys, map[string]string{
		"block/dm-0/queue/scheduler":          "none",
		"block/dm-0/dm/name":                  "vg0-lv0",
		"block/dm-0/dm/uuid":                  "LVM-abc",
		"block/dm-0/dm/suspended":             "0",
		"devices/x/block/sdd/queue/scheduler": "[none]",
		"devices/x/block/sdd/sdd1/partition":  "1",
	})
	link(t, sys, filepath.Join(sys, "devices/x/block/sdd"), "block/sdd")
	link(t, sys, filepath.Join(sys, "devices/x/block/sdd/sdd1"), "class/block/sdd1")
	link(t, sys, filepath.Join(sys, "devices/x/block/sdd/sdd1"), "block/dm-0/slaves/sdd1")
	info, dump := CollectDump("dm-0", Options{Sys: sys, Debug: t.TempDir(), Run: fakeRun(nil)})
	if info.Interface != "device-mapper" || info.Queue == nil || info.Queue.DMMod == nil || info.Queue.DMMod.Loaded {
		t.Fatalf("info = %+v queue = %+v", info, info.Queue)
	}
	for _, want := range []string{
		"block/dm-0/dm/name = vg0-lv0", "block/dm-0/dm/suspended = 0", "slaves: sdd1",
		"sdd1 is a partition of sdd: its queue is the disk's",
		"== Queue and scheduling: sdd (member of dm-0) ==", "block/sdd/queue/scheduler = [none]",
		"module/dm_mod: not loaded",
		"module/dm_mod/parameters/use_blk_mq = not present (not on this kernel)",
		"module/dm_mod/parameters/dm_mq_nr_hw_queues = not present (not on this kernel)",
		"module/dm_mod/parameters/dm_mq_queue_depth = not present (not on this kernel)",
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q:\n%s", want, dump)
		}
	}
	if !strings.Contains(spaces.ReplaceAllString(dump, " "), "dm_mod: not loaded") {
		t.Errorf("summary:\n%s", dump)
	}
}

// With debugfs readable every blk-mq file is dumped, binary files skipped,
// each file capped and the device section bounded.
func TestQueue_DebugfsAndLimits(t *testing.T) {
	sys, dbg := t.TempDir(), t.TempDir()
	nvmeQueueTree(t, sys)
	put(t, dbg, map[string]string{
		"block/nvme0n1/state":               "SAME_COMP|IO_STAT",
		"block/nvme0n1/hctx0/state":         "",
		"block/nvme0n1/hctx0/busy":          "",
		"block/nvme0n1/hctx0/dispatch_busy": "0",
		"block/nvme0n1/hctx0/queued":        "12345",
		"block/nvme0n1/hctx0/run":           "678",
		"block/nvme0n1/hctx0/tags":          strings.Repeat("x", 300),
		"block/nvme0n1/sched/queued":        "0 0 0",
	})
	if err := os.WriteFile(filepath.Join(dbg, "block/nvme0n1/hctx0/blob"), []byte{1, 0, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, dump := CollectDump("nvme0n1", Options{Sys: sys, Debug: dbg, Run: fakeRun(nil), MaxFile: 100})
	for _, want := range []string{
		"debugfs: block/nvme0n1/hctx0/queued = 12345", "debugfs: block/nvme0n1/hctx0/run = 678",
		"debugfs: block/nvme0n1/hctx0/dispatch_busy = 0", "debugfs: block/nvme0n1/sched/queued = 0 0 0",
		"debugfs: block/nvme0n1/state = SAME_COMP|IO_STAT", "debugfs: block/nvme0n1/hctx0/busy = (empty)",
		"debugfs: block/nvme0n1/hctx0/blob = (binary, 3 bytes, skipped)",
		"(truncated at 100 bytes)",
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q:\n%s", want, dump)
		}
	}
	if strings.Contains(dump, "is not readable") {
		t.Error("debugfs is readable here")
	}
	_, dump = CollectDump("nvme0n1", Options{Sys: sys, Debug: dbg, Run: fakeRun(nil), MaxDevice: 300})
	if !strings.Contains(dump, "truncated: the section of nvme0n1 reached 300 bytes") {
		t.Errorf("device bound:\n%s", dump)
	}
	if strings.Contains(dump, "debugfs: block/nvme0n1/sched/queued") {
		t.Error("the device section went on past its bound")
	}
}

// An unreadable file is said so, not left out.
func TestQueue_UnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	sys := t.TempDir()
	put(t, sys, map[string]string{"block/sdz/queue/scheduler": "none", "block/sdz/queue/secret": "x"})
	if err := os.Chmod(filepath.Join(sys, "block/sdz/queue/secret"), 0o200); err != nil {
		t.Fatal(err)
	}
	_, dump := CollectDump("sdz", Options{Sys: sys, Debug: t.TempDir(), Run: fakeRun(nil),
		DriveWCE: func(string) (bool, error) { return false, errNo }})
	if !strings.Contains(dump, "block/sdz/queue/secret = unreadable: ") {
		t.Errorf("dump:\n%s", dump)
	}
}

// Collect (no dump) fills the summary but adds no queue lines to the sysfs
// log, and a device without a queue directory has no summary.
func TestQueue_SummaryOnly(t *testing.T) {
	sys := t.TempDir()
	nvmeQueueTree(t, sys)
	if got := Collect("nvme0n1", Options{Sys: sys, Run: fakeRun(nil)}); got.Queue == nil || got.Queue.NrHWQueues != 2 {
		t.Errorf("%+v", got.Queue)
	}
	if got := Collect("sdq", Options{Sys: sys, Run: fakeRun(nil), DriveWCE: func(string) (bool, error) { return false, errNo }}); got.Queue != nil {
		t.Errorf("no queue dir: %+v", got.Queue)
	}
}
