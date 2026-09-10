# CE Observability Plan — Zero-Dependency Developer Monitoring & Metrics

**Author:** ce-feature-architect agent
**Status:** Design proposal — no Go source modified yet
**Applies to:** `pkg/proxy/`, `internal/logging/`, `internal/wrapper/`, `cmd/finops-run/`, `cmd/proxy/` in `github.com/adaleks/finops-proxy`
**Date:** 2026-09-09

---

## 1. Purpose

The core today emits rich *events* (`pkg/logstream.LogEvent`, `proxy.Event`) through the transport-agnostic `logstream.Hub` and a `json.Encoder`-based stdout logger (`internal/logging/jsonlog.go`), but it has **no in-process metric registry, no JSON metrics endpoint, no embedded dashboard, and no live CLI status view**. A developer running `cmd/proxy` + `cmd/finops-run` has to squint at interleaved JSON lines to know how much the proxy is blocking, saving, or forwarding.

This plan adds a **local, zero-dependency observability surface** with three layers, each strictly offline and stdlib-only:

| # | Layer | Delivers | Primary files |
|---|---|---|---|
| 1 | **Live CLI status bar** | Real-time ASCII gauges for `finops-run` | `internal/wrapper/stats.go`, `cmd/finops-run` |
| 2 | **Metrics registry + embedded dashboard** | `/v1/metrics` JSON + `/dashboard` web UI | `pkg/proxy/metrics.go`, `pkg/proxy/dashboard.go` |
| 3 | **Structured `slog` event logger** | JSON events, `jq`-pipeable | `internal/logging/slog.go` |

All three satisfy the `ce-core-architecture` constraints: **zero external Go dependencies** (stdlib only — `sync/atomic`, `log/slog`, `embed`, `net/http`), **hot-path overhead < 1 ms** (actually < 1 µs — atomic adds), **no new background goroutines on the hot path**, and **no import of enterprise modules** (no webhooks, no OTel exporters, no multi-tenant hierarchy).

**Non-goals.** This is *developer* observability, not production telemetry. The EE dashboard (`internal/api/metrics.go`, `internal/api/logs.go`, the SSE HTTP surface, the multi-tenant control plane) remains EE-only per the `ce-feature-gate` matrix. This plan adds no persistence, no external exporters, no third-party SaaS, and no changes to `pkg/circuitbreaker` or `pkg/pricing`.

---

## 2. What already exists (grounding)

The plan builds on three existing mechanisms, none of which it changes:

- **`pkg/logstream/hub.go`** — a non-blocking, concurrency-safe pub/sub `Hub` that fans out `LogEvent` values. It already carries every signal a dashboard needs: `type` (`request.complete`, `loop_blocked`, `budget_exceeded`, `budget_level`, `cost_alert`), `level`, `timestamp`, `agent_id`, `model`, `prompt_tokens`/`completion_tokens`, `cost_micro_usd`, `saved_cost_micro_usd`, `status_code`, `reason`, `retry_after`. The proxy publishes through it in `pkg/proxy/logstream.go` (`publishLog`).
- **`pkg/proxy/handler.go`** — the request flow already computes, once per request, the exact values the metrics need:
  - in the 429 path (step *e*): `costMicro = h.price.SavedCost(body)` (the saved µUSD), `pricing.ExtractModel(body)`, `h.price.EstimatePromptTokens(body)`, and `res.Reason`.
  - in `cost_tracking.go`'s `onCost`: the real `promptTok`/`completionTok` and the `reqMeta` that already flows from the handler to the `ModifyResponse` hook.
- **`internal/wrapper/`** — `finops-run` already owns a stdlib `ProxyClient` (`proxyclient.go`) that talks to the proxy, and already detects loop triggers from child output (`signals.go`, `matchLoop`, `fire`) — the "immediate loop feedback" signal this plan's status bar reuses.

Two useful seams the metrics layer reads but never changes:

- **`circuitbreaker.DetectorStatus`** (`pkg/circuitbreaker/status.go`) — `BlockedAgents()` gives the current blocked-agent count for the dashboard's "blocked agents" gauge.
- **`pricing.PriceSource`** — token estimates and saved-cost math the metrics layer reuses rather than re-implements.

---

## 3. Layer 1 — Metrics Registry (`pkg/proxy/metrics.go`)

### 3.1 Design principle: atomics on the hot path, one mutex off it

Every required gauge is a `sync/atomic.Int64`. A hot-path *record* is a single atomic `Add` — nanoseconds, no lock, no allocation, no goroutine. The only `sync.Mutex` in the registry guards the **bounded** token-velocity ring and the **bounded** trigger-history ring, and it is only touched at record time, never by the JSON endpoint (which takes a snapshot under a read lock for microseconds).

This mirrors the existing handler idiom exactly: **nil = disabled**. `WithMetrics(nil)` (or omitting it) makes every `mark*` call a no-op, so a handler built without metrics is byte-for-byte the same as today.

### 3.2 The registry type

