# CE Transformation Plan — Extracting the Open-Source `finops-proxy` Core from `finops-proxy-ee`

**Author:** ce-architect agent
**Status:** Draft for review by the core refactor engineer
**Applies to:** source tree at `/home/goran/work/finops-proxy/finops-proxy-ee` (audited read-only) and the target module `/home/goran/work/finops-proxy/finops-proxy`.
**Date:** 2026-09-03

---

## 1. Executive Summary

`finops-proxy-ee` is a mature, ~14k-LOC (non-test) Go codebase that currently ships under `module finops-proxy`. It is a single binary (`cmd/proxy`) that fuses a circuit-breaker LLM reverse proxy with an enterprise multi-tenant dashboard, session/SSO auth, webhook alerting, and i18n. The audit confirms the code is well-engineered with narrow seams (interfaces like `auth.TenantStore`, `pricing.PriceSource`, `alerting.Notifier`, `proxy.CostThresholdReader`, `logstream.TenantNameResolver`), which makes decoupling feasible with modest surgery.

This plan maps every package/file to the `ce-feature-gate` matrix and prescribes an **ordered extraction** that yields:

1. A public Apache-2.0 module **`github.com/adaleks/finops-proxy`** with `pkg/proxy`, `pkg/circuitbreaker`, `pkg/pricing` (+ `pkg/logstream`) as the clean, importable core, plus a private `internal/store` reference implementation and lean `cmd/*` binaries. It must run offline with only In-Memory/SQLite storage and emit standardized JSON to stdout.
2. An enterprise module **`finops-proxy-ee`** that keeps the multi-tenant dashboard, SSO/session auth, webhook (Slack/Teams/Discord/custom) alerting, reports/i18n, and EE DB tables — and imports `github.com/adaleks/finops-proxy` for the engine.

The single most important structural change is resolving the **module-name collision**: today EE's `go.mod` declares `module finops-proxy`. The public core must claim that (future) import path, so EE is renamed to `finops-proxy-ee` and re-pointed at the core module. The second most important change is **interface extraction in `internal/proxy`**, which today imports the EE-only concrete packages `alerting`, `auth`, and `db` directly.

---

## 2. Current Repository Structure (as actually observed)

Root: `/home/goran/work/finops-proxy/finops-proxy-ee`

```
finops-proxy-ee/
├── go.mod                      # module finops-proxy ; go 1.22
├── go.sum
├── Dockerfile                  # multi-stage: runtime (proxy) + upstream (mockserver)
├── docker-compose.yml          # single-host orchestration; SQLite single-writer
├── CLAUDE.md                   # MVP-era project notes; describes internal/* layout
├── .env / .env.example         # FINOPS_* auth/session/alert env knobs
├── config/
│   ├── pricing.json            # static price table (required, fatal-if-missing)
│   └── alerting.json           # webhook/budget config (optional)
├── cmd/
│   ├── proxy/main.go           # wires EVERYTHING (core + EE) into one binary
│   ├── finops-run/main.go      # CLI loop-protection wrapper (stdlib)
│   ├── mockserver/main.go      # dev-only mock LLM upstream
│   ├── mockagent/main.go       # dev agent simulator
│   ├── seed/main.go            # seeds pricing + tenant config
│   └── tenants/main.go         # generates tenants.json
└── internal/
    ├── proxy/                  # handler.go reverseproxy.go budget.go cost_tracking.go alerting.go logstream.go
    ├── detector/               # detector.go hash_detector.go status.go
    ├── pricing/                # calculator.go load.go price_source.go registry.go fetcher.go service.go
    ├── alerting/               # alerting.go config.go manager.go webhook.go slack.go teams.go discord.go custom.go format.go budget.go
    ├── auth/                   # auth.go oauth.go session*.go password.go user_store.go invites.go web_*.go middleware.go state_store.go config.go bootstrap.go
    ├── api/                    # tenants.go keys.go incidents.go alerts.go reports.go reports_pdf.go metrics.go logs.go pricing.go settings.go login.go i18n.go embed.go middleware/rbac.go (web/ UI)
    ├── db/                     # sqlite.go users.go pricing.go reports.go settings.go alert_config.go
    ├── wrapper/                # args.go child.go config.go proxyclient.go signals.go wrapper.go
    ├── i18n/                   # locales en/de/sr
    ├── logstream/              # hub.go resolver.go
    └── settings/               # service.go
```

Package sizes (non-test LOC):

| Package | LOC | |
|---|---|---|
| `internal/api` | 3,746 | dashboard + control-plane REST + embedded web UI |
| `internal/auth` | 2,175 | API-key + session/SSO auth |
| `internal/db` | 1,970 | SQLite persistence (core + EE tables mixed) |
| `internal/alerting` | 1,381 | webhook notifier + budget monitor |
| `internal/pricing` | 1,348 | token cost math + static/remote price table |
| `internal/proxy` | 862 | the circuit-breaker reverse proxy engine |
| `internal/wrapper` | 495 | `finops-run` CLI loop protection |
| `internal/detector` | 362 | hash loop detection |
| `internal/i18n` | 397 | dashboard locales |
| `internal/logstream` | 287 | SSE/pub-sub hub + tenant resolver |
| `internal/settings` | 183 | runtime knobs (loop threshold, cost-alert threshold) |
| `cmd/*` | 878 | binaries |

Dependencies (go.mod direct): `github.com/glebarez/sqlite` (pure-Go SQLite), `github.com/signintech/gopdf` (PDF reports → EE), `golang.org/x/crypto` (bcrypt → EE), `gorm.io/gorm` (ORM used by both core and EE tables). No SaaS SDK is imported anywhere — alerting is plain HTTP webhook POSTs.

