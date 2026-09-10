# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Circuit-breaker reverse proxy (`/v1/chat/completions`) for OpenAI, Anthropic, DeepSeek, and Ollama.
- Exact-hash loop detection (SHA-256 sliding window) with per-agent scoping and `429 agent_loop_exception`.
- Deterministic non-LLM dynamic detectors: structural MinHash, tool-cycle, and token-velocity.
- Budget enforcement: per-key monthly/daily and global daily caps in micro-dollars (µUSD).
- Storage backends: in-memory (default) and SQLite (pure-Go, no CGO).
- JSON-lines logging to stdout.
- Embedded developer observability: `/v1/metrics` and the read-only `/dashboard`.
- `finops-run` CLI wrapper for loop detection plus kill/restart recovery of a child agent.
- Mock LLM upstream (`cmd/mockserver`) and Docker Compose quickstart.
- CI (`ci.yml`), zero-dependency gate, and GoReleaser release automation.

[Unreleased]: https://github.com/adaleks/finops-proxy/commits/main
