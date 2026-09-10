package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/adaleks/finops-proxy/pkg/circuitbreaker"
	"github.com/adaleks/finops-proxy/pkg/logstream"
	"github.com/adaleks/finops-proxy/pkg/pricing"
)

// MaxBodyBytes caps the request body that is read and hashed by the proxy.
//
// Requests whose declared Content-Length exceeds this cap are forwarded
// untouched (fail-open, the circuit breaker is skipped for them); the cap also
// backs http.MaxBytesReader for chunked or unknown-length bodies, so a body
// that overflows it during the read yields a 413 instead of a hash.
// Tests may lower this variable.
var MaxBodyBytes int64 = 10 << 20 // 10 MiB

// AgentIDHeader is the request header carrying the caller's agent id. It scopes
// loop history per agent and is stripped before the request is forwarded
// upstream (it is proxy-internal).
const AgentIDHeader = "X-FinOps-Agent-Id"

// ProjectHeader is the request header carrying the caller's project name. It
// scopes cost accounting per project and is stripped before the request is
// forwarded upstream (it is proxy-internal). The empty string is the default
// bucket when the header is absent.
const ProjectHeader = "X-FinOps-Project"

// handler is the circuit-breaker HTTP handler. It owns a ReverseProxy for the
// upstream, the detector consulted for each hashed body, and — when provided —
// the pricing table and the store used to record the cost of every proxied
// call, plus the optional notifier and budget monitor.
type handler struct {
	rp    *httputil.ReverseProxy
	det   circuitbreaker.Detector
	price pricing.PriceSource

	store     RequestLogStore  // nil = skip cost recording
	ledger    BudgetLedger     // nil = skip budget enforcement
	incidents IncidentRecorder // nil = skip durable incident recording

	notifier Notifier      // nil disables ALL alerting
	budget   BudgetSpender // nil disables budget-level alerts

	auth Authenticator // nil = auth disabled (fail-open)

	hub           *logstream.Hub      // nil disables the live log stream
	costThreshold CostThresholdReader // nil disables per-request cost alerts
	emitter       Emitter             // nil disables async payload emission

	metrics *Metrics // nil disables observability collection
}

// HandlerOption customizes a handler at construction time. Options are applied
// in order after the handler struct is built and before the mux is created.
type HandlerOption func(*handler)

// WithNotifier wires a Notifier into the handler. A nil notifier (or omitting
// this option) disables all alerting: emit becomes a no-op.
func WithNotifier(n Notifier) HandlerOption {
	return func(h *handler) { h.notifier = n }
}

// WithBudget wires a daily budget monitor built from a BudgetConfig.
// NewBudgetMonitor returns an inert monitor for a zero budget or empty
// thresholds, so Spend no-ops either way.
func WithBudget(cfg BudgetConfig) HandlerOption {
	return func(h *handler) { h.budget = NewBudgetMonitor(cfg) }
}

// WithBudgetSpender wires a BudgetSpender (a runtime-reconfigurable manager, or
// any test fake) into the handler's budget seam. It is the preferred wiring
// when the budget is reloadable at runtime; WithBudget remains for the
// file-only path and tests.
func WithBudgetSpender(s BudgetSpender) HandlerOption {
	return func(h *handler) { h.budget = s }
}

// WithAuth wires an authenticator into the handler. A nil authenticator (or
// omitting this option) disables authentication: the proxy behaves exactly as
// it did without the auth layer. When set, NewHandler wraps the whole mux in
// Middleware, so the proxy path and the DELETE /v1/agent/{id}/history route
// both require a valid key.
func WithAuth(a Authenticator) HandlerOption {
	return func(h *handler) { h.auth = a }
}