---

## 3. Component-by-Component Mapping (CORE vs EE)

The authoritative gate matrix:

| ALLOWED IN PUBLIC CORE | EXCLUSIVE TO EE |
|---|---|
| In-line Reverse Proxy Engine (`/v1/chat/completions`) for OpenAI, Anthropic, DeepSeek, Ollama | Multi-Tenant Hierarchy (Company → Department → Team → User) |
| Basic Loop Detection: Exact Hash Matching on input prompts in a sliding window | Enterprise SSO/Auth (SAML 2.0, OAuth2, LDAP, Active Directory) |
| Budget Controls: Global and Per-API-Key monthly/daily caps | Semantic/Vector-based Loop Detection (embeddings similarity) |
| Storage: In-Memory / SQLite driver support | Enterprise Data Drivers (PostgreSQL, ClickHouse, OpenTelemetry exporters) |
| Logging: Standardized JSON output to stdout/console | Real-Time Webhook Alerting (Slack / Microsoft Teams integration) |

### 3.1 `internal/proxy` → CORE (after decoupling) + thin EE wiring points

Audited facts:
- `handler.go` implements the circuit breaker: hashes non-empty POST bodies (SHA-256), consults `detector.Detector`, answers 429 `agent_loop_exception`, forwards via `httputil.ReverseProxy`, exposes `DELETE /v1/agent/{id}/history`. This is pure core.
- `reverseproxy.go` builds the `ReverseProxy` with `FlushInterval=-1`, compression disabled, SSE-safe streaming. Pure core.
- `budget.go` is month-boundary math. Pure core.
- `cost_tracking.go` wraps upstream response bodies in a passive usage observer and records cost. **Core**, but it imports `db.DB`, `db.RequestLog`, `db.IncidentInput`, `pricing`, `logstream` — must be reduced to interfaces.
- `alerting.go` (`emit`, `loopAlertsEnabled`, `checkBudget`) — **core budget behavior** but coupled to the EE `alerting` package types.
- `logstream.go` (`publishLog`) — core, coupled to `logstream.Hub`.

Coupling that must be extracted (the proxy package imports the following EE-bounded packages today):

| Current import | Purpose | Extraction |
|---|---|---|
| `internal/alerting` (`Notifier`, `Event`, `BudgetSpender`, `BudgetConfig`, `NewBudgetMonitor`, `LoopAlertGate`) | webhook/budget seam | Move the **types/seams** (`Event`, `EventType`, `Notifier`, `BudgetSpender`, `BudgetConfig`, `BudgetMonitor`, `LoopAlertGate`) into the core module (proposed `pkg/proxy`). EE `internal/alerting` keeps the transport (Slack/Teams/Discord/custom POST + `WebhookNotifier` + `Manager`) and implements the core interfaces. |
| `internal/auth` (`Authenticator`, `Tenant`, `TenantFrom`, `Middleware`) | per-key identity + monthly budget gate | Replace with core `Caller`/`KeyAuthenticator` seam in `pkg/proxy`. EE `internal/auth` keeps SAML/OAuth/session and implements the core seam with its `Tenant` type. |
| `internal/db` (`DB`, `RequestLog`, `IncidentInput`, `CreateOrTouchIncident`, `SumActualCostSince*`) | persistence | Replace with core `RequestLogStore`, `BudgetLedger`, and optional `IncidentRecorder` interfaces. EE `internal/db` implements them. |
| `internal/logstream` (`Hub`, `LogEvent`) | live stream | Move `Hub`/`LogEvent` to core `pkg/logstream`; the proxy consumes it through an interface or nil-able hub. EE adds the SSE HTTP surface + tenant resolver. |
| `internal/pricing` (`PriceSource`) | cost math | Move to core `pkg/pricing`; no change of shape. |
| `internal/detector` | loop policy | Move to core `pkg/circuitbreaker`. |

**Decision:** the proxy **engine** stays core; EE never forks it. EE only supplies implementations of the seams (auth, DB/ledger, webhook notifier, incident recorder, SSE resolver).

### 3.2 `internal/detector` → CORE, renamed `pkg/circuitbreaker`

Audited facts: `detector.go` is a tiny pure contract (`Detector`, `CheckResult`, `Fingerprint`, `AgentID`, optional `DetectorStatus`/`DetectorLimitSetter`). `hash_detector.go` is the SHA-256 sliding-window implementation, stdlib-only, `maxEntries=100_000`, per-agent keys. `status.go` is the read-side capability. This is exactly "Basic Loop Detection: Exact Hash Matching." Fully core; rename package to `circuitbreaker` per core CLAUDE.md. No EE references.

### 3.3 `internal/pricing` → CORE, renamed `pkg/pricing`

Audited facts: `calculator.go` (µUSD math, prefix-match price lookup), `load.go` (static `config/pricing.json` loader), `price_source.go` (`PriceSource` contract), `registry.go` (in-memory immutable snapshot + optional background refresh of a remote catalog), `fetcher.go` (stdlib HTTP fetch of an OpenRouter-style catalog; static-only when `Endpoint==""`). All of this is token-pricing, i.e. **core**, and the whole package is stdlib-only except `service.go`.

`service.go` is the **one EE-coupled file**: it persists/reloads `model_prices` rows through `db.DB` (GORM) and drives the EE REST `/api/v1/pricing/*` surface. Extraction options, in order of preference:
1. Keep `service.go` out of the core. The core engine only needs a `PriceSource`; the reference SQLite store (see §5) implements a small `PriceStore` (`List`, `Upsert`, `ReplaceSynced`) if a persisted price book is desired, and `cmd/proxy` rebuilds the serving `Registry` from it.
2. If full parity is required quickly, move `service.go` into the core but have it consume a core-defined `PriceStore` interface instead of `*db.DB`.

