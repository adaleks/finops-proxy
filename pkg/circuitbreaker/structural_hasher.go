package circuitbreaker

import (
	"sync"
	"time"
)

// StructuralHasher is a fuzzy loop detector that collapses the dynamic parts of
// a JSON prompt — numbers, UUIDs, timestamps, and long hex strings — into
// placeholders, hashes the remaining structure into a fixed-width MinHash
// sketch, and blocks a request whose structure has been seen `limit` times
// within a sliding window.
//
// It complements HashDetector: exact matching catches byte-identical repeats;
// structural matching catches the far more common loop that re-issues the same
// request with a few mutable fields changed each turn.
//
// History is scoped per agent and safe for concurrent use. It implements the
// PayloadChecker capability (it needs the body, not just the fingerprint);
// Reset and the Detector contract are composed by the Pipeline.
type StructuralHasher struct {
	cfg  StructuralConfig
	mu   sync.Mutex
	seen map[AgentID]*structuralState
	now  func() time.Time // injectable clock; defaults to time.Now
}

// StructuralConfig tunes the fuzzy structural detector. Zero-valued fields are
// replaced by defaults in NewStructuralHasher.
type StructuralConfig struct {
	// Threshold is the Jaccard similarity at or above which two structural
	// signatures are considered "the same structure" (default 0.85).
	Threshold float64
	// Window is the maximum number of recent signatures retained per agent for
	// comparison (default 8).
	Window int
	// Limit is the number of similar signatures that must be observed within
	// the TTL before Block is reported (default 3).
	Limit int
	// TTL is the lifetime of the per-agent window (default 10 minutes).
	TTL time.Duration
	// MaxEntries caps the number of tracked agent states (default 10_000).
	MaxEntries int
}

// structuralState is the per-agent bookkeeping.
type structuralState struct {
	sigs      []sigEntry // bounded FIFO of recent signatures, all inside the window
	expires   time.Time  // end of the fixed TTL window, set at first sighting
	blockedAt time.Time  // first time a block fired in this window (zero until then)
}

// sigEntry is one retained structural signature.
type sigEntry struct {
	sig signature
}

// Defaults applied by NewStructuralHasher.
const (
	defaultStructuralThreshold = 0.85
	defaultStructuralWindow    = 8
	defaultStructuralLimit     = 3
	defaultStructuralTTL       = 10 * time.Minute
	defaultStructuralMax       = 10_000

	// minHashK is the fixed MinHash signature width. It is a compile-time
	// constant (not a config knob) so signature is a value type and the hot path
	// stays allocation-free.
	minHashK = 64

	// shingleSize is the n-gram width over the normalized structural form.
	shingleSize = 3

	// maxShingles caps the number of shingles hashed per body, bounding
	// worst-case work on large payloads.
	maxShingles = 256
)

// signature is a K-element MinHash sketch of a structural shingle set. It is a
// value type so it lives on the stack and needs no sync.Pool.
type signature struct {
	v [minHashK]uint64
}

// FNV-1a 64-bit constants.
const (
	fnv1aOffset = uint64(14695981039346656037)
	fnv1aPrime  = uint64(1099511628211)
)

// goldenRatio is the Knuth multiplicative-hash constant used to project one
// shingle hash into minHashK independent-ish MinHash slots.
const goldenRatio = uint64(0x9E3779B97F4A7C15)

// NewStructuralHasher builds a StructuralHasher with the given config, replacing
// zero/invalid fields with defaults.
func NewStructuralHasher(cfg StructuralConfig) *StructuralHasher {
	if cfg.Threshold <= 0 || cfg.Threshold > 1 {
		cfg.Threshold = defaultStructuralThreshold
	}
	if cfg.Window <= 0 {
		cfg.Window = defaultStructuralWindow
	}
	if cfg.Limit <= 0 {
		cfg.Limit = defaultStructuralLimit
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultStructuralTTL
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = defaultStructuralMax
	}
	return &StructuralHasher{
		cfg:  cfg,
		seen: make(map[AgentID]*structuralState),
		now:  time.Now,
	}
}

