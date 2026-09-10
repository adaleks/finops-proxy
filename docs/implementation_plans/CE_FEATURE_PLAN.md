# CE Feature Plan — Advanced, Non-LLM Dynamic Loop Detection

**Author:** ce-feature-architect agent
**Status:** Design proposal — no Go source modified yet
**Applies to:** `pkg/circuitbreaker/` in the `github.com/adaleks/finops-proxy` core module
**Date:** 2026-09-09

---

## 1. Purpose

The core today ships only **Basic Loop Detection: Exact Hash Matching** (`pkg/circuitbreaker/hash_detector.go`). That is a byte-exact SHA-256 repeat detector: it catches an agent that re-sends the *identical* prompt, but it is blind to the far more common real-world failure mode — an agentic loop that re-issues a request with the *same structure* while a few dynamic fields change each turn (a counter, a UUID, a timestamp, a generated token, a database row id). This plan specifies three **non-LLM, deterministic** detectors that close that gap, plus the pipeline that composes them without disturbing the existing exact-hash behavior.

The three new detectors:

| # | File | Detects | Signal |
|---|---|---|---|
| 1 | `structural_hasher.go` | Near-duplicate prompts that differ only in dynamic values | Structural MinHash / n-gram signature |
| 2 | `tool_matcher.go` | Cyclic tool-call patterns (`A → B → A → B`) | Ordered tool-name sequence over history |
| 3 | `velocity_breaker.go` | Abnormal token consumption spikes | Token rate / acceleration per agent |

They are all **pure math + stdlib**, matching the `ce-core-architecture` constraints: zero external dependencies, <1 ms hot-path overhead, `sync.Pool`-backed allocation discipline, offline-first, and no new import surface outside `pkg/circuitbreaker`.

**Non-goals.** Semantic/vector similarity (embeddings) remains EE-only per the `ce-feature-gate` matrix. This plan adds no persistence, no external I/O, and no changes to the enterprise module.

---

## 2. Design Strategy: Backward-Compatible Capability Extension

### 2.1 The problem

`pkg/circuitbreaker/detector.go` defines a deliberately tiny contract:

```go
type Detector interface {
    Check(agent AgentID, fp Fingerprint) CheckResult
    Reset(agent AgentID) int
}
```

`Check` receives only `(agent, fp)` — the **pre-computed fingerprint**, not the body bytes. The three new detectors need more than the fingerprint:

- **StructuralHasher** needs the raw body to strip dynamic values and re-hash.
- **ToolMatcher** needs the body to extract the tool-call sequence.
- **VelocityBreaker** needs a token count (derivable from the body).

We must not change `Detector` — the `finops-run` wrapper tests, the existing `HashDetector`, and third-party mocks compile against it. Changing it would be a breaking change to the public core's exported contract.

### 2.2 The established pattern

`detector.go` already solves exactly this shape of problem twice, with *optional capability interfaces* that callers **type-assert at startup**:

```go
type DetectorStatus interface { ... }        // read-side dashboard capability
type DetectorLimitSetter interface { SetLimit(n int) }
```

`HashDetector` opts in by implementing them; the base `Detector` interface is untouched. The plan extends that pattern with one new optional capability.

### 2.3 The new capability interface

Add to `detector.go`:

```go
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
```

The handler (see §7) then prefers the payload-aware path when available, and falls back to `Check` for any plain `Detector`:

```go
// pkg/proxy/handler.go — step (e), future change (not in this plan's scope)
var res circuitbreaker.CheckResult
if pc, ok := h.det.(circuitbreaker.PayloadChecker); ok {
    res = pc.CheckPayload(agent, fp, body)
} else {
    res = h.det.Check(agent, fp)
}
```

This is a one-line, fail-safe integration: a plain `HashDetector` behaves exactly as before; a `Pipeline` (which satisfies both `Detector` and `PayloadChecker`) enables all four checks. The actual handler edit and `cmd/proxy` wiring are deferred to the implementation step (§10).

### 2.4 Why a composed `Pipeline` and not four separate `Detector`s

The handler holds a single `det Detector`. Composing the four detectors behind one `Pipeline` that also satisfies `Detector` means:

- `NewHandler` takes one detector, unchanged.
- `DELETE /v1/agent/{id}/history` calls `det.Reset(agent)` once and the pipeline fans it out to every sub-detector, so a `finops-run` kill + reset clears **all** loop state, not just exact-hash state.
- The existing `proxy_test.go` and any EE wiring keep compiling.

---

## 3. `detector.go` Additions (types only, no logic)

The concrete implementations live in their own files. `detector.go` gains only the shared contract types and reason constants:

