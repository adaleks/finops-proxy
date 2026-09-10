# Security Policy

finops-proxy is a circuit breaker that protects LLM infrastructure against
[OWASP LLM04 — Unbounded Consumption](https://genai.owasp.org/llmrisk/llm04-unbounded-consumption/).
Its own security boundary matters, and reports are taken seriously.

## Supported versions

The project has not yet tagged a release, so only the default branch is supported.

| Version | Supported |
|---|---|
| `main` / `master` (unreleased) | ✅ |
| released tags | — (none yet) |

## Reporting a vulnerability

**Do not open a public issue.** Report vulnerabilities privately so maintainers
can ship a fix before disclosure.

1. **GitHub private vulnerability reporting** (preferred): use the
   [Report a vulnerability](https://github.com/adaleks/finops-proxy/security/advisories/new)
   flow.
2. **Email:** `security@adaleks.com`.

Include as much as you can: affected version or commit, a minimal reproduction,
and the impact. Expect an initial response within 5 business days; you'll be kept
updated and credited in the advisory unless you ask not to be.

## Scope

**In scope:**

- Bypass or weakening of loop detection (exact-hash, structural MinHash, tool-cycle, velocity).
- Bypass of budget enforcement (per-key monthly/daily or global caps).
- Leakage of API keys or request data through the proxy, logs, or dashboard.
- Denial of service against the proxy itself.

**Out of scope:**

- Vulnerabilities in the upstream LLM provider.
- The enterprise edition (`finops-proxy-ee`) — report via its own channel.
- Missing features and non-security bugs — use the issue tracker.

## Security model

- **Fail-open on parse errors, fail-closed on enforcement.** An oversized body or
  read failure never wedges the proxy, but a detected loop or an exhausted budget
  answers `429` and is **never forwarded** upstream.
- **Raw API keys are never persisted.** Only the SHA-256 hash of each key is kept
  in memory.
- The embedded `/dashboard` and `/v1/metrics` are single-node, read-only, and
  **unauthenticated** — do not expose them to untrusted networks.