### 3.4 `internal/logstream` → CORE (`hub.go`) + EE (`resolver.go`, SSE surface)

Audited facts: `hub.go` is a transport-agnostic non-blocking pub/sub hub (`Hub`, `Subscription`, `LogEvent`, `TenantNameResolver func(tenantID) (name string, ok bool)`). It does **not** import HTTP. `resolver.go` (`CachedTenantResolver`) imports `db.DB` and resolves tenant_id → team name, and is a dashboard concern.

**Decision:** move `hub.go` to `pkg/logstream` in the core (it is generic stream logging infrastructure and satisfies the "JSON/stream logging" allowance; core `cmd/proxy` can publish structured JSON events through it or a stdout logger). Keep `CachedTenantResolver` in EE and pass it as a `pkg/logstream.TenantNameResolver`. The SSE HTTP surface (`internal/api/logs.go`) stays EE.

### 3.5 `internal/settings` → CORE

Audited facts: `settings/service.go` implements two runtime knobs — loop-detection repeat threshold and per-request cost-alert threshold — persisted to a single settings row, and applies them live to the detector. This is core behavior (loop threshold + cost alert). It imports `db.DB` (persistence) and `logstream.Hub`.

**Decision:** core keeps the service, decoupled to a core `SettingsStore` interface + `pkg/logstream` hub. The REST `GET/PUT /api/v1/settings` mux stays EE.

### 3.6 `internal/db` → SPLIT (the messiest package)

Audited facts: a single `DB` wraps one GORM handle, one mutex, and one `AutoMigrate` list containing **both** core and EE tables:
- `sqlite.go`: `Open()` (WAL pragmas, indexes), `RequestLog` + `LogRequest`, cost/agent **aggregations** (dashboard), `SumActualCostSince`/`SumActualCostSinceForTenant` (budget), `AddTenantSpend` (display counter), `Tenant`/`APIKey`/`User`/`Invitation`/`Incident` models + management methods, `ModelPrice` seeding helpers.
- `pricing.go`: `ModelPrice` model + seed/manual/sync persistence.
- `settings.go`: `Settings` single-row model.
- `users.go`, `reports.go`, `alert_config.go`: `User`/`Invitation`, report read-models, `AlertConfig` — EE.

Core slice (tables/behaviors allowed in the public core): **`RequestLog`** (usage/cost log — core), **`Settings`** (runtime knobs — core), **`ModelPrice`** (price book — core, optional). Also the budget `SUM` queries over `request_logs`. Aggregations are convenience for dashboards → EE.

EE slice: `Tenant`, `APIKey`, `User`, `Invitation`, `Incident`, `AlertConfig` models + all tenant/key/incident/alert management and dashboard aggregations/reports. The **multi-tenant hierarchy** and its management REST are EE.

**Decision:** split into (a) core `internal/store/sqlite` (or `pkg/store` if EE must reuse it — see §5) with only core tables + core methods, and (b) EE `internal/db` that either wraps the shared GORM handle or — recommended — keeps its own handle and implements the core **interfaces** on its `DB` type (with a small adapter for the public core record types). This avoids two GORM owners of one SQLite file.

### 3.7 `internal/alerting` → EE (transport) after moving seams to core

Audited facts: `alerting.go` (Event/Notifier/Stats), `webhook.go` (async `WebhookNotifier`), `slack.go`/`teams.go`/`discord.go`/`custom.go`/`format.go` (payload formatters), `config.go` (`alerting.json` + env loading), `manager.go` (runtime-reconfigurable `Manager`), `budget.go` (`BudgetMonitor`, `BudgetSpender`).

The **Slack/Microsoft Teams real-time webhook delivery is squarely EE**. However:
- The **budget-monitor logic** (`BudgetConfig`, `BudgetMonitor.Spend` threshold dedupe, `BudgetSpender`) is core "Budget Controls" behavior — but today it lives in the alerting package and fires webhook-shaped events.
- The `Event`/`Notifier`/`LoopAlertGate` types are the seam the core proxy needs so it can emit budget/loop events without knowing the transport.

**Decision:** move `Event`, `EventType`, `Notifier`, `LoopAlertGate`, `BudgetConfig`, `BudgetMonitor`, `BudgetSpender` into core `pkg/proxy` (the budget gate runs on the proxy hot path). Keep `WebhookNotifier`, all formatters, `config.go`, and `Manager` in EE. EE's `Manager` implements the core `Notifier` + `BudgetSpender` + `LoopAlertGate` interfaces, so `cmd/proxy` (EE) wires the same seams it wires today. Core `cmd/proxy` wires a **JSON-stdout notifier** instead of webhooks.

### 3.8 `internal/auth` → EE

Audited facts: `auth.go` is the API-key `Authenticator`/`TenantStore` seam (stdlib); `middleware.go` injects `Tenant` into ctx; `oauth.go` is Google OAuth2; `session*.go`, `password.go` (bcrypt → `x/crypto`), `invites.go`, `user_store.go`, `web_handlers.go`, `bootstrap.go`, `state_store.go`, `config.go` implement the browser/session/admin surface. Enterprise SSO/auth (OAuth2, and future SAML/LDAP/AD) is EE.

