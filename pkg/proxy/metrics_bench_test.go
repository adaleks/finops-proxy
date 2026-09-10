package proxy

import (
	"testing"
	"time"
)

// BenchmarkMarkForwarded measures the per-call cost of recording one forwarded
// request (atomic adds + one token-window push).
func BenchmarkMarkForwarded(b *testing.B) {
	m := NewMetrics()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.markForwarded(time.Millisecond, 1000, 500)
	}
}

// BenchmarkMarkLoopBlocked measures the per-call cost of recording one 429.
func BenchmarkMarkLoopBlocked(b *testing.B) {
	m := NewMetrics()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.markLoopBlocked(42, 10)
	}
}

// BenchmarkObservabilityPerRequest measures the full observability cost a single
// request pays on the hot path: one start, one forwarded, one done. This is the
// additive overhead of enabling metrics and must stay far below the 1ms budget.
func BenchmarkObservabilityPerRequest(b *testing.B) {
	m := NewMetrics()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.markStart()
		m.markForwarded(time.Millisecond, 1000, 500)
		m.markDone()
	}
}

// BenchmarkMetricsSnapshot measures the (off-hot-path) cost of producing the
// JSON endpoint's snapshot. It is expected to allocate; it is not on the request
// path.
func BenchmarkMetricsSnapshot(b *testing.B) {
	m := NewMetrics()
	for i := 0; i < 1000; i++ {
		m.markForwarded(time.Millisecond, 100, 50)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = m.Snapshot()
	}
}

// TestMetricsMarksNoAllocs fails the build if a hot-path mark regresses into
// allocating — the observability layer must stay zero-allocation on the path.
func TestMetricsMarksNoAllocs(t *testing.T) {
	m := NewMetrics()
	if n := testing.AllocsPerRun(1000, func() {
		m.markStart()
		m.markForwarded(time.Millisecond, 1000, 500)
		m.markDone()
	}); n != 0 {
		t.Fatalf("hot-path marks allocate %.1f allocs/op, want 0", n)
	}
}