// WithStore wires a combined Store into the handler: it becomes the request
// logger, the budget ledger, and (if the store also satisfies IncidentRecorder)
// the incident recorder. This is the primary wiring for the reference stores.
// Individual concerns can be wired or nil'd separately with WithRequestLogStore,
// WithBudgetLedger, and WithIncidentRecorder.
func WithStore(s Store) HandlerOption {
	return func(h *handler) {
		h.store = s
		h.ledger = s
		if ir, ok := s.(IncidentRecorder); ok {
			h.incidents = ir
		}
	}
}

// WithRequestLogStore wires cost recording independently of the budget ledger.
func WithRequestLogStore(s RequestLogStore) HandlerOption {
	return func(h *handler) { h.store = s }
}

// WithBudgetLedger wires budget enforcement independently of cost recording.
func WithBudgetLedger(l BudgetLedger) HandlerOption {
	return func(h *handler) { h.ledger = l }
}

// WithIncidentRecorder wires durable incident recording. A nil recorder (the
// default) skips incident persistence; loop-blocked rows still land in the
// request log.
func WithIncidentRecorder(r IncidentRecorder) HandlerOption {
	return func(h *handler) { h.incidents = r }
}

// CostThresholdReader is the narrow seam through which the proxy reads the
// runtime cost-alert threshold on the hot path. The enterprise settings service
// satisfies it; defining the interface here keeps package proxy free of a
// settings import.
type CostThresholdReader interface {
	// CostAlertThresholdMicro returns the per-request cost alert threshold in
	// integer µUSD. 0 disables per-request cost alerts.
	CostAlertThresholdMicro() int64
}

// WithLogStream wires a live logstream.Hub into the handler so interception
// events (request.complete, loop_blocked, budget_exceeded, budget_level,
// cost_alert) are published to the stream. A nil hub (or omitting this option)
// disables live logging; publishLog is a no-op.
func WithLogStream(hub *logstream.Hub) HandlerOption {
	return func(h *handler) { h.hub = hub }
}

// WithCostThreshold wires a CostThresholdReader so every completed request whose
// cost reaches the threshold emits a cost_alert live event. A nil reader (or
// omitting this option) disables per-request cost alerts. The reader must be
// safe for concurrent calls from the proxy's hot path.
func WithCostThreshold(r CostThresholdReader) HandlerOption {
	return func(h *handler) { h.costThreshold = r }
}

// NewHandler builds the FinOps circuit-breaker proxy handler for the given
// upstream. Every non-empty POST body up to MaxBodyBytes is hashed and checked
// against det before being forwarded to upstream; blocked repeats receive an
// HTTP 429 and are never forwarded. The DELETE /v1/agent/{id}/history route
// clears one agent's history via det.Reset.
//
// When price and a request-log store are wired (WithStore), every proxied call
// is recorded with its computed cost, and every 429 carries the estimated saved
// cost. When either is absent, all cost recording is skipped and the proxy
// behaves exactly as it did without the pricing layer.
//
// Optional HandlerOptions configure the alerting layer: WithNotifier enables
// loop_blocked and budget_level events, WithBudget enables daily-budget
// monitoring. Omitting them leaves alerting fully disabled.
func NewHandler(upstream *url.URL, det circuitbreaker.Detector, price pricing.PriceSource, opts ...HandlerOption) http.Handler {
	h := &handler{det: det, price: price}
	for _, opt := range opts {
		opt(h)
	}
	h.rp = newReverseProxy(upstream, h.modifyResponse)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/agent/{id}/history", h.handleDeleteHistory)
	mux.Handle("/", h)
	if h.auth != nil {
		return Middleware(h.auth, mux) // covers the proxy path + DELETE route
	}
	return mux
}

