#!/usr/bin/env bash
# Trigger all three dynamic loop detectors against a running finops-proxy.
# Usage:  bash test-dynamic.sh [base-url]
#   default base-url is http://localhost:8080
# Prereq: proxy must be started with -dynamic (or the individual flags).
set -u

BASE="${1:-http://localhost:8080}/v1/chat/completions"
H='Content-Type: application/json'

echo "== STRUCTURAL (same prompt, growing attempt: expect 429 on #4) =="
for i in 1 2 3 4; do
  code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE" -H "$H" -H 'X-FinOps-Agent-Id: s1' \
    -d "{\"model\":\"gpt-4o\",\"attempt\":$i,\"messages\":[{\"role\":\"user\",\"content\":\"deploy the fix\"}]}")
  echo "  attempt=$i -> $code"
done

echo "== TOOL-CYCLE (A,B,A,B,A: expect 429 on #5) =="
for spec in "A apple" "B banana" "A cherry" "B date" "A elderberry"; do
  set -- $spec; name=$1; word=$2
  code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE" -H "$H" -H 'X-FinOps-Agent-Id: t1' \
    -d "{\"model\":\"gpt-4o\",\"note\":\"$word\",\"messages\":[{\"role\":\"assistant\",\"tool_calls\":[{\"id\":\"x\",\"type\":\"function\",\"function\":{\"name\":\"$name\"}}]}]}")
  echo "  tool=$name -> $code"
done

echo "== VELOCITY (1MB body: expect 429 immediately) =="
python3 -c "print('{\"model\":\"gpt-4o\",\"messages\":[{\"role\":\"user\",\"content\":\"' + 'A'*1000000 + '\"}]}')" > /tmp/big.json
code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE" -H "$H" -H 'X-FinOps-Agent-Id: v1' -d @/tmp/big.json)
echo "  big-body -> $code"