```go
// Reason constants: each detector reports a distinct, operator-actionable
// reason so logs, 429 bodies, and the dashboard can distinguish the loop class.
// The exact-hash literal currently inlined in hash_detector.go is promoted to
// ReasonExactHash during implementation (pure refactor, no behavior change).
const (
    ReasonExactHash  = "identical request fingerprint observed within TTL window (possible infinite loop)"
    ReasonStructural = "structurally identical request observed within TTL window (possible loop with dynamic values)"
    ReasonToolCycle  = "repeating tool-call cycle detected (possible A->B->A->B agent loop)"
    ReasonVelocity   = "abnormal token consumption velocity (possible unbounded consumption loop)"
)
```

Plus `PayloadChecker` (§2.3) and `TokenEstimator` (§7.1). Nothing else in `detector.go` changes. `CheckResult` is reused verbatim — every new detector reports the same shape (`Block`, `Reason`, `RetryAfter`, `FirstBlock`), so the proxy's 429 path, saved-cost accounting, incident recording, notification, and live-log emission all work unchanged.

---

## 4. Detector 1 — Fuzzy Structural Hashing (`structural_hasher.go`)

### 4.1 Goal and failure model

An agent that calls an LLM in a loop with a slightly-mutated prompt — e.g. a retry loop that bumps a `"attempt": N` counter, embeds a fresh `request_id`, or appends a generated token — produces a different SHA-256 every turn. Exact hashing never fires. Structural hashing normalizes away those dynamic values and compares the *shape* of the request, so `{"attempt":1,"query":"same"}` and `{"attempt":2,"query":"same"}` collapse to one structural fingerprint.

### 4.2 Normalization: strip dynamic values

The core of the detector is `normalizePayload`, a single left-to-right scan over the raw JSON bytes (no `json.Unmarshal`, no re-marshalling, one pooled output buffer):

- **Numbers** (`-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?`) → `#`.
- **UUIDs** (36-char `8-4-4-4-12` hex pattern) → `U`.
- **Timestamps** (RFC3339 `"2026-09-09T12:34:56Z"` or 10–13-digit epoch digits inside a string) → `T`.
- **Long hex strings** (≥ 8 chars, all `[0-9a-fA-F]`) → `H`. (Short hex that is a common enum value — e.g. a single color token — is left verbatim; the 8-char floor avoids over-stripping.)
- **Booleans** (`true`/`false`) → `B`; **null** → `N`.
- **JSON object keys** and **static string values** are kept verbatim.

The output is the "structural form": a byte stream where only the structural skeleton and static content remain. Malformed or non-JSON bodies are not an error — the scanner degrades to treating the bytes as a single static blob (structural form ≈ original), so the detector is safe on arbitrary payloads.

```go
// normalizePayload rewrites dynamic JSON values to placeholders into *dst
// (a pooled buffer), returning the canonical structural form. It is a single
// pass; it never allocates beyond the pooled buffer and never fails.
func normalizePayload(dst *[]byte, body []byte)
```

### 4.3 Structural signature: MinHash over n-grams

The structural form is converted into a fixed-width **MinHash sketch**:

1. **Tokenize** the normalized form into n-grams ("shingles"). Shingle size `n = 3` (byte-level; rune-splitting is unnecessary for near-duplicate detection and slower).
2. **Hash each shingle once** with 64-bit FNV-1a (`hash/fnv`, stdlib) → `h`.
3. **Project `h` into K signature slots** by a cheap per-slot mix, keeping the minimum per slot. This is the standard "one hash → K MinHash values" bit-mixing trick, deterministic and dependency-free:

```go
// minHashK is the fixed MinHash signature width. It is a compile-time constant
// (not a config knob) so the signature is a value type [minHashK]uint64 and the
// hot path stays allocation-free.
const minHashK = 64

type signature struct {
    v [minHashK]uint64
}

// minHash computes the MinHash sketch of the structural form of body.
func minHash(body []byte) signature
```

The slot projection for slot `i` is `hi = h ^ (uint64(i+1) * 0x9E3779B97F4A7C15)` (the golden-ratio constant), and `sig.v[i] = min(sig.v[i], hi)` over all shingles. Shingles are capped at a fixed count (`maxShingles = 256`) to bound worst-case work on large bodies; the cap drops the *tail* shingles, which is fine because the head of a prompt carries the structural skeleton.

### 4.4 Similarity and threshold matching

Jaccard similarity of two shingle sets is estimated directly from the sketches:

```
sim(a, b) = |{ i : a.v[i] == b.v[i] }| / minHashK
```

Two structural forms are considered the **same structure** when `sim ≥ threshold` (default `0.85`). With K=64 the estimate's standard deviation is ~0.125 — more than enough to separate 0.85 from, say, 0.5 with a comfortable margin, while halving memory vs. K=128.

### 4.5 Sliding window and block logic

Per agent, keep a **bounded FIFO** of the most recent signatures (a ring of `Window` entries, default 8), each stamped with its arrival time and a fixed TTL window that starts at first sighting — the same semantics as `HashDetector`.

On each `CheckPayload`:

1. Compute the structural signature `sig` of `body`.
2. Sweep the agent's window, dropping entries whose timestamp is outside the TTL (lazy expiry).
3. Compare `sig` against every retained signature; count how many reach `threshold`.
4. If the match count ≥ `Limit` (default 3), report `Block` with `ReasonStructural` and `RetryAfter` = ceiling of remaining TTL, reusing the `FirstBlock` transition logic from `HashDetector`.
5. Otherwise append `sig` to the window (evicting the oldest when at capacity) and return a zero result.

Window *count* vs. TTL: the window holds at most `Window` signatures; TTL bounds their *age*. A match is only meaningful inside the TTL, so old entries are dropped regardless of how few are present.

### 4.6 Struct and constructor

```go
// StructuralConfig tunes the fuzzy structural detector. Zero-valued fields are
// replaced by defaults in NewStructuralHasher.
type StructuralConfig struct {
    Threshold  float64       // Jaccard similarity counting as "same structure" (default 0.85)
    Window     int           // max recent signatures retained per agent (default 8)
    Limit      int           // structural matches within TTL that block (default 3)
    TTL        time.Duration // window lifetime (default 10m)
    MaxEntries int           // hard cap on tracked agent states (default 10_000)
}

type StructuralHasher struct {
    cfg  StructuralConfig
    mu   sync.Mutex
    seen map[AgentID]*structuralState
    now  func() time.Time // injectable clock; defaults to time.Now
}

type structuralState struct {
    sigs      []sigEntry // bounded FIFO of recent signatures
    count     int        // matches observed inside the current window
    expires   time.Time  // end of the fixed TTL window
    blockedAt time.Time  // first time count reached Limit (zero until then)
    lastBlock time.Time  // last time CheckPayload returned Block
}

type sigEntry struct {
    sig signature
    at  time.Time
}

func NewStructuralHasher(cfg StructuralConfig) *StructuralHasher
```

`StructuralHasher` satisfies `PayloadChecker` (and, for `Reset`/`Detector`, is composed via the `Pipeline` — it does not itself implement `Detector.Check`).

### 4.7 Algorithm summary (pseudo-code)

```go
func (s *StructuralHasher) CheckPayload(agent AgentID, _ Fingerprint, body []byte) CheckResult {
    buf := normalizeBufPool.Get().(*[]byte)   // pooled
    defer normalizeBufPool.Put(buf)
    *buf = (*buf)[:0]
    normalizePayload(buf, body)

    sig := minHash(*buf)

    s.mu.Lock()
    defer s.mu.Unlock()
    st := s.state(agent)                       // get-or-create, bounded by MaxEntries
    now := s.now()

    // lazy expiry + window sweep
    st.sigs = st.sigs[:0]
    for _, e := range ... { if now.Before(st.expires) { keep } }
    matches := countOverThreshold(sig, st.sigs, s.cfg.Threshold)

    if matches >= s.cfg.Limit {
        first := st.blockedAt.IsZero()
        if first { st.blockedAt = now }
        st.lastBlock = now
        return CheckResult{Block: true, FirstBlock: first,
            Reason: ReasonStructural, RetryAfter: ceil(st.expires.Sub(now))}
    }
    st.sigs = append(st.sigs, sigEntry{sig, now}) // ring: evict oldest at capacity
    return CheckResult{}
}
```

---

## 5. Detector 2 — Tool-Call Loop Matcher (`tool_matcher.go`)

### 5.1 Goal and failure model

An agentic loop that alternates between a small set of tools without converging — `query_db → edit_file → query_db → edit_file → …` — produces different prompt text every turn but a stable, repeating *tool-call* signature. The tool matcher extracts the ordered tool-name sequence from the request and detects a cyclic repetition of it.

### 5.2 Tool-name extraction

Tool calls appear in three well-known shapes across the supported providers:

- **OpenAI/Anthropic assistant `tool_calls`**: `messages[].tool_calls[].function.name` (the *actual* calls made in the conversation).
- **Legacy OpenAI `functions`**: `functions[].name` (function definitions).
- **`tools[].function.name`**: tool *definitions*, present in the request schema.

The detector prioritizes **calls actually made** (`tool_calls[].function.name`) over **definitions** (`tools`/`functions`), because the loop signal is in what the agent *did*, not what it was *allowed* to do.

```go
// extractToolNames returns the ordered tool/function names the body references,
// calls-before-definitions. It returns an empty slice for a body with no tool
// surface; it never fails (unparseable input yields an empty slice, not an
// error), so the detector degrades to a pass.
func extractToolNames(dst *[]string, body []byte)
```

For the <1 ms budget, a streaming `json.Decoder.Token()` walk is preferred over `json.Unmarshal` into a full message tree: it visits only the tokens we care about, allocates only the pooled `[]string`, and is immune to deeply-nested or adversarial JSON. (A hand-rolled scan for `"tool_calls"`/`"function"`/`"name"` is the fallback if profiling shows the decoder token walk is too slow; the interface below isolates that choice.)

