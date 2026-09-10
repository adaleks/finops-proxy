# FinOps Proxy - Development Guidelines

## Project Overview
High-performance, Apache 2.0-licensed AI API Gateway & Circuit Breaker written in Go. Protects infrastructure against OWASP LLM04 (Unbounded Consumption/Loops) and enforces token budget caps.

## Core Commands
- **Build:** `go build -o bin/finops-proxy ./cmd/proxy`
- **Test All:** `go test -v ./...`
- **Test Package:** `go test -v ./pkg/circuitbreaker/...`
- **Run Locally:** `go run ./cmd/proxy`
- **Lint:** `golangci-lint run`

## Code Style & Architecture Guidelines
- **Go Version:** Go 1.22+
- **Architecture:** Clean Architecture. Strict separation between Proxy Routing (`pkg/proxy`), Circuit Breaker (`pkg/circuitbreaker`), and Token Pricing (`pkg/pricing`).
- **Zero Enterprise Imports in Core:** Code in `pkg/` MUST NEVER import enterprise modules, SAML drivers, or third-party SaaS SDKs.
- **Exported Core Interfaces:** Maintain clean Go interfaces so `finops-proxy-ee` can cleanly import `github.com/adaleks/finops-proxy` as a Go module dependency.
- **Error Handling:** Return wrapped errors (`fmt.Errorf("...: %w", err)`). Always respect HTTP context cancellation.
- **Storage Strategy:** Pure In-Memory or SQLite for core. External storage drivers (PostgreSQL/ClickHouse) belong exclusively to EE.
