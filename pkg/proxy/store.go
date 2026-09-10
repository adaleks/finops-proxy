package proxy

import "context"

// RequestRecord is the public form of one persisted proxy request. It is the
// enterprise-agnostic row the handler hands to a RequestLogStore; KeyID
// replaces the EE tenant_id column, so the core never models multi-tenancy.
// Money is integer micro-dollars (µUSD); Timestamp is Unix epoch seconds (UTC).
type RequestRecord struct {
	AgentID          string
	KeyID            string // caller id; "" = anonymous / legacy bucket
	ProjectName      string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	CostUSD          int64 // µUSD; saved cost for a loop-blocked row, actual for a forwarded row
	IsLoopBlocked    bool
	Timestamp        int64
}

// RequestLogStore is the persistence seam the handler uses to record each
// proxied (or loop-blocked) request. Implementations MUST be safe for
// concurrent use and MUST NOT block the request path: the handler treats a
// LogRequest error as non-fatal.
type RequestLogStore interface {
	LogRequest(ctx context.Context, r RequestRecord) error
}

// BudgetLedger is the read seam the handler consults for budget enforcement.
// SumActualCostSince backs the global daily budget monitor;
// SumActualCostForKeySince backs the per-caller monthly/daily gates. Both sums
// cover only non-loop-blocked (actual) spend, the source of truth for
// enforcement. Implementations MUST be safe for concurrent use.
type BudgetLedger interface {
	// SumActualCostSince returns the sum of actual cost (µUSD) for every
	// request logged at or after since (Unix epoch seconds).
	SumActualCostSince(ctx context.Context, since int64) (int64, error)

	// SumActualCostForKeySince returns the sum of actual cost (µUSD) for one
	// caller's requests logged at or after since (Unix epoch seconds). The empty
	// keyID selects the anonymous/legacy bucket.
	SumActualCostForKeySince(ctx context.Context, keyID string, since int64) (int64, error)
}

// SettingsRow is the single-row runtime settings snapshot: the loop-detection
// repeat threshold and the per-request cost-alert threshold. Money is µUSD.
type SettingsRow struct {
	LoopThresholdCount      int   // first sighting always passes; the count-th repeat blocks
	CostAlertThresholdMicro int64 // 0 = per-request cost alerts disabled
}

// SettingsStore is the persistence seam for the single runtime settings row.
// The handler does not consume it directly — it reads the live loop threshold
// through circuitbreaker.DetectorLimitSetter and the cost-alert threshold
// through CostThresholdReader — but the reference stores and the enterprise
// settings service both implement it.
type SettingsStore interface {
	GetSettings(ctx context.Context) (SettingsRow, error)
	UpdateSettings(ctx context.Context, s SettingsRow) error
}

// IncidentInput carries the fields a blocked request contributes to a durable
// incident record. Fingerprint is the lowercase SHA-256 hex of the request body.
type IncidentInput struct {
	AgentID     string
	KeyID       string
	ProjectName string
	Model       string
	Fingerprint string
	CostMicro   int64 // µUSD would-be cost of this blocked request
}

// IncidentRecorder is the optional seam through which the proxy persists a
// durable loop episode on each block. A nil recorder (the default for the core
// standalone binary) skips incident recording entirely — loop-blocked rows are
// still present in request_logs with IsLoopBlocked set. The enterprise module
// supplies the implementation backed by its incidents table.
type IncidentRecorder interface {
	RecordIncident(ctx context.Context, in IncidentInput) error
}

// Store is the aggregate persistence seam the reference stores (memory, sqlite)
// implement: request logging, the budget ledger, and settings. The handler
// consumes it through WithStore; the narrower interfaces let a caller wire or
// nil each concern independently.
//
// Incident recording is deliberately NOT part of Store: it is an enterprise
// concern surfaced only through the separate IncidentRecorder seam.
type Store interface {
	RequestLogStore
	BudgetLedger
	SettingsStore
}

// Compile-time assertions that the seams are satisfied by the concrete types in
// this module are left to the store packages (importing proxy there would
// otherwise create an import cycle).
