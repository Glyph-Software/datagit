// Package obs is DataGit's observability surface (§17.3).
//
// The metrics here are chosen because each one answers a question an operator
// will actually ask, and several exist because Phase 0 found the underlying cost
// surprising: resolution segment depth (reads degrade with it), commits per
// branch (capped by the ref lock at ~850/s regardless of writers, finding F10),
// and sidecar size (3.3x at rest, finding in §14.2).
package obs

import (
	"sync"
	"sync/atomic"
	"time"
)

// Metrics is a small, dependency-free collector. It exposes counters and
// latency histograms in a shape a Prometheus or OpenTelemetry exporter can read
// without this package depending on either.
type Metrics struct {
	mu sync.RWMutex

	commits       atomic.Int64
	commitRows    atomic.Int64
	commitFailed  atomic.Int64
	merges        atomic.Int64
	mergeConflict atomic.Int64
	reads         atomic.Int64
	driftFound    atomic.Int64
	sessionsOpen  atomic.Int64

	hist map[string]*histogram
	// depth tracks resolution chain depth, because branch reads degrade with it
	// and the §18 cap only bounds the worst case.
	depth map[int]*atomic.Int64
}

func New() *Metrics {
	m := &Metrics{hist: map[string]*histogram{}, depth: map[int]*atomic.Int64{}}
	for i := 1; i <= 8; i++ {
		var c atomic.Int64
		m.depth[i] = &c
	}
	return m
}

func (m *Metrics) CommitOK(rows int, d time.Duration) {
	m.commits.Add(1)
	m.commitRows.Add(int64(rows))
	m.observe("commit_seconds", d)
}

func (m *Metrics) CommitFailed() { m.commitFailed.Add(1) }

func (m *Metrics) Merge(conflicts int, d time.Duration) {
	m.merges.Add(1)
	if conflicts > 0 {
		m.mergeConflict.Add(1)
	}
	m.observe("merge_seconds", d)
}

// Read records a branch read and the chain depth it resolved through.
func (m *Metrics) Read(chainDepth int, d time.Duration) {
	m.reads.Add(1)
	if c, ok := m.depth[chainDepth]; ok {
		c.Add(1)
	}
	m.observe("read_seconds", d)
}

func (m *Metrics) Drift(n int)    { m.driftFound.Add(int64(n)) }
func (m *Metrics) SessionOpened() { m.sessionsOpen.Add(1) }
func (m *Metrics) SessionClosed() { m.sessionsOpen.Add(-1) }

func (m *Metrics) observe(name string, d time.Duration) {
	m.mu.Lock()
	h, ok := m.hist[name]
	if !ok {
		h = newHistogram()
		m.hist[name] = h
	}
	m.mu.Unlock()
	h.observe(d.Seconds())
}

// Snapshot is a point-in-time reading, for an exporter or a health endpoint.
type Snapshot struct {
	Commits          int64
	CommitRows       int64
	CommitsFailed    int64
	Merges           int64
	MergesConflicted int64
	Reads            int64
	DriftFindings    int64
	SessionsOpen     int64
	ChainDepth       map[int]int64
	Latency          map[string]LatencySummary
}

type LatencySummary struct {
	Count int64
	P50   float64
	P95   float64
	P99   float64
}

func (m *Metrics) Snapshot() Snapshot {
	s := Snapshot{
		Commits: m.commits.Load(), CommitRows: m.commitRows.Load(),
		CommitsFailed: m.commitFailed.Load(),
		Merges:        m.merges.Load(), MergesConflicted: m.mergeConflict.Load(),
		Reads: m.reads.Load(), DriftFindings: m.driftFound.Load(),
		SessionsOpen: m.sessionsOpen.Load(),
		ChainDepth:   map[int]int64{}, Latency: map[string]LatencySummary{},
	}
	for d, c := range m.depth {
		if v := c.Load(); v > 0 {
			s.ChainDepth[d] = v
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, h := range m.hist {
		s.Latency[name] = h.summary()
	}
	return s
}

// histogram uses fixed exponential buckets, which is enough for the percentiles
// an operator reads and avoids a dependency for the sake of one type.
type histogram struct {
	mu      sync.Mutex
	buckets []float64
	counts  []int64
	total   int64
}

func newHistogram() *histogram {
	// 100us to ~100s, roughly 2x apart.
	b := []float64{0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.02,
		0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 100}
	return &histogram{buckets: b, counts: make([]int64, len(b)+1)}
}

func (h *histogram) observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total++
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
			return
		}
	}
	h.counts[len(h.counts)-1]++
}

func (h *histogram) summary() LatencySummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := LatencySummary{Count: h.total}
	if h.total == 0 {
		return out
	}
	pick := func(q float64) float64 {
		target := int64(float64(h.total) * q)
		var seen int64
		for i, c := range h.counts {
			seen += c
			if seen >= target {
				if i < len(h.buckets) {
					return h.buckets[i]
				}
				return h.buckets[len(h.buckets)-1]
			}
		}
		return 0
	}
	out.P50, out.P95, out.P99 = pick(0.50), pick(0.95), pick(0.99)
	return out
}