```go
// pkg/proxy/metrics.go
package proxy

import (
    "encoding/json"
    "net/http"
    "sync"
    "sync/atomic"
    "time"

    "github.com/adaleks/finops-proxy/pkg/circuitbreaker"
)

// Metrics is the zero-dependency in-memory observability registry behind
// GET /v1/metrics and the embedded /dashboard. Every counter is an atomic so the
// hot path records at the cost of a single atomic Add (no lock, no allocation,
// no goroutine); the only mutex guards the two bounded history rings.
type Metrics struct {
    active     atomic.Int64 // in-flight requests
    requests   atomic.Int64 // forwarded requests completed
    loopBlocks atomic.Int64 // loop-blocked 429s
    savedCost  atomic.Int64 // cumulative µUSD saved (Σ SavedCost on each block)

    latencyNs atomic.Int64 // Σ upstream latency in ns (forwarded requests only)
    latencyN  atomic.Int64 // latency sample count (for the average)

    tokens   tokenWindow   // bounded sliding token-velocity gauge (mutex-guarded)
    triggers triggerRing   // bounded recent loop_blocked history (mutex-guarded)

    started time.Time // registry start (uptime)
}

// NewMetrics returns an empty registry stamped with the current time.
func NewMetrics() *Metrics { return &Metrics{started: time.Now()} }

// WithMetrics wires a Metrics registry into the handler. A nil registry (or
// omitting this option) disables collection: every mark* call is a no-op and the
// request path is byte-for-byte unchanged.
func WithMetrics(m *Metrics) HandlerOption { return func(h *handler) { h.metrics = m } }
```

### 3.3 Hot-path record points (the only edits to `handler.go`)

Four marks, all nil-guarded, at four places that already exist:

```go
// All methods are nil-safe: a nil *Metrics receiver makes them no-ops, so the
// handler never needs a metrics != nil check.
func (m *Metrics) markStart() { if m != nil { m.active.Add(1) } }
func (m *Metrics) markDone()  { if m != nil { m.active.Add(-1) } }

// markLoopBlocked records one 429: +1 block, +saved µUSD, +prompt tokens to the
// velocity window. Called in the block branch of ServeHTTP (step e).
func (m *Metrics) markLoopBlocked(saved int64, promptTok int) {
    if m == nil { return }
    m.loopBlocks.Add(1)
    m.savedCost.Add(saved)
    m.tokens.add(time.Now(), int64(promptTok))
}

// markForwarded records one completed proxied call: +1 request, latency sample,
// and prompt+completion tokens into the velocity window. Called from onCost.
func (m *Metrics) markForwarded(latency time.Duration, promptTok, completionTok int) {
    if m == nil { return }
    m.requests.Add(1)
    m.latencyNs.Add(int64(latency))
    m.latencyN.Add(1)
    m.tokens.add(time.Now(), int64(promptTok+completionTok))
}
```

The wiring inside `ServeHTTP` (step-by-step, matching the existing letters):

- **Top of `ServeHTTP`, before any early return** (steps *a*–*b* fast paths must still be counted as in-flight, so the gauge is honest): `h.metrics.markStart()` and `defer h.metrics.markDone()`. A single `defer` covers every exit path — the non-POST fast path, the budget gate, the oversized fail-open, the 413, the 429, and the forwarded path.
- **429 branch** (step *e*), after `costMicro` is computed: `h.metrics.markLoopBlocked(costMicro, h.price.EstimatePromptTokens(body))`.
- **`onCost`** (`cost_tracking.go`), where `promptTok`/`completionTok` are final: `h.metrics.markForwarded(time.Since(meta.start), promptTok, completionTok)` — see §3.4 for the `start` field.

The token-velocity window gets **prompt tokens at request time** *and* **completion tokens at response time**, so the gauge reflects near-real-time consumption, not just completed calls.

### 3.4 Latency measurement (the one new field)

`reqMeta` (`cost_tracking.go`) already carries per-request metadata from the handler to `onCost`. It gains one field:

```go
type reqMeta struct {
    // ... existing fields ...
    start time.Time // captured just before h.rp.ServeHTTP; drives avg latency
}
```

Set in the handler right before `h.rp.ServeHTTP(w, r)` (step *f*), read in `onCost`. This is a **zero-cost change**: `reqMeta` is a value already placed on the context for every tracked request, and one extra `time.Time` (24 bytes) does not change its allocation class.

**"Average proxy latency"** is defined as the end-to-end lifetime of a *forwarded* request: from just before `h.rp.ServeHTTP` to the `onCost` callback (i.e. upstream response fully consumed, or stream closed). It excludes 429/budget rejections (they have no upstream call) but includes upstream time. The average is `latencyNs / latencyN` in µs.

### 3.5 The token-velocity gauge (bounded, mutex-guarded)

A fixed ring of 1-second buckets, mirroring the `velocity_breaker.go` design in `CE_FEATURE_PLAN.md` for consistency. It is O(1) amortized (append + advance a rolling sum), preallocated, and never grows:

```go
const (
    velocityBuckets = 64   // seconds of token history retained (64s)
    velocityWindow  = 5    // seconds over which token_velocity is computed
)

// tokenWindow is a fixed ring of per-second token buckets. add is O(1); velocity
// divides the trailing velocityWindow seconds' tokens by the window.
type tokenWindow struct {
    mu      sync.Mutex
    buckets [velocityBuckets]int64
    second  int64 // absolute second index of buckets[0] (monotonic)
}

func (w *tokenWindow) add(now time.Time, tokens int64) {
    if tokens <= 0 { return }
    w.mu.Lock(); defer w.mu.Unlock()
    sec := now.Unix()
    // advance: zero any buckets between the last recorded second and now
    for w.second < sec {
        w.buckets[(sec)%velocityBuckets] = 0 // (see implementation note below)
        w.second++
    }
    w.buckets[sec%velocityBuckets] += tokens
}

func (w *tokenWindow) velocity(now time.Time) (tokensPerSec float64) {
    w.mu.Lock(); defer w.mu.Unlock()
    var sum int64
    for i := 0; i < velocityWindow; i++ {
        sum += w.buckets[(now.Unix()-int64(i))%velocityBuckets]
    }
    return float64(sum) / float64(velocityWindow)
}

func (w *tokenWindow) series(now time.Time) []TokenPoint {
    // last N seconds of tokens for the dashboard sparkline (bounded, ≤ velocityBuckets)
}
```

