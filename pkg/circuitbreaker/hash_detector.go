package circuitbreaker

import (
	"sort"
	"sync"
	"time"
)

// maxEntries caps the number of fingerprints kept in the in-memory map so it
// cannot grow without bound under a flood of many distinct request hashes.
// When the cap is reached, the entry with the oldest timestamp (smallest
// expires) is evicted to make room for a new one.
const maxEntries = 100_000

// HashDetector is a Detector that tracks fingerprints in a fixed-TTL window
// starting from the first sighting. A fingerprint that is seen `limit` times
// within the window causes Block to be reported; the window resets once it
// expires, so the next identical request opens a fresh window.
//
// History is scoped per agent: the composite (agent, fingerprint) key means one
// agent's repeats never leak into another's bucket, and Reset can clear a
// single agent's history in isolation.
//
// It is safe for concurrent use: all state is guarded by a single mutex.
type HashDetector struct {
	mu    sync.Mutex
	seen  map[key]entry
	ttl   time.Duration
	limit int              // repeat threshold; default 2
	now   func() time.Time // injectable clock; defaults to time.Now
	max   int              // hard cap on the number of tracked entries
}

// key identifies a single (agent, fingerprint) history entry.
type key struct {
	agent AgentID
	fp    Fingerprint
}

// entry is the per-key bookkeeping kept in the seen map.
type entry struct {
	count     int
	expires   time.Time // end of the fixed TTL window, set at first sighting
	blockedAt time.Time // first time count reached the limit (zero until then)
	lastBlock time.Time // last time Check returned Block for this fp
}

// NewHashDetector builds a HashDetector with a fixed TTL window of ttl and a
// repeat threshold of limit. A non-positive limit is replaced with the default
// of 2 (i.e. the first sighting passes, the second repeat blocks).
func NewHashDetector(ttl time.Duration, limit int) *HashDetector {
	if limit <= 0 {
		limit = 2
	}
	return &HashDetector{
		seen:  make(map[key]entry),
		ttl:   ttl,
		limit: limit,
		now:   time.Now,
		max:   maxEntries,
	}
}

// SetLimit adjusts the repeat threshold for all future Check evaluations. A
// value below 1 is clamped to 1 — the first sighting always passes, so limit=1
// behaves identically to limit=2 (documented in the plan, D11). Existing
// in-window entries keep their counts: raising the limit never retroactively
// unblocks an already-blocked window (it keeps returning 429 until the TTL
// expires), while lowering it can block an entry whose count already reaches
// the new threshold on the next sighting.
func (d *HashDetector) SetLimit(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n < 1 {
		n = 1
	}
	d.limit = n
}

// Check implements check-and-record semantics.
//
// On the first sighting of the (agent, fp) pair — or on any sighting after the
// previous window has expired — the pair is recorded with count=1 and a fresh
// window, and a zero CheckResult is returned (the request proceeds).
//
// On a repeat within the window, the count is incremented. If the count has
// reached the limit, the result is Block=true with a RetryAfter equal to the
// number of whole seconds remaining in the window, rounded up. Otherwise the
// request proceeds.
func (d *HashDetector) Check(agent AgentID, fp Fingerprint) CheckResult {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	k := key{agent: agent, fp: fp}
	e, ok := d.seen[k]
	if !ok || !now.Before(e.expires) { // absent, or TTL expired → fresh window
		d.evictIfNeeded()
		d.seen[k] = entry{count: 1, expires: now.Add(d.ttl)}
		return CheckResult{}
	}

	e.count++
	firstBlock := false
	if e.count >= d.limit {
		if e.blockedAt.IsZero() {
			e.blockedAt = now // first time this fp reached the limit
			firstBlock = true // transition into blocked state → FirstBlock for this window
		}
		e.lastBlock = now
	}
	d.seen[k] = e
	if e.count < d.limit {
		return CheckResult{}
	}

	remaining := e.expires.Sub(now)
	return CheckResult{
		Block:      true,
		FirstBlock: firstBlock,
		Reason:     ReasonExactHash,
		RetryAfter: int((remaining + time.Second - 1) / time.Second), // ceil(remaining seconds)
	}
}

