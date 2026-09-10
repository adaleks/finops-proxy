// Thread-safe price registry: the single owner of the active price table, the
// background refresh loop, and the fallback state machine. It starts from the
// static config/pricing.json table (the guaranteed seed) and, when a fetcher is
// configured, overlays fresh catalog prices in the background.
//
// Reader invariants (D4/D7): readers never see an empty/partial/torn table and
// never block on the network. cur is an immutable *Pricing swapped whole under
// mu; a refresh failure keeps the last-good table and never reverts to the
// static file once a network table has been adopted.
package pricing

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// SourceKind identifies the current state of the registry's table.
type SourceKind int

const (
	// SourceStatic: the static config/pricing.json table; no successful network
	// fetch has ever replaced it (never a successful fetch).
	SourceStatic SourceKind = iota
	// SourceLive: a network table, last refresh OK.
	SourceLive
	// SourceStale: a network table was adopted, but the last refresh FAILED.
	SourceStale
)

// Registry is a thread-safe PriceSource. See the package comment for the reader
// guarantees and the fallback ordering (live > stale-last-good > static).
type Registry struct {
	mu  sync.RWMutex
	cur *Pricing // immutable snapshot; never mutated after construction

	base    *Pricing // static seed (default + estimation knobs + config keys)
	fetcher *Fetcher // nil => static-only mode
	cfg     Config

	onSync func(ctx context.Context, models map[string]ModelPrice) (*Pricing, error)

	kind     SourceKind
	lastSync time.Time // last SUCCESSFUL fetch; zero if never
	lastErr  error     // last refresh failure (nil on success)
	fails    int       // consecutive refresh failures (backoff input)

	refreshMu sync.Mutex // serializes refresh (background loop vs RefreshNow)
	stopCh    chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// NewRegistry builds a registry seeded with base (the loaded static table).
// base must be non-nil — it is the guaranteed seed served until the first
// successful network fetch. fetcher may be nil for static-only mode.
func NewRegistry(base *Pricing, fetcher *Fetcher, cfg Config) *Registry {
	cfg = normalizeConfig(cfg)
	return &Registry{
		cur:     base,
		base:    base,
		fetcher: fetcher,
		cfg:     cfg,
		kind:    SourceStatic,
		stopCh:  make(chan struct{}),
	}
}

// Bootstrap performs one synchronous, bounded startup fetch so the proxy can
// serve a live table immediately on boot. On failure it logs a WARN and keeps
// the static table — a network outage must not prevent startup (8.2). It is a
// no-op in static-only mode.
func (r *Registry) Bootstrap(ctx context.Context) {
	if r == nil || r.fetcher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.StartupTimeout)
	defer cancel()
	if err := r.refresh(ctx); err != nil {
		log.Printf("[pricing] network bootstrap failed, using static table %s: %v", r.cfg.PricingPath, err)
	}
}

// Start launches the background refresh loop and returns immediately (7.3). The
// loop does an immediate first fetch — unless Bootstrap already populated the
// table (lastSync non-zero), which avoids a redundant double-fetch at startup —
// then re-fetches on a timer with exponential backoff on failure. Start is a
// no-op in static-only mode or when background refresh is disabled
// (RefreshInterval <= 0).
func (r *Registry) Start(ctx context.Context) {
	if r == nil || r.fetcher == nil || r.cfg.RefreshInterval <= 0 {
		return
	}
	r.wg.Add(1)
	go r.loop(ctx)
}

// loop is the single background refresh goroutine. One goroutine, one timer:
// failures shorten the next delay (backoff), success resets it to the base
// interval. It exits on ctx.Done or stopCh.
func (r *Registry) loop(ctx context.Context) {
	defer r.wg.Done()
	if !r.bootstrapped() {
		if err := r.refresh(ctx); err != nil {
			r.logRefreshFailure(err)
		}
	}
	for {
		timer := time.NewTimer(r.nextDelay())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			if err := r.refresh(ctx); err != nil {
				r.logRefreshFailure(err)
			}
		}
	}
}

// Stop stops the background loop and waits for any in-flight refresh to finish
// (bounded by HTTPTimeout). It is idempotent and safe to call multiple times.
// Call it after Start; the wg counter is set by Start, so Stop without Start is
// a no-op that just closes stopCh.
func (r *Registry) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stopCh)
		r.wg.Wait()
	})
}

