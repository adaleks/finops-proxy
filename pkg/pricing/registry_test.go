package pricing

// White-box tests for the Registry fallback state machine (§8) and its
// concurrency guarantees. All network traffic goes to local httptest.Server
// instances; RefreshNow is called synchronously where determinism matters, so
// nothing races the background loop.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// catalogBody is a minimal OpenRouter-shaped catalog used by the registry
// tests: one model ("gpt-4o") that overrides the static seed at a different
// price, and one brand-new model the static table does not know.
const catalogBody = `{
  "data": [
    {"id":"gpt-4o","pricing":{"prompt":"0.000000834","completion":"0.000002501"}},
    {"id":"brand-new","pricing":{"prompt":"0.000001","completion":"0.000002"}}
  ]
}`

// newFlipCatalogServer returns an httptest server whose status is read from the
// atomic on every request: 200 serves catalogBody, anything else serves an
// error with that status. Tests flip the atomic to simulate a refresh failure.
func newFlipCatalogServer(status *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := status.Load(); code != http.StatusOK {
			http.Error(w, "boom", int(code))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, catalogBody)
	}))
}

// newRegistry returns a registry seeded with the static fixture, wired to srv
// via a real fetcher, and a config with tiny timeouts so the tests never sleep
// and the background loop (a long RefreshInterval) never interferes.
func newRegistry(t *testing.T, srv *httptest.Server) (*Registry, *Pricing) {
	t.Helper()
	base := loadTestPricing(t)
	cfg := DefaultConfig()
	cfg.Endpoint = srv.URL
	cfg.StartupTimeout = 200 * time.Millisecond
	cfg.HTTPTimeout = 200 * time.Millisecond
	cfg.RefreshInterval = time.Hour // RefreshNow is synchronous; loop stays idle
	reg := NewRegistry(base, NewFetcher(cfg), cfg)
	return reg, base
}

// dynamicGpt4o is the price catalogBody assigns to "gpt-4o", overriding the
// static seed's {2_500_000, 10_000_000}.
var dynamicGpt4o = ModelPrice{InputUSD: 834_000, OutputUSD: 2_501_000}

// TestRegistryStartsStatic verifies the seed state: a fresh registry serves the
// static table, reports SourceStatic, and has a non-nil snapshot.
func TestRegistryStartsStatic(t *testing.T) {
	base := loadTestPricing(t)
	reg := NewRegistry(base, nil, DefaultConfig()) // nil fetcher ⇒ static-only mode

	if k := reg.SourceKind(); k != SourceStatic {
		t.Errorf("SourceKind() = %v, want SourceStatic", k)
	}
	if sn := reg.Snapshot(); sn == nil {
		t.Fatal("Snapshot() = nil, want the static seed")
	} else if sn != base {
		t.Error("Snapshot() must be the static seed before any successful fetch")
	}
	want := ModelPrice{InputUSD: 2_500_000, OutputUSD: 10_000_000}
	if mp, ok := reg.PriceFor("gpt-4o"); !ok || mp != want {
		t.Errorf("PriceFor(gpt-4o) = %+v ok=%v, want %+v ok=true", mp, ok, want)
	}
}

// TestRegistryRefreshSuccessGoesLiveAndOverrides covers the Static→Live
// transition and the D5 merge semantics: the dynamic exact-id price wins over
// the static key, static-only keys survive, brand-new dynamic keys are added,
// and the static base is never mutated.
func TestRegistryRefreshSuccessGoesLiveAndOverrides(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	srv := newFlipCatalogServer(status)
	defer srv.Close()
	reg, base := newRegistry(t, srv)

	if err := reg.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow(success) = %v, want nil", err)
	}
	if k := reg.SourceKind(); k != SourceLive {
		t.Errorf("SourceKind() = %v, want SourceLive", k)
	}
	if mp, ok := reg.PriceFor("gpt-4o"); !ok || mp != dynamicGpt4o {
		t.Errorf("PriceFor(gpt-4o) = %+v ok=%v, want overridden %+v ok=true", mp, ok, dynamicGpt4o)
	}
	static := ModelPrice{InputUSD: 150_000, OutputUSD: 600_000} // gpt-4o-mini from the fixture
	if mp, ok := reg.PriceFor("gpt-4o-mini"); !ok || mp != static {
		t.Errorf("PriceFor(gpt-4o-mini) = %+v ok=%v, want static %+v ok=true", mp, ok, static)
	}
	brandNew := ModelPrice{InputUSD: 1_000_000, OutputUSD: 2_000_000}
	if mp, ok := reg.PriceFor("brand-new"); !ok || mp != brandNew {
		t.Errorf("PriceFor(brand-new) = %+v ok=%v, want %+v ok=true", mp, ok, brandNew)
	}
	if mp, _ := base.PriceFor("gpt-4o"); mp != (ModelPrice{InputUSD: 2_500_000, OutputUSD: 10_000_000}) {
		t.Errorf("static base mutated by merge: gpt-4o = %+v, want original %+v", mp, ModelPrice{InputUSD: 2_500_000, OutputUSD: 10_000_000})
	}
	ts, err := reg.LastRefresh()
	if err != nil {
		t.Errorf("LastRefresh() err = %v, want nil", err)
	}
	if ts.IsZero() {
		t.Error("LastRefresh() time must be non-zero after a successful refresh")
	}
}