### 5.3 Cycle detection

Maintain a **bounded FIFO** of the last `MaxSeq` tool names per agent (default 64). After appending the newly observed names, test whether the tail is a repeated cycle:

> The sequence ends in a period-`p` cycle iff `seq[len-2p : len-p] == seq[len-p : len]` for some `p`, i.e. the last `2p` elements are `P` followed by `P` again.

`A → B → A → B` is exactly a period-2 cycle with `p = 2`. `A → B → C → A → B → C` is period-3. A **confirmation** is recorded when the *same* period `p` is detected on two consecutive observations (`Limit = 2`); that second confirmation is what blocks, preventing a false positive from a single coincidental repetition. A period mismatch resets the confirmation counter.

```go
func detectCycle(seq []string, maxPeriod int) (period int, ok bool) {
    n := len(seq)
    for p := 2; p <= maxPeriod && 2*p <= n; p++ {
        if equal(seq[n-2*p:n-p], seq[n-p:n]) {
            return p, true
        }
    }
    return 0, false
}
```

### 5.4 Struct and constructor

```go
type ToolConfig struct {
    MaxSeq     int           // max retained tool names per agent (default 64)
    MaxPeriod  int           // longest cycle period tested (default 8)
    Limit      int           // consecutive confirmations before blocking (default 2)
    TTL        time.Duration // history lifetime (default 10m)
    MaxEntries int           // hard cap on tracked agent states (default 10_000)
}

type ToolMatcher struct {
    cfg    ToolConfig
    mu     sync.Mutex
    states map[AgentID]*toolState
    now    func() time.Time
}

type toolState struct {
    seq       []string  // bounded FIFO of recent tool names
    period    int       // last confirmed cycle period (0 = none yet)
    confirms  int       // consecutive confirmations of `period`
    expires   time.Time
    blockedAt time.Time
    lastBlock time.Time
}

func NewToolMatcher(cfg ToolConfig) *ToolMatcher
```

`ToolMatcher` satisfies `PayloadChecker`.

### 5.5 Algorithm summary (pseudo-code)

```go
func (m *ToolMatcher) CheckPayload(agent AgentID, _ Fingerprint, body []byte) CheckResult {
    names := toolNamePool.Get().(*[]string)   // pooled
    defer toolNamePool.Put(names)
    *names = (*names)[:0]
    extractToolNames(names, body)
    if len(*names) == 0 { return CheckResult{} }  // no tool surface → pass fast

    m.mu.Lock()
    defer m.mu.Unlock()
    st := m.state(agent)
    now := m.now()
    if !now.Before(st.expires) { st.reset(now) }  // expired → fresh window

    append(st.seq, *names...)                    // ring: evict oldest at capacity
    p, ok := detectCycle(st.seq, m.cfg.MaxPeriod)
    if ok && p == st.period { st.confirms++ } else { st.period, st.confirms = p, boolToInt(ok) }

    if st.confirms >= m.cfg.Limit {
        first := st.blockedAt.IsZero()
        if first { st.blockedAt = now }
        st.lastBlock = now
        return CheckResult{Block: true, FirstBlock: first,
            Reason: ReasonToolCycle, RetryAfter: ceil(st.expires.Sub(now))}
    }
    return CheckResult{}
}
```

---

## 6. Detector 3 — Token Velocity Breaker (`velocity_breaker.go`)

### 6.1 Goal and failure model

An unbounded-consumption loop (OWASP LLM04) burns tokens at an abnormally accelerating rate even when each prompt is unique and no tool cycles: a runaway generation that spawns a child agent per step, or a re-prompt loop with ever-growing context. Exact hashing, structural hashing, and tool matching all miss it because the *content* is fresh. The velocity breaker watches the **token rate**, not the content.

### 6.2 Signal: velocity and acceleration

Two complementary triggers, both over a per-agent sliding time window:

- **Burst cap** — tokens consumed within `Window` (default 30s) exceed `BurstTokens` (default 200 000). A hard ceiling that stops a firehose regardless of baseline.
- **Spike / acceleration** — the *instantaneous* rate (tokens in `ShortWindow`, default 5s) exceeds the *sustained* rate (tokens in `Window`) by `SpikeFactor` (default 5×). This is the "acceleration" term: it fires when consumption suddenly jumps relative to its own recent history, not when it is merely high in absolute terms.

Using a ratio against the agent's own recent baseline makes the breaker **adaptive**: a legitimate batch job running at a steady high rate stays under the spike trigger (short ≈ long), while a loop that doubles output every round trips it.

### 6.3 Data structure

Per agent, two FIFO ring buffers of `tokenEvent{t time.Time, n int64}`, each with a running sum:

- `events` + `sumLong` — the `Window` horizon.
- `short` + `sumShort` — the `ShortWindow` horizon (a tail of `events`).

On each observation, append `{now, tokens}`, advance both sums, and expire events older than their horizon. Both are O(1) amortized (append + head-pop); the ring is preallocated to a bound and reused, never growing.

