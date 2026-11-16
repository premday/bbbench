// SPDX-License-Identifier: Dual BSD/GPL
//
// Times block requests and bios in the kernel: the start of a request or bio
// is stored under its pointer, its completion computes the latency and adds
// it to a log-bucketed histogram. Nothing leaves the kernel per I/O.
//
// The bucket layout is internal/iotrace/hist.go's (see TestBucketLayout).
// The structs below are the few kernel fields used, relocated against the
// running kernel's BTF (CO-RE), so no vmlinux.h is needed.

#include "common.h"
#include "bpf_tracing.h"

char LICENSE[] SEC("license") = "Dual BSD/GPL";

#define NBUCKETS 2240 // linearBuckets + (maxExp-firstExp+1)<<subBits in hist.go
#define MAX_DEVS 10
#define KINDS 3 // device, blocklayer, bio
#define MAX_SLOTS (MAX_DEVS * KINDS * 2)

enum { KIND_DEVICE = 0, KIND_BLOCKLAYER = 1, KIND_BIO = 2 };
enum { SRC_BLOCK = 0, SRC_BIO = 1 };
enum { ST_STARTED, ST_COMPLETED, ST_NOSTART, ST_FULL, ST_MAX };

typedef __u32 dev_t;

struct gendisk;

struct block_device {
	dev_t bd_dev;
	struct gendisk *bd_disk;
} __attribute__((preserve_access_index));

struct gendisk {
	struct block_device *part0;
} __attribute__((preserve_access_index));

struct request_queue {
	struct gendisk *disk;
} __attribute__((preserve_access_index));

struct request {
	struct request_queue *q;
	unsigned int cmd_flags;
	unsigned int __data_len;
} __attribute__((preserve_access_index));

struct bvec_iter {
	unsigned int bi_size;
} __attribute__((preserve_access_index));

struct bio {
	struct block_device *bi_bdev;
	unsigned int bi_opf;
	struct bvec_iter bi_iter;
} __attribute__((preserve_access_index));

struct devinfo {
	__u32 idx;
	__u32 source;
};

struct start {
	__u64 ins_ts; // block_rq_insert (or bio queue); 0 when issued directly
	__u64 iss_ts; // block_rq_issue; 0 for a bio
	__u32 slot_base;
	__u32 source;
};

struct hist {
	__u64 buckets[NBUCKETS];
	__u64 count;
	__u64 sum;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 64);
	__type(key, __u32);
	__type(value, struct devinfo);
} devs SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 262144);
	__type(key, __u64);
	__type(value, struct start);
} starts SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, MAX_SLOTS);
	__type(key, __u32);
	__type(value, struct hist);
} hists SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, ST_MAX);
	__type(key, __u32);
	__type(value, __u64);
} stats SEC(".maps");

static __always_inline void stat(__u32 which)
{
	__u64 *v = bpf_map_lookup_elem(&stats, &which);
	if (v)
		*v += 1;
}

static __always_inline __u32 bucket(__u64 v)
{
	if (v < 128)
		return v;
	int e = 63 - __builtin_clzll(v);
	if (e > 39)
		return NBUCKETS - 1;
	return 128 + ((e - 7) << 6) + ((v >> (e - 6)) & 63);
}

static __always_inline void record(__u32 slot, __u64 ns)
{
	if (slot >= MAX_SLOTS)
		return;
	struct hist *h = bpf_map_lookup_elem(&hists, &slot);
	if (!h)
		return;
	__u32 b = bucket(ns);
	if (b >= NBUCKETS)
		return;
	h->buckets[b] += 1;
	h->count += 1;
	h->sum += ns;
}

// The whole disk's number: a request's part is set only while the disk keeps
// I/O statistics (queue/iostats), so it is taken from the queue's disk; a
// bio's bdev may be a partition. NULL links read as 0, which is in no map.
static __always_inline dev_t rq_dev(struct request *rq)
{
	return rq->q->disk->part0->bd_dev;
}

static __always_inline dev_t bio_dev(struct bio *bio)
{
	return bio->bi_bdev->bd_disk->part0->bd_dev;
}

// slot_of is the histogram of (device, kind, direction).
static __always_inline __u32 slot_of(__u32 idx, __u32 kind, __u32 write)
{
	return (idx * KINDS + kind) * 2 + write;
}

