package proxy

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adaleks/finops-proxy/pkg/circuitbreaker"
)

// Metrics is the zero-dependency, in-memory observability registry behind
// GET /v1/metrics and the embedded /dashboard. Every counter is an atomic so the
// hot path records at the cost of a single atomic Add (no lock, no allocation,
// no goroutine); the only mutexes guard the two bounded history rings. A nil
// *Metrics makes every mark method a no-op, so a handler built without
// WithMetrics is byte-for-byte identical to one without observability.
type Metrics struct {
	active     atomic.Int64 // in-flight requests
	requests   atomic.Int64 // forwarded requests completed
	loopBlocks atomic.Int64 // loop-blocked 429s
	savedCost  atomic.Int64 // cumulative µUSD saved (Σ SavedCost on each block)

	latencyNs atomic.Int64 // Σ upstream latency in ns (forwarded requests only)
	latencyN  atomic.Int64 // latency sample count (for the average)

	tokens   tokenWindow // bounded sliding token-velocity gauge
	triggers triggerRing // bounded recent loop-blocked history

	started time.Time // registry start (uptime)
}

// NewMetrics returns an empty registry stamped with the current time.
func NewMetrics() *Metrics { return &Metrics{started: time.Now()} }

// WithMetrics wires a Metrics registry into the handler. A nil registry (or
// omitting this option) disables collection.
func WithMetrics(m *Metrics) HandlerOption {
	return func(h *handler) { h.metrics = m }
}

// markStart records that one request is now in flight.
func (m *Metrics) markStart() {
	if m != nil {
		m.active.Add(1)
	}
}

// markDone records that one in-flight request has completed.
func (m *Metrics) markDone() {
	if m != nil {
		m.active.Add(-1)
	}
}

// markLoopBlocked records one loop-blocked 429: +1 block, +saved µUSD, and the
// prompt-token contribution to the velocity window.
func (m *Metrics) markLoopBlocked(saved int64, promptTok int) {
	if m == nil {
		return
	}
	m.loopBlocks.Add(1)
	m.savedCost.Add(saved)
	m.tokens.add(time.Now(), int64(promptTok))
}

// markForwarded records one completed proxied call: +1 request, a latency
// sample, and prompt+completion tokens into the velocity window.
func (m *Metrics) markForwarded(latency time.Duration, promptTok, completionTok int) {
	if m == nil {
		return
	}
	m.requests.Add(1)
	m.latencyNs.Add(int64(latency))
	m.latencyN.Add(1)
	m.tokens.add(time.Now(), int64(promptTok+completionTok))
}

// recordTrigger pushes one loop-blocked episode into the trigger history.
func (m *Metrics) recordTrigger(t TriggerEvent) {
	if m == nil {
		return
	}
	m.triggers.push(t)
}

// TokenPoint is one second of the token-consumption sparkline.
type TokenPoint struct {
	Second int64 `json:"second"`
	Tokens int64 `json:"tokens"`
}

// TriggerEvent is one recent loop-blocked episode for the trigger-history panel.
type TriggerEvent struct {
	Timestamp  int64  `json:"timestamp"`
	AgentID    string `json:"agent_id,omitempty"`
	Model      string `json:"model,omitempty"`
	Reason     string `json:"reason"`
	SavedMicro int64  `json:"saved_cost_micro_usd"`
}

// MetricsSnapshot is the JSON shape served at GET /v1/metrics.
type MetricsSnapshot struct {
	ActiveRequests    int64          `json:"active_requests"`
	TotalRequests     int64          `json:"total_requests"`
	LoopBlocks        int64          `json:"loop_blocks"`
	BlockedAgents     int            `json:"blocked_agents"`
	SavedCostMicroUSD int64          `json:"saved_cost_micro_usd"`
	AvgLatencyMicro   int64          `json:"avg_latency_micro"`
	TokenVelocity     float64        `json:"token_velocity"`
	TokenSeries       []TokenPoint   `json:"token_series"`
	RecentTriggers    []TriggerEvent `json:"recent_triggers"`
	UptimeSeconds     int64          `json:"uptime_seconds"`
}