// ServeHTTP implements the request flow:
//
//  1. Non-POST methods and empty bodies are forwarded directly — only non-empty
//     POST bodies are hashed (so GET /healthz can never collide on the SHA-256
//     of an empty body).
//  2. A declared Content-Length above MaxBodyBytes is forwarded untouched
//     (fail-open on oversized).
//  3. Otherwise the body is read under http.MaxBytesReader; a read error (client
//     abort or an oversized chunked body) yields a 413 and the request ends.
//  4. The body is fingerprinted (lowercase hex of SHA-256) and checked. A block
//     yields the exact 429 and the request ends.
//  5. Otherwise the body is rewound (Body/ContentLength/GetBody) so the upstream
//     receives the full, unaltered bytes, and the request is proxied.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	agent := circuitbreaker.AgentID(r.Header.Get(AgentIDHeader))

	// Observability: count this request as in-flight for its whole lifetime. A
	// single defer covers every exit path (fast path, budget gate, oversized,
	// 413, 429, and the forwarded path); a nil registry makes both calls no-ops.
	h.metrics.markStart()
	defer h.metrics.markDone()

	// (a) Hash only non-empty POST bodies.
	if r.Method != http.MethodPost || r.ContentLength == 0 {
		h.rp.ServeHTTP(w, r)
		return
	}

	// Per-caller budget gate (monthly + daily). Sits after the free fast-path
	// (a) so that free probes (GET, empty POST) are never blocked, and before
	// (b) so that even an oversized body — which is otherwise forwarded
	// fail-open — is stopped once the caller's budget is exhausted. Enforcement
	// reads the request_logs SUM (source of truth), never a denormalized
	// counter, and fails open on a read error. A zero budget means unlimited.
	if h.ledger != nil {
		if caller, ok := CallerFrom(r.Context()); ok {
			now := time.Now()
			if caller.MonthlyBudgetMicro > 0 {
				since := utcMonthStart(now)
				if spent, err := h.ledger.SumActualCostForKeySince(r.Context(), caller.ID, since); err != nil {
					log.Printf("budget: monthly sum for caller %q: %v", caller.ID, err) // fail-open
				} else if spent >= caller.MonthlyBudgetMicro {
					h.writeBudgetExceeded(w, caller, spent, nextMonthStart(now)-now.Unix(), "Monthly budget exceeded")
					return
				}
			}
			if caller.DailyBudgetMicro > 0 {
				since := utcDayStart(now)
				if spent, err := h.ledger.SumActualCostForKeySince(r.Context(), caller.ID, since); err != nil {
					log.Printf("budget: daily sum for caller %q: %v", caller.ID, err) // fail-open
				} else if spent >= caller.DailyBudgetMicro {
					h.writeBudgetExceeded(w, caller, spent, nextDayStart(now)-now.Unix(), "Daily budget exceeded")
					return
				}
			}
		}
	}

	// (b) Fail-open on oversized declared bodies: do not read, forward untouched.
	if r.ContentLength > MaxBodyBytes {
		h.rp.ServeHTTP(w, r)
		return
	}

	// (c) Read the body under a hard cap.
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	r.Body.Close() // release the socket as soon as the body is consumed
	if err != nil {
		// MaxBytesError (oversized chunked/unknown-length body) or client abort:
		// Go does not send 413 itself, so write it here and stop.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		io.WriteString(w, `{"error":{"type":"request_too_large"}}`)
		return
	}

	// (d) Fingerprint = lowercase hex of the SHA-256 of the raw body bytes.
	sum := sha256.Sum256(body)
	fp := circuitbreaker.Fingerprint(hex.EncodeToString(sum[:]))

	// (d') Fire-and-forget payload emission to the async semantic consumer.
	// Strictly non-blocking (Emitter.Emit never blocks); a nil emitter is a
	// no-op. Emitted for every hashed POST body regardless of the exact-hash
	// decision, so semantic drift sees near-duplicates the fingerprint check
	// lets through.
	if h.emitter != nil {
		h.emitter.Emit(PayloadEvent{
			AgentID:     string(agent),
			KeyID:       h.callerID(r.Context()),
			Project:     r.Header.Get(ProjectHeader),
			Model:       pricing.ExtractModel(body),
			Fingerprint: string(fp),
			Body:        body,
			Timestamp:   time.Now().Unix(),
		})
	}

	// (e) Consult the detector; a block short-circuits before forwarding.
	// Prefer the body-aware PayloadChecker capability when the detector (a
	// Pipeline with the dynamic detectors) implements it; a plain Detector
	// falls back to Check. Exactly one of the two runs per request.
	var res circuitbreaker.CheckResult
	if pc, ok := h.det.(circuitbreaker.PayloadChecker); ok {
		res = pc.CheckPayload(agent, fp, body)
	} else {
		res = h.det.Check(agent, fp)
	}
	if res.Block {
		// The would-be cost of this blocked request, shared by the saved-cost
		// row, the incident record, the notification and the live log event.
		// Computed once (a price lookup plus a couple of integer ops) — it must
		// not delay the 429 response.
		costMicro := int64(0)
		if h.price != nil {
			costMicro = h.price.SavedCost(body)
		}
		// Observability: record the block (count, saved µUSD, prompt tokens) and
		// push a trigger-history entry for the dashboard.
		promptTok := 0
		if h.price != nil {
			promptTok = h.price.EstimatePromptTokens(body)
		}
		h.metrics.markLoopBlocked(costMicro, promptTok)
		h.metrics.recordTrigger(TriggerEvent{
			Timestamp:  time.Now().Unix(),
			AgentID:    string(agent),
			Model:      pricing.ExtractModel(body),
			Reason:     res.Reason,
			SavedMicro: costMicro,
		})
		// Record the estimated saved cost before the 429 leaves.
		if h.price != nil && h.store != nil {
			_ = h.store.LogRequest(r.Context(), RequestRecord{
				AgentID:          string(agent),
				KeyID:            h.callerID(r.Context()),
				ProjectName:      r.Header.Get(ProjectHeader),
				Model:            pricing.ExtractModel(body),
				PromptTokens:     int64(h.price.EstimatePromptTokens(body)),
				CompletionTokens: int64(h.price.AssumedCompletionTokens()),
				CostUSD:          costMicro,
				IsLoopBlocked:    true,
				Timestamp:        time.Now().Unix(),
			})
		}
		// Persist a durable incident record (optional; enterprise-only). The
		// first block records a new episode, every subsequent block touches it.
		// Fire-and-forget: a recorder error is logged and must never delay the
		// 429 response.
		if h.incidents != nil {
			in := IncidentInput{
				AgentID:     string(agent),
				KeyID:       h.callerID(r.Context()),
				ProjectName: r.Header.Get(ProjectHeader),
				Model:       pricing.ExtractModel(body),
				Fingerprint: string(fp),
				CostMicro:   costMicro,
			}
			if err := h.incidents.RecordIncident(r.Context(), in); err != nil {
				log.Printf("incident: record: %v", err)
			}
		}
		// Emit exactly one loop_blocked event per incident (the transition into
		// the blocked state), not one per 429 — a stuck agent that sends 100
		// repeats must not generate 100 deliveries. Fire-and-forget.
		if res.FirstBlock {
			ev := Event{
				Type:       EventLoopBlocked,
				Timestamp:  time.Now().Unix(),
				AgentID:    string(agent),
				Project:    r.Header.Get(ProjectHeader),
				Reason:     res.Reason,
				RetryAfter: res.RetryAfter,
			}
			if h.price != nil {
				ev.Model = pricing.ExtractModel(body)
				ev.SavedCostMicro = costMicro
			}
			// The loop-alert gate toggles the loop_blocked notification class
			// only — the live loop_blocked log line below stays unconditional.
			if h.loopAlertsEnabled() {
				h.emit(ev)
			}
		}
		// Live log: EVERY 429 emits a loop_blocked event so observers see the
		// stuck agent's repeats, with first_block true only on the transition
		// into the blocked window. cost stays 0 — the request was never
		// forwarded — while saved_cost reflects the would-be spend the breaker
		// avoided.
		h.publishLog(logstream.LogEvent{
			Type:              logstream.TypeLoopBlocked,
			Level:             "warn",
			Timestamp:         time.Now().Unix(),
			TenantID:          h.callerID(r.Context()),
			AgentID:           string(agent),
			Model:             pricing.ExtractModel(body),
			ProjectName:       r.Header.Get(ProjectHeader),
			SavedCostMicroUSD: costMicro,
			StatusCode:        http.StatusTooManyRequests,
			FirstBlock:        res.FirstBlock,
			Reason:            res.Reason,
			RetryAfter:        res.RetryAfter,
		})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(res.RetryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"type":"agent_loop_exception","message":"`+
			res.Reason+`","code":"agent_loop_exception","retry_after":`+
			strconv.Itoa(res.RetryAfter)+`}}`)
		return
	}

	// (f) Rewind the body so the upstream gets the full, unaltered bytes.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	// Stash cost-tracking metadata for the ModifyResponse hook. Only hashed
	// POST bodies that reached the forwarding path are tracked; the hook is a
	// no-op for everyone else. Cost tracking is disabled entirely when the
	// handler was built without pricing/store.
	if h.price != nil && h.store != nil {
		meta := reqMeta{
			model:       pricing.ExtractModel(body),
			promptTok:   h.price.EstimatePromptTokens(body),
			agent:       agent,
			fp:          fp,
			projectName: r.Header.Get(ProjectHeader),
			callerID:    h.callerID(r.Context()),
			trackCost:   true,
			start:       time.Now(), // captured just before forwarding; drives avg latency
		}
		r = r.WithContext(context.WithValue(r.Context(), reqMetaKey{}, meta))
	}

	h.rp.ServeHTTP(w, r)
}

