package disktemp

import (
	"sync"
	"time"
)

// Sample is one temperature reading: milli-°C, ms since sampling started.
type Sample struct {
	TimeMs int64
	MilliC int64
}

// Sampler reads a sensor every interval until stopped.
type Sampler struct {
	sensor   *Sensor
	interval time.Duration
	start    time.Time
	onSample func(Sample)

	mu      sync.Mutex
	samples []Sample
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// StartSampler reads the sensor now and then every interval. onSample (may be
// nil) is called for each reading, from the sampler's goroutine.
func StartSampler(s *Sensor, interval time.Duration, onSample func(Sample)) *Sampler {
	sm := &Sampler{sensor: s, interval: interval, start: time.Now(), onSample: onSample,
		stop: make(chan struct{}), done: make(chan struct{})}
	go sm.run()
	return sm
}

func (s *Sampler) sample() {
	c, err := s.sensor.Read()
	if err != nil {
		return // a missed reading is a gap, not a failure
	}
	x := Sample{TimeMs: time.Since(s.start).Milliseconds(), MilliC: int64(c*1000 + 0.5)}
	s.mu.Lock()
	s.samples = append(s.samples, x)
	s.mu.Unlock()
	if s.onSample != nil {
		s.onSample(x)
	}
}

func (s *Sampler) run() {
	defer close(s.done)
	s.sample()
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.sample()
		}
	}
}

// Stop takes a last reading and returns every sample. It is safe to call twice.
func (s *Sampler) Stop() []Sample {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		s.sample()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sample(nil), s.samples...)
}
