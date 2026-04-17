#!/bin/bash
# MarketFlow E2E Test Script — validates core flows after deployment.
# Usage: MF_API_KEY=<key> MF_TENANT_ID=<uuid> ./scripts/e2e-test.sh
set -euo pipefail

API="${MF_API_URL:-https://marketflowapi.codevertexitsolutions.com}"
AI="${MF_AI_URL:-https://marketflowai.codevertexitsolutions.com}"
UI="${MF_UI_URL:-https://marketflow.codevertexitsolutions.com}"
KEY="${MF_API_KEY:?MF_API_KEY required}"
TID="${MF_TENANT_ID:?MF_TENANT_ID required}"

PASS=0
FAIL=0

check() {
  local name="$1"
  local ok="$2"
  if [ "$ok" = "true" ]; then
    echo "  PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $name"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== MarketFlow E2E Tests ==="
echo "API: $API"
echo "AI:  $AI"
echo ""

# 1. Health checks
echo "--- Health Checks ---"
API_HEALTH=$(curl -sf "$API/healthz" | grep -c "ok" 2>/dev/null || echo 0)
check "API healthz" "$([ "$API_HEALTH" -gt 0 ] && echo true || echo false)"

AI_HEALTH=$(curl -sf "$AI/healthz" | grep -c "ok" 2>/dev/null || echo 0)
check "AI healthz" "$([ "$AI_HEALTH" -gt 0 ] && echo true || echo false)"

# 2. Knowledge base has entries
echo ""
echo "--- Knowledge Base ---"
KB=$(curl -sf "$API/api/v1/knowledge/entries?limit=1" \
  -H "X-API-Key: $KEY" -H "X-Tenant-ID: $TID" 2>/dev/null || echo '{"total":0}')
KB_TOTAL=$(echo "$KB" | python3 -c "import sys,json; print(json.load(sys.stdin).get('total',0))" 2>/dev/null || echo 0)
check "KB has entries (total=$KB_TOTAL)" "$([ "$KB_TOTAL" -gt 0 ] && echo true || echo false)"

# 3. AI chat responds
echo ""
echo "--- AI Chat ---"
CHAT=$(curl -sf -X POST "$AI/api/v1/chat" \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $KEY" -H "X-Tenant-ID: $TID" \
  -d '{"question":"What is C9?","stream":false}' 2>/dev/null || echo '{}')
CHAT_OK=$(echo "$CHAT" | python3 -c "import sys,json; d=json.load(sys.stdin); print('true' if d.get('result',{}).get('response','') else 'false')" 2>/dev/null || echo false)
check "AI chat returns response" "$CHAT_OK"

# 4. Public profile accessible
echo ""
echo "--- Public Profile ---"
PROFILE=$(curl -sf "$API/api/v1/public/profile/254200047984" 2>/dev/null || echo '{}')
PROFILE_OK=$(echo "$PROFILE" | python3 -c "import sys,json; print('true' if json.load(sys.stdin).get('name') else 'false')" 2>/dev/null || echo false)
check "Public profile loads" "$PROFILE_OK"

# 5. Integrations available endpoint
echo ""
echo "--- Integrations ---"
INTEGRATIONS=$(curl -sf "$API/api/v1/integrations/available" \
  -H "X-API-Key: $KEY" -H "X-Tenant-ID: $TID" 2>/dev/null || echo '{"available":[]}')
INTEG_OK=$(echo "$INTEGRATIONS" | python3 -c "import sys,json; print('true' if len(json.load(sys.stdin).get('available',[])) > 0 else 'false')" 2>/dev/null || echo false)
check "Available integrations returned" "$INTEG_OK"

# 6. Campaigns list
echo ""
echo "--- Campaigns ---"
CAMPAIGNS=$(curl -sf "$API/api/v1/campaigns" \
  -H "X-API-Key: $KEY" -H "X-Tenant-ID: $TID" 2>/dev/null || echo '[]')
CAMP_OK=$(echo "$CAMPAIGNS" | python3 -c "import sys,json; print('true')" 2>/dev/null || echo false)
check "Campaigns endpoint reachable" "$CAMP_OK"

# 7. UI public page
echo ""
echo "--- UI Public Page ---"
UI_PAGE=$(curl -sf "$UI/p/254200047984" 2>/dev/null | grep -c "MarketFlow" || echo 0)
check "UI public page renders" "$([ "$UI_PAGE" -gt 0 ] && echo true || echo false)"

# Summary
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] && exit 0 || exit 1