// handleDeleteHistory serves DELETE /v1/agent/{id}/history: it clears the named
// agent's loop history and returns 204 No Content. The clear is idempotent —
// an unknown agent is indistinguishable from "already empty", so it returns 204
// regardless of how many entries were removed.
func (h *handler) handleDeleteHistory(w http.ResponseWriter, r *http.Request) {
	h.det.Reset(circuitbreaker.AgentID(r.PathValue("id")))
	w.WriteHeader(http.StatusNoContent)
}

// callerID extracts the authenticated caller id from ctx, or "" when auth is
// disabled or the request never passed through Middleware. The empty string
// selects the legacy (untagged) bucket.
func (h *handler) callerID(ctx context.Context) string {
	if c, ok := CallerFrom(ctx); ok {
		return c.ID
	}
	return ""
}

// writeBudgetExceeded answers a 429 with the budget_exceeded error shape and a
// Retry-After equal to the seconds until the next budget window. The type/code
// discriminator lets cmd/finops-run distinguish an exhausted budget (stop, do
// not retry) from an agent_loop_exception (kill + restart). No row is written
// to the request log: a budget-blocked request has no cost and its body is
// never read.
func (h *handler) writeBudgetExceeded(w http.ResponseWriter, caller Caller, spent, retryAfter int64, reason string) {
	if retryAfter < 1 {
		retryAfter = 1
	}
	// Live log: budget_exceeded carries the caller's name directly, so the hub
	// resolver is not consulted for it. The budget and current spend figures let
	// an observer render the exhaustion at a glance.
	h.publishLog(logstream.LogEvent{
		Type:               logstream.TypeBudgetExceeded,
		Level:              "error",
		Timestamp:          time.Now().Unix(),
		TenantID:           caller.ID,
		TenantName:         caller.Name,
		SpentMicroUSD:      spent,
		MonthlyBudgetMicro: caller.MonthlyBudgetMicro,
		Reason:             reason,
		RetryAfter:         int(retryAfter),
		StatusCode:         http.StatusTooManyRequests,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
	w.WriteHeader(http.StatusTooManyRequests)
	io.WriteString(w, `{"error":{"type":"budget_exceeded","message":"`+reason+`","code":"budget_exceeded","retry_after":`+
		strconv.FormatInt(retryAfter, 10)+`}}`)
}