// TestRegistryRefreshFailureKeepsLastGood covers Live→Stale: a refresh failure
// records the error, keeps the last-good table serving, and never tears the
// table down.
func TestRegistryRefreshFailureKeepsLastGood(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	srv := newFlipCatalogServer(status)
	defer srv.Close()
	reg, _ := newRegistry(t, srv)

	if err := reg.RefreshNow(context.Background()); err != nil {
		t.Fatalf("setup RefreshNow(success) = %v, want nil", err)
	}
	if mp, _ := reg.PriceFor("gpt-4o"); mp != dynamicGpt4o {
		t.Fatalf("setup: gpt-4o = %+v, want %+v", mp, dynamicGpt4o)
	}

	status.Store(http.StatusInternalServerError)
	if err := reg.RefreshNow(context.Background()); err == nil {
		t.Fatal("RefreshNow(500) = nil error, want error")
	}
	if k := reg.SourceKind(); k != SourceStale {
		t.Errorf("SourceKind() = %v, want SourceStale", k)
	}
	// The last-good table survives: still the fetched price, not the static one.
	if mp, ok := reg.PriceFor("gpt-4o"); !ok || mp != dynamicGpt4o {
		t.Errorf("PriceFor(gpt-4o) after failure = %+v ok=%v, want last-good %+v ok=true", mp, ok, dynamicGpt4o)
	}
	if ts, err := reg.LastRefresh(); ts.IsZero() || err == nil {
		t.Errorf("LastRefresh() = %v, %v; want non-zero time and a non-nil error", ts, err)
	}
}

// TestRegistryEmptyTableIsError pins D7 in the registry: a syntactically valid
// but empty catalog is a refresh failure (ErrEmptyTable), never swaps in an
// empty table, and leaves the registry Static.
func TestRegistryEmptyTableIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	reg, base := newRegistry(t, srv)

	err := reg.RefreshNow(context.Background())
	if !errors.Is(err, ErrEmptyTable) {
		t.Fatalf("RefreshNow(empty) err = %v, want ErrEmptyTable", err)
	}
	if k := reg.SourceKind(); k != SourceStatic {
		t.Errorf("SourceKind() = %v, want SourceStatic (empty table never adopted)", k)
	}
	if sn := reg.Snapshot(); sn != base {
		t.Error("Snapshot() must remain the static seed after an empty fetch")
	}
	if ts, err := reg.LastRefresh(); !ts.IsZero() || err == nil {
		t.Errorf("LastRefresh() = %v, %v; want zero time and ErrEmptyTable", ts, err)
	}
}

// TestRegistryNeverRevertsToStatic asserts D4: once a network table is adopted,
// repeated failures keep the registry Stale and it never goes back to Static.
func TestRegistryNeverRevertsToStatic(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	srv := newFlipCatalogServer(status)
	defer srv.Close()
	reg, _ := newRegistry(t, srv)

	if err := reg.RefreshNow(context.Background()); err != nil {
		t.Fatalf("setup RefreshNow = %v, want nil", err)
	}
	if reg.SourceKind() != SourceLive {
		t.Fatalf("setup SourceKind = %v, want SourceLive", reg.SourceKind())
	}

	status.Store(http.StatusInternalServerError)
	for i := 0; i < 3; i++ {
		if err := reg.RefreshNow(context.Background()); err == nil {
			t.Fatalf("failure %d: RefreshNow = nil, want error", i+1)
		}
		if k := reg.SourceKind(); k != SourceStale {
			t.Errorf("failure %d: SourceKind() = %v, want SourceStale (never back to static)", i+1, k)
		}
	}
	if mp, _ := reg.PriceFor("gpt-4o"); mp != dynamicGpt4o {
		t.Errorf("PriceFor(gpt-4o) after repeated failures = %+v, want last-good %+v", mp, dynamicGpt4o)
	}
}