// Snapshot returns a point-in-time read of every gauge. It is off the hot path
// (only invoked when /v1/metrics is polled), so the bounded slices it builds are
// acceptable.
func (m *Metrics) Snapshot() MetricsSnapshot {
	now := time.Now()
	n := m.latencyN.Load()
	var avg int64
	if n > 0 {
		avg = m.latencyNs.Load() / n / int64(time.Microsecond) // ns → µs
	}
	return MetricsSnapshot{
		ActiveRequests:    m.active.Load(),
		TotalRequests:     m.requests.Load(),
		LoopBlocks:        m.loopBlocks.Load(),
		SavedCostMicroUSD: m.savedCost.Load(),
		AvgLatencyMicro:   avg,
		TokenVelocity:     m.tokens.velocity(now),
		TokenSeries:       m.tokens.series(now),
		RecentTriggers:    m.triggers.snapshot(),
		UptimeSeconds:     int64(now.Sub(m.started).Seconds()),
	}
}

// MetricsHandler serves GET /v1/metrics. It takes the detector so it can report
// the current blocked-agent count via the optional DetectorStatus capability,
// exactly as the EE control plane does (type-assert at startup, no core change).
func MetricsHandler(m *Metrics, det circuitbreaker.Detector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		snap := MetricsSnapshot{}
		if m != nil {
			snap = m.Snapshot()
		}
		if ds, ok := det.(circuitbreaker.DetectorStatus); ok {
			snap.BlockedAgents = len(ds.BlockedAgents())
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snap)
	})
}

// --- token velocity gauge ----------------------------------------------------

const (
	velocityBuckets = 64 // seconds of token history retained
	velocityWindow  = 5  // seconds over which token_velocity is computed
)

// tokenWindow is a fixed ring of per-second token buckets. add is O(1) amortized;
// velocity returns the trailing velocityWindow seconds' tokens divided by the
// window. It is mutex-guarded and never grows.
type tokenWindow struct {
	mu      sync.Mutex
	buckets [velocityBuckets]int64
	last    int64 // absolute unix second of the most recent observation
}

// advance zeroes every bucket between last+1 and sec (inclusive, bounded by the
// ring size) so stale buckets are never returned by a later read. mu must be
// held.
func (w *tokenWindow) advance(sec int64) {
	if sec <= w.last {
		return
	}
	first := w.last + 1
	if sec-w.last > velocityBuckets {
		first = sec - velocityBuckets + 1
	}
	for i := first; i <= sec; i++ {
		w.buckets[i%velocityBuckets] = 0
	}
	w.last = sec
}

func (w *tokenWindow) add(now time.Time, tokens int64) {
	if tokens <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	sec := now.Unix()
	w.advance(sec)
	w.buckets[sec%velocityBuckets] += tokens
}

func (w *tokenWindow) velocity(now time.Time) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	sec := now.Unix()
	w.advance(sec)
	var sum int64
	for i := 0; i < velocityWindow; i++ {
		s := sec - int64(i)
		if s < 0 {
			break
		}
		sum += w.buckets[s%velocityBuckets]
	}
	return float64(sum) / float64(velocityWindow)
}

func (w *tokenWindow) series(now time.Time) []TokenPoint {
	w.mu.Lock()
	defer w.mu.Unlock()
	sec := now.Unix()
	w.advance(sec)
	out := make([]TokenPoint, 0, velocityBuckets)
	for i := velocityBuckets - 1; i >= 0; i-- {
		s := sec - int64(i)
		if s < 0 {
			continue
		}
		out = append(out, TokenPoint{Second: s, Tokens: w.buckets[s%velocityBuckets]})
	}
	return out
}

// --- trigger history ring ----------------------------------------------------

// triggerCap bounds the retained loop-blocked history.
const triggerCap = 100

// triggerRing is a bounded FIFO of recent loop-blocked episodes, newest first on
// read. push is O(1); it is mutex-guarded and never grows past triggerCap.
type triggerRing struct {
	mu   sync.Mutex
	ring []TriggerEvent // chronological
}

func (r *triggerRing) push(ev TriggerEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ring = append(r.ring, ev)
	if len(r.ring) > triggerCap {
		r.ring = r.ring[len(r.ring)-triggerCap:]
	}
}

func (r *triggerRing) snapshot() []TriggerEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TriggerEvent, len(r.ring))
	for i := range r.ring {
		out[i] = r.ring[len(r.ring)-1-i] // newest first
	}
	return out
}
