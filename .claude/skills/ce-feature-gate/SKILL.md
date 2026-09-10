# Skill: CE Feature Gate Matrix

Enforce the following feature matrix when decoupling code for the open-source `finops-proxy` core:

### ALLOWED IN PUBLIC CORE (`finops-proxy` / Apache 2.0):
- In-line Reverse Proxy Engine (`/v1/chat/completions`) for OpenAI, Anthropic, DeepSeek, and Ollama.
- Loop Detection: Exact Hash Matching, plus deterministic non-LLM heuristics (structural near-duplicate, tool-cycle, token-velocity). Embeddings are NOT core.
- Budget Controls: Global and Per-API-Key monthly/daily caps.
- Storage: In-Memory / SQLite driver support.
- Logging: Standardized JSON output to stdout/console.
- Local Developer Observability: in-process `/v1/metrics` JSON endpoint and a single-node, read-only, unauthenticated embedded `/dashboard`. No exporters.

### EXCLUSIVE TO ENTERPRISE EDITION (`finops-proxy-ee`):
- Multi-Tenant Hierarchy (Company -> Department -> Team -> User).
- Enterprise SSO/Auth (SAML 2.0, OAuth2, LDAP, Active Directory).
- Semantic / Vector-based Loop Detection (Embeddings similarity).
- Enterprise Data Drivers (PostgreSQL, ClickHouse, OpenTelemetry exporters).
- Real-Time Webhook Alerting (Slack / Microsoft Teams integration).
- Multi-Tenant Dashboard / Control Plane (session auth, RBAC, i18n, PDF reports).

### Boundary rule of thumb
- **Core:** deterministic, stdlib-only, single-node, offline, <1ms hot path.
- **EE:** embeddings/semantic similarity, multi-tenant hierarchy, SaaS SDKs, external exporters (OTel), webhooks, SSO.
