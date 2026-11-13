package flushtrace

import (
	"math"
	"time"
)

// Merge bounds: an idle CPU stops holding back events that reached the
// merger at least mergeHold ago (an event it stores later reaches its reader
// after an irq_work wake-up plus a goroutine wake-up, which mergeHold covers,
// including the Go netpoller's ~10 ms worst case when every P is busy); more
// than mergeCap queued events are emitted regardless (memory bound, about
// 128 MB, when a reader stays behind).
const (
	mergeHold = 100 * time.Millisecond
	mergeCap  = 1 << 20
)

const (
	cpuBusy = iota // read data last; its next event is not older than lastTs
	cpuIdle        // its last read found the buffer empty
	cpuDone        // drained or at EOF: nothing more comes
)

type mergeCPU struct {
	q      [][]Event // batches, oldest first; q[0][pos] is the head
	pos    int
	state  int
	lastTs int64 // newest timestamp delivered
	seen   bool
}

func (c *mergeCPU) empty() bool { return len(c.q) == 0 }
func (c *mergeCPU) head() int64 { return c.q[0][c.pos].TimeNs }

type arrival struct {
	at    time.Time
	maxTs int64
}

// merger puts the per-CPU event streams of a raw reader back into one
// time-ordered stream, as the text pipe delivers it (ties: the lower CPU
// first, like the kernel's trace_find_next_entry). It emits up to a
// watermark: a busy CPU with nothing queued limits it to its last
// timestamp, an idle one to the larger of that and the newest timestamp of
// the batches that reached the merger at least hold ago, a done one not at
// all. An event older than one already emitted is late: fed anyway, in
// arrival order, and counted. It is not safe for concurrent use: one
// goroutine owns it.
type merger struct {
	cpus     []mergeCPU
	held     int
	arrivals []arrival
	aged     int64 // newest timestamp of the batches that arrived at least hold ago
	lastOut  int64
	out      bool // something was emitted
	late     int64
	hold     time.Duration
	cap      int
	recycle  func([]Event) // a fully emitted batch, for reuse (nil = none)
	heap     []int         // CPUs with queued events, by (head, cpu)
}

func newMerger(cpus int) *merger {
	return &merger{cpus: make([]mergeCPU, cpus), aged: math.MinInt64, hold: mergeHold, cap: mergeCap}
}

// add queues a batch of one CPU (in time order) that arrived at now.
func (m *merger) add(cpu int, evs []Event, now time.Time) {
	c := &m.cpus[cpu]
	c.state = cpuBusy
	if len(evs) == 0 {
		if m.recycle != nil {
			m.recycle(evs)
		}
		return
	}
	wasEmpty := c.empty()
	c.q = append(c.q, evs)
	m.held += len(evs)
	last := evs[len(evs)-1].TimeNs
	if !c.seen || last > c.lastTs {
		c.lastTs = last
	}
	c.seen = true
	m.arrivals = append(m.arrivals, arrival{at: now, maxTs: last})
	if wasEmpty {
		m.push(cpu)
	}
}

func (m *merger) setIdle(cpu int) {
	if m.cpus[cpu].state != cpuDone {
		m.cpus[cpu].state = cpuIdle
	}
}

func (m *merger) setDone(cpu int) { m.cpus[cpu].state = cpuDone }

func (m *merger) allDone() bool {
	for i := range m.cpus {
		if m.cpus[i].state != cpuDone {
			return false
		}
	}
	return true
}

// bound is how far the events of the other CPUs may be emitted while cpu
// has nothing queued.
func (m *merger) bound(cpu int) int64 {
	c := &m.cpus[cpu]
	if c.state == cpuDone {
		return math.MaxInt64
	}
	b := int64(math.MinInt64)
	if c.seen {
		b = c.lastTs
	}
	if c.state == cpuIdle {
		b = max(b, m.aged)
	}
	return b
}

// emit passes to fn, in order, every event that can be emitted at now, at
// most limit of them (limit <= 0: no limit), and returns how many. The event
// is only valid during the call.
func (m *merger) emit(now time.Time, limit int, fn func(*Event)) int {
	n := 0
	k := 0
	for k < len(m.arrivals) && now.Sub(m.arrivals[k].at) >= m.hold {
		m.aged = max(m.aged, m.arrivals[k].maxTs)
		k++
	}
	m.arrivals = m.arrivals[k:]
	w := int64(math.MaxInt64)
	for i := range m.cpus {
		if m.cpus[i].empty() {
			w = min(w, m.bound(i))
		}
	}
	for len(m.heap) > 0 && (limit <= 0 || n < limit) {
		cpu := m.heap[0]
		c := &m.cpus[cpu]
		if c.head() > w && m.held <= m.cap {
			break
		}
		m.pop()
		// the run of this CPU up to the next CPU's head (ties: lower CPU first)
		next, nextCPU := int64(math.MaxInt64), 0
		if len(m.heap) > 0 {
			next, nextCPU = m.cpus[m.heap[0]].head(), m.heap[0]
		}
		for !c.empty() && (limit <= 0 || n < limit) {
			ts := c.head()
			if ts > w && m.held <= m.cap {
				break
			}
			if ts > next || (ts == next && cpu > nextCPU) {
				break
			}
			if m.out && ts < m.lastOut {
				m.late++
			} else {
				m.lastOut = ts
			}
			m.out = true
			fn(&c.q[0][c.pos])
			n++
			m.held--
			c.pos++
			if c.pos == len(c.q[0]) {
				if m.recycle != nil {
					m.recycle(c.q[0])
				}
				c.q[0] = nil
				c.q = c.q[1:]
				c.pos = 0
			}
		}
		if c.empty() {
			w = min(w, m.bound(cpu))
		} else {
			m.push(cpu)
		}
	}
	return n
}

// deadline is when emit should run again because an idle CPU's hold ends:
// ok false when no queued event waits on an idle CPU.
func (m *merger) deadline() (time.Time, bool) {
	if m.held == 0 || len(m.arrivals) == 0 {
		return time.Time{}, false
	}
	for i := range m.cpus {
		if m.cpus[i].state == cpuIdle && m.cpus[i].empty() {
			return m.arrivals[0].at.Add(m.hold), true
		}
	}
	return time.Time{}, false
}

// ---- a binary heap of CPUs by (head timestamp, cpu) ----

func (m *merger) less(a, b int) bool {
	ha, hb := m.cpus[a].head(), m.cpus[b].head()
	return ha < hb || (ha == hb && a < b)
}

func (m *merger) push(cpu int) {
	m.heap = append(m.heap, cpu)
	for i := len(m.heap) - 1; i > 0; {
		p := (i - 1) / 2
		if !m.less(m.heap[i], m.heap[p]) {
			break
		}
		m.heap[i], m.heap[p] = m.heap[p], m.heap[i]
		i = p
	}
}

func (m *merger) pop() {
	n := len(m.heap) - 1
	m.heap[0] = m.heap[n]
	m.heap = m.heap[:n]
	for i := 0; ; {
		l, r, s := 2*i+1, 2*i+2, i
		if l < n && m.less(m.heap[l], m.heap[s]) {
			s = l
		}
		if r < n && m.less(m.heap[r], m.heap[s]) {
			s = r
		}
		if s == i {
			return
		}
		m.heap[i], m.heap[s] = m.heap[s], m.heap[i]
		i = s
	}
}
