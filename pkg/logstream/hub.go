// Package logstream implements the transport-agnostic pub/sub hub behind the
// live log stream. It fans LogEvent values out to every subscriber without
// ever blocking the publisher; SSE framing and the HTTP surface live in
// internal/api/logs.go, so the hub has no HTTP dependency.
package logstream

import (
	"context"
	"sync"
)

// TenantNameResolver maps a tenant_id to its display team name. The bool is
// false when the tenant is unknown — including the legacy "" bucket, which has
// no tenants row. A nil resolver leaves TenantName exactly as the publisher set
// it.
type TenantNameResolver func(tenantID string) (string, bool)

// LogEvent is one live stream event. All money fields are integer micro-dollars
// (µUSD); float USD conversion happens only at the JSON boundary in the API
// layer. Timestamp is Unix epoch seconds (UTC), like db.RequestLog.Timestamp.
type LogEvent struct {
	Type      string `json:"type"`
	Level     string `json:"level"`
	Timestamp int64  `json:"timestamp"`

	TenantID    string `json:"tenant_id,omitempty"`
	TenantName  string `json:"tenant_name,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	Model       string `json:"model,omitempty"`
	ProjectName string `json:"project_name,omitempty"`

	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`

	CostMicroUSD       int64 `json:"cost_micro_usd,omitempty"`
	SavedCostMicroUSD  int64 `json:"saved_cost_micro_usd,omitempty"`
	BudgetMicroUSD     int64 `json:"budget_micro_usd,omitempty"`     // daily budget figure (budget_level)
	SpentMicroUSD      int64 `json:"spent_micro_usd,omitempty"`      // actual spend at the moment of the event
	MonthlyBudgetMicro int64 `json:"monthly_budget_micro,omitempty"` // per-caller monthly budget (budget_exceeded)

	StatusCode    int    `json:"status_code,omitempty"`
	FirstBlock    bool   `json:"first_block,omitempty"`
	Reason        string `json:"reason,omitempty"`
	RetryAfter    int    `json:"retry_after,omitempty"`
	ThresholdPct  int    `json:"threshold_pct,omitempty"`
	DroppedEvents uint64 `json:"dropped_events,omitempty"`
}

// Event type discriminators — the "type" field on the wire. TypeRequestStart is
// reserved for future use and never emitted in the MVP.
const (
	TypeRequestComplete = "request.complete"
	TypeLoopBlocked     = "loop_blocked"
	TypeBudgetExceeded  = "budget_exceeded"
	TypeBudgetLevel     = "budget_level"
	TypeCostAlert       = "cost_alert"
	TypeSettingsUpdated = "settings.updated"
)

// Hub is a fan-out pub/sub hub. Publish delivers a LogEvent to every current
// subscriber without ever blocking: a slow subscriber drops the oldest buffered
// event (then the new one on a still-full buffer) and counts the drop. It is
// safe for concurrent use.
type Hub struct {
	mu       sync.RWMutex
	subs     map[*subscriber]struct{}
	resolver TenantNameResolver
}

// subscriber is one buffered delivery queue. mu serializes the drain-then-push
// on overflow so two concurrent Publish calls can never interleave on the same
// channel.
type subscriber struct {
	mu      sync.Mutex
	ch      chan LogEvent
	dropped uint64
}

// Subscription is a live delivery handle. Reading from C yields buffered events
// in FIFO order. Close unsubscribes from the hub and closes C; it is idempotent
// and safe to call from any goroutine.
type Subscription struct {
	hub  *Hub
	sub  *subscriber
	once sync.Once
	C    <-chan LogEvent
}

// NewHub builds a Hub. resolver may be nil, in which case TenantName is never
// re-resolved at publish time.
func NewHub(resolver TenantNameResolver) *Hub {
	return &Hub{
		subs:     make(map[*subscriber]struct{}),
		resolver: resolver,
	}
}

// Subscribe registers a new subscriber with a buffered channel of size buf
// (buf < 1 is treated as 1) and returns its Subscription. The context is
// accepted so the SSE handler can pass the request context, but cancellation is
// the caller's responsibility: the handler selects on r.Context().Done() and
// defers Close. A cancelled context alone does NOT unregister.
func (h *Hub) Subscribe(_ context.Context, buf int) *Subscription {
	if buf < 1 {
		buf = 1
	}
	sub := &subscriber{ch: make(chan LogEvent, buf)}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return &Subscription{hub: h, sub: sub, C: sub.ch}
}

// Close unsubscribes s from the hub and closes its delivery channel. It is
// idempotent. A Publish already delivering to this subscriber completes before
// the channel closes: Close takes the hub's write lock, which blocks until every
// in-flight Publish (which holds the read lock across its fan-out) has finished.
func (s *Subscription) Close() {
	s.once.Do(func() {
		h := s.hub
		h.mu.Lock()
		delete(h.subs, s.sub)
		h.mu.Unlock()
		close(s.sub.ch)
	})
}

// Dropped returns the number of events this subscriber has dropped: drop-oldest
// plus drop-new on a persistently full buffer.
func (s *Subscription) Dropped() uint64 {
	s.sub.mu.Lock()
	defer s.sub.mu.Unlock()
	return s.sub.dropped
}

// QueueDepth returns the number of events currently buffered for this
// subscriber. It powers the stream_stats control event's queue_depth field.
func (s *Subscription) QueueDepth() int {
	s.sub.mu.Lock()
	defer s.sub.mu.Unlock()
	return len(s.sub.ch)
}

// Len returns the number of currently registered subscribers. It exists for
// tests and diagnostics.
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Publish delivers ev to every registered subscriber. It resolves TenantName
// via the injected resolver (once, before fan-out) when the publisher left it
// empty and a tenant is bound; otherwise the publisher's TenantName is kept
// verbatim (the budget_exceeded path sets it directly from auth.Tenant). It
// never blocks: a subscriber with a full buffer first drops its oldest queued
// event, then, if the buffer is still full, drops the new event instead. Both
// drops are counted under the subscriber's mutex so concurrent publishers
// cannot interleave on one channel.
func (h *Hub) Publish(ev LogEvent) {
	if h.resolver != nil && ev.TenantName == "" && ev.TenantID != "" {
		if name, ok := h.resolver(ev.TenantID); ok {
			ev.TenantName = name
		}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subs {
		sub.mu.Lock()
		// Drop the oldest buffered event only when the buffer is full, so a slow
		// client always sees the most recent traffic while a healthy backlog is
		// preserved. A non-blocking receive on a full channel pops exactly one
		// element, making room for the new event below.
		if len(sub.ch) == cap(sub.ch) {
			select {
			case <-sub.ch:
				sub.dropped++
			default:
			}
		}
		// If it is STILL full (e.g. buffer of 1), drop the new event instead.
		select {
		case sub.ch <- ev:
		default:
			sub.dropped++
		}
		sub.mu.Unlock()
	}
}
