#!/usr/bin/env bash
# Fail if `go list -m all` contains any module outside an allowlist of vetted,
# pure-Go dependency orgs. This is the "no SaaS / no CGO / no enterprise SDK"
# guard — NOT a literal "zero modules" claim. The optional SQLite store
# (internal/store/sqlite) uses GORM + a pure-Go driver, which is the reason any
# module exists here at all; the exported pkg/ core is stdlib-only (see
# check-deps.sh).
#
# The allowlist is prefix-based on purpose: it tolerates transitive version bumps
# within already-vetted orgs while still flagging any brand-new dependency
# (e.g. a new SaaS SDK) for human review.
set -euo pipefail

ALLOWED=(
  "github.com/dustin/"
  "github.com/glebarez/"
  "github.com/google/"
  "github.com/jinzhu/"
  "github.com/kballard/"
  "github.com/klauspost/"
  "github.com/mattn/"
  "github.com/remyoudompheng/"
  "golang.org/x/"
  "gorm.io/"
  "lukechampine.com/"
  "modernc.org/"
)

offenders=""
while read -r mod; do
  [[ -z "$mod" ]] && continue
  [[ "$mod" == "github.com/adaleks/finops-proxy" ]] && continue   # the root module itself
  allowed=0
  for p in "${ALLOWED[@]}"; do
    if [[ "$mod" == "$p"* ]]; then
      allowed=1
      break
    fi
  done
  if [[ "$allowed" != "1" ]]; then
    offenders+="$mod"$'\n'
  fi
done < <(go list -m all | awk '{print $1}')

if [[ -n "$offenders" ]]; then
  echo "FAIL: module graph contains non-allowlisted modules:" >&2
  printf '%s' "$offenders" >&2
  echo "Update .github/scripts/check-modules.sh ALLOWED if this is a deliberate, pure-Go addition." >&2
  exit 1
fi

echo "OK: module graph matches the allowlist."