**Implementation note:** the `for` loop in `add` must advance one second at a time and zero each stale bucket (a naive `w.second = sec` skips the zeroing). Since the gap between successive requests is almost always 0 or 1 second, the loop body runs once in the common case and is bounded by the ring size in the worst case. The exact index arithmetic is the implementer's to finalize; the invariants are: fixed `[64]int64`, rolling sum over a trailing 5s window, no allocation, no goroutine.

### 3.6 The snapshot and the `/v1/metrics` handler

```go
// TokenPoint is one second of the token-consumption sparkline.
type TokenPoint struct {
    Second int64 `json:"second"`
    Tokens int64 `json:"tokens"`
}

// TriggerEvent is one recent loop-blocked episode for the trigger-history panel.
type TriggerEvent struct {
    Timestamp  int64  `json:"timestamp"`
    AgentID    string `json:"agent_id,omitempty"`
    Model      string `json:"model,omitempty"`
    Reason     string `json:"reason"`
    SavedMicro int64  `json:"saved_cost_micro_usd"`
}

// MetricsSnapshot is the JSON shape served at GET /v1/metrics.
type MetricsSnapshot struct {
    ActiveRequests    int64          `json:"active_requests"`
    TotalRequests     int64          `json:"total_requests"`
    LoopBlocks        int64          `json:"loop_blocks"`
    BlockedAgents     int            `json:"blocked_agents"`
    SavedCostMicroUSD int64          `json:"saved_cost_micro_usd"`
    AvgLatencyMicro   int64          `json:"avg_latency_micro"`
    TokenVelocity     float64        `json:"token_velocity"`
    TokenSeries       []TokenPoint   `json:"token_series"`
    RecentTriggers    []TriggerEvent `json:"recent_triggers"`
    UptimeSeconds     int64          `json:"uptime_seconds"`
}

func (m *Metrics) Snapshot() MetricsSnapshot {
    now := time.Now()
    n := m.latencyN.Load()
    var avg int64
    if n > 0 { avg = m.latencyNs.Load() / n / 1_000 } // ns → µs
    return MetricsSnapshot{
        ActiveRequests:    m.active.Load(),
        TotalRequests:     m.requests.Load(),
        LoopBlocks:        m.loopBlocks.Load(),
        SavedCostMicroUSD: m.savedCost.Load(),
        AvgLatencyMicro:   avg,
        TokenVelocity:     m.tokens.velocity(now),
        TokenSeries:       m.tokens.series(now),
        RecentTriggers:    m.triggers.snapshot(),
        UptimeSeconds:     int64(now.Sub(m.started).Seconds()),
    }
}

// MetricsHandler serves GET /v1/metrics. It takes the detector so it can report
// the current blocked-agent count via the optional DetectorStatus capability,
// exactly as the EE control plane does (type-assert at startup, no core change).
func MetricsHandler(m *Metrics, det circuitbreaker.Detector) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
            w.Header().Set("Allow", http.MethodGet)
            w.WriteHeader(http.StatusMethodNotAllowed)
            return
        }
        snap := m.Snapshot()
        if ds, ok := det.(circuitbreaker.DetectorStatus); ok {
            snap.BlockedAgents = len(ds.BlockedAgents())
        }
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(snap)
    })
}
```

`json.NewEncoder` in the endpoint is fine: it runs **off the hot path** (only when `/v1/metrics` is polled), so its allocation is not part of the < 1 ms request-path budget.

### 3.7 The trigger-history ring

`triggerRing` is a bounded FIFO of `TriggerEvent` (cap 100), fed from the same 429 branch that already emits the `loop_blocked` `logstream.LogEvent`. `recordTrigger` is called alongside `h.publishLog(...)` in `handler.go` step *e*, with `agent`, `model`, `reason`, and `costMicro`. This is what lets the dashboard show **trigger history** by polling `/v1/metrics` — no SSE endpoint is needed in the core (see §5.4 for why that is deliberate).

---

## 4. Layer 2a — `/v1/metrics` endpoint & wiring

`NewHandler` already builds and returns an `http.ServeMux` (`handler.go`), but the observability routes are mounted by **`cmd/proxy` on the outer `root` mux**, not inside the handler, so:

- the handler stays oblivious to HTTP observability (it only feeds `*Metrics` via `mark*` calls),
- the routes are opt-in and can be disabled with a flag,
- the routes sit **outside** the auth middleware, so they are reachable without a key (dev convenience — see §7.2 for the caveat).

```go
// cmd/proxy/main.go — additions
m := proxy.NewMetrics()
opts = append(opts, proxy.WithMetrics(m))
handler := proxy.NewHandler(upstream, det, priceSource, opts...)

root := http.NewServeMux()
root.HandleFunc("GET /healthz", ...) // existing
if *metrics {                         // new -metrics flag, see §7.2
    root.Handle("GET /v1/metrics", proxy.MetricsHandler(m, det))
}
if *dashboard {                       // new -dashboard flag, see §7.2
    root.Handle("GET /dashboard", proxy.DashboardHandler())
}
root.Handle("/", handler)
```

