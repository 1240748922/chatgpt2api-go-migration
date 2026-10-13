package store

import (
	"sort"
	"time"
)

type latencySample struct {
	at    time.Time
	ms    int64
	write bool
}

func (s *Store) observe(start time.Time, write bool) {
	s.metricsMu.Lock()
	defer s.metricsMu.Unlock()
	s.latencies = append(s.latencies, latencySample{at: time.Now(), ms: time.Since(start).Milliseconds(), write: write})
	if len(s.latencies) > 256 {
		s.latencies = s.latencies[len(s.latencies)-256:]
	}
}
func (s *Store) LatencyMetrics() map[string]any {
	s.metricsMu.Lock()
	defer s.metricsMu.Unlock()
	reads, writes := []int64{}, []int64{}
	for _, v := range s.latencies {
		if time.Since(v.at) > time.Minute {
			continue
		}
		if v.write {
			writes = append(writes, v.ms)
		} else {
			reads = append(reads, v.ms)
		}
	}
	p95 := func(values []int64) int64 {
		if len(values) == 0 {
			return 0
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		return values[(len(values)-1)*95/100]
	}
	return map[string]any{"database_p95_ms": p95(reads), "account_write_p95_ms": p95(writes), "read_samples": len(reads), "write_samples": len(writes), "window_seconds": 60}
}
