package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adaleks/finops-proxy/internal/store/memory"
	"github.com/adaleks/finops-proxy/pkg/circuitbreaker"
	"github.com/adaleks/finops-proxy/pkg/pricing"
	"github.com/adaleks/finops-proxy/pkg/proxy"
)

// fakePrice is a minimal pricing.PriceSource for tests: every model resolves to
// a fixed price and every body estimates to fixed token counts.
type fakePrice struct{}

func (fakePrice) PriceFor(string) (pricing.ModelPrice, bool) {
	return pricing.ModelPrice{InputUSD: 1_000_000, OutputUSD: 2_000_000}, true
}
func (fakePrice) EstimatePromptTokens([]byte) int { return 10 }
func (fakePrice) SavedCost([]byte) int64          { return 42 }
func (fakePrice) AssumedCompletionTokens() int    { return 5 }

// newTestHandler spins up a mock upstream and returns a handler wired with the
// given memory store and any extra options.
func newTestHandler(t *testing.T, mem *memory.Store, opts ...proxy.HandlerOption) http.Handler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	det := circuitbreaker.NewHashDetector(time.Minute, 2)
	all := append([]proxy.HandlerOption{proxy.WithStore(mem)}, opts...)
	return proxy.NewHandler(u, det, fakePrice{}, all...)
}

func TestLoopBlockAndHistoryReset(t *testing.T) {
	mem := memory.New()
	h := newTestHandler(t, mem)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(proxy.AgentIDHeader, "agent-1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	rec := do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("repeat request: got %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "agent_loop_exception") {
		t.Fatalf("429 body missing agent_loop_exception: %s", rec.Body.String())
	}

	// Two rows so far: one forwarded, one loop-blocked.
	if got := len(mem.Logs()); got != 2 {
		t.Fatalf("logs after block = %d, want 2", got)
	}
	blocked := mem.Logs()[1]
	if !blocked.IsLoopBlocked || blocked.CostUSD != 42 {
		t.Fatalf("blocked row not recorded correctly: %+v", blocked)
	}

	// Clearing the agent's history unblocks the same body.
	del := httptest.NewRequest(http.MethodDelete, "/v1/agent/agent-1/history", nil)
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, del)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("history reset: got %d, want 204", delRec.Code)
	}
	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("request after reset: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestBudgetGateMonthly(t *testing.T) {
	mem := memory.New()
	// Seed spend above the caller's monthly budget.
	if err := mem.LogRequest(context.Background(), proxy.RequestRecord{
		KeyID: "k1", CostUSD: 600, IsLoopBlocked: false, Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, mem)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(proxy.WithCaller(req.Context(), proxy.Caller{
		ID: "k1", Name: "Acme", MonthlyBudgetMicro: 500,
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("monthly budget gate: got %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "budget_exceeded") {
		t.Fatalf("429 body missing budget_exceeded: %s", rec.Body.String())
	}
}

func TestBudgetGateDaily(t *testing.T) {
	mem := memory.New()
	if err := mem.LogRequest(context.Background(), proxy.RequestRecord{
		KeyID: "k1", CostUSD: 100, IsLoopBlocked: false, Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, mem)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(proxy.WithCaller(req.Context(), proxy.Caller{
		ID: "k1", Name: "Acme", DailyBudgetMicro: 50,
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("daily budget gate: got %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Daily budget exceeded") {
		t.Fatalf("429 body missing daily reason: %s", rec.Body.String())
	}
}

func TestMiddlewareAuth(t *testing.T) {
	ks := memory.NewKeyStore()
	ks.Set(proxy.HashAPIKey("secret"), proxy.Caller{ID: "k1", Name: "Acme"})
	auth := proxy.NewAuthenticator(ks)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := proxy.CallerFrom(r.Context())
		if !ok || c.ID != "k1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Header.Get(proxy.APIKeyHeader) != "" { // must be stripped
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := proxy.Middleware(auth, next)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(proxy.APIKeyHeader, "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid key: got %d, want 200", rec.Code)
	}

	bad := httptest.NewRequest(http.MethodPost, "/", nil)
	bad.Header.Set(proxy.APIKeyHeader, "wrong")
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key: got %d, want 401", badRec.Code)
	}
}

func TestBudgetMonitorFiresOncePerThreshold(t *testing.T) {
	var events []proxy.Event
	m := proxy.NewBudgetMonitor(proxy.BudgetConfig{DailyBudgetMicro: 1000, ThresholdPct: []int{50, 100}})
	now := time.Now()

	m.Spend(600, now, func(ev proxy.Event) bool { events = append(events, ev); return true })
	if len(events) != 1 || events[0].ThresholdPct != 50 {
		t.Fatalf("spend 600: got %d events %+v, want [50]", len(events), events)
	}
	m.Spend(1200, now, func(ev proxy.Event) bool { events = append(events, ev); return true })
	if len(events) != 2 {
		t.Fatalf("spend 1200: got %d events, want 2", len(events))
	}
	m.Spend(1200, now, func(ev proxy.Event) bool { events = append(events, ev); return true })
	if len(events) != 2 {
		t.Fatalf("spend 1200 again: got %d events, want 2 (deduped)", len(events))
	}
}