Go 1.22 method-and-path patterns (`GET /v1/metrics`) are more specific than the `"/"` catch-all, so they win; `handler.ServeHTTP` is only reached for everything else. `det` (a `*circuitbreaker.HashDetector`, or later a `*circuitbreaker.Pipeline`) satisfies `DetectorStatus`, so `BlockedAgents` resolves without a separate dependency.

---

## 5. Layer 2b — Embedded Dashboard (`pkg/proxy/dashboard.go`)

### 5.1 `go:embed` a single self-contained HTML file

The dashboard is **one** `index.html` with inline `<style>` and `<script>` (vanilla JS, no CDN, no framework, no build step). This makes `go:embed` a single directive and guarantees the page renders fully offline — which is also a hard requirement of the artifact/CDN rules had this been a web artifact; here it is *embedded in the Go binary*, so there is no network at all.

```
pkg/proxy/
├── dashboard.go
└── dashboard/
    └── index.html      # pure HTML + inline CSS + inline vanilla JS
```

```go
// pkg/proxy/dashboard.go
package proxy

import (
    "embed"
    "net/http"
)

//go:embed dashboard/index.html
var dashboardFS embed.FS

// DashboardHandler serves the embedded developer dashboard. The page polls the
// same-origin /v1/metrics endpoint, so it needs no server-side config.
func DashboardHandler() http.Handler {
    body, _ := dashboardFS.ReadFile("dashboard/index.html")
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.Write(body)
    })
}
```

### 5.2 What the page renders (all inline SVG, drawn from a `/v1/metrics` poll)

The single page has four regions, refreshed on a 500 ms `fetch('/v1/metrics')` timer (the poll interval is a `const` in the JS, tunable):

1. **KPI row** — four stat tiles: active requests, loop blocks, µUSD saved (formatted as `$0.0123`), average latency (µs). Pure HTML/CSS, no chart.
2. **Token-consumption sparkline** — an inline-SVG polyline of `token_series` (last ~60 seconds), plus a text gauge for `token_velocity` (tokens/sec). This is the "token consumption spike" visual: a spike is a visibly steep segment in the polyline.
3. **Trigger history table** — the last N `recent_triggers` rows (time, agent, model, reason, saved µUSD), newest first. A row is highlighted for a few seconds after it appears so a fresh trigger "flashes".
4. **Blocked-agents + uptime footer** — `blocked_agents` and `uptime_seconds`.

The JS is deliberately small: `fetch` → `json()` → mutate the DOM and re-compute SVG `points`. Inline SVG is generated by building the `<polyline points="…">` string from the series — no chart library, no dependencies.

### 5.3 Why "real-time" is polling, not SSE

The task says "real-time", and a 500 ms poll of a local `json.NewEncoder` endpoint is effectively real-time for a human watching a dev dashboard, at **zero** new surface. The alternative — a core SSE endpoint over the existing `logstream.Hub` — would be ~40 lines and stdlib-only, but the established split (`CE_TRANSFORMATION_PLAN.md` §3.4, §3.9) places **the SSE HTTP surface in EE**. Polling keeps the core's `/v1/metrics` as the *single* observability endpoint and respects that boundary. If true push is ever wanted, the hub already exposes `Subscribe`, and EE can add `internal/api/logs.go`-style SSE without touching core.

### 5.4 Dashboard data flow (diagram)

```
                    ┌────────────────────────────── pkg/proxy ──────────────────────────────┐
request ──> ServeHTTP ──> markStart/markLoopBlocked/markForwarded ──> Metrics (atomics+rings)
     │                                   │                                                    │
     └── forwarded ──> onCost ──> markForwarded(latency, tokens)                              │
                                                                                              │
GET /v1/metrics ──> MetricsHandler ──> Snapshot() ──> JSON ──┬───────────────────────────────┘
GET /dashboard ──> DashboardHandler ──> index.html (embedded) │  poll 500ms
                                    └─────────────────────────┘
```

---

## 6. Layer 3 — Live CLI Status Bar (`cmd/finops-run` + `internal/wrapper`)

### 6.1 Flags and config

Two new flags on `finops-run` (in `args.go`), backed by two new `Config` fields (`config.go`). Per the task's `--watch` *or* `--stats` wording, both names are accepted and are aliases for the same behavior (Go's `flag` has no native alias, so both `BoolVar`s point at the same `&cfg.Watch`; both default `false`, so order is irrelevant):

```go
// config.go
type Config struct {
    // ... existing fields ...
    Watch         bool          // render a live status bar polling the proxy /v1/metrics
    WatchInterval time.Duration // status bar refresh interval
}

// DefaultConfig() additions:
Watch:         false,
WatchInterval: time.Second,

// args.go — inside ParseArgs's flag set:
fs.BoolVar(&cfg.Watch, "watch", false, "render a live status bar from the proxy /v1/metrics")
fs.BoolVar(&cfg.Watch, "stats", false, "alias for -watch")
fs.DurationVar(&cfg.WatchInterval, "watch-interval", cfg.WatchInterval, "status bar refresh interval")
```

### 6.2 The stats client (decoupled DTO, stdlib-only)

`internal/wrapper` is today a **stdlib-only** package (it does not import `pkg/proxy`), and `CE_TRANSFORMATION_PLAN.md` records `cmd/finops-run ──> stdlib (+ net/http)`. To preserve that clean boundary, the CLI does **not** import `pkg/proxy.MetricsSnapshot`; it declares a tiny local DTO with the same JSON tags:

