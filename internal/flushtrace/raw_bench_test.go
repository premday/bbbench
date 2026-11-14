package flushtrace

import (
	"testing"

	"bbbench/internal/tracefake"
)

// benchStream is a synthetic block and NVMe stream of n events.
func benchStream(n int) []tracefake.Event {
	d := tracefake.Dev(8, 16)
	evs := make([]tracefake.Event, 0, n)
	for i := 0; len(evs) < n; i++ {
		ts := int64(1e9 + i*2000)
		if i%2 == 0 {
			evs = append(evs,
				tracefake.Event{TimeNs: ts, Name: "block_rq_issue", Dev: d, Sector: uint64(i * 8), Nr: 8, RWBS: "R", Comm: "fio"},
				tracefake.Event{TimeNs: ts + 900, Name: "block_rq_complete", Dev: d, Sector: uint64(i * 8), Nr: 8, RWBS: "R"})
		} else {
			evs = append(evs,
				tracefake.Event{TimeNs: ts, Name: "nvme_setup_cmd", CtrlID: 0, Disk: "nvme0n1", QID: 3, CID: i % 1024, Opcode: 2},
				tracefake.Event{TimeNs: ts + 900, Name: "nvme_complete_rq", CtrlID: 0, Disk: "nvme0n1", QID: 3, CID: i % 1024})
		}
	}
	return evs
}

// BenchmarkParseLineStream: the text reader's userspace work per event (the
// kernel's own text formatting, which the raw reader also avoids, is not
// included).
func BenchmarkParseLineStream(b *testing.B) {
	in := benchStream(4096)
	lines := make([]string, len(in))
	bytes := 0
	for i, e := range in {
		lines[i] = e.Line() + "\n"
		bytes += len(lines[i])
	}
	b.SetBytes(int64(bytes / len(lines)))
	b.ResetTimer()
	n := 0
	for k := 0; k < b.N; k++ {
		if _, ok := ParseLine(lines[k%len(lines)]); ok {
			n++
		}
	}
	b.ReportMetric(float64(n)/b.Elapsed().Seconds(), "events/s")
}

// BenchmarkDecodePage: the raw reader's work per event, in pages.
func BenchmarkDecodePage(b *testing.B) {
	tr, err := tracefake.WriteTree(b.TempDir(), 4096, rawTestEvents...)
	if err != nil {
		b.Fatal(err)
	}
	in := benchStream(4096)
	pages, err := tr.Pages(in, tracefake.PageOptions{})
	if err != nil {
		b.Fatal(err)
	}
	rf, err := loadRawFormats(tr.Root, enableOf(rawTestEvents))
	if err != nil {
		b.Fatal(err)
	}
	d := newDecoder(rf)
	ps := pages[0]
	perPage := len(in) / len(ps)
	b.SetBytes(int64(4096 / perPage))
	out := make([]Event, 0, 256)
	b.ResetTimer()
	n := 0
	for k := 0; k < b.N; {
		out, _ = d.decodePage(ps[(k/perPage)%len(ps)], out[:0])
		n += len(out)
		k += len(out)
	}
	b.ReportMetric(float64(n)/b.Elapsed().Seconds(), "events/s")
}
