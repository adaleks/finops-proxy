package circuitbreaker

import (
	"encoding/json"
	"sync"
	"time"
)

// ToolMatcher detects cyclic tool-call patterns — e.g. A -> B -> A -> B — in an
// agent's sliding conversation history. It extracts the ordered tool/function
// names a request references, appends them to a bounded per-agent FIFO, and
// blocks when the tail of that sequence repeats a fixed period `limit` times.
//
// It is safe for concurrent use and implements the PayloadChecker capability
// (it needs the body, not just the fingerprint).
type ToolMatcher struct {
	cfg    ToolConfig
	mu     sync.Mutex
	states map[AgentID]*toolState
	now    func() time.Time // injectable clock; defaults to time.Now
}

// ToolConfig tunes the tool-loop detector. Zero-valued fields are replaced by
// defaults in NewToolMatcher.
type ToolConfig struct {
	// MaxSeq is the maximum number of tool names retained per agent (default 64).
	MaxSeq int
	// MaxPeriod is the longest cycle period tested (default 8).
	MaxPeriod int
	// Limit is the number of consecutive confirmations of the same period before
	// Block is reported (default 2).
	Limit int
	// TTL is the lifetime of the per-agent history (default 10 minutes).
	TTL time.Duration
	// MaxEntries caps the number of tracked agent states (default 10_000).
	MaxEntries int
}

// toolState is the per-agent bookkeeping.
type toolState struct {
	seq       []string  // bounded FIFO of recent tool names
	period    int       // last confirmed cycle period (0 = none)
	confirms  int       // consecutive confirmations of period
	expires   time.Time // end of the fixed TTL window
	blockedAt time.Time // first time a block fired in this window (zero until then)
}

// Defaults applied by NewToolMatcher.
const (
	defaultToolMaxSeq     = 64
	defaultToolMaxPeriod  = 8
	defaultToolLimit      = 2
	defaultToolTTL        = 10 * time.Minute
	defaultToolMaxEntries = 10_000
)

// NewToolMatcher builds a ToolMatcher with the given config, replacing
// zero/invalid fields with defaults.
func NewToolMatcher(cfg ToolConfig) *ToolMatcher {
	if cfg.MaxSeq <= 0 {
		cfg.MaxSeq = defaultToolMaxSeq
	}
	if cfg.MaxPeriod < 2 {
		cfg.MaxPeriod = defaultToolMaxPeriod
	}
	if cfg.Limit <= 0 {
		cfg.Limit = defaultToolLimit
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultToolTTL
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = defaultToolMaxEntries
	}
	return &ToolMatcher{
		cfg:    cfg,
		states: make(map[AgentID]*toolState),
		now:    time.Now,
	}
}

// CheckPayload implements the PayloadChecker capability. It extracts the tool
// names referenced by body, appends them to the agent's sequence, and reports a
// block when a repeating cycle is confirmed.
func (m *ToolMatcher) CheckPayload(agent AgentID, _ Fingerprint, body []byte) CheckResult {
	names := toolNamePool.Get().(*[]string)
	defer toolNamePool.Put(names)
	*names = (*names)[:0]
	extractToolNames(names, body)
	if len(*names) == 0 {
		return CheckResult{} // no tool surface → pass fast
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	st := m.state(agent, now)

	if !now.Before(st.expires) { // TTL expired → fresh window
		st.reset(now.Add(m.cfg.TTL))
	}

	st.push(names, m.cfg.MaxSeq)
	p, ok := detectCycle(st.seq, m.cfg.MaxPeriod)
	if ok {
		if p == st.period {
			st.confirms++
		} else {
			st.period = p
			st.confirms = 1
		}
	} else {
		st.period = 0
		st.confirms = 0
	}

	if st.confirms >= m.cfg.Limit {
		first := st.blockedAt.IsZero()
		if first {
			st.blockedAt = now
		}
		return CheckResult{
			Block:      true,
			FirstBlock: first,
			Reason:     ReasonToolCycle,
			RetryAfter: ceilSeconds(st.expires.Sub(now)),
		}
	}
	return CheckResult{}
}

// Reset clears one agent's state and returns 1 if it existed.
func (m *ToolMatcher) Reset(agent AgentID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.states[agent]; ok {
		delete(m.states, agent)
		return 1
	}
	return 0
}

func (m *ToolMatcher) state(agent AgentID, now time.Time) *toolState {
	st, ok := m.states[agent]
	if !ok {
		m.evictIfNeeded()
		st = &toolState{expires: now.Add(m.cfg.TTL)}
		m.states[agent] = st
	}
	return st
}

func (m *ToolMatcher) evictIfNeeded() {
	for len(m.states) >= m.cfg.MaxEntries {
		var oldest AgentID
		var oldestT time.Time
		first := true
		for a, st := range m.states {
			if first || st.expires.Before(oldestT) {
				oldest, oldestT, first = a, st.expires, false
			}
		}
		if first {
			return
		}
		delete(m.states, oldest)
	}
}

// reset clears the state for a fresh TTL window.
func (st *toolState) reset(expires time.Time) {
	st.seq = st.seq[:0]
	st.period = 0
	st.confirms = 0
	st.expires = expires
	st.blockedAt = time.Time{}
}

// push appends names and trims to at most cap, keeping the most recent tail
// (cycle detection inspects the tail).
func (st *toolState) push(names *[]string, cap int) {
	st.seq = append(st.seq, (*names)...)
	if len(st.seq) > cap {
		st.seq = st.seq[len(st.seq)-cap:]
	}
}

// detectCycle reports whether the tail of seq is a repeated period-p cycle (P
// followed by P again), and the period. p ranges over [2, maxPeriod].
func detectCycle(seq []string, maxPeriod int) (int, bool) {
	n := len(seq)
	for p := 2; p <= maxPeriod && 2*p <= n; p++ {
		if equalStrings(seq[n-2*p:n-p], seq[n-p:n]) {
			return p, true
		}
	}
	return 0, false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// toolNamePool reuses the tool-name slice across requests.
var toolNamePool = sync.Pool{
	New: func() any { s := make([]string, 0, 16); return &s },
}

// toolCallEnvelope is the minimal subset of a chat-completions body that carries
// tool-call/function references. Unknown fields are skipped by json.Unmarshal,
// so allocation stays bounded to the tool surfaces actually present.
type toolCallEnvelope struct {
	Messages []struct {
		ToolCalls []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Functions []struct {
		Name string `json:"name"`
	} `json:"functions"`
	Tools []struct {
		Name     string `json:"name"` // Anthropic-style top-level name
		Function struct {
			Name string `json:"name"` // OpenAI-style function.name
		} `json:"function"`
	} `json:"tools"`
}

// extractToolNames appends the ordered tool/function names referenced by body
// into *dst, calls-before-definitions: assistant tool_calls (actual calls made),
// then legacy functions, then tools (definitions). It never fails: unparseable
// input yields an empty result.
func extractToolNames(dst *[]string, body []byte) {
	out := (*dst)[:0]
	var env toolCallEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		*dst = out
		return
	}
	for i := range env.Messages {
		for j := range env.Messages[i].ToolCalls {
			if name := env.Messages[i].ToolCalls[j].Function.Name; name != "" {
				out = append(out, name)
			}
		}
	}
	for i := range env.Functions {
		if name := env.Functions[i].Name; name != "" {
			out = append(out, name)
		}
	}
	for i := range env.Tools {
		name := env.Tools[i].Function.Name
		if name == "" {
			name = env.Tools[i].Name
		}
		if name != "" {
			out = append(out, name)
		}
	}
	*dst = out
}