**Decision:** all of `internal/auth` stays in EE. Its lean core-shaped ideas (hash an API key, authenticate a caller, carry the caller in ctx, strip the internal header) are lifted into `pkg/proxy` as the `Caller`/`KeyAuthenticator`/`CallerStore` seams, and EE `auth` implements them. `golang.org/x/crypto` therefore remains EE-only.

### 3.9 `internal/api` → EE (entirely)

Audited facts: dashboard/metrics control plane (`metrics.go`), tenants/keys/incidents/alerts/pricing/settings/logs management muxes, session login + embedded web UI (`embed.go`, `web/login.html`, dashboard assets), `reports_pdf.go` (uses `gopdf`), i18n surface, RBAC middleware. All are mounted behind session auth + `RequireRole` in `cmd/proxy/main.go` and belong to the multi-tenant dashboard/control plane.

**Decision:** `internal/api` stays EE and imports core packages for types (`pkg/circuitbreaker` for `DetectorStatus`, `pkg/logstream` for the hub, `pkg/pricing` for the price source/service if the pricing control plane survives in EE). The EE binary owns the top-level mux.

### 3.10 `internal/i18n` → EE

Audited facts: locales `en/de/sr` for the dashboard/login/i18n REST surface. Dashboard i18n is EE.

### 3.11 `internal/wrapper` + `cmd/finops-run` → CORE

Audited facts: `wrapper.go` runs a CLI agent child, watches for a loop marker (HTTP 429 `agent_loop_exception`), kills it, calls `DELETE /v1/agent/{id}/history` via `proxyclient.go`, and restarts with a recovery prompt. Entirely stdlib; this is the client-side half of loop protection ("Loop protection wrapper"). `cmd/proxy` already exposes the matching DELETE route. `config.go`/`args.go` are stdlib.

**Decision:** core. Moves to the core repo as `cmd/finops-run` + a package (suggest `pkg/finopsrun` or `internal/wrapper`). Note: EE's `finops-run` currently also reacts to `budget_exceeded` (stop, don't retry) — that discrimination string is emitted by the core handler, so it survives unchanged.

### 3.12 `cmd/*` → split

- `cmd/proxy`: split. **Core** `cmd/proxy` = lean engine binary (upstream, TTL, limit, pricing file, optional sqlite store/keys file). **EE** `cmd/proxy` (in the EE repo, after rename) = the full assembly importing `github.com/adaleks/finops-proxy`.
- `cmd/mockserver`: dev/test mock LLM upstream → **core** (enables offline demo and E2E tests; also referenced by Docker compose dev profile).
- `cmd/mockagent`: dev agent loop simulator → **core** (test fixture) or drop.
- `cmd/seed`, `cmd/tenants`: tenant/key seeding tooling → **EE**.
- `cmd/finops-run`: → **core** (§3.11).

### 3.13 Infra / config files

- `Dockerfile`, `docker-compose.yml`: EE artifacts. The core repo should gain its own minimal `Dockerfile` (single runtime target) plus keep the mockserver upstream target for local demo/tests.
- `config/pricing.json`: copy into core (required seed). `config/alerting.json` is EE (webhook config); core uses no alerting file.
- `.env.example`, `.env`: EE (session/SSO vars).
- `CLAUDE.md`, `.claude/`: the EE CLAUDE.md documents the old `internal/*` MVP layout and must not propagate. The core repo already carries the authoritative CLAUDE.md + skills.

---

## 4. Cross-Cutting Concern: Dependency Direction after the Split

Today the dependency graph inside EE is effectively flat; several "core" packages import EE packages:

```
internal/proxy ──> internal/alerting, internal/auth, internal/db, internal/logstream, internal/pricing, internal/detector
internal/pricing/service.go ──> internal/db
internal/settings ──> internal/db, internal/logstream
internal/logstream/resolver.go ──> internal/db
internal/auth ──> (stdlib only, db implements its interfaces)   ← the good pattern
```

Target graph in the core module (nothing under `pkg/` or `internal/` may import EE):

```
pkg/proxy ──> pkg/circuitbreaker, pkg/pricing, pkg/logstream   (seams defined here)
pkg/pricing ──> stdlib only
pkg/circuitbreaker ──> stdlib only
pkg/logstream ──> stdlib only
internal/store/* ──> pkg/proxy, pkg/pricing (implements seams)
cmd/proxy ──> pkg/*, internal/store, internal/logging
cmd/finops-run ──> stdlib (+ net/http)
```

EE module depends on core module and **only supplies implementations**:

```
finops-proxy-ee
  internal/db ──> github.com/adaleks/finops-proxy/{pkg/proxy,pkg/pricing}   (implements store seams)
  internal/auth ──> github.com/adaleks/finops-proxy/pkg/proxy             (implements KeyAuthenticator/CallerStore)
  internal/alerting ──> github.com/adaleks/finops-proxy/pkg/proxy         (implements Notifier/BudgetSpender/LoopAlertGate)
  internal/api, internal/i18n ──> pkg/circuitbreaker, pkg/logstream, pkg/pricing
  cmd/proxy ──> all of the above
```

Rule of thumb for the refactor engineer: **in the core repo, the string `finops-proxy-ee` may appear only in docs; `github.com/glebarez/sqlite` and `gorm.io/gorm` may appear only under `internal/store`; `signintech/gopdf` and `golang.org/x/crypto` must not appear at all.** In the EE repo, all `finops-proxy/internal/*` import paths become `finops-proxy-ee/internal/*` and the engine packages are replaced by `github.com/adaleks/finops-proxy/pkg/*`.

---

## 5. Target Go Module Structure for the Core

### 5.1 Why `pkg/*` instead of `internal/*`?