```go
// internal/wrapper/stats.go
type metricsSnapshot struct {
    ActiveRequests    int64   `json:"active_requests"`
    TotalRequests     int64   `json:"total_requests"`
    LoopBlocks        int64   `json:"loop_blocks"`
    BlockedAgents     int     `json:"blocked_agents"`
    SavedCostMicroUSD int64   `json:"saved_cost_micro_usd"`
    AvgLatencyMicro   int64   `json:"avg_latency_micro"`
    TokenVelocity     float64 `json:"token_velocity"`
    UptimeSeconds     int64   `json:"uptime_seconds"`
}
```

`ProxyClient` gains one method (a sibling of the existing `DeleteHistory`):

```go
// Metrics fetches the proxy's /v1/metrics snapshot into a local DTO.
func (c *ProxyClient) Metrics(ctx context.Context) (metricsSnapshot, error) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/metrics", nil)
    resp, err := c.httpc.Do(req)
    if err != nil { return metricsSnapshot{}, err }
    defer resp.Body.Close()
    var snap metricsSnapshot
    if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil { return metricsSnapshot{}, err }
    return snap, nil
}
```

### 6.3 The status bar renderer (TTY-aware, no `isatty` dependency)

The renderer draws a one-line ASCII status bar using ANSI escape sequences, but **only when the target is a terminal**. TTY detection uses stdlib `os.File.Stat()` — no `mattn/go-isatty`:

```go
func isTerminal(f *os.File) bool {
    fi, err := f.Stat()
    if err != nil { return false }
    return fi.Mode()&os.ModeCharDevice != 0
}
```

```go
type statusBar struct {
    client   *ProxyClient
    out      *os.File  // stderr, the bar's exclusive line
    interval time.Duration
    tty      bool
    mu       sync.Mutex
    loopFn   func() // renders the immediate "loop detected" line
}

// render draws (or redraws) the bar. On a TTY it emits "\x1b[2K\r" + line and no
// newline, so the next tick overwrites it in place. Off a TTY it is a no-op —
// the child's own stderr is not polluted with a periodic bar when piped.
func (s *statusBar) render(snap metricsSnapshot) {
    if !s.tty { return }
    saved := float64(snap.SavedCostMicroUSD) / 1_000_000
    line := fmt.Sprintf(
        "[finops-run] active=%d tok/s=%.0f blocks=%d blocked=%d saved=$%.4f avg=%dµs",
        snap.ActiveRequests, snap.TokenVelocity, snap.LoopBlocks,
        snap.BlockedAgents, saved, snap.AvgLatencyMicro,
    )
    fmt.Fprintf(s.out, "\x1b[2K\r%s", line)
}
```

The bar reports exactly the required gauges: **active requests**, **token velocity** (tokens/sec), **loop blocks**, and **saved µUSD** — plus blocked agents and average latency for free.

### 6.4 Wiring into the supervision loop

`Run` (`wrapper.go`) already constructs `client := NewProxyClient(...)` and detects loops via `fire`/`matched`. The status bar is started when `cfg.Watch` and its goroutine is stopped on exit:

```go
// inside Run, after `client` is created:
var bar *statusBar
if cfg.Watch {
    bar = newStatusBar(client, os.Stderr, cfg.WatchInterval)
    bar.start(ctx) // goroutine: ticker → client.Metrics → render
}

// inside the loop-detected branch, before restarting:
if bar != nil { bar.loopDetected(snap) } // immediate, highlighted "loop detected" line
```

`bar.loopDetected` renders one non-overwritten line (e.g. `\x1b[2K\r[finops-run] ⚠ loop detected — restarting agent` + `\n`) so the **immediate feedback on a loop trigger** is visible even though the periodic bar would otherwise scroll it away. The `loopDetected` call reuses the `fire` signal the wrapper already has; no new detection logic is added.

**Stream discipline:** the bar renders to **stderr** on its own line (ANSI `\r` redraw), while the child's stdout/stderr pass through unchanged via `newLineTee`. On a TTY this interleaving is contained by the `\x1b[2K\r` redraw; when `stderr` is piped, the bar is disabled entirely, so `finops-run --watch cmd | tee log` stays clean. (If a future iteration wants the bar on stdout, that is a one-line change to the `out` field; stderr is chosen so the child's real stdout is never corrupted.)

---

## 7. Layer 3 — Structured `slog` Event Logger (`internal/logging/slog.go`)

### 7.1 Honest framing of "zero-allocation"

The task asks for a "zero-allocation structured logger (`slog`)". `slog` is **stdlib** (Go 1.21+; the module already declares `go 1.22`), so it satisfies *zero external dependencies*. It is **not** literally zero-allocation — `slog` allocates a `Record` and the `Attr` slice per call. This plan's honest contract is therefore:

- **Hot path (the `Metrics` counters): truly zero-allocation** — `sync/atomic` adds, no `slog` call, no `json` marshal. This is where the < 1 ms (really < 1 µs) budget applies.
- **Event path (the `slog` logger): allocation is fine and off the hot path.** The logger runs on the `logstream.Hub` subscription goroutine (`Subscribe`), never inside `ServeHTTP`'s request flow. A loop-blocked or request-complete *event* is emitted a handful of times per request at most, and each is a bounded, GC-friendly allocation.

This is the same discipline the existing `jsonlog.go` already has (it serializes off the request path via the hub subscription), just with `slog` as the encoder instead of a hand-rolled `json.Encoder`.

### 7.2 The logger