static __always_inline int begin(__u64 key, dev_t dev, unsigned int opf, unsigned int bytes, int bio, int issue)
{
	unsigned int op = opf & 0xff;
	if (op > 1 || bytes == 0) // reads and writes that carry data (not flushes)
		return 0;
	struct devinfo *di = bpf_map_lookup_elem(&devs, &dev);
	if (!di || (di->source == SRC_BIO) != bio)
		return 0;
	__u64 now = bpf_ktime_get_ns();
	struct start *s = bpf_map_lookup_elem(&starts, &key);
	if (s) {
		if (issue)
			s->iss_ts = now; // a requeue restarts the device time
		else
			s->ins_ts = now;
		return 0;
	}
	struct start n = {
		.ins_ts = issue ? 0 : now,
		.iss_ts = issue ? now : 0,
		.slot_base = slot_of(di->idx, 0, 0),
		.source = di->source,
	};
	if (bpf_map_update_elem(&starts, &key, &n, BPF_NOEXIST) != 0) {
		stat(ST_FULL);
		return 0;
	}
	stat(ST_STARTED);
	return 0;
}

SEC("tp_btf/block_rq_insert")
int BPF_PROG(rq_insert, struct request *rq)
{
	return begin((__u64)rq, rq_dev(rq), rq->cmd_flags, rq->__data_len, 0, 0);
}

SEC("tp_btf/block_rq_issue")
int BPF_PROG(rq_issue, struct request *rq)
{
	return begin((__u64)rq, rq_dev(rq), rq->cmd_flags, rq->__data_len, 0, 1);
}

SEC("tp_btf/block_rq_complete")
int BPF_PROG(rq_complete, struct request *rq, int error, unsigned int nr_bytes)
{
	__u64 key = (__u64)rq;
	struct start *s = bpf_map_lookup_elem(&starts, &key);
	if (!s) {
		// A read or write of a traced disk completing without a stored
		// start: it began before the program attached. Flushes, discards
		// and the other bookkeeping completions are not counted.
		dev_t dev = rq_dev(rq);
		if ((rq->cmd_flags & 0xff) <= 1 && rq->__data_len > 0 && bpf_map_lookup_elem(&devs, &dev))
			stat(ST_NOSTART);
		return 0;
	}
	if (nr_bytes < rq->__data_len) // partial completion: the request goes on
		return 0;
	__u64 now = bpf_ktime_get_ns();
	__u32 write = (rq->cmd_flags & 0xff) == 1;
	__u32 base = s->slot_base;
	__u64 iss = s->iss_ts ? s->iss_ts : s->ins_ts;
	__u64 ins = s->ins_ts ? s->ins_ts : s->iss_ts;
	bpf_map_delete_elem(&starts, &key);
	record(base + KIND_DEVICE * 2 + write, now - iss);
	record(base + KIND_BLOCKLAYER * 2 + write, now - ins);
	stat(ST_COMPLETED);
	return 0;
}

SEC("tp_btf/block_bio_queue")
int BPF_PROG(bio_queue, struct bio *bio)
{
	return begin((__u64)bio, bio_dev(bio), bio->bi_opf, bio->bi_iter.bi_size, 1, 0);
}

// The tracepoint has a request_queue argument first (kernels up to at least
// 6.12); a kernel without it needs the other variant, see bio_complete1.
SEC("tp_btf/block_bio_complete")
int BPF_PROG(bio_complete, struct request_queue *q, struct bio *bio)
{
	__u64 key = (__u64)bio;
	struct start *s = bpf_map_lookup_elem(&starts, &key);
	if (!s) // flushes, bios in flight at attach: expected, not counted
		return 0;
	__u64 now = bpf_ktime_get_ns();
	__u32 write = (bio->bi_opf & 0xff) == 1;
	__u32 base = s->slot_base;
	__u64 ins = s->ins_ts;
	bpf_map_delete_elem(&starts, &key);
	record(base + KIND_BIO * 2 + write, now - ins);
	stat(ST_COMPLETED);
	return 0;
}

SEC("tp_btf/block_bio_complete")
int BPF_PROG(bio_complete1, struct bio *bio)
{
	__u64 key = (__u64)bio;
	struct start *s = bpf_map_lookup_elem(&starts, &key);
	if (!s) // flushes, bios in flight at attach: expected, not counted
		return 0;
	__u64 now = bpf_ktime_get_ns();
	__u32 write = (bio->bi_opf & 0xff) == 1;
	__u32 base = s->slot_base;
	__u64 ins = s->ins_ts;
	bpf_map_delete_elem(&starts, &key);
	record(base + KIND_BIO * 2 + write, now - ins);
	stat(ST_COMPLETED);
	return 0;
}
