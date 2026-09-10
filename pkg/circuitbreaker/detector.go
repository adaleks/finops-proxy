// Package circuitbreaker defines the loop-prevention policy engine the proxy
// consults before forwarding every request that carries a body.
//
// The contract is deliberately small and pure: it contains only the types
// exchanged between the HTTP handler and the policy implementation. No
// implementation, state, or I/O lives here — concrete detectors live in the
// same package and satisfy the Detector interface.
package circuitbreaker

// Fingerprint uniquely identifies a request payload.
//
// For the MVP this is the lowercase hex encoding of the SHA-256 digest of the
// raw request body. Two requests with byte-identical bodies produce the same
// fingerprint; requests that differ in any byte produce a different one.
type Fingerprint string

// AgentID scopes loop history to a single caller (e.g. one finops-run child).
//
// The empty AgentID is the default bucket and preserves the pre-scoping
// behaviour: clients that do not identify themselves all share one history.
type AgentID string

// CheckResult tells the proxy what to do with the request that was checked.
type CheckResult struct {
	// Block is true when the request is a repeat observed within the TTL
	// window and must be rejected with HTTP 429 Too Many Requests.
	Block bool

	// Reason is a short, human-readable explanation used in logs and the
	// 429 response body.
	Reason string

	// RetryAfter is the number of seconds the client should wait before
	// retrying. It is only meaningful when Block == true.
	RetryAfter int

	// FirstBlock is true only on the first time a fingerprint reaches the block
	// threshold within the current TTL window (i.e. the transition into blocked
	// state). Used to emit exactly one loop-blocked notification per incident.
	FirstBlock bool
}

// DetectorLimitSetter is the optional seam through which a live loop-threshold
// update reaches the in-memory detector. *HashDetector satisfies it; the base
// Detector interface is deliberately unchanged so existing mocks keep
// compiling. Callers type-assert (as with DetectorStatus) rather than changing
// the core contract.
type DetectorLimitSetter interface {
	// SetLimit adjusts the repeat threshold for all future Check evaluations.
	SetLimit(n int)
}

// Detector is consulted for every proxied LLM request that carries a payload.
//
// The semantics are "check-and-record": a Check call observes the fingerprint,
// so two concurrent requests with the same fingerprint can never both pass.
// Implementations MUST be safe for concurrent use and are responsible for
// expiring fingerprints after the TTL.
type Detector interface {
	// Check records fp for the given agent and reports whether the request
	// carrying it must be blocked. A zero CheckResult means "proceed".
	Check(agent AgentID, fp Fingerprint) CheckResult

	// Reset clears every tracked fingerprint for one agent and returns the
	// number of entries removed (0 if the agent had no history). It is the
	// backing operation for the DELETE /v1/agent/{id}/history control route.
	Reset(agent AgentID) int
}

// Reason strings returned in CheckResult.Reason, one per loop class, so logs,
// 429 bodies, and the dashboard can distinguish them. ReasonExactHash is the
// string previously inlined in HashDetector (promoted here as a pure refactor).
const (
	ReasonExactHash  = "identical request fingerprint observed within TTL window (possible infinite loop)"
	ReasonStructural = "structurally identical request observed within TTL window (possible loop with dynamic values)"
	ReasonToolCycle  = "repeating tool-call cycle detected (possible A->B->A->B agent loop)"
	ReasonVelocity   = "abnormal token consumption velocity (possible unbounded consumption loop)"

	// EE semantic detector reasons (produced by the EE worker's verdict, not by
	// the core detectors).
	ReasonSemanticLoop = "semantically near-duplicate request observed within TTL window (possible semantic loop)"
	ReasonIntentDrift  = "conversation semantics diverged from the original system intent"
)

// TokenEstimator maps a request body to its prompt-token estimate. The proxy
// injects pricing.PriceSource.EstimatePromptTokens; a nil estimator is replaced
// by a byte-length heuristic by the Pipeline.
type TokenEstimator func(body []byte) int

// PayloadChecker is the optional capability through which the handler passes
// the raw request body (in addition to its fingerprint) to a detector. A
// detector that needs the body — structural hashing, tool-cycle matching, token
// velocity — opts in by implementing it; the base Detector contract stays
// unchanged so existing mocks keep compiling. Callers type-assert at startup,
// exactly as they do for DetectorStatus and DetectorLimitSetter.
//
// CheckPayload is the body-aware superset of Check: an implementation must also
// satisfy Detector, and CheckPayload must subsume whatever Check does, so the
// handler calls exactly one of the two per request.
type PayloadChecker interface {
	CheckPayload(agent AgentID, fp Fingerprint, body []byte) CheckResult
}