```go
// internal/logging/slog.go
package logging

import (
    "io"
    "log/slog"
    "sync"

    "github.com/adaleks/finops-proxy/pkg/logstream"
    "github.com/adaleks/finops-proxy/pkg/proxy"
)

// SlogLogger writes proxy.Events and logstream.LogEvents as newline-delimited
// JSON via slog's JSONHandler, so each line is a single JSON object pipeable
// through `jq`. It is safe for concurrent use and never blocks the request path
// (it is driven by the hub subscription goroutine, off the hot path).
type SlogLogger struct {
    mu sync.Mutex
    l  *slog.Logger
}

// NewSlogLogger returns a SlogLogger writing JSON to w.
func NewSlogLogger(w io.Writer, level slog.Level) *SlogLogger {
    return &SlogLogger{l: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))}
}

// Notify implements proxy.Notifier.
func (s *SlogLogger) Notify(ev proxy.Event) {
    s.mu.Lock(); defer s.mu.Unlock()
    s.l.LogAttrs(context.Background(), slog.LevelWarn, "proxy event", eventAttrs(ev)...)
}

// Log writes one live log event as a JSON line.
func (s *SlogLogger) Log(ev logstream.LogEvent) {
    s.mu.Lock(); defer s.mu.Unlock()
    level := levelFor(ev.Level)
    s.l.LogAttrs(context.Background(), level, ev.Type, logEventAttrs(ev)...)
}

// Subscribe attaches the logger to a hub, mirroring Logger.Subscribe.
func (s *SlogLogger) Subscribe(hub *logstream.Hub) (stop func()) { /* same shape as jsonlog.go */ }

var _ proxy.Notifier = (*SlogLogger)(nil)
```

### 7.3 `jq`-compatible flat output

To keep `jq '.cost_micro_usd'` / `jq '.type'` working against the *same flat field names* the existing `logstream.LogEvent` JSON already uses, the logger hand-builds `slog.Attr` (flat keys, not a nested `slog.Any("event", ev)` group):

```go
// logEventAttrs flattens one LogEvent into slog attrs, reusing the exact JSON
// field names of logstream.LogEvent so the wire format is unchanged from the
// jsonlog.go encoder. Zero-value fields are omitted (omitempty semantics) to
// keep lines minimal — the same contract the existing logger has.
func logEventAttrs(ev logstream.LogEvent) []slog.Attr {
    a := make([]slog.Attr, 0, 16)
    a = append(a,
        slog.String("type", ev.Type),
        slog.String("level", ev.Level),
        slog.Int64("timestamp", ev.Timestamp),
    )
    if ev.TenantID != ""   { a = append(a, slog.String("tenant_id", ev.TenantID)) }
    if ev.AgentID != ""    { a = append(a, slog.String("agent_id", ev.AgentID)) }
    if ev.Model != ""      { a = append(a, slog.String("model", ev.Model)) }
    if ev.ProjectName != ""{ a = append(a, slog.String("project_name", ev.ProjectName)) }
    if ev.PromptTokens != 0      { a = append(a, slog.Int64("prompt_tokens", ev.PromptTokens)) }
    if ev.CompletionTokens != 0  { a = append(a, slog.Int64("completion_tokens", ev.CompletionTokens)) }
    if ev.CostMicroUSD != 0      { a = append(a, slog.Int64("cost_micro_usd", ev.CostMicroUSD)) }
    if ev.SavedCostMicroUSD != 0 { a = append(a, slog.Int64("saved_cost_micro_usd", ev.SavedCostMicroUSD)) }
    if ev.StatusCode != 0        { a = append(a, slog.Int("status_code", ev.StatusCode)) }
    if ev.Reason != ""           { a = append(a, slog.String("reason", ev.Reason)) }
    if ev.RetryAfter != 0        { a = append(a, slog.Int("retry_after", ev.RetryAfter)) }
    return a
}
```

(`eventAttrs` is analogous for `proxy.Event`; `levelFor` maps `"warn"`/`"error"`/`"info"` to `slog.Level`.)

### 7.4 Migration note (no source change now)

`cmd/proxy/main.go` currently wires `logging.New(os.Stdout)` + `logger.Subscribe(hub)`. Swapping in `logging.NewSlogLogger(os.Stdout, slog.LevelInfo)` is a **one-line change** at the call site — the `proxy.Notifier` interface is unchanged, so the handler, hub, and all tests keep compiling. The plan **keeps `jsonlog.go`** (it is the reference for the `proxy.Notifier` wire format and is used by tests) and adds `slog.go` alongside; deleting `jsonlog.go` is a follow-up decision, not a prerequisite.

---

## 8. Hot-Path Performance Budget (< 1 ms)

The governing budget is the `ce-core-architecture` rule: any middleware/detection logic < 1 ms. The observability layer's hot-path cost is three orders of magnitude under that:

| Action | Mechanism | Cost |
|---|---|---|
| `markStart`/`markDone` | one `atomic.Int64.Add` each | ~5–10 ns |
| `markLoopBlocked` | three `atomic.Add` + one ring push (mutex) | ~100 ns typical |
| `markForwarded` | three `atomic.Add` + one `time.Since` + one ring push | ~200 ns typical |
| `reqMeta.start` | one `time.Time` field set on a value already in the context | ~0 ns (no new allocation) |
| `recordTrigger` | one bounded ring push (mutex, only on a 429) | ~100 ns, only on blocks |

**Total additive overhead: < 1 µs** per request — well inside the 1 ms budget and below the noise floor of the existing `io.ReadAll` + SHA-256 that the handler already pays. The mutex-guarded rings are contended only at ring-update granularity (nanoseconds held), never across body parsing.