// CheckPayload implements the PayloadChecker capability. It normalizes the body
// to its structural form, computes its MinHash sketch, and records the sketch
// for the agent. A block is reported when `limit` structurally-similar
// signatures are observed within the window.
func (s *StructuralHasher) CheckPayload(agent AgentID, _ Fingerprint, body []byte) CheckResult {
	buf := normalizeBufPool.Get().(*[]byte)
	defer normalizeBufPool.Put(buf)
	*buf = (*buf)[:0]
	normalizePayload(buf, body)

	form := *buf
	if len(form) < shingleSize {
		return CheckResult{} // degenerate/empty structure: no signal, fail open
	}
	sig := minHash(form)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	st := s.state(agent, now)

	if !now.Before(st.expires) { // TTL expired → fresh window
		st.sigs = st.sigs[:0]
		st.expires = now.Add(s.cfg.TTL)
		st.blockedAt = time.Time{}
	}

	// Count retained signatures that are structurally similar to this one.
	matches := 0
	for _, e := range st.sigs {
		if jaccard(sig, e.sig) >= s.cfg.Threshold {
			matches++
		}
	}

	if matches >= s.cfg.Limit {
		first := st.blockedAt.IsZero()
		if first {
			st.blockedAt = now
		}
		st.sigs = appendWindow(st.sigs, sigEntry{sig: sig}, s.cfg.Window)
		return CheckResult{
			Block:      true,
			FirstBlock: first,
			Reason:     ReasonStructural,
			RetryAfter: ceilSeconds(st.expires.Sub(now)),
		}
	}

	st.sigs = appendWindow(st.sigs, sigEntry{sig: sig}, s.cfg.Window)
	return CheckResult{}
}

// Reset clears the tracked state for one agent and returns 1 if it existed.
func (s *StructuralHasher) Reset(agent AgentID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[agent]; ok {
		delete(s.seen, agent)
		return 1
	}
	return 0
}

// state returns the agent's state, creating it (and evicting if necessary) to
// stay within MaxEntries. The caller must hold s.mu.
func (s *StructuralHasher) state(agent AgentID, now time.Time) *structuralState {
	st, ok := s.seen[agent]
	if !ok {
		s.evictIfNeeded()
		st = &structuralState{expires: now.Add(s.cfg.TTL)}
		s.seen[agent] = st
	}
	return st
}

// evictIfNeeded keeps len(s.seen) bounded by MaxEntries, dropping the agent
// whose window expires soonest. The caller must hold s.mu.
func (s *StructuralHasher) evictIfNeeded() {
	for len(s.seen) >= s.cfg.MaxEntries {
		var oldest AgentID
		var oldestT time.Time
		first := true
		for a, st := range s.seen {
			if first || st.expires.Before(oldestT) {
				oldest, oldestT, first = a, st.expires, false
			}
		}
		if first {
			return
		}
		delete(s.seen, oldest)
	}
}

// appendWindow appends e and trims the FIFO to at most cap entries, keeping the
// most recent.
func appendWindow(sigs []sigEntry, e sigEntry, cap int) []sigEntry {
	sigs = append(sigs, e)
	if len(sigs) > cap {
		sigs = sigs[len(sigs)-cap:]
	}
	return sigs
}

// jaccard estimates the Jaccard similarity of two sketches as the fraction of
// MinHash slots that agree.
func jaccard(a, b signature) float64 {
	var match int
	for i := 0; i < minHashK; i++ {
		if a.v[i] == b.v[i] {
			match++
		}
	}
	return float64(match) / minHashK
}

// minHash computes the MinHash sketch of the structural form. Each shingle is
// hashed once (FNV-1a); the hash is projected into minHashK slots via the
// golden-ratio mix and the minimum per slot is retained.
func minHash(form []byte) signature {
	var sig signature
	for i := range sig.v {
		sig.v[i] = ^uint64(0)
	}
	n := len(form)
	if n < shingleSize {
		return sig
	}
	count := 0
	for i := 0; i+shingleSize <= n; i++ {
		h := fnv1a(form[i : i+shingleSize])
		for k := 0; k < minHashK; k++ {
			hi := h ^ (uint64(k+1) * goldenRatio)
			if hi < sig.v[k] {
				sig.v[k] = hi
			}
		}
		count++
		if count >= maxShingles {
			break
		}
	}
	return sig
}

// fnv1a computes the FNV-1a 64-bit hash of b without allocating a hash.Hash.
func fnv1a(b []byte) uint64 {
	h := fnv1aOffset
	for _, c := range b {
		h ^= uint64(c)
		h *= fnv1aPrime
	}
	return h
}