// Reset clears every tracked entry for one agent and returns the count removed.
//
// It is O(n) in the number of entries, which is acceptable for a rare
// control-plane call (n is bounded by maxEntries). It holds d.mu for the whole
// scan, and Check also holds d.mu, so there is no torn state: a concurrent
// request either lands fully before the reset (its 429 already decided) or
// fully after.
func (d *HashDetector) Reset(agent AgentID) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	n := 0
	for k := range d.seen {
		if k.agent == agent {
			delete(d.seen, k)
			n++
		}
	}
	return n
}

// AgentStatus returns a snapshot of one agent's live loop state, and whether
// the detector currently tracks any (non-expired) entries for the agent at all.
//
// It performs a lazy sweep under d.mu: expired entries are skipped and deleted
// (the same expiry pattern Check applies), then the remaining entries are
// aggregated. An agent whose only blocked fingerprint has expired reports
// Blocked==false and drops off the blocked list automatically.
func (d *HashDetector) AgentStatus(agent AgentID) (AgentStatus, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	var st AgentStatus
	st.Agent = agent
	found := false
	for k, e := range d.seen {
		if k.agent != agent {
			continue
		}
		if !now.Before(e.expires) { // expired → lazy delete
			delete(d.seen, k)
			continue
		}
		found = true
		st.Fingerprints++
		if e.count >= d.limit {
			st.BlockedFP++
			if !e.blockedAt.IsZero() && (st.FirstBlocked.IsZero() || e.blockedAt.Before(st.FirstBlocked)) {
				st.FirstBlocked = e.blockedAt
			}
			if !e.lastBlock.IsZero() && e.lastBlock.After(st.LastBlocked) {
				st.LastBlocked = e.lastBlock
			}
		}
	}
	if !found {
		return AgentStatus{}, false
	}
	st.Blocked = st.BlockedFP > 0
	return st, true
}

// BlockedAgents returns every agent currently blocked, sorted by Agent.
//
// Like AgentStatus it lazily sweeps expired entries under d.mu before
// aggregating. Only agents with Blocked==true are returned.
func (d *HashDetector) BlockedAgents() []AgentStatus {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	byAgent := make(map[AgentID]*AgentStatus)
	for k, e := range d.seen {
		if !now.Before(e.expires) { // expired → lazy delete
			delete(d.seen, k)
			continue
		}
		st, ok := byAgent[k.agent]
		if !ok {
			s := AgentStatus{Agent: k.agent}
			st = &s
			byAgent[k.agent] = st
		}
		st.Fingerprints++
		if e.count >= d.limit {
			st.BlockedFP++
			if !e.blockedAt.IsZero() && (st.FirstBlocked.IsZero() || e.blockedAt.Before(st.FirstBlocked)) {
				st.FirstBlocked = e.blockedAt
			}
			if !e.lastBlock.IsZero() && e.lastBlock.After(st.LastBlocked) {
				st.LastBlocked = e.lastBlock
			}
		}
	}

	out := make([]AgentStatus, 0, len(byAgent))
	for _, st := range byAgent {
		st.Blocked = st.BlockedFP > 0
		if st.Blocked {
			out = append(out, *st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// evictIfNeeded keeps len(d.seen) bounded by d.max. If the map has grown past
// the cap, entries with the smallest expires (i.e. the oldest windows) are
// removed — these are also the most likely to be expired. Eviction is a
// linear scan, so it only runs when the map is already at capacity.
//
// d.mu must be held by the caller.
func (d *HashDetector) evictIfNeeded() {
	for len(d.seen) >= d.max {
		var oldestKey key
		var oldest time.Time
		first := true
		for k, e := range d.seen {
			if first || e.expires.Before(oldest) {
				oldestKey = k
				oldest = e.expires
				first = false
			}
		}
		if first {
			return // map empty; nothing to evict
		}
		delete(d.seen, oldestKey)
	}
}