**Allocation discipline:**

| Allocation | Where | Handling |
|---|---|---|
| Counters/gauges | `Metrics` | `atomic.Int64` value fields — none |
| Token buckets | `tokenWindow` | fixed `[64]int64` value array — none, preallocated |
| Trigger history | `triggerRing` | fixed-cap slice, reused — one preallocation, zero steady-state |
| `reqMeta.start` | `cost_tracking.go` | field on an existing value — none |
| JSON snapshot / `slog` records | `/v1/metrics` handler, logger goroutine | **off the hot path**, bounded per poll/event |

**No goroutines on the hot path.** The only new goroutine is the CLI status bar's poller, which lives in `finops-run` (a separate process), not in the proxy. The proxy gains no timers, no background sweeps, no GC churn from observability.

---

## 9. Feature-Gate & Architecture Compliance

The `ce-feature-gate` matrix and `CLAUDE.md` are the authority. This plan's compliance:

| Rule | How this plan satisfies it |
|---|---|
| **Zero Enterprise Imports in Core** | `metrics.go`/`dashboard.go` import only stdlib + `pkg/circuitbreaker` (already a core dep). No `internal/` EE packages, no SAML/SaaS/OTel. |
| **Exported Core Interfaces** | `Metrics`, `MetricsSnapshot`, `MetricsHandler`, `DashboardHandler`, `WithMetrics` are exported and typed; the handler depends on a nil-able `*Metrics`, not a concrete external type. EE can reuse `MetricsSnapshot` in its own dashboard without importing anything new. |
| **Error Handling** | Hot-path marks never fail (nil-safe). The only new I/O (the JSON endpoint, the CLI poll) returns `wrapped` errors where applicable; the CLI poll is fail-open (a `Metrics` error just skips a render). |
| **Respect HTTP context cancellation** | The CLI poll uses `http.NewRequestWithContext(ctx, …)` and the status-bar goroutine selects on `ctx.Done()`. The endpoint uses the request context via the stdlib server. |
| **Storage: in-memory/SQLite core** | The registry is pure in-memory (atomics + bounded rings); no new storage driver. EE's Postgres/ClickHouse/OTel are untouched. |
| **Matrix: logging allowed, webhooks/OTel EE-only** | The `slog` logger is "standardized JSON to stdout" (allowed). The embedded dashboard is a **localhost, read-only, single-user dev view** — it is *not* the EE multi-tenant dashboard, *not* a webhook notifier, and *not* an OTel exporter. |