// normalizeBufPool reuses the structural-form buffer across requests so the hot
// path stays allocation-free.
var normalizeBufPool = sync.Pool{
	New: func() any { b := make([]byte, 0, 2048); return &b },
}

// normalizePayload rewrites the dynamic values of a JSON body into placeholders
// and appends the resulting structural form to *dst. It is a single pass that
// never allocates beyond the pooled buffer and never fails: unparseable input
// degrades to a static blob.
//
// Placeholders: number → '#', UUID → 'U', timestamp → 'T', long hex → 'H',
// bool → 'B', null → 'N'. Object keys and static string values are kept
// verbatim (with their quotes), so the string "123" stays distinct from the
// number 123. Insignificant whitespace outside strings is dropped so
// pretty-printed and compact encodings normalize identically.
func normalizePayload(dst *[]byte, body []byte) {
	out := (*dst)[:0]
	i, n := 0, len(body)
	for i < n {
		c := body[i]
		switch {
		case c == '"':
			j := i + 1
			for j < n {
				if body[j] == '\\' {
					j += 2
					continue
				}
				if body[j] == '"' {
					break
				}
				j++
			}
			if j >= n { // unterminated string → copy the rest verbatim
				out = append(out, body[i:]...)
				i = n
				continue
			}
			content := body[i+1 : j]
			if tag, ok := classifyString(content); ok {
				out = append(out, tag)
			} else {
				out = append(out, body[i:j+1]...)
			}
			i = j + 1
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			for j < n && isNumberByte(body[j]) {
				j++
			}
			out = append(out, '#')
			i = j
		case c == 't' && hasWord(body, i, "true"):
			out = append(out, 'B')
			i += 4
		case c == 'f' && hasWord(body, i, "false"):
			out = append(out, 'B')
			i += 5
		case c == 'n' && hasWord(body, i, "null"):
			out = append(out, 'N')
			i += 4
		default:
			out = append(out, c)
			i++
		}
	}
	*dst = out
}

// hasWord reports whether body[i:] starts with the literal word w.
func hasWord(body []byte, i int, w string) bool {
	if i+len(w) > len(body) {
		return false
	}
	return string(body[i:i+len(w)]) == w
}

// isNumberByte reports whether c can appear in a JSON number literal.
func isNumberByte(c byte) bool {
	return (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E'
}

// classifyString classifies a JSON string value's content and returns a
// placeholder tag for dynamic kinds, or ok=false to keep it verbatim.
func classifyString(s []byte) (byte, bool) {
	if len(s) == 0 {
		return 0, false
	}
	if isUUID(s) {
		return 'U', true
	}
	if isRFC3339(s) {
		return 'T', true
	}
	if (len(s) == 10 || len(s) == 13) && isAllDigits(s) {
		return 'T', true
	}
	if len(s) >= 8 && isHexWithDigit(s) {
		return 'H', true
	}
	return 0, false
}

func isUUID(s []byte) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
		} else if !isHex(s[i]) {
			return false
		}
	}
	return true
}

// isRFC3339 recognizes a minimal YYYY-MM-DDTHH:MM prefix (a date-time string).
func isRFC3339(s []byte) bool {
	if len(s) < 16 {
		return false
	}
	for i := 0; i < 4; i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	if s[4] != '-' || !isDigit(s[5]) || !isDigit(s[6]) || s[7] != '-' ||
		!isDigit(s[8]) || !isDigit(s[9]) {
		return false
	}
	if s[10] != 'T' && s[10] != 't' {
		return false
	}
	if !isDigit(s[11]) || !isDigit(s[12]) || s[13] != ':' || !isDigit(s[14]) || !isDigit(s[15]) {
		return false
	}
	return true
}

func isAllDigits(s []byte) bool {
	for _, c := range s {
		if !isDigit(c) {
			return false
		}
	}
	return true
}

// isHexWithDigit reports whether s is all hex and contains at least one digit,
// which avoids stripping ordinary all-letter words like "deadbeef".
func isHexWithDigit(s []byte) bool {
	hasDigit := false
	for _, c := range s {
		if !isHex(c) {
			return false
		}
		if isDigit(c) {
			hasDigit = true
		}
	}
	return hasDigit
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// ceilSeconds returns the ceiling of d in whole seconds, minimum 1.
func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	return int((d + time.Second - 1) / time.Second)
}