Go's `internal/` tree is not importable by other modules. `finops-proxy-ee` must import the core engine, seams, and types as `github.com/adaleks/finops-proxy/...` — so everything EE consumes must be **public** (`pkg/...`). The core's own private implementation details (the reference SQLite store, JSON logger) that EE does *not* need can remain under `internal/`. This also satisfies the core CLAUDE.md, which mandates clean separation of `pkg/proxy`, `pkg/circuitbreaker`, and `pkg/pricing`.

### 5.2 Proposed tree

```
finops-proxy/                                # module github.com/adaleks/finops-proxy (Apache-2.0)
├── go.mod                                   # module github.com/adaleks/finops-proxy; go 1.22
├── LICENSE                                  # Apache 2.0
├── README.md
├── CLAUDE.md                                # already present (authoritative)
├── .claude/skills/ce-feature-gate, open-source-prep
├── config/pricing.json                      # copied from EE; required static price seed
├── cmd/
│   ├── proxy/main.go                        # lean engine binary
│   ├── finops-run/main.go                   # loop-protection CLI wrapper (moved)
│   └── mockserver/main.go                   # dev mock LLM upstream (moved)
├── pkg/
│   ├── proxy/                               # ← internal/proxy (engine) + moved seams
│   │   ├── handler.go                       # circuit-breaker handler (decoupled)
│   │   ├── reverseproxy.go                  # SSE-safe httputil.ReverseProxy
│   │   ├── options.go                       # HandlerOption: WithKeyAuth, WithStore, WithNotifier, WithLogStream…
│   │   ├── cost_tracking.go                 # usage observer (decoupled)
│   │   ├── budget.go                        # BudgetConfig, BudgetMonitor, BudgetSpender (moved from alerting)
│   │   ├── notifier.go                      # Event, EventType, Notifier, LoopAlertGate (moved from alerting)
│   │   ├── caller.go                        # Caller/KeyAuthenticator/CallerStore + context carrier (lifted from auth)
│   │   └── store.go                         # RequestLogStore, BudgetLedger, IncidentRecorder, SettingsStore seams
│   ├── circuitbreaker/                      # ← internal/detector
│   │   ├── detector.go                      # Detector, CheckResult, Fingerprint, AgentID
│   │   ├── hash_detector.go                 # SHA-256 sliding-window implementation
│   │   └── status.go                        # AgentStatus, DetectorStatus
│   ├── pricing/                             # ← internal/pricing minus service.go
│   │   ├── calculator.go  load.go  price_source.go
│   │   ├── registry.go    fetcher.go        # remote catalog refresh (optional)
│   │   └── config.go                        # (was in fetcher.go) — Config + DefaultConfig
│   └── logstream/                           # ← internal/logstream/hub.go
│       ├── hub.go  events.go                # Hub, Subscription, LogEvent, TenantNameResolver
└── internal/                                # private to core module
    ├── store/
    │   ├── memory/memory.go                 # in-memory RequestLogStore/BudgetLedger/SettingsStore (offline/tests)
    │   └── sqlite/sqlite.go                 # reference SQLite store: RequestLog, Settings, ModelPrice tables only
    └── logging/jsonlog.go                   # stdout JSON notifier/logger (implements pkg/proxy.Notifier)
```

### 5.3 Mapping table: old EE path → new core path

| EE source (audited) | New core location | Action |
|---|---|---|
| `internal/proxy/handler.go`, `reverseproxy.go`, `budget.go`, `cost_tracking.go`, `alerting.go`, `logstream.go` | `pkg/proxy/...` | Move, then decouple imports per §4/§6 |
| `internal/detector/detector.go`, `hash_detector.go`, `status.go` | `pkg/circuitbreaker/...` | Move, rename package, update refs |
| `internal/pricing/calculator.go`, `price_source.go`, `load.go`, `registry.go`, `fetcher.go` | `pkg/pricing/...` | Move |
| `internal/pricing/service.go` | (none in core) | Leave in EE; EE implements `pkg/pricing.PriceStore` if needed |
| `internal/logstream/hub.go` | `pkg/logstream/...` | Move |
| `internal/logstream/resolver.go` | (none in core) | Leave in EE |
| `internal/settings/service.go` | `pkg/proxy` or `pkg/settings` (consumer keeps interface in `pkg/proxy`) | Decouple from `db`; optionally move under `internal/settings` |
| `internal/alerting/alerting.go`, `budget.go` (types only) | `pkg/proxy/notifier.go`, `pkg/proxy/budget.go` | Move seam types |
| `internal/alerting/{webhook,slack,teams,discord,custom,format,config,manager}.go` | EE repo | Keep in EE |
| `internal/auth/*` | EE repo | Keep in EE (implements core seams) |
| `internal/api/*`, `internal/i18n/*` | EE repo | Keep in EE |
| `internal/db/sqlite.go` (core tables/methods), `pricing.go`, `settings.go` | `internal/store/sqlite` | Split out core tables + budget/usage queries |
| `internal/db/users.go`, `reports.go`, `alert_config.go`, EE tables in `sqlite.go` | EE repo | Keep in EE |
| `internal/wrapper/*`, `cmd/finops-run` | `internal/wrapper` or `pkg/finopsrun`, `cmd/finops-run` | Move (stdlib) |
| `cmd/mockserver`, `cmd/mockagent` | `cmd/mockserver`, `cmd/mockagent` | Move |
| `cmd/proxy/main.go` | both: lean core binary + EE binary | Split |
| `cmd/seed`, `cmd/tenants` | EE repo | Keep in EE |

---

## 6. Module-Name Collision Resolution

