//go:build linux && (amd64 || arm64)

package ebpf

import (
	"errors"
	"fmt"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// nvmeModule defines the nvme_setup_cmd and nvme_complete_rq tracepoints.
const nvmeModule = "nvme_core"

// program names in bpf/iolat.bpf.c and the tracepoint each attaches to.
var progTracepoints = map[string]string{
	"rq_insert":     "block_rq_insert",
	"rq_issue":      "block_rq_issue",
	"rq_complete":   "block_rq_complete",
	"nvme_setup":    "nvme_setup_cmd",   // (void *, request *, nvme_command *), nvme-core module
	"nvme_done":     "nvme_complete_rq", // (void *, request *), nvme-core module
	"bio_queue":     "block_bio_queue",
	"bio_complete":  "block_bio_complete", // (void *, request_queue *, bio *)
	"bio_complete1": "block_bio_complete", // (void *, bio *)
}

// prog is the loaded collector.
type prog struct {
	coll     *cebpf.Collection
	links    []link.Link
	attached []string
	devs     int
	sources  map[int]Source // device index -> how it is timed
}

// Open loads the program (histograms for ndevs devices, at most MaxDevs) against the running kernel's BTF and attaches it.
// Without root, BTF or the tracepoints it fails with a reason; nothing is
// left attached then.
func Open(ndevs int) (Program, error) {
	spec, err := loadIolat()
	if err != nil {
		return nil, fmt.Errorf("load ebpf object: %w", err)
	}
	kspec, err := btf.LoadKernelSpec()
	if err != nil {
		return nil, fmt.Errorf("%w: no kernel BTF: %v", ErrUnsupported, err)
	}
	if err := selectPrograms(spec, kspec); err != nil {
		return nil, err
	}
	// The per-CPU histograms are 18 KB each: only as many as the devices need.
	spec.Maps["hists"].MaxEntries = uint32(min(max(ndevs, 1), MaxDevs) * Kinds * 2)
	// Older kernels count BPF memory against RLIMIT_MEMLOCK.
	_ = rlimit.RemoveMemlock()
	coll, err := cebpf.NewCollectionWithOptions(spec, cebpf.CollectionOptions{})
	if err != nil {
		var ve *cebpf.VerifierError
		if errors.As(err, &ve) {
			return nil, fmt.Errorf("the kernel verifier rejected the program: %+v", ve)
		}
		return nil, fmt.Errorf("load ebpf program: %w", err)
	}
	p := &prog{coll: coll}
	for name, pr := range coll.Programs {
		l, err := link.AttachTracing(link.TracingOptions{Program: pr})
		if err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("attach %s: %w", progTracepoints[name], err)
		}
		p.links = append(p.links, l)
		p.attached = append(p.attached, progTracepoints[name])
	}
	return p, nil
}

// selectPrograms drops the programs the running kernel cannot take: a
// tracepoint it lacks, and the bio_complete variant whose argument list is
// not the kernel's. issue and complete are required.
func selectPrograms(spec *cebpf.CollectionSpec, kspec *btf.Spec) error {
	have := func(tp string) (btf.Type, bool) {
		var td *btf.Typedef
		if err := kspec.TypeByName("btf_trace_"+tp, &td); err == nil {
			return td, true
		}
		if tp == "nvme_setup_cmd" || tp == "nvme_complete_rq" {
			// Defined by the nvme-core module (not built in on most distributions).
			if mspec, err := btf.LoadKernelModuleSpec(nvmeModule); err == nil {
				if err := mspec.TypeByName("btf_trace_"+tp, &td); err == nil {
					return td, true
				}
			}
		}
		return nil, false
	}
	// The two NVMe programs go together: one alone cannot time a command.
	_, setup := have("nvme_setup_cmd")
	_, done := have("nvme_complete_rq")
	nvme := setup && done
	for name, tp := range progTracepoints {
		if _, ok := spec.Programs[name]; !ok {
			return fmt.Errorf("ebpf object has no program %s", name)
		}
		td, ok := have(tp)
		if (tp == "nvme_setup_cmd" || tp == "nvme_complete_rq") && !nvme {
			ok = false
		}
		if !ok {
			if tp == "block_rq_issue" || tp == "block_rq_complete" {
				return fmt.Errorf("%w: the kernel has no %s tracepoint in its BTF", ErrUnsupported, tp)
			}
			delete(spec.Programs, name)
			continue
		}
		if tp == "block_bio_complete" {
			if hasQueueArg(td) != (name == "bio_complete") {
				delete(spec.Programs, name)
			}
		}
	}
	return nil
}

