#!/usr/bin/env bash
# Enforce that the exported core (pkg/) imports only the Go standard library.
#
# `go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}'` prints exactly
# the non-stdlib packages in the transitive import graph of the named packages,
# so we can assert the zero-dependency rule precisely instead of grepping import
# paths by hand.
set -euo pipefail

CORE="./pkg/circuitbreaker ./pkg/proxy ./pkg/pricing ./pkg/logstream"

offenders="$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' $CORE \
  | grep -v '^github.com/adaleks/finops-proxy' || true)"

if [[ -n "$offenders" ]]; then
  echo "FAIL: pkg/ core imports third-party modules:" >&2
  echo "$offenders" >&2
  echo "Move the dependency into internal/ (e.g. internal/store) or into finops-proxy-ee." >&2
  exit 1
fi

echo "OK: pkg/ core is stdlib-only."
