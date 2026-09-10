#!/usr/bin/env bash
# Test ALL finops-proxy protections against one running proxy.
#
# Prereq (for everything to work): start the proxy with
#   ./bin/finops-proxy -upstream http://localhost:8081 -pricing config/pricing.json \
#       -dynamic -keys keys.example.json
#
#   Loop tests use key "sk-loop"  (budget 0 = unlimited, so the budget gate doesn't interfere).
#   Budget test uses key "sk-budget" (daily limit 1 µUSD).
#
# Usage: bash test-all.sh [base-url]     (default http://localhost:8080)
set -u

BASE="${1:-http://localhost:8080}"
B="$BASE/v1/chat/completions"
H='Content-Type: application/json'
LOOP_KEY='X-FinOps-API-Key: sk-loop'
BUDGET_KEY='X-FinOps-API-Key: sk-budget'

# post <agent-id> <api-key-header> <json>  ->  prints the HTTP status code
post() {
  curl -s -o /dev/null -w "%{http_code}" -X POST "$B" \
    -H "$H" -H "X-FinOps-Agent-Id: $1" -H "$2" -d "$3"
}

echo "== 1) EXACT-HASH LOOP (always enabled) =="
echo "  req#1 -> $(post e1 "$LOOP_KEY" '{"model":"gpt-4o","messages":[{"role":"user","content":"identical"}]}')  (expect 200)"
echo "  req#2 -> $(post e1 "$LOOP_KEY" '{"model":"gpt-4o","messages":[{"role":"user","content":"identical"}]}')  (expect 429)"

echo ""
echo "== 2) STRUCTURAL (near-duplicate, growing attempt) =="
for i in 1 2 3 4; do
  echo "  attempt=$i -> $(post s1 "$LOOP_KEY" "{\"model\":\"gpt-4o\",\"attempt\":$i,\"messages\":[{\"role\":\"user\",\"content\":\"deploy the fix\"}]}")"
done

echo ""
echo "== 3) TOOL-CYCLE (A,B,A,B,A) =="
for spec in "A apple" "B banana" "A cherry" "B date" "A elderberry"; do
  set -- $spec; n=$1; w=$2
  echo "  tool=$n -> $(post t1 "$LOOP_KEY" "{\"model\":\"gpt-4o\",\"note\":\"$w\",\"messages\":[{\"role\":\"assistant\",\"tool_calls\":[{\"id\":\"x\",\"type\":\"function\",\"function\":{\"name\":\"$n\"}}]}]}")"
done

echo ""
echo "== 4) VELOCITY (1MB body) =="
python3 -c "print('{\"model\":\"gpt-4o\",\"messages\":[{\"role\":\"user\",\"content\":\"' + 'A'*1000000 + '\"}]}')" > /tmp/big.json
echo "  big-body -> $(curl -s -o /dev/null -w "%{http_code}" -X POST "$B" -H "$H" -H 'X-FinOps-Agent-Id: v1' -H "$LOOP_KEY" -d @/tmp/big.json)  (expect 429)"

echo ""
echo "== 5) PER-KEY BUDGET (sk-budget, daily limit 1 µUSD) =="
echo "  req#1 -> $(post b1 "$BUDGET_KEY" '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}')  (expect 200)"
echo "  req#2 -> $(post b1 "$BUDGET_KEY" '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}')  (expect 429 budget_exceeded)"

echo ""
echo "Note: the global daily budget (-daily-budget-micro) is advisory — it raises a"
echo "budget_level alert in the proxy log, not a hard 429."
