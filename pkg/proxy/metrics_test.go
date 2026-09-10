package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adaleks/finops-proxy/pkg/circuitbreaker"
	"github.com/adaleks/finops-proxy/pkg/pricing"
)

// fakePriceSrc is a minimal pricing.PriceSource for metrics tests.
type fakePriceSrc struct{}

func (fakePriceSrc) PriceFor(string) (pricing.ModelPrice, bool) {
	return pricing.ModelPrice{InputUSD: 1_000_000, OutputUSD: 2_000_000}, true
}
func (fakePriceSrc) EstimatePromptTokens([]byte) int { return 10 }
func (fakePriceSrc) SavedCost([]byte) int64          { return 42 }
func (fakePriceSrc) AssumedCompletionTokens() int    { return 5 }

// fakeStore is a minimal RequestLogStore for metrics tests.
type fakeStore struct{}

func (fakeStore) LogRequest(context.Context, RequestRecord) error { return nil }

func TestMetricsNilSafe(t *testing.T) {
	var m *Metrics
	m.markStart()
	m.markDone()
	m.markForwarded(time.Millisecond, 10, 5)
	m.markLoopBlocked(42, 10)
	m.recordTrigger(TriggerEvent{})
	// reaching here without a panic is the assertion
}

func TestMetricsCounters(t *testing.T) {
	m := NewMetrics()
	m.markStart()
	m.markStart()
	m.markDone()
	m.markLoopBlocked(100, 10)
	m.markLoopBlocked(42, 5)
	m.markForwarded(time.Millisecond, 10, 5)

	s := m.Snapshot()
	if s.ActiveRequests != 1 {
		t.Fatalf("active = %d, want 1", s.ActiveRequests)
	}
	if s.LoopBlocks != 2 {
		t.Fatalf("loop blocks = %d, want 2", s.LoopBlocks)
	}
	if s.SavedCostMicroUSD != 142 {
		t.Fatalf("saved = %d, want 142", s.SavedCostMicroUSD)
	}
	if s.TotalRequests != 1 {
		t.Fatalf("total requests = %d, want 1", s.TotalRequests)
	}
}

func TestMetricsAvgLatency(t *testing.T) {
	m := NewMetrics()
	m.markForwarded(time.Millisecond, 1, 1)   // 1000 µs
	m.markForwarded(3*time.Millisecond, 1, 1) // 3000 µs
	s := m.Snapshot()
	if s.AvgLatencyMicro != 2000 {
		t.Fatalf("avg latency = %d µs, want 2000", s.AvgLatencyMicro)
	}
}

func TestTokenWindowVelocity(t *testing.T) {
	var w tokenWindow
	now := time.Unix(1_000_000, 0)
	// Five consecutive seconds × 100 tokens each: the trailing 5s window at the
	// last add holds 500 tokens → 100 tokens/s.
	for i := 0; i < 5; i++ {
		w.add(now.Add(time.Duration(i)*time.Second), 100)
	}
	if got := w.velocity(now.Add(4 * time.Second)); got != 100.0 {
		t.Fatalf("velocity = %f, want 100.0", got)
	}
}

func TestTokenWindowRollover(t *testing.T) {
	var w tokenWindow
	now := time.Unix(1_000_000, 0)
	w.add(now, 100)
	// Ten seconds later the token has aged out of the 5s velocity window.
	if got := w.velocity(now.Add(10 * time.Second)); got != 0 {
		t.Fatalf("velocity after rollover = %f, want 0", got)
	}
}

func TestTriggerRingBounded(t *testing.T) {
	var r triggerRing
	for i := 0; i < triggerCap+50; i++ {
		r.push(TriggerEvent{Timestamp: int64(i)})
	}
	s := r.snapshot()
	if len(s) != triggerCap {
		t.Fatalf("ring len = %d, want %d", len(s), triggerCap)
	}
	if s[0].Timestamp != triggerCap+49 {
		t.Fatalf("newest = %d, want %d", s[0].Timestamp, triggerCap+49)
	}
	if s[len(s)-1].Timestamp != 50 {
		t.Fatalf("oldest = %d, want 50", s[len(s)-1].Timestamp)
	}
}

func TestMetricsHandler(t *testing.T) {
	m := NewMetrics()
	m.markLoopBlocked(42, 10)
	det := circuitbreaker.NewHashDetector(time.Minute, 2)

	req := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	rec := httptest.NewRecorder()
	MetricsHandler(m, det).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var snap MetricsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.LoopBlocks != 1 || snap.SavedCostMicroUSD != 42 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.BlockedAgents != 0 {
		t.Fatalf("blocked agents = %d, want 0", snap.BlockedAgents)
	}
}

func TestMetricsHandlerBlockedAgents(t *testing.T) {
	m := NewMetrics()
	det := circuitbreaker.NewHashDetector(time.Minute, 2)
	det.Check("a", "fp1")
	det.Check("a", "fp1") // second sighting blocks

	req := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	rec := httptest.NewRecorder()
	MetricsHandler(m, det).ServeHTTP(rec, req)

	var snap MetricsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.BlockedAgents != 1 {
		t.Fatalf("blocked agents = %d, want 1", snap.BlockedAgents)
	}
}

func TestMetricsHandlerMethodNotAllowed(t *testing.T) {
	m := NewMetrics()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", nil)
	rec := httptest.NewRecorder()
	MetricsHandler(m, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestHandlerMetricsIntegration exercises the full wiring: a real handler built
// with WithMetrics forwards one request and blocks its repeat, and the registry
// reflects both, with the in-flight gauge returning to zero.
func TestHandlerMetricsIntegration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	det := circuitbreaker.NewHashDetector(time.Minute, 2)
	m := NewMetrics()
	h := NewHandler(u, det, fakePriceSrc{}, WithRequestLogStore(fakeStore{}), WithMetrics(m))

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(AgentIDHeader, "agent-1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := do(); got != http.StatusOK {
		t.Fatalf("first request = %d, want 200", got)
	}
	if got := do(); got != http.StatusTooManyRequests {
		t.Fatalf("repeat request = %d, want 429", got)
	}

	s := m.Snapshot()
	if s.ActiveRequests != 0 {
		t.Fatalf("active = %d, want 0", s.ActiveRequests)
	}
	if s.TotalRequests != 1 {
		t.Fatalf("total requests = %d, want 1", s.TotalRequests)
	}
	if s.LoopBlocks != 1 {
		t.Fatalf("loop blocks = %d, want 1", s.LoopBlocks)
	}
	if s.SavedCostMicroUSD != 42 {
		t.Fatalf("saved = %d, want 42", s.SavedCostMicroUSD)
	}
	if len(s.RecentTriggers) != 1 {
		t.Fatalf("triggers = %d, want 1", len(s.RecentTriggers))
	}
	if s.RecentTriggers[0].AgentID != "agent-1" {
		t.Fatalf("trigger agent = %q, want agent-1", s.RecentTriggers[0].AgentID)
	}
}