The problem, stated precisely:
- EE `go.mod` line 1: `module finops-proxy`. Every file in EE imports `finops-proxy/internal/...` and `finops-proxy/cmd/...`.
- The public core will (eventually) live at `github.com/adaleks/finops-proxy`. Two modules cannot both own the import path `finops-proxy`.

Resolution — **do the rename first, before any core extraction**, so every subsequent move carries the new path:

1. **Rename the EE module.** Change EE `go.mod` line 1 to `module finops-proxy-ee`.
2. **Rewrite EE import paths** mechanically: every `finops-proxy/` prefix on imports becomes `finops-proxy-ee/`. This touches ~90 non-test Go files across `cmd/`, `internal/`. Safe mechanical pass: `sed -i 's#"finops-proxy/#"finops-proxy-ee/#g' $(find . -name '*.go')`. Update `go.mod`/`go.sum` (module path appears in `go.sum` only in comment lines; `go mod tidy` after rename).
3. **Verify:** `go build ./... && go test ./...` in the EE tree still passes; behavior is identical (a pure path rename).
4. **Create/own the core import path.** Initialize `finops-proxy/go.mod` as `module github.com/adaleks/finops-proxy`. (Until the repo is actually published at that remote, use a `replace` directive or vanity path; the module line is what matters.)
5. **Add the dependency from EE to core.** EE `go.mod` gains:
   ```
   require github.com/adaleks/finops-proxy v0.0.0
   replace github.com/adaleks/finops-proxy => ../finops-proxy   // local dev until published
   ```
6. **Delete moved packages from EE only after core publishes the replacement** (order matters; see step-by-step §7). After the split, EE must no longer contain `internal/proxy`, `internal/detector`, `internal/pricing`, `internal/logstream` (engine parts) or `internal/wrapper` — its `cmd/proxy` and EE packages import them from `github.com/adaleks/finops-proxy/pkg/...`.

Collision edge case to keep in mind: the **Docker image / compose name** `finops-proxy` is a *container image name*, not a Go module path; it can stay `finops-proxy` for EE images or be tagged `finops-proxy-ee` in the EE repo. The **binary names** (`proxy`, `finops-run`, `mockserver`) are unaffected.

---

## 7. Ordered Refactor / Decoupling Steps

Each step ends at a green build (`go build ./...`, `go vet ./...`, `go test ./...`) for whichever module is being edited. Steps 1–3 are prerequisite; steps 4–8 are the actual surgery; step 9 is verification.

### Step 1 — Baseline snapshot (EE, read-only)
- `cd finops-proxy-ee && go build ./... && go test ./...` — record the green state.
- `go list -deps ./...` to enumerate which packages drag in `signintech/gopdf` and `golang.org/x/crypto` (expect: `api/reports_pdf`, `auth/password`).
- Copy the engine seam inventory from §3.1 as a checklist of every `alerting.`, `auth.`, `db.` reference inside `internal/proxy`.

### Step 2 — Module rename EE → `finops-proxy-ee`
- Apply the mechanical import rewrite and `go.mod` change from §6. Verify build/tests.
- This is the commit that makes future diffs reviewable.

### Step 3 — Create the core module skeleton
- `finops-proxy/go.mod` (`module github.com/adaleks/finops-proxy`, go 1.22), `LICENSE` (Apache-2.0), `README.md`, copy `config/pricing.json`.
- Add a tiny `main.go` under `cmd/proxy` that prints a version string so `go build ./cmd/proxy` works from day one.

### Step 4 — Move the pure stdlib core packages (no coupling surgery yet)
- Move, in this order, with `git mv`/`cp` + import-path fixes inside the core module:
  1. `internal/detector/*` → `pkg/circuitbreaker/*` (rename `package detector` → `package circuitbreaker`).
  2. `internal/pricing/{calculator,price_source,load,registry,fetcher}.go` → `pkg/pricing/*`.
  3. `internal/logstream/hub.go` → `pkg/logstream/*`.
  4. `internal/wrapper/*` + `cmd/finops-run`, `cmd/mockserver` → core.
- At each move, update the (temporarily still-in-EE) references? **No** — keep both modules compiling by doing the moves inside the core module and deleting the EE copies only when EE no longer needs them. Practically this means: after the engine is exported from core, EE's remaining callers (`cmd/proxy/main.go`, `api`, `settings`, `proxy`) switch to `github.com/adaleks/finops-proxy/pkg/...` and the old EE copies are deleted in Step 8.

### Step 5 — Define the core seams (interfaces) — the heart of the work
Add to `pkg/proxy` (sketches; exact signatures owned by the implementing engineer):

