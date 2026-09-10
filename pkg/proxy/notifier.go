package proxy

// This file carries the advisory notification seam: the two events the request
// path can emit (loop_blocked, budget_level) and the Notifier interface the
// handler calls to deliver them. It is the transport-agnostic half of the
// enterprise alerting package — the webhook/Slack/Teams delivery lives in the
// enterprise module and simply implements Notifier.
//
// Delivery MUST be asynchronous and strictly non-blocking: Notify returns
// without ever waiting on the network, a worker, or a full queue, so alerting
// can never slow down or break a proxied request. Money is carried as integer
// micro-dollars (µUSD).

// EventType discriminates the two events the notification layer can carry.
type EventType string

const (
	// EventLoopBlocked reports a request the circuit breaker blocked (429).
	// The carried SavedCostMicro is the estimated µUSD the proxy avoided
	// spending by not forwarding the request.
	EventLoopBlocked EventType = "loop_blocked"

	// EventBudgetLevel reports that real daily spend crossed a configured
	// threshold percentage of the daily budget.
	EventBudgetLevel EventType = "budget_level"

	// EventSemanticLoop reports that the EE semantic worker flagged a
	// semantically near-duplicate conversation (cosine drift above threshold).
	// Emitted by the EE worker, never the request path.
	EventSemanticLoop EventType = "semantic_loop"

	// EventIntentDrift reports that conversation semantics diverged from the
	// original system intent (cosine to the intent anchor below the floor).
	// Emitted by the EE worker.
	EventIntentDrift EventType = "intent_drift"

	// EventQuotaLevel reports that an org/team/user hierarchical quota reached a
	// configured percentage (e.g. 80% or 100%). Emitted by the EE quota monitor.
	EventQuotaLevel EventType = "quota_level"
)

// Event is the single value the notification layer transports. µUSD fields
// carry integer micro-dollars; a USD string for display is produced only by
// the consumer. Zero-valued fields are omitted from the JSON encoding so
// payloads stay minimal.
type Event struct {
	Type      EventType `json:"type"`
	Timestamp int64     `json:"timestamp"` // Unix epoch seconds (UTC), like RequestRecord.Timestamp

	AgentID string `json:"agent_id,omitempty"`
	Project string `json:"project,omitempty"`
	Model   string `json:"model,omitempty"`

	// loop_blocked
	SavedCostMicro int64  `json:"saved_cost_micro_usd,omitempty"`
	Reason         string `json:"reason,omitempty"`
	RetryAfter     int    `json:"retry_after,omitempty"`

	// budget_level
	BudgetMicroUSD int64 `json:"budget_micro_usd,omitempty"`
	SpentMicroUSD  int64 `json:"spent_micro_usd,omitempty"`
	ThresholdPct   int   `json:"threshold_pct,omitempty"`

	// semantic_loop / intent_drift (EE)
	Similarity  float64 `json:"similarity,omitempty"`   // cosine that tripped the loop
	IntentScore float64 `json:"intent_score,omitempty"` // cosine to the intent anchor

	// quota_level (EE)
	QuotaKind string `json:"quota_kind,omitempty"` // "org" | "team" | "user"
	QuotaID   string `json:"quota_id,omitempty"`
}

// Notifier is the seam through which the proxy ties into alerting — never a
// concrete sink, so tests can inject a fake notifier and the handler can stay
// oblivious to the transport.
type Notifier interface {
	// Notify delivers one event. It MUST return promptly (no blocking I/O on
	// the caller), MUST be safe for concurrent use, and MUST NOT panic.
	// Implementations with slow delivery (network/disk) MUST buffer internally
	// and return immediately.
	Notify(event Event)
	// Close flushes any remaining events within a bounded timeout and then
	// stops the worker. It is idempotent.
	Close()
}

// noopNotifier is returned when notification is disabled. Notify and Close are
// no-ops, so a caller always holds a non-nil Notifier and needs no nil checks.
type noopNotifier struct{}

func (noopNotifier) Notify(Event) {}
func (noopNotifier) Close()       {}

// NoopNotifier returns a Notifier that discards every event. It is the
// "alerting disabled" default and a convenient test double.
func NoopNotifier() Notifier { return noopNotifier{} }

// LoopAlertGate is the narrow seam the proxy's 429 path checks before emitting
// a loop_blocked event (D9). A Notifier that implements the gate reports its
// live flag; one that does not (a noop, a test fake, the JSON stdout logger)
// defaults to enabled. Implementations MUST be safe for concurrent use and MUST
// NOT block.
type LoopAlertGate interface {
	LoopAlertsEnabled() bool
}