// RefreshNow performs one fetch+merge synchronously and returns the outcome. It
// is safe to call concurrently with the background loop — refreshMu serializes.
func (r *Registry) RefreshNow(ctx context.Context) error {
	return r.refresh(ctx)
}

// LastRefresh reports the time of the last successful refresh and the last
// refresh error (nil on success, or when a fetch was never attempted).
func (r *Registry) LastRefresh() (time.Time, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastSync, r.lastErr
}

// SourceKind reports the current table source (static/live/stale).
func (r *Registry) SourceKind() SourceKind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.kind
}

// SetOnSync installs the persistence hook for the background refresh loop. When
// set, refresh calls fn with the freshly fetched models instead of merge, and
// swaps in the *Pricing fn returns. The hook is the Service.OnSync seam; it is
// installed once at startup before Bootstrap/Start. Safe for concurrent use.
func (r *Registry) SetOnSync(fn func(ctx context.Context, models map[string]ModelPrice) (*Pricing, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onSync = fn
}

// Replace swaps in tbl as the current serving table, preserving the current
// kind, last-sync time and failure counter. It is the manual-PUT path: the
// caller has already persisted the row and rebuilt a fresh *Pricing. A nil tbl
// is a no-op (defensive).
func (r *Registry) Replace(tbl *Pricing) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tbl != nil {
		r.cur = tbl
	}
}

// ReplaceLive swaps in tbl and marks the table live, resetting the failure
// counter, last error and last-sync time. It is the manual-sync path (POST
// /api/v1/pricing/sync). A nil tbl is a no-op (defensive).
func (r *Registry) ReplaceLive(tbl *Pricing, lastSync time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tbl != nil {
		r.cur = tbl
	}
	r.lastSync = lastSync
	r.lastErr = nil
	r.fails = 0
	r.kind = SourceLive
}

// Snapshot returns the current immutable table. The registry never mutates the
// returned *Pricing, so a caller may hold it indefinitely.
func (r *Registry) Snapshot() *Pricing {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur
}

// The four read paths delegate to cur under RLock. cur is immutable after
// construction, so a reader can never observe a torn/partial table, and the
// write path builds a complete new *Pricing off-lock before swapping the
// pointer under Lock().

func (r *Registry) PriceFor(model string) (ModelPrice, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur.PriceFor(model)
}

func (r *Registry) EstimatePromptTokens(body []byte) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur.EstimatePromptTokens(body)
}

func (r *Registry) SavedCost(body []byte) int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur.SavedCost(body)
}

func (r *Registry) AssumedCompletionTokens() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur.AssumedCompletionTokens()
}

// bootstrapped reports whether a successful fetch already populated the table.
// Start skips its immediate first fetch when it has, avoiding a redundant
// double-fetch at startup (Bootstrap runs just before Start in main).
func (r *Registry) bootstrapped() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.lastSync.IsZero()
}

// refresh performs one fetch+merge under refreshMu (7.2). On success it swaps
// in the merged table, resets the failure counter, and marks the table live. On
// failure it records the error, keeps the last-good table, and bumps the
// failure counter — never touching cur, and never going back to Static once a
// network table was adopted.
func (r *Registry) refresh(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	if r.fetcher == nil {
		err := errors.New("pricing: no network fetcher configured")
		r.mu.Lock()
		r.lastErr = err
		r.fails++
		r.mu.Unlock()
		return err
	}

	// Bound one fetch by HTTPTimeout; when the caller (Bootstrap) already
	// imposed a shorter deadline, the derived context inherits the earlier one.
	childCtx, cancel := context.WithTimeout(ctx, r.cfg.HTTPTimeout)
	defer cancel()

	models, err := r.fetcher.FetchModels(childCtx, r.cfg.Endpoint)
	if err != nil {
		return r.refreshFailed(err)
	}
	if len(models) == 0 {
		// A fetch that succeeded but produced no priced models is a failure
		// (D7): swapping in an empty table would silently zero every cost.
		return r.refreshFailed(ErrEmptyTable)
	}

	// When a persistence hook is installed, delegate the merge to it (it
	// persists the sync rows and returns a DB-rebuilt table); otherwise use the
	// plain in-memory merge. Either way a complete table is built off-lock and
	// swapped in whole below.
	var tbl *Pricing
	r.mu.RLock()
	onSync := r.onSync
	r.mu.RUnlock()
	if onSync != nil {
		var err error
		tbl, err = onSync(childCtx, models)
		if err != nil {
			return r.refreshFailed(err)
		}
	} else {
		tbl = r.merge(models)
	}

	r.mu.Lock()
	r.cur = tbl
	r.lastSync = time.Now()
	r.lastErr = nil
	r.fails = 0
	r.kind = SourceLive
	r.mu.Unlock()
	return nil
}