**Explicit boundary note:** the core's `/dashboard` is deliberately distinct from EE's `internal/api` dashboard. The core dashboard is: unauthenticated (dev), read-only (no DELETE/PUT), single-node (reads one process's `Metrics`), and embedded (no server-side templates/i18n). Anything tenant-scoped, role-gated, or multi-process belongs in EE. This is recorded so the split is reviewable.

---

## 10. Implementation Order (future — not this plan's scope)

Each step ends at `go build ./... && go test ./...`:

1. `pkg/proxy/metrics.go` — `Metrics`, `tokenWindow`, `triggerRing`, `MetricsSnapshot`, `WithMetrics`, `MetricsHandler` (+ `metrics_test.go`).
2. `pkg/proxy/handler.go` — the four `mark*` call sites + the `metrics` field (nil-guarded).
3. `pkg/proxy/cost_tracking.go` — add `reqMeta.start`; call `markForwarded` in `onCost`.
4. `pkg/proxy/dashboard.go` + `pkg/proxy/dashboard/index.html` — embed + handler (+ a smoke test that the handler serves non-empty HTML and the embed is resolvable).
5. `cmd/proxy/main.go` — `-metrics` / `-dashboard` flags, `WithMetrics`, mount the two routes.
6. `internal/logging/slog.go` — `SlogLogger` + `logEventAttrs`/`eventAttrs` (+ parity test against `jsonlog.go`'s field names).
7. `internal/wrapper/config.go`, `args.go` — `Watch`/`WatchInterval` + `-watch`/`-stats`/`-watch-interval`.
8. `internal/wrapper/stats.go` — `metricsSnapshot`, `ProxyClient.Metrics`, `statusBar` (+ `stats_test.go`).
9. `internal/wrapper/wrapper.go` — start/stop the bar; `loopDetected` hook.
10. (optional) swap `cmd/proxy` to `NewSlogLogger`; keep `jsonlog.go`.

---

## 11. File Map (this plan introduces; no existing file is edited now)

| File | Contents |
|---|---|
| `pkg/proxy/metrics.go` | `Metrics`, `MetricsSnapshot`, `TokenPoint`, `TriggerEvent`, `WithMetrics`, `MetricsHandler`, `tokenWindow`, `triggerRing` |
| `pkg/proxy/metrics_test.go` | §12.1 |
| `pkg/proxy/handler.go` *(extend, impl step)* | four `mark*` call sites + `metrics` field |
| `pkg/proxy/cost_tracking.go` *(extend, impl step)* | `reqMeta.start`; `markForwarded` in `onCost` |
| `pkg/proxy/dashboard.go` | `//go:embed dashboard/index.html`; `DashboardHandler` |
| `pkg/proxy/dashboard/index.html` | self-contained HTML/CSS/vanilla-JS + inline SVG |
| `cmd/proxy/main.go` *(extend, impl step)* | `-metrics`/`-dashboard` flags; mount routes |
| `internal/logging/slog.go` | `SlogLogger`, `logEventAttrs`, `eventAttrs`, `levelFor` |
| `internal/wrapper/config.go` *(extend)* | `Watch`, `WatchInterval` |
| `internal/wrapper/args.go` *(extend)* | `-watch`, `-stats`, `-watch-interval` |
| `internal/wrapper/stats.go` | `metricsSnapshot`, `ProxyClient.Metrics`, `statusBar` |
| `internal/wrapper/wrapper.go` *(extend, impl step)* | start/stop the bar; `loopDetected` |

---

## 12. Test Plan

### 12.1 Metrics registry (`metrics_test.go`)

- **Nil metrics**: `WithMetrics(nil)` (or omitted) → handler behaves identically to today; `mark*` on a nil `*Metrics` are no-ops (no panic).
- **Active gauge**: `markStart`/`markDone` bracket a request; `Snapshot().ActiveRequests` returns 0 after all done, N under concurrency.
- **Loop blocks & saved cost**: `markLoopBlocked(42, 10)` → `LoopBlocks==1`, `SavedCostMicroUSD==42`, velocity window received 10 tokens.
- **Latency average**: two `markForwarded` samples of known durations → `AvgLatencyMicro` equals the mean, truncated to µs.
- **Token velocity**: `tokenWindow.add` with a known sequence over a fixed clock → `velocity` matches `Σ(trailing 5s)/5`; bucket rollover zeroes stale slots; a >64 s gap produces an all-zero series (no stale tokens).
- **Trigger ring**: push > cap entries → oldest evicted, newest first, cap respected.
- **MetricsHandler**: `GET /v1/metrics` returns 200 JSON with the expected keys; a `Detector` that is *not* `DetectorStatus` yields `blocked_agents: 0`; a `HashDetector` that is yields the real count; non-GET returns 405.
- **Concurrency**: parallel `mark*` under `go test -race`; no torn snapshot fields.

### 12.2 Dashboard (`dashboard` smoke test, in `pkg/proxy`)

- `DashboardHandler` serves `text/html` with non-empty body containing the embedded marker text.
- `dashboardFS` resolves `dashboard/index.html` (embed is wired, not a broken path).

### 12.3 `slog` logger (`internal/logging`)

- **Parity**: for a fixed `logstream.LogEvent`, `SlogLogger` output and `jsonlog.go` output carry the same field *names* (assert by unmarshalling both into `map[string]any` and comparing keys).
- **`jq`-compatibility**: every `Log`/`Notify` call emits exactly one JSON object per line (no partial writes); `json.Unmarshal` of one line succeeds.
- **`proxy.Notifier` conformance**: compile-time `var _ proxy.Notifier = (*SlogLogger)(nil)`.

### 12.4 CLI status bar (`internal/wrapper`)

- **TTY detection**: `isTerminal` returns false for a regular file / pipe, true for a char device (test with a fake via interface, or skip the `*os.File` assertion and unit-test the branch logic).
- **Poll client**: `ProxyClient.Metrics` decodes a canned `/v1/metrics` body into the DTO; a non-2xx or network error returns an error and the bar skips the render (fail-open).
- **Render**: off-TTY `render` writes nothing; on-TTY it emits the `\x1b[2K\r` + expected gauges string; `loopDetected` emits a distinct, newline-terminated line.
- **Lifecycle**: `start`/`stop` — the goroutine exits on `ctx.Done()` and never leaks (assert with a done channel + timeout).

### 12.5 Benchmarks

- `BenchmarkMarkForwarded`, `BenchmarkMarkLoopBlocked`, `BenchmarkTokenWindowAdd` — assert (by recording) `-benchmem` shows **0 allocs/op** on the mark paths and p99 wall-clock < 1 µs, confirming the hot-path stays allocation-free.
- A `TestMetricsNoAllocs` regression using `testing.AllocsPerRun` to fail the build if a hot-path mark regresses into allocating.

---

## 13. Open Questions

1. **Default enablement.** Should `-metrics`/`-dashboard` default to **on** in the reference `cmd/proxy` binary (friendly for local dev, but exposes an unauthenticated read-only endpoint), or **off** (safest)? *Recommendation: default on, document "localhost/dev only — do not bind to a public interface", and always firewalled behind the listener, not the auth middleware.*
2. **`/v1/metrics` vs `/metrics` path.** The plan uses `/v1/metrics` for consistency with the existing `/v1/agent/{id}/history` and EE's `/api/v1/...` convention. A bare `/metrics` (Prometheus-text) would be a different format; the plan is JSON-only. Confirm JSON is the desired contract (it is what the dashboard and CLI consume).
3. **Dashboard token-velocity window.** `velocityWindow = 5s` and `velocityBuckets = 64` mirror the velocity-breaker defaults for consistency; they should be validated against a real dev session (is 5 s too jumpy / too smooth?).
4. **`--watch` poll interval.** `1 s` default, `500 ms` in the dashboard. A sub-second CLI poll is cheap for a localhost endpoint but adds a little stderr redraw churn; confirm the default.
5. **slog migration vs. coexistence.** Keep `jsonlog.go` (used by tests) and add `slog.go`, or fully replace? *Recommendation: coexist for one release, then delete `jsonlog.go` once EE and any downstream consumers confirm field-name parity.*
6. **Trigger-ring capacity (100).** Enough for a busy agent loop or too small? Bounded by memory, so raising it is cheap; confirm the default.

---

*End of plan. The authoritative gate is `ce-feature-gate`; the authoritative build rules are `ce-core-architecture` and the root `CLAUDE.md`.*