```go
// caller.go — replaces auth.Tenant inside the core proxy.
type Caller struct {
    ID   string // API key / key id; "" = anonymous
    Name string // display name
    MonthlyBudgetMicro int64 // 0 = unlimited
    DailyBudgetMicro   int64 // 0 = unlimited
}
type CallerStore interface {
    CallerByKeyHash(ctx context.Context, hash string) (Caller, error) // ErrUnknownKey
}
// Middleware reads X-FinOps-API-Key (stripped before upstream), resolves via
// CallerStore, injects Caller into ctx. auth.Middleware is the EE equivalent.
func WithCaller(ctx context.Context, c Caller) context.Context
func CallerFrom(ctx context.Context) (Caller, bool)

// store.go — persistence seams the handler consumes instead of *db.DB.
type RequestRecord struct { // public form of db.RequestLog (KeyID replaces TenantID)
    AgentID, KeyID, ProjectName, Model string
    PromptTokens, CompletionTokens, CostUSD int64
    IsLoopBlocked bool
    Timestamp int64
}
type RequestLogStore interface {
    LogRequest(ctx context.Context, r RequestRecord) error
}
type BudgetLedger interface {
    SumActualCostSince(ctx context.Context, since int64) (int64, error)            // global daily
    SumActualCostForKeySince(ctx context.Context, keyID string, since int64) (int64, error) // per-key monthly/daily
}
type SettingsStore interface { // Get/Update single settings row (loop threshold + cost-alert threshold)
    GetSettings(ctx context.Context) (SettingsRow, error)
    UpdateSettings(ctx context.Context, s SettingsRow) error
}
type IncidentRecorder interface { // optional; EE implements with its incidents table
    RecordIncident(ctx context.Context, in IncidentInput) error
}

// notifier.go / budget.go — moved from internal/alerting (types only).
type EventType string
const ( EventLoopBlocked EventType = "loop_blocked"; EventBudgetLevel EventType = "budget_level" )
type Event struct { ... }           // as audited in alerting.go
type Notifier interface { Notify(Event); Close() }
type BudgetSpender interface { Spend(spentMicro int64, now time.Time, emit func(Event) bool) }
type LoopAlertGate interface { LoopAlertsEnabled() bool }
type BudgetConfig struct { DailyBudgetMicro int64; ThresholdPct []int }
type BudgetMonitor struct { ... } // moved verbatim from alerting/budget.go
```

- `pkg/proxy/handler.go` then depends only on `pkg/circuitbreaker.Detector`, `pkg/pricing.PriceSource`, and the seams above. Delete the `alerting`/`auth`/`db` imports. The per-tenant monthly gate becomes a **per-key** gate fed by `Caller` + `BudgetLedger`. The incident upsert becomes an optional `IncidentRecorder` (nil = skipped). `writeBudgetExceeded` uses `Caller.Name` instead of `auth.Tenant.TeamName`.

### Step 6 — Reference storage + logging in the core
- `internal/store/memory`: in-memory `RequestLogStore`/`BudgetLedger`/`SettingsStore` for offline mode and tests.
- `internal/store/sqlite`: port the core slice of `db/sqlite.go` + `db/pricing.go` + `db/settings.go` — GORM + `glebarez/sqlite`, WAL pragmas, migrations for only `request_logs`, `model_prices`, `settings`, plus the budget indexes (`idx_request_logs_actual_time`, `idx_request_logs_tenant_actual_time` analog). Do **not** migrate EE tables.
- `internal/logging/jsonlog.go`: a `Notifier` that writes the `Event`/`LogEvent` as a JSON line to stdout; wire it into `cmd/proxy` by default.
- `cmd/proxy/main.go` (core): flags `-addr -upstream -ttl -limit -pricing [-db path] [-keys path]`; optional keys file for per-key budgets; builds `HashDetector`, static `pricing.Load`, optional registry refresh, memory/sqlite store, JSON stdout notifier.

### Step 7 — Rebuild EE on top of the core
- Delete EE `internal/proxy`, `internal/detector`, `internal/pricing`, `internal/logstream`, `internal/wrapper` (all moved).
- EE `internal/db` keeps its GORM handle for EE tables and implements the core seams. Where method names collide with different types (`LogRequest(ctx, db.RequestLog)` vs core `LogRequest(ctx, proxy.RequestRecord)`), add **adapter types** in EE, e.g. `type requestLogStore struct{ *db.DB }` in a new `internal/store_adapter.go`, converting core public types ↔ EE row structs.
- EE `internal/auth` implements `pkg/proxy.CallerStore`/`KeyAuthenticator`: its `Tenant` already carries `MonthlyBudgetMicro`; the adapter flattens the EE hierarchy (Company → Department → Team → User) into the flat core `Caller` for the proxy path, while the EE dashboard keeps the full hierarchy.
- EE `internal/alerting` keeps its `Manager`/`WebhookNotifier`/formatters/config and implements the core `Notifier`/`BudgetSpender`/`LoopAlertGate`.
- EE `cmd/proxy/main.go` keeps its full mux but calls `proxy.NewHandler(...)` from `github.com/adaleks/finops-proxy/pkg/proxy` and passes EE implementations of the seams. The EE binary now imports core for the engine only.
- EE adds `internal/store_adapter` unit tests proving the EE DB satisfies each core interface (compile-time `var _ proxy.RequestLogStore = (*adapter)(nil)`).

### Step 8 — Purge EE-only references from core; verify zero-EE-import rule
- `grep -R "finops-proxy-ee" finops-proxy/pkg finops-proxy/cmd finops-proxy/internal` → must be empty.
- `grep -R "signintech/gopdf\|golang.org/x/crypto" finops-proxy` → must be empty outside `go.sum` (and must not be in `go.mod`).
- Confirm no `pkg/*` file imports `gorm.io/gorm` except `internal/store/sqlite`.

### Step 9 — Open-source readiness + verification
- Run the `open-source-prep` checklist: no secrets/keys in source/tests/config; Apache-2.0 LICENSE present; offline autonomy (core builds and a `docker run` of `cmd/proxy` serves `/healthz` without network — static pricing mode `-pricing-url ""`).
- `go test ./...` in both modules; `go vet ./...`; `golangci-lint run` (per core CLAUDE.md).
- Add E2E: core `cmd/proxy` + `cmd/mockserver` + a looped client → expect 429 `agent_loop_exception`, `request_logs` row, and JSON stdout event.

