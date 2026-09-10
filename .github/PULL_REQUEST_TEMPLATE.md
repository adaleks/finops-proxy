<!-- Thank you. Please confirm the checklist before requesting review. -->

## Summary
What and why, in a few sentences. Link the issue: `Closes #123`.

## Type of change
- [ ] Bug fix
- [ ] New feature / detector
- [ ] Performance
- [ ] Docs / CI / tooling
- [ ] Refactor (no behavior change)

## Checklist
- [ ] `go build ./...` passes
- [ ] `go test ./...` passes (and `go test -race ./...` where concurrency is touched)
- [ ] New hot-path code is benchmarked and adds no new allocations (or justifies them)
- [ ] No new non-stdlib import under `pkg/`
- [ ] Errors are wrapped and handlers respect context cancellation
- [ ] Feature-gate check: this belongs in the core, not EE

## Benchmark (if performance-relevant)
| Before | After | Δ |
|---|---|---|
| `X ns/op · Y allocs` | `X' ns/op · Y' allocs` | … |