### 6.4 Struct and constructor

```go
type VelocityConfig struct {
    Window      time.Duration // sustained-velocity horizon (default 30s)
    ShortWindow time.Duration // instantaneous-rate horizon (default 5s)
    BurstTokens int64         // hard cap: tokens within Window (default 200_000)
    SpikeFactor float64       // shortRate/longRate that blocks (default 5.0)
    MaxEntries  int           // hard cap on tracked agent states (default 10_000)
}

type VelocityBreaker struct {
    cfg    VelocityConfig
    mu     sync.Mutex
    states map[AgentID]*velocityState
    now    func() time.Time
}

type velocityState struct {
    events []tokenEvent // FIFO within Window
    short  []tokenEvent // FIFO within ShortWindow (⊆ events)
    sumLong  int64
    sumShort int64
    blockedAt time.Time
}

type tokenEvent struct {
    t time.Time
    n int64
}

func NewVelocityBreaker(cfg VelocityConfig) *VelocityBreaker
```

`VelocityBreaker` does **not** need the raw body — only a token count. It therefore does not implement `PayloadChecker` directly; the pipeline computes the token count once and calls:

```go
// CheckVelocity records a prompt-token observation for the agent and reports
// whether the consumption spike must be blocked.
func (v *VelocityBreaker) CheckVelocity(agent AgentID, tokens int64) CheckResult
```