// refreshFailed records a refresh failure without touching cur: the last-good
// table keeps serving. Kind stays Static until the first success, then moves to
// Stale on any later failure (never back to Static, D4).
func (r *Registry) refreshFailed(err error) error {
	r.mu.Lock()
	r.lastErr = err
	r.fails++
	if r.kind != SourceStatic {
		r.kind = SourceStale
	}
	r.mu.Unlock()
	return err
}

// logRefreshFailure emits the §8.3 failure line: how many consecutive failures,
// that the last-good table is kept, and — once the table has aged past
// StaleAfter — that it is stale. Informational only.
func (r *Registry) logRefreshFailure(err error) {
	r.mu.RLock()
	fails := r.fails
	lastSync := r.lastSync
	r.mu.RUnlock()

	switch {
	case !lastSync.IsZero() && time.Since(lastSync) > r.cfg.StaleAfter:
		log.Printf("[pricing] refresh failed (%d consecutive), keeping last-good table (lastSync=%s, age=%v), table is stale: %v",
			fails, lastSync.Format(time.RFC3339), time.Since(lastSync).Round(time.Second), err)
	case !lastSync.IsZero():
		log.Printf("[pricing] refresh failed (%d consecutive), keeping last-good table (lastSync=%s): %v",
			fails, lastSync.Format(time.RFC3339), err)
	default:
		log.Printf("[pricing] refresh failed (%d consecutive), no network table yet, serving static: %v", fails, err)
	}
}

// merge overlays the dynamic table over the static seed (7.2, D3/D5):
// merged.models starts as a copy of base.models and each dynamic exact-id entry
// replaces its base entry. The default price and the estimation knobs always
// come from base — the remote never supplies a default. base is never mutated.
func (r *Registry) merge(models map[string]ModelPrice) *Pricing {
	base := r.base
	merged := &Pricing{
		assumedCompletionTokens:       base.assumedCompletionTokens,
		PromptEstimationCharsPerToken: base.PromptEstimationCharsPerToken,
		models:                        make(map[string]ModelPrice, len(base.models)+len(models)),
		def:                           base.def,
	}
	for k, v := range base.models {
		merged.models[k] = v
	}
	for k, v := range models {
		merged.models[k] = v
	}
	return merged
}

// nextDelay returns the delay before the next refresh attempt: the base
// RefreshInterval, or the exponential backoff after consecutive failures —
// BackoffBase × 2^(fails−1) — capped at BackoffMax and then at RefreshInterval
// (7.3). A success resets fails to 0, so the delay returns to RefreshInterval.
func (r *Registry) nextDelay() time.Duration {
	r.mu.RLock()
	fails := r.fails
	r.mu.RUnlock()
	if fails <= 0 {
		return r.cfg.RefreshInterval
	}
	backoff := r.cfg.BackoffBase
	// Double backoff (fails−1) times, stopping early once it reaches BackoffMax.
	// Saturating early keeps the value far below int64 nanoseconds and avoids the
	// overflow that a single BackoffBase<<shift multiply hits at large shift (with
	// the default 10s base, fails≥31 wraps negative and busy-loops a dead endpoint).
	for i := 1; i < fails && backoff < r.cfg.BackoffMax; i++ {
		backoff *= 2
	}
	if backoff > r.cfg.BackoffMax {
		backoff = r.cfg.BackoffMax
	}
	if backoff < r.cfg.RefreshInterval {
		return backoff
	}
	return r.cfg.RefreshInterval
}