// hasQueueArg reports whether a tracepoint's btf_trace_ typedef is
// (void *, struct request_queue *, struct bio *) rather than (void *, bio *).
func hasQueueArg(t btf.Type) bool {
	for {
		switch v := t.(type) {
		case *btf.Typedef:
			t = v.Type
		case *btf.Pointer:
			t = v.Target
		case *btf.FuncProto:
			return len(v.Params) >= 3
		default:
			return false
		}
	}
}

func (p *prog) Attached() []string { return append([]string(nil), p.attached...) }

func (p *prog) SetDevices(specs []DevSpec) error {
	if len(specs) > MaxDevs {
		return fmt.Errorf("at most %d devices can be timed, got %d", MaxDevs, len(specs))
	}
	m := p.coll.Maps["devs"]
	var key uint32
	var old []uint32
	it := m.Iterate()
	var v iolatDevinfo
	for it.Next(&key, &v) {
		old = append(old, key)
	}
	for _, k := range old {
		_ = m.Delete(k)
	}
	for _, d := range specs {
		if d.Idx < 0 || d.Idx >= MaxDevs {
			return fmt.Errorf("device index %d out of range", d.Idx)
		}
		if err := m.Put(d.Dev, iolatDevinfo{Idx: uint32(d.Idx), Source: uint32(d.Source)}); err != nil {
			return fmt.Errorf("set device %d: %w", d.Dev, err)
		}
	}
	p.devs = 0
	p.sources = map[int]Source{}
	for _, d := range specs {
		p.devs = max(p.devs, d.Idx+1)
		p.sources[d.Idx] = d.Source
	}
	return nil
}

func (p *prog) Read() (Snapshot, error) {
	snap := Snapshot{Slots: map[SlotKey]*Slot{}}
	if p.coll == nil {
		return Snapshot{}, errors.New("the eBPF program is closed")
	}
	hists := p.coll.Maps["hists"]
	per := make([]iolatHist, cebpf.MustPossibleCPU())
	for idx := 0; idx < p.devs; idx++ {
		for kind := 0; kind < Kinds; kind++ {
			// A request-timed device has no bio histogram and the reverse.
			if (p.sources[idx] == SourceBio) != (kind == KindBio) {
				continue
			}
			for w := 0; w < 2; w++ {
				slot := uint32((idx*Kinds+kind)*2 + w)
				if err := hists.Lookup(slot, &per); err != nil {
					return Snapshot{}, fmt.Errorf("read histogram %d: %w", slot, err)
				}
				s := &Slot{}
				for i := range per {
					s.Count += per[i].Count
					s.SumNs += per[i].Sum
					for b, c := range per[i].Buckets {
						s.Buckets[b] += c
					}
				}
				if s.Count > 0 {
					snap.Slots[SlotKey{Idx: idx, Kind: kind, Write: w == 1}] = s
				}
			}
		}
	}
	counters := make([]uint64, cebpf.MustPossibleCPU())
	for i, dst := range []*uint64{&snap.Started, &snap.Completed, &snap.NoStart, &snap.Full} {
		if err := p.coll.Maps["stats"].Lookup(uint32(i), &counters); err != nil {
			return Snapshot{}, fmt.Errorf("read counter %d: %w", i, err)
		}
		for _, c := range counters {
			*dst += c
		}
	}
	return snap, nil
}

func (p *prog) Close() error {
	var errs []error
	for _, l := range p.links {
		errs = append(errs, l.Close())
	}
	p.links = nil
	if p.coll != nil {
		p.coll.Close()
		p.coll = nil
	}
	return errors.Join(errs...)
}
