package circuitbreaker

import (
	"sync"
	"time"
)

// VelocityBreaker detects abnormally accelerating token consumption per agent —
// the "unbounded consumption" (OWASP LLM04) signal that content-based detectors
// miss. It watches the prompt-token rate over a sliding window and blocks on
// either a hard burst cap or an acceleration spike relative to the agent's own
// recent baseline.
//
// It is safe for concurrent use. It does not implement PayloadChecker: the
// pipeline computes the token count once and calls CheckVelocity.
type VelocityBreaker struct {
	cfg    VelocityConfig
	mu     sync.Mutex
	states map[AgentID]*velocityState
	now    func() time.Time // injectable clock; defaults to time.Now
}

// VelocityConfig tunes the velocity detector. Zero-valued fields are replaced
// by defaults in NewVelocityBreaker.
type VelocityConfig struct {
	// Window is the sustained-velocity horizon (default 30s).
	Window time.Duration
	// ShortWindow is the instantaneous-rate horizon (default 5s).
	ShortWindow time.Duration
	// BurstTokens is the hard cap: tokens observed within Window (default 200_000).
	BurstTokens int64
	// SpikeFactor is the short/long rate ratio that blocks (default 5.0).
	SpikeFactor float64
	// MaxEntries caps the number of tracked agent states (default 10_000).
	MaxEntries int
}

// velocityState is the per-agent bookkeeping: two FIFO token-event rings with
// running sums.
type velocityState struct {
	events    []tokenEvent // FIFO within Window
	sumLong   int64
	short     []tokenEvent // FIFO within ShortWindow (a tail of events)
	sumShort  int64
	firstAt   time.Time // first observation; gates the spike detector until warmed
	lastAt    time.Time // last observation; used for eviction ordering
	blockedAt time.Time // first time a block fired (zero until then)
}

// tokenEvent is one token-count observation.
type tokenEvent struct {
	t time.Time
	n int64
}

// Defaults applied by NewVelocityBreaker.
const (
	defaultVelocityWindow      = 30 * time.Second
	defaultVelocityShortWindow = 5 * time.Second
	defaultVelocityBurst       = 200_000
	defaultVelocitySpike       = 5.0
	defaultVelocityMaxEntries  = 10_000

	// maxVelocityEvents bounds each per-agent ring. It only engages under an
	// extreme flood (more than this many requests inside a window), at which
	// point the burst cap has almost certainly already fired; dropping the
	// oldest events then trades a little accuracy for a hard memory bound.
	maxVelocityEvents = 1024
)

// NewVelocityBreaker builds a VelocityBreaker with the given config, replacing
// zero/invalid fields with defaults.
func NewVelocityBreaker(cfg VelocityConfig) *VelocityBreaker {
	if cfg.Window <= 0 {
		cfg.Window = defaultVelocityWindow
	}
	if cfg.ShortWindow <= 0 || cfg.ShortWindow > cfg.Window {
		cfg.ShortWindow = defaultVelocityShortWindow
	}
	if cfg.BurstTokens <= 0 {
		cfg.BurstTokens = defaultVelocityBurst
	}
	if cfg.SpikeFactor <= 0 {
		cfg.SpikeFactor = defaultVelocitySpike
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = defaultVelocityMaxEntries
	}
	return &VelocityBreaker{
		cfg:    cfg,
		states: make(map[AgentID]*velocityState),
		now:    time.Now,
	}
}

// CheckVelocity records a prompt-token observation for agent and reports a block
// when consumption spikes abnormally. The RetryAfter is short (the
// ShortWindow): a transient spike may clear immediately, unlike the
// repetition-based detectors' longer cooldown.
func (v *VelocityBreaker) CheckVelocity(agent AgentID, tokens int64) CheckResult {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.now()
	st := v.state(agent)
	st.append(now, tokens, v.cfg.Window, v.cfg.ShortWindow)

	block := st.sumLong > v.cfg.BurstTokens
	// The spike trigger is gated until the long window has filled (at least one
	// full Window of observations), so a cold-start agent whose short rate is
	// transiently high relative to an empty long window is not falsely blocked.
	if !block && now.Sub(st.firstAt) >= v.cfg.Window {
		longRate := float64(st.sumLong) / v.cfg.Window.Seconds()
		shortRate := float64(st.sumShort) / v.cfg.ShortWindow.Seconds()
		if longRate > 0 && shortRate/longRate > v.cfg.SpikeFactor {
			block = true
		}
	}

	if block {
		first := st.blockedAt.IsZero()
		if first {
			st.blockedAt = now
		}
		return CheckResult{
			Block:      true,
			FirstBlock: first,
			Reason:     ReasonVelocity,
			RetryAfter: int(v.cfg.ShortWindow.Seconds()) + 1,
		}
	}

	// Spike subsided: clear the incident flag after one ShortWindow so the next
	// genuine spike re-fires FirstBlock (mirrors the TTL reset of HashDetector).
	if !st.blockedAt.IsZero() && now.Sub(st.blockedAt) >= v.cfg.ShortWindow {
		st.blockedAt = time.Time{}
	}
	return CheckResult{}
}

// Reset clears one agent's state and returns 1 if it existed.
func (v *VelocityBreaker) Reset(agent AgentID) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.states[agent]; ok {
		delete(v.states, agent)
		return 1
	}
	return 0
}

func (v *VelocityBreaker) state(agent AgentID) *velocityState {
	st, ok := v.states[agent]
	if !ok {
		v.evictIfNeeded()
		st = &velocityState{}
		v.states[agent] = st
	}
	return st
}

func (v *VelocityBreaker) evictIfNeeded() {
	for len(v.states) >= v.cfg.MaxEntries {
		var oldest AgentID
		var oldestT time.Time
		first := true
		for a, st := range v.states {
			if first || st.lastAt.Before(oldestT) {
				oldest, oldestT, first = a, st.lastAt, false
			}
		}
		if first {
			return
		}
		delete(v.states, oldest)
	}
}

// append records a token event and expires events older than the two horizons,
// then enforces the hard ring cap. Amortized O(1).
func (st *velocityState) append(now time.Time, tokens int64, longWin, shortWin time.Duration) {
	if st.firstAt.IsZero() {
		st.firstAt = now
	}
	st.lastAt = now

	st.events = append(st.events, tokenEvent{now, tokens})
	st.sumLong += tokens
	st.short = append(st.short, tokenEvent{now, tokens})
	st.sumShort += tokens

	cutoff := now.Add(-longWin)
	i := 0
	for i < len(st.events) && !st.events[i].t.After(cutoff) {
		st.sumLong -= st.events[i].n
		i++
	}
	st.events = st.events[i:]

	cutoffS := now.Add(-shortWin)
	j := 0
	for j < len(st.short) && !st.short[j].t.After(cutoffS) {
		st.sumShort -= st.short[j].n
		j++
	}
	st.short = st.short[j:]

	for len(st.events) > maxVelocityEvents {
		st.sumLong -= st.events[0].n
		st.events = st.events[1:]
	}
	for len(st.short) > maxVelocityEvents {
		st.sumShort -= st.short[0].n
		st.short = st.short[1:]
	}
}