(Keeping the body out of this method's signature also keeps `VelocityBreaker` usable from a response-side observer later, where only completion tokens are known.)

### 6.5 Algorithm summary (pseudo-code)

```go
func (v *VelocityBreaker) CheckVelocity(agent AgentID, tokens int64) CheckResult {
    v.mu.Lock()
    defer v.mu.Unlock()
    st := v.state(agent)
    now := v.now()

    st.append(now, tokens)                      // advance sums, expire old
    longRate  := float64(st.sumLong)  / v.cfg.Window.Seconds()
    shortRate := float64(st.sumShort) / v.cfg.ShortWindow.Seconds()

    block := st.sumLong > v.cfg.BurstTokens ||
             (longRate > 0 && shortRate/longRate > v.cfg.SpikeFactor)

    if block {
        first := st.blockedAt.IsZero()
        if first { st.blockedAt = now }
        return CheckResult{Block: true, FirstBlock: first,
            Reason: ReasonVelocity, RetryAfter: int(v.cfg.ShortWindow.Seconds()) + 1}
    }
    return CheckResult{}
}
```

`RetryAfter` for a velocity block is short (the `ShortWindow`), reflecting that a transient spike may clear immediately; the other detectors' TTL-based cooldown is longer because their signal is persistent repetition.

---

## 7. Unified Pipeline

### 7.1 Token estimation is injected, not imported

`pkg/circuitbreaker` must stay stdlib-only and must not import `pkg/pricing` (dependency direction: `pkg/proxy → pkg/circuitbreaker`, never the reverse). The pipeline therefore receives a **token estimator** rather than computing tokens itself:

```go
// TokenEstimator maps a request body to its prompt-token estimate. The proxy
// injects price.EstimatePromptTokens; a nil estimator falls back to a
// byte-length heuristic (len(body)/4), keeping the pipeline self-contained.
type TokenEstimator func(body []byte) int
```

### 7.2 Composition

```go
// Pipeline composes the exact-hash detector with the three dynamic detectors
// into one check-and-record entry point. It satisfies Detector (legacy
// contract: Check == exact hash only) and PayloadChecker (CheckPayload == full
// pipeline), so both old and new callers work unchanged.
type Pipeline struct {
    exact      Detector
    structural *StructuralHasher
    tools      *ToolMatcher
    velocity   *VelocityBreaker
    estimate   TokenEstimator
}

func NewPipeline(exact Detector, opts ...PipelineOption) *Pipeline

// PipelineOption: WithStructural(*StructuralHasher), WithTools(*ToolMatcher),
// WithVelocity(*VelocityBreaker), WithTokenEstimator(TokenEstimator).
// An omitted detector is simply not run (nil = disabled), so a pipeline can be
// built incrementally and the exact-hash detector alone is a valid pipeline.
```

### 7.3 Evaluation order and short-circuit semantics

`CheckPayload` runs the detectors in **cost/precision order** and returns the first block:

1. **Exact hash** (`p.exact.Check`) — the cheapest and most precise signal; byte-identical repeats are caught here first, preserving today's behavior bit-for-bit.
2. **Structural** — fuzzy near-duplicate.
3. **Tool cycle** — cyclic tool sequence.
4. **Velocity** — token-rate spike (tokens computed once via `p.estimate`).

```go
func (p *Pipeline) CheckPayload(agent AgentID, fp Fingerprint, body []byte) CheckResult {
    if res := p.exact.Check(agent, fp); res.Block { return res }  // (1) backward-compatible
    if p.structural != nil {
        if res := p.structural.CheckPayload(agent, fp, body); res.Block { return res }
    }
    if p.tools != nil {
        if res := p.tools.CheckPayload(agent, fp, body); res.Block { return res }
    }
    if p.velocity != nil {
        if res := p.velocity.CheckVelocity(agent, p.estimate(body)); res.Block { return res }
    }
    return CheckResult{}
}
```

`Pipeline.Check(agent, fp)` delegates to `p.exact.Check` only — it is the `Detector` implementation for the legacy call path and the control-plane `Reset` route, and it guarantees that a caller who only ever calls `Check` gets *exactly* the old behavior.

`Pipeline.Reset(agent)` calls `Reset` on the exact detector and clears the agent's `structural`, `tools`, and `velocity` state, returning the total entries removed. This is what makes `finops-run`'s kill + `DELETE /v1/agent/{id}/history` restart genuinely clean: **all** loop classes reset together.

### 7.4 Compile-time assertions

```go
var _ Detector       = (*Pipeline)(nil)
var _ PayloadChecker = (*Pipeline)(nil)
```

---

## 8. Memory Budget & Allocation Discipline

The `ce-core-architecture` skill mandates `sync.Pool` and low GC pressure. The plan's allocation accounting:

### 8.1 Hot-path allocations → pooled

| Allocation | Where | Pool |
|---|---|---|
| Structural-form buffer `[]byte` | `normalizePayload` | `normalizeBufPool` (`make([]byte, 0, 2048)`) |
| Tool-name slice `[]string` | `extractToolNames` | `toolNamePool` (`make([]string, 0, 16)`) |
| MinHash signature `[64]uint64` | `minHash` | **none** — value type on the stack |
| Velocity events | state-owned ring buffers | **none** — preallocated, reused |

```go
var (
    normalizeBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 2048); return &b }}
    toolNamePool     = sync.Pool{New: func() any { s := make([]string, 0, 16); return &s }}
)
```

Every pooled buffer is reset (`[:0]` / `[:0]`) before reuse, so no stale data leaks between requests. `signature` is deliberately a **value type** (`[64]uint64`, 512 bytes) so it lives on the stack and needs no pool.

### 8.2 State memory → bounded

Each detector owns a `map[AgentID] → state`, bounded by its own `MaxEntries` cap (default 10 000 for the dynamic detectors, vs. `HashDetector`'s existing 100 000). Overflow uses the same **evict-oldest** policy as `HashDetector.evictIfNeeded`. Worst-case per-agent footprint:

| Detector | Per-agent state | Worst case (10 000 agents) |
|---|---|---|
| Structural | `Window`(8) × `signature`(512 B) + bookkeeping ≈ 4.5 KB | ~45 MB |
| Tool | `MaxSeq`(64) × ~16 B + header ≈ 1.5 KB | ~15 MB |
| Velocity | two rings ≤ a few hundred events ≈ 4 KB | ~40 MB |

These are hard upper bounds; in practice the map is keyed by live agents and the TTL continuously recycles state. The totals (~100 MB at the extreme) are a deliberate trade — bounded and GC-stable — not an unbounded leak. All three caps are tunable in their `Config`.

### 8.3 Eviction and lazy expiry

Every detector mirrors `HashDetector`'s two mechanisms: (a) **lazy expiry** — sweep expired entries only on access, never on a timer goroutine; (b) **hard eviction** — when a map reaches `MaxEntries`, drop the oldest `expires`. No background goroutines, no timers.

---

## 9. Performance Budget (< 1 ms)

Per-request hot-path cost, worst case (bounded by `MaxBodyBytes = 10 MiB` on the handler, but each detector independently caps its work):

| Detector | Work | Bound |
|---|---|---|
| Structural | one byte scan + ≤256 shingles × (1 FNV + K mix) | ~256 × 64 = 16k cheap 64-bit ops ≈ tens of µs |
| Tool | streaming JSON scan to first tool region | O(n) bytes ≈ tens of µs |
| Velocity | O(1) amortized ring append + 2 divisions | ~sub-µs |
| Exact hash | SHA-256 of ≤10 MiB (already paid today) | unchanged |

Total additive overhead over the existing path is well under 1 ms for any realistic prompt (KBs–hundreds of KB). The 10 MiB pathological case is dominated by the already-existing `io.ReadAll` + SHA-256, not by the new detectors, and is capped by `maxShingles`.

**Lock contention:** each detector holds its **own** `sync.Mutex` (not a shared one), and the pipeline holds none — it calls sub-detectors sequentially. The structural/tool/velocity locks are only held for the map/ring update, not across body parsing or normalization, which happen before the lock is taken.

---

## 10. Implementation Order (future — not this plan's scope)

The plan is design-only. The subsequent implementation, when approved, will follow this order, each step ending at a green `go build ./... && go test ./...`:

1. `detector.go`: add `PayloadChecker`, `TokenEstimator`, and the four `Reason*` constants (pure additions; promote `HashDetector`'s inlined reason literal to `ReasonExactHash`).
2. `structural_hasher.go` + `structural_hasher_test.go`.
3. `tool_matcher.go` + `tool_matcher_test.go`.
4. `velocity_breaker.go` + `velocity_breaker_test.go`.
5. `pipeline.go` (or `detector.go`) + `pipeline_test.go`.
6. `pkg/proxy/handler.go`: the one-line `PayloadChecker` type-assert in step (e).
7. `cmd/proxy/main.go`: build a `Pipeline` with the four detectors + `WithTokenEstimator(price.EstimatePromptTokens)` instead of a bare `HashDetector`, gated behind flags (`-structural`, `-tool-loop`, `-velocity` or a single `-dynamic` flag) so the exact-hash-only mode remains available.

---

## 11. Configuration Defaults (single source of truth)

| Knob | Type | Default | Notes |
|---|---|---|---|
| `StructuralConfig.Threshold` | float64 | 0.85 | Jaccard similarity floor |
| `StructuralConfig.Window` | int | 8 | recent signatures compared |
| `StructuralConfig.Limit` | int | 3 | matches to block |
| `StructuralConfig.TTL` | duration | 10m | window lifetime |
| `StructuralConfig.MaxEntries` | int | 10 000 | agent-state cap |
| `ToolConfig.MaxSeq` | int | 64 | retained tool names |
| `ToolConfig.MaxPeriod` | int | 8 | longest cycle tested |
| `ToolConfig.Limit` | int | 2 | consecutive confirmations |
| `ToolConfig.TTL` | duration | 10m | history lifetime |
| `ToolConfig.MaxEntries` | int | 10 000 | agent-state cap |
| `VelocityConfig.Window` | duration | 30s | sustained horizon |
| `VelocityConfig.ShortWindow` | duration | 5s | instantaneous horizon |
| `VelocityConfig.BurstTokens` | int64 | 200 000 | hard token cap |
| `VelocityConfig.SpikeFactor` | float64 | 5.0 | short/long ratio |
| `VelocityConfig.MaxEntries` | int | 10 000 | agent-state cap |
| `minHashK` | const | 64 | MinHash width (compile-time) |
| `maxShingles` | const | 256 | shingle-count cap |

Defaults are **conservative** (higher thresholds, higher limits) so the CE core fails toward *allowing* traffic; operators tighten them for their own traffic. Each constructor replaces non-positive/out-of-range values with these defaults, mirroring `NewHashDetector`'s limit clamp.

---

## 12. Test Plan

All tests reuse the `fakeClock` / `staticClock` pattern from `hash_detector_test.go` (injectable `now`) and run under `go test -race`.

### 12.1 Structural hasher (`structural_hasher_test.go`)

- **Dynamic-value stripping**: bodies differing only in a number, a UUID, a timestamp, and a hex token produce `sim ≥ threshold` (block on repeat); bodies differing in *static* text do not.
- **Normalization unit tests**: each token class (`#`, `U`, `T`, `H`, `B`, `N`) replaced correctly; keys and static strings preserved.
- **Threshold boundary**: similarity just below `Threshold` passes; just above blocks.
- **Window/TTL**: `Window` distinct structures do not cross-contaminate; TTL expiry resets the window.
- **Malformed/non-JSON body**: degrades safely (pass, no panic).
- **Empty body**: no shingles → pass.
- **Eviction bound**: insert > `MaxEntries` agents, assert the map stays bounded and the detector stays live.
- **Concurrency**: parallel `CheckPayload` with the race detector.

### 12.2 Tool matcher (`tool_matcher_test.go`)

- **Period-2 cycle**: `A,B,A,B` blocks; `A,B,A,C` does not.
- **Period-3/4 cycles** and the `MaxPeriod` boundary.
- **No tool surface** (plain chat body) passes fast.
- **Calls vs. definitions**: `tool_calls[].function.name` is prioritized over `tools`/`functions`.
- **Consecutive-confirmation requirement**: a single repetition does not block; two do.
- **TTL / reset / agent isolation / eviction / concurrency**.

### 12.3 Velocity breaker (`velocity_breaker_test.go`)

- **Burst cap**: `Window`-token sum over `BurstTokens` blocks; steady rate under cap passes.
- **Spike**: short rate > 5× long rate blocks; steady high rate does not.
- **Window slide**: events older than `Window`/`ShortWindow` expire and no longer count.
- **Adaptive baseline**: the same absolute rate blocks for a cold agent and passes for a warm one (ratio, not absolute).
- **Concurrency** with the race detector.

### 12.4 Pipeline (`pipeline_test.go`)

- **Backward compatibility**: a `Pipeline` wrapping only a `HashDetector` returns identical results to the bare detector for `Check`; a mock `Detector` (not `PayloadChecker`) still routes through `Check`.
- **Short-circuit ordering**: a body that is both a byte-exact repeat and a structural match reports `ReasonExactHash` (exact wins).
- **Full-path hits**: a near-duplicate triggers structural; a tool cycle triggers `ReasonToolCycle`; a burst triggers `ReasonVelocity`.
- **`Reset` fan-out**: clearing an agent removes state from all four detectors.
- **Nil sub-detectors**: a pipeline with a disabled detector skips it without error.

### 12.5 Handler integration (`pkg/proxy/proxy_test.go`, implementation step)

- A `Pipeline` detector blocks a byte-identical repeat (existing `TestLoopBlockAndHistoryReset` semantics preserved) and additionally blocks a dynamic near-duplicate, a tool cycle, and a velocity burst via the `PayloadChecker` path.
- `DELETE /v1/agent/{id}/history` clears all loop state.

### 12.6 Benchmarks

- `BenchmarkStructuralHasher`, `BenchmarkToolMatcher`, `BenchmarkVelocityBreaker`, `BenchmarkPipelineCheckPayload` over a representative multi-KB chat body.
- **Assert (by recording)**: `-benchtime=1s -benchmem` shows **0 allocs/op** on the pooled paths and p99 wall-clock < 1 ms (target: < 100 µs for typical bodies).
- A `TestStructuralHasherNoAllocs`-style regression test using `testing.AllocsPerRun` to fail the build if a hot-path allocation regresses.

---

## 13. Error Handling & Concurrency Rules

- **No new errors on the hot path.** Normalization, tool extraction, and velocity math are total functions: unparseable or empty input yields a *pass* (fail-open), never an error and never a panic.
- **Wrapped errors** (`fmt.Errorf("...: %w", err)`) apply only to the future constructor/control-plane surface, if any — the hot path has none.
- **One mutex per detector**, held only for map/ring mutation; body parsing and normalization happen **before** acquiring the lock.
- **Inject `now func() time.Time`** in every detector (mirrors `HashDetector`) for deterministic, sleep-free TTL tests.
- **Context cancellation**: the detectors are synchronous and sub-millisecond; they do not perform I/O and thus do not need to observe `context.Context`. The handler continues to respect `r.Context()` upstream as it does today.

---

## 14. File Map (this plan introduces, no existing file is edited now)

| File | Contents |
|---|---|
| `pkg/circuitbreaker/detector.go` *(extend)* | `PayloadChecker`, `TokenEstimator`, `Reason*` constants (additions only) |
| `pkg/circuitbreaker/structural_hasher.go` | `StructuralConfig`, `StructuralHasher`, `normalizePayload`, `minHash`, `signature` |
| `pkg/circuitbreaker/tool_matcher.go` | `ToolConfig`, `ToolMatcher`, `extractToolNames`, `detectCycle` |
| `pkg/circuitbreaker/velocity_breaker.go` | `VelocityConfig`, `VelocityBreaker`, `velocityState`, `CheckVelocity` |
| `pkg/circuitbreaker/pipeline.go` | `Pipeline`, `PipelineOption`, `NewPipeline`, compile-time assertions |
| `pkg/circuitbreaker/structural_hasher_test.go` | §12.1 |
| `pkg/circuitbreaker/tool_matcher_test.go` | §12.2 |
| `pkg/circuitbreaker/velocity_breaker_test.go` | §12.3 |
| `pkg/circuitbreaker/pipeline_test.go` | §12.4 |
| `pkg/proxy/handler.go` *(extend, impl step)* | one-line `PayloadChecker` type-assert |
| `cmd/proxy/main.go` *(extend, impl step)* | build `Pipeline` + dynamic-detector flags |

---

## 15. Open Questions

1. **Config surface.** Expose the four detectors' knobs via `cmd/proxy` CLI flags (verbose) or a single `config/detection.json` (consistent with `config/pricing.json`)? Recommend: flags first, a JSON config as a fast follow.
2. **Structural `Threshold` calibration.** 0.85 and K=64 are principled defaults; they should be validated against captured loop traffic before GA. Should `minHashK` remain a compile-time const, or become a `StructuralConfig` field backed by a pooled `[]uint64` (trading hot-path allocation for configurability)? Recommendation: keep it a const.
3. **Velocity on the response path.** Completion tokens are known only in `ModifyResponse`. Should the velocity breaker also observe *output* tokens (a later, response-side integration), or is prompt-token velocity sufficient for v1? Recommendation: prompt-token velocity first.
4. **Default `BurstTokens` (200 000)** is a placeholder for a ~4 chars/token heuristic; it should be derived from a real traffic profile before the public release.
5. **`Reason` strings** are new user-visible text; confirm they match the existing tone and are wired through i18n only in EE (the core ships English literals, as `ReasonExactHash` does today).

---

*End of plan. The authoritative gate is `ce-feature-gate`; the authoritative build rules are `ce-core-architecture` and the root `CLAUDE.md`.*
