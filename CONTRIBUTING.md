# Contributing to finops-proxy

Thanks for contributing. This document keeps the core dependency-free, fast, and safe to merge.

## The rules that cannot be bent

1. **Zero external dependencies in the core.** Code under `pkg/` — `pkg/circuitbreaker`,
   `pkg/proxy`, `pkg/pricing`, `pkg/logstream` — must import **only the Go standard library**.
   Enterprise modules, SAML drivers, and third-party SaaS SDKs belong in `finops-proxy-ee`, never
   here. CI enforces this with a `go list -deps` gate (`.github/scripts/check-deps.sh`).
2. **< 1 ms hot path.** Any middleware or detection logic must execute in under 1 ms. Benchmarks
   live in `pkg/circuitbreaker/pipeline_bench_test.go`; keep them green and report `ns/op` +
   `allocs/op` in PRs.
3. **Low GC pressure.** Use `sync.Pool` for hot-path buffers; no per-request heap churn.
4. **Offline first.** No calls to third-party APIs or embedding providers in the core.
5. **Interface isolation.** `pkg/circuitbreaker` and `pkg/proxy` depend strictly on the core
   interfaces (seams). The enterprise edition supplies real implementations — never fork the
   engine to add a feature.

## Feature gate

Deterministic, stdlib-only, single-node, offline features belong in the core. Embeddings /
semantic similarity, multi-tenant hierarchy, SSO, SaaS SDKs, exporters, and webhooks belong in
`finops-proxy-ee`. When in doubt, open an issue first — a `feature request` or `detector proposal`
template exists for exactly this.

## Getting started

```bash
go build -o bin/finops-proxy ./cmd/proxy
go test ./...                        # unit + integration
go test -race ./...                  # race detector
go test -bench=. -benchmem ./pkg/circuitbreaker/   # performance
golangci-lint run                    # lint
```

## Pull request checklist

- [ ] `go build ./...` and `go test ./...` pass.
- [ ] New hot-path code has a benchmark and adds **no new allocations** (or justifies them).
- [ ] New detectors ship with tests covering the false-positive and fail-open cases.
- [ ] No new non-stdlib import appears under `pkg/` (the CI gate catches this anyway).
- [ ] Errors are wrapped (`fmt.Errorf("...: %w", err)`) and HTTP handlers respect context
      cancellation.

## Commit & PR style

- One logical change per PR. Reference the issue it closes.
- Keep the diff focused; do not mix a refactor with a feature.
- Tests are expected with behavior changes.

## Releasing

Releasing is one command — everything else is automatic. To ship a version:

```bash
make release v=0.1.0
```

That tags the current commit and pushes the tag; CI then runs the tests, builds
`finops-proxy` and `finops-run` for linux/darwin/windows × amd64/arm64, generates
checksums, and publishes the GitHub Release with notes auto-generated from merged
pull requests.

Tags follow [Semantic Versioning](https://semver.org) with a `v` prefix
(`v0.1.0`, `v0.2.0`, …). Before 1.0, minor bumps may include breaking changes.

`CHANGELOG.md` is regenerated automatically from commit messages by git-cliff
(`cliff.toml` + `.github/workflows/changelog.yml`) each time a `v*` tag is pushed.
Use conventional commit prefixes (`feat:`, `fix:`, `docs:`, `ci:`, …) so the
entries are grouped meaningfully.

## License

By contributing, you agree your contribution is licensed under Apache 2.0 (see `LICENSE`).