// TestRegistryNextDelay pins the §8.3 backoff formula:
// nextDelay = min(RefreshInterval, min(BackoffBase × 2^(fails−1), BackoffMax)).
func TestRegistryNextDelay(t *testing.T) {
	base := loadTestPricing(t)
	newReg := func(interval, backoffBase, backoffMax time.Duration) *Registry {
		cfg := DefaultConfig()
		cfg.RefreshInterval = interval
		cfg.BackoffBase = backoffBase
		cfg.BackoffMax = backoffMax
		return NewRegistry(base, nil, cfg)
	}
	setFails := func(r *Registry, n int) {
		r.mu.Lock()
		r.fails = n
		r.mu.Unlock()
	}

	t.Run("defaults", func(t *testing.T) {
		reg := newReg(6*time.Hour, 10*time.Second, 5*time.Minute)
		tests := []struct {
			fails int
			want  time.Duration
		}{
			{fails: 0, want: 6 * time.Hour},     // no failures ⇒ base interval
			{fails: 1, want: 10 * time.Second},  // 10s × 2^0
			{fails: 2, want: 20 * time.Second},  // 10s × 2^1
			{fails: 3, want: 40 * time.Second},  // 10s × 2^2
			{fails: 5, want: 160 * time.Second}, // 10s × 2^4
			{fails: 6, want: 5 * time.Minute},   // 10s × 2^5 = 320s → capped at BackoffMax
			{fails: 20, want: 5 * time.Minute},  // cap holds (10s × 2^19 still fits int64 ns)
		}
		for _, tt := range tests {
			setFails(reg, tt.fails)
			if got := reg.nextDelay(); got != tt.want {
				t.Errorf("nextDelay(fails=%d) = %v, want %v", tt.fails, got, tt.want)
			}
		}
	})

	t.Run("clamped by interval", func(t *testing.T) {
		// A RefreshInterval shorter than the first backoff must win: the loop
		// never waits longer than the base interval between attempts.
		reg := newReg(5*time.Second, 10*time.Second, time.Minute)
		setFails(reg, 1)
		if got := reg.nextDelay(); got != 5*time.Second {
			t.Errorf("nextDelay(fails=1, interval=5s) = %v, want %v", got, 5*time.Second)
		}
	})
}

// TestRegistryNextDelaySaturates pins the fix for a former int64-overflow bug:
// with the default BackoffBase of 10s, fails≥31 used to wrap the shift multiply
// (10s × 2^30 > MaxInt64 ns) into a NEGATIVE duration, which made the background
// loop's timer fire immediately and busy-hammer a dead endpoint. The saturation
// loop now stops at BackoffMax before the value can overflow, so any large
// consecutive-failure count must resolve to exactly BackoffMax.
func TestRegistryNextDelaySaturates(t *testing.T) {
	base := loadTestPricing(t)
	cfg := DefaultConfig() // BackoffBase = 10s, BackoffMax = 5m
	reg := NewRegistry(base, nil, cfg)

	for _, fails := range []int{31, 32, 100, 1000, 1 << 20} {
		reg.mu.Lock()
		reg.fails = fails
		reg.mu.Unlock()

		if got := reg.nextDelay(); got != 5*time.Minute {
			t.Errorf("nextDelay(fails=%d) = %v, want %v (BackoffMax, no overflow)", fails, got, 5*time.Minute)
		}
	}
}

// TestBootstrapFailingServerStaysStatic covers §8.2: a failed bounded bootstrap
// must not panic and must leave the registry serving the static seed.
func TestBootstrapFailingServerStaysStatic(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusInternalServerError)
	srv := newFlipCatalogServer(status)
	defer srv.Close()
	reg, base := newRegistry(t, srv)

	reg.Bootstrap(context.Background())
	if k := reg.SourceKind(); k != SourceStatic {
		t.Errorf("SourceKind() = %v, want SourceStatic after failed bootstrap", k)
	}
	if sn := reg.Snapshot(); sn == nil {
		t.Fatal("Snapshot() = nil after failed bootstrap")
	} else if sn != base {
		t.Error("Snapshot() must stay the static seed when bootstrap fails")
	}
}

// TestBootstrapWorkingServerGoesLive covers the successful bootstrap path: the
// registry serves the fetched table immediately on boot.
func TestBootstrapWorkingServerGoesLive(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	srv := newFlipCatalogServer(status)
	defer srv.Close()
	reg, _ := newRegistry(t, srv)

	reg.Bootstrap(context.Background())
	if k := reg.SourceKind(); k != SourceLive {
		t.Errorf("SourceKind() = %v, want SourceLive after successful bootstrap", k)
	}
	if mp, _ := reg.PriceFor("gpt-4o"); mp != dynamicGpt4o {
		t.Errorf("PriceFor(gpt-4o) after bootstrap = %+v, want fetched %+v", mp, dynamicGpt4o)
	}
}