### Step 10 — (Post-split, optional) close core feature gaps
- The matrix allows **per-API-key monthly/daily** caps and **global** caps. The audited code enforces per-key **monthly** (handler gate) and global **daily** (budget monitor). Add per-key daily caps and an explicit global monthly cap to the core `BudgetLedger`/`BudgetMonitor` so the matrix is fully satisfied by the public core alone.
- Restrict/route the core proxy to the LLM endpoints (`/v1/chat/completions` and documented provider prefixes) if desired; the audited handler is a catch-all for non-empty POSTs.

---

## 8. Risks and Open Questions

**Risks**

1. **Type-identity churn in EE `internal/db`.** The DB layer currently returns its own `db.RequestLog`/`db.Tenant`/`db.Incident` structs used by `api` and `proxy`. After the split, core interfaces use core-owned record types; EE needs adapters. Medium effort, low design risk, but easy to get subtly wrong at the `tenant_id` ↔ `key_id` rename. Mitigate with compile-time interface assertions and adapter tests (Step 7).
2. **Two GORM owners of one SQLite file.** If EE reuses the core SQLite store and *also* opens the same file for EE tables, WAL + two single-connection handles can serialize poorly. Recommended mitigation: EE owns the one handle and implements core interfaces; core ships its own SQLite store only for the standalone core binary/tests. Do not have both modules open the same DB file in one process.
3. **The budget gate semantics change names.** The EE concept `tenant` (a flat API-key bucket in today's `auth`) becomes core `Caller`/key and EE-only hierarchy above it. Any subtle behavior relying on the *empty-string legacy bucket* (requests without a tenant/key) must be preserved in the core (anonymous caller with global budget only). Flagged in `handler.go` (`tenantID(r.Context())` returns `""` when auth disabled).
4. **Event/log duplication.** Core will carry both `pkg/proxy.Event` (notification seam) and `pkg/logstream.LogEvent` (stream), mirroring today's dual emission. Risk of drift; consider consolidating in a later pass but do not block the split on it.
5. **Behavioral regression from removing the incident upsert from the core.** Today every 429 also upserts a durable `incidents` row. If the core skips incidents (EE-only `IncidentRecorder`), standalone core loses durable incident history — acceptable (loop-blocked rows already exist in `request_logs` with `is_loop_blocked=1`), but the 429 path must still emit the single `FirstBlock` notifier event.
6. **`go.sum`/toolchain noise.** `glebarez/sqlite` pulls `modernc.org/*` indirect deps into the core `go.mod`. Acceptable (pure-Go SQLite), but it violates a naive "zero deps" reading of the old EE CLAUDE.md. The core CLAUDE.md already sanctions SQLite, so keep GORM+glebarez under `internal/store` only.

**Open questions for the maintainers**

1. **Module path.** Confirm `github.com/adaleks/finops-proxy` (the placeholder used throughout this plan) is the real future path; adjust the `go.mod` module line and EE `replace` accordingly.
2. **Pricing control plane in EE.** Should EE keep its `/api/v1/pricing/*` management REST (manual price pins, manual sync) backed by its DB, with the core only ever holding a static or registry price source? Recommended: yes — EE keeps it; core stays lean.
3. **`cmd/mockagent`.** Ship it in the core as a dev/test fixture or drop it? (mockserver is clearly worth shipping for E2E.)
4. **Core binary flags.** Does the public core binary need a keys/quotas file today, or is CLI-flag global budget + anonymous mode sufficient for the first public release? (Matrix says per-API-key caps are core — recommend shipping a minimal keys-file flag.)
5. **Embedded metrics/control plane.** The audited `api.NewMetricsHandler` (summary/chart/agents/reset) is dashboard-oriented. Confirm it is EE-only for v1 and no public `/metrics` is required in the core.
6. **Naming of the EE binary/module.** After rename, EE's container image stays `finops-proxy` in its compose file, or becomes `finops-proxy-ee`? (Recommend EE-specific tags to avoid registry collisions with the future official core image.)

---

## 9. Appendix — EE Files Read During the Audit (grounding)

Core engine and policy: `internal/proxy/handler.go`, `internal/proxy/reverseproxy.go`, `internal/proxy/budget.go`, `internal/proxy/cost_tracking.go`, `internal/proxy/alerting.go`, `internal/proxy/logstream.go`, `internal/detector/detector.go`, `internal/detector/hash_detector.go`, `internal/detector/status.go`.

Pricing: `internal/pricing/calculator.go`, `internal/pricing/price_source.go`, `internal/pricing/load.go`, `internal/pricing/registry.go`, `internal/pricing/fetcher.go`, `internal/pricing/service.go`.

Alerting: `internal/alerting/alerting.go`, `internal/alerting/config.go`, `internal/alerting/webhook.go`, `internal/alerting/slack.go`, `internal/alerting/manager.go`, `internal/alerting/budget.go`.

Auth/DB/stream/settings: `internal/auth/auth.go`, `internal/auth/oauth.go`, `internal/auth/middleware.go`, `internal/auth/config.go`, `internal/db/sqlite.go`, `internal/db/users.go`, `internal/db/pricing.go`, `internal/db/settings.go`, `internal/db/alert_config.go`, `internal/db/reports.go`, `internal/logstream/hub.go`, `internal/logstream/resolver.go`, `internal/settings/service.go`, `internal/api/metrics.go`, `internal/api/logs.go`, `internal/api/embed.go`, `internal/api/login.go`, `internal/wrapper/*`, `cmd/proxy/main.go`, `cmd/finops-run/main.go`, plus `go.mod`, `Dockerfile`, `docker-compose.yml`, `.env.example`, `CLAUDE.md`.

---

*End of plan. The authoritative feature gate is the `ce-feature-gate` skill matrix quoted in §3.*
