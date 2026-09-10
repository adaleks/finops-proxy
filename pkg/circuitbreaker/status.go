package circuitbreaker

import "time"

// AgentStatus is a read snapshot of one agent's live loop state in the
// detector.
//
// Blocked is a transient detector state — a consequence of identical requests
// observed within the TTL window — never a durable fact. The zero-value fields
// (FirstBlocked/LastBlocked) are only meaningful when Blocked is true.
type AgentStatus struct {
	Agent        AgentID
	Blocked      bool      // true when >=1 active fingerprint has count >= limit
	Fingerprints int       // active (agent, fp) entries inside the TTL window
	BlockedFP    int       // how many of those entries have reached the limit
	FirstBlocked time.Time // min(blockedAt) over active blocked fps; zero if Blocked==false
	LastBlocked  time.Time // max(lastBlock) over active blocked fps; zero if Blocked==false
}

// DetectorStatus is an optional read-side surface for the dashboard / control
// plane.
//
// It deliberately does NOT extend the Detector interface: the hot path stays
// minimal, and a Detector implementation may opt into this capability without
// breaking existing callers or mocks. The API layer type-asserts Detector to
// DetectorStatus at startup.
type DetectorStatus interface {
	// AgentStatus returns a snapshot of one agent's live state and whether the
	// detector currently tracks any (non-expired) entries for that agent.
	AgentStatus(agent AgentID) (AgentStatus, bool)

	// BlockedAgents returns every agent with Blocked==true, sorted by Agent.
	BlockedAgents() []AgentStatus
}

// Compile-time assertion: HashDetector implements the DetectorStatus
// capability.
var _ DetectorStatus = (*HashDetector)(nil)