// TestRegistryMergeSemantics isolates the D3/D5 merge: dynamic exact-id wins,
// static-only keys survive, brand-new keys are added, the default and the
// estimation knobs come from the static seed, and base is never mutated.
func TestRegistryMergeSemantics(t *testing.T) {
	base := loadTestPricing(t)
	reg := NewRegistry(base, nil, DefaultConfig())

	merged := reg.merge(map[string]ModelPrice{
		"gpt-4o":    {InputUSD: 1, OutputUSD: 2},
		"new/model": {InputUSD: 3, OutputUSD: 4},
	})

	if mp, _ := merged.PriceFor("gpt-4o"); mp != (ModelPrice{InputUSD: 1, OutputUSD: 2}) {
		t.Errorf("merged gpt-4o = %+v, want dynamic %+v", mp, ModelPrice{InputUSD: 1, OutputUSD: 2})
	}
	if mp, _ := merged.PriceFor("gpt-4o-mini"); mp != (ModelPrice{InputUSD: 150_000, OutputUSD: 600_000}) {
		t.Errorf("merged gpt-4o-mini = %+v, want static %+v", mp, ModelPrice{InputUSD: 150_000, OutputUSD: 600_000})
	}
	if mp, ok := merged.PriceFor("new/model"); !ok || mp != (ModelPrice{InputUSD: 3, OutputUSD: 4}) {
		t.Errorf("merged new/model = %+v ok=%v, want %+v ok=true", mp, ok, ModelPrice{InputUSD: 3, OutputUSD: 4})
	}
	if mp, ok := merged.PriceFor("unknown-xyz"); ok || mp != base.def {
		t.Errorf("merged default = %+v ok=%v, want base default %+v ok=false", mp, ok, base.def)
	}
	if merged.AssumedCompletionTokens() != base.AssumedCompletionTokens() {
		t.Errorf("merged AssumedCompletionTokens = %d, want %d", merged.AssumedCompletionTokens(), base.AssumedCompletionTokens())
	}
	if mp, _ := base.PriceFor("gpt-4o"); mp != (ModelPrice{InputUSD: 2_500_000, OutputUSD: 10_000_000}) {
		t.Errorf("static base mutated: gpt-4o = %+v, want %+v", mp, ModelPrice{InputUSD: 2_500_000, OutputUSD: 10_000_000})
	}
}

// TestRegistryConcurrentReadsDuringRefresh is a smoke test for the reader
// guarantees: many goroutines call PriceFor/Snapshot/SourceKind while
// RefreshNow alternately succeeds and fails, swapping cur under lock. Run under
// -race to validate that no reader can observe a torn table.
func TestRegistryConcurrentReadsDuringRefresh(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	srv := newFlipCatalogServer(status)
	defer srv.Close()
	reg, _ := newRegistry(t, srv)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 16; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				reg.PriceFor("gpt-4o")
				reg.Snapshot()
				reg.SourceKind()
			}
		}()
	}

	// Exercise the state machine under load: alternate success/failure so cur is
	// swapped in and out while readers hold the RLock. Errors on the odd (500)
	// iterations are expected and ignored.
	for i := 0; i < 30; i++ {
		if i%2 == 0 {
			status.Store(http.StatusOK)
		} else {
			status.Store(http.StatusInternalServerError)
		}
		_ = reg.RefreshNow(context.Background())
	}
	close(stop)
	readers.Wait()

	if k := reg.SourceKind(); k != SourceLive && k != SourceStale {
		t.Errorf("final SourceKind() = %v, want Live or Stale (never Static after a success)", k)
	}
}

// TestDefaultConfig pins the §10 default knobs.
func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RefreshInterval != 6*time.Hour {
		t.Errorf("RefreshInterval = %v, want 6h", cfg.RefreshInterval)
	}
	if cfg.HTTPTimeout != 30*time.Second {
		t.Errorf("HTTPTimeout = %v, want 30s", cfg.HTTPTimeout)
	}
	if !cfg.HardFailOnEmpty {
		t.Error("HardFailOnEmpty = false, want true")
	}
	if cfg.Endpoint == "" {
		t.Error("Endpoint = \"\", want the OpenRouter catalog URL")
	}
	if cfg.BackoffBase != 10*time.Second {
		t.Errorf("BackoffBase = %v, want 10s", cfg.BackoffBase)
	}
	if cfg.BackoffMax != 5*time.Minute {
		t.Errorf("BackoffMax = %v, want 5m", cfg.BackoffMax)
	}
	if cfg.StartupTimeout != 3*time.Second {
		t.Errorf("StartupTimeout = %v, want 3s", cfg.StartupTimeout)
	}
}
