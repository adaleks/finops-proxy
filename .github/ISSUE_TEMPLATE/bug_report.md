---
name: 🐛 Bug report
about: Something is broken or behaves unexpectedly
title: "[bug] "
labels: bug
assignees: ""
---

### Describe the bug
A clear, concise description of what is broken.

### To reproduce
Steps plus a minimal request body / command:

```bash
curl -s -X POST http://localhost:8080/v1/chat/completions \
  -H 'X-FinOps-API-Key: sk-loop' -H 'X-FinOps-Agent-Id: demo' \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

### Expected vs actual
What you expected, what happened (status code, error body).

### Environment
- finops-proxy version / commit: `finops-proxy -version`
- Go version: `go version`
- OS / arch:
- Flags used (`-upstream`, `-dynamic`, `-db`, …):

### Logs
Relevant JSON log lines (redact any keys/secret material).
