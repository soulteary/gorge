#!/usr/bin/env bash
#
# End-to-end smoke test for gorge-render, run against an already-running
# instance. It starts nothing and cleans up nothing.
#
#   docker compose -f deploy/compose/docker-compose.yml up -d
#   bash tests/e2e/render.sh
#
#   BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/render.sh
#
# Exits non-zero on the first failed scenario.

set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8140}"
TOKEN="${TOKEN:-}"
CURL_TIMEOUT="${CURL_TIMEOUT:-10}"

if [ -t 1 ]; then
  C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_RED=''; C_GREEN=''; C_DIM=''; C_OFF=''
fi

pass_count=0
fail_count=0

pass() {
  pass_count=$((pass_count + 1))
  printf '%sPASS%s %s\n' "$C_GREEN" "$C_OFF" "$1"
}

fail() {
  fail_count=$((fail_count + 1))
  printf '%sFAIL%s %s\n' "$C_RED" "$C_OFF" "$1"
  if [ -n "${2:-}" ]; then
    printf '%s     %s%s\n' "$C_DIM" "$2" "$C_OFF"
  fi
}

# request METHOD PATH [BODY] [TOKEN_HEADER_VALUE]
#
# Writes the response body to $RESP_BODY and the status code to $RESP_STATUS.
# An unset fourth argument means "send no token header at all", which is
# distinct from sending an empty one.
request() {
  local method="$1" path="$2" body="${3:-}" token="${4-__unset__}"
  local args=(-sS --max-time "$CURL_TIMEOUT" -o "$tmp_body" -w '%{http_code}'
              -X "$method" "${BASE_URL}${path}")

  if [ "$token" != "__unset__" ]; then
    args+=(-H "X-Service-Token: ${token}")
  fi
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' --data "$body")
  fi

  RESP_STATUS="$(curl "${args[@]}" 2>"$tmp_err")"
  local rc=$?
  RESP_BODY="$(cat "$tmp_body")"
  if [ $rc -ne 0 ]; then
    RESP_STATUS='000'
    RESP_BODY="$(cat "$tmp_err")"
  fi
  return 0
}

tmp_body="$(mktemp)"
tmp_err="$(mktemp)"
trap 'rm -f "$tmp_body" "$tmp_err"' EXIT

printf 'gorge-render e2e\n'
printf '  base url : %s\n' "$BASE_URL"
if [ -n "$TOKEN" ]; then
  printf '  token    : (set)\n\n'
else
  printf '  token    : (empty — auth disabled, the 401 scenario is skipped)\n\n'
fi

# --- 1. liveness ------------------------------------------------------------
request GET /healthz
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"status"'*'"ok"'* ]]; then
  fail 'GET /healthz' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" == *'"data"'* ]]; then
  # Probe payloads stay outside the {data,error} envelope on purpose.
  fail 'GET /healthz' "probe payload must not use the envelope: ${RESP_BODY}"
else
  pass 'GET /healthz returns a bare {"status":"ok"}'
fi

# --- 2. readiness -----------------------------------------------------------
request GET /readyz
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"status"'*'"ok"'* ]]; then
  pass 'GET /readyz reports ready'
else
  fail 'GET /readyz' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 3. unauthorized --------------------------------------------------------
if [ -n "$TOKEN" ]; then
  request POST /api/highlight/render '{"source":"x = 1","language":"python"}'
  if [ "$RESP_STATUS" = '401' ] && [[ "$RESP_BODY" == *'ERR_UNAUTHORIZED'* ]]; then
    pass 'POST /api/highlight/render without a token gives 401 ERR_UNAUTHORIZED'
  else
    fail 'POST /api/highlight/render without a token' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  printf '%sSKIP%s POST /api/highlight/render without a token (set TOKEN to exercise auth)\n' \
    "$C_DIM" "$C_OFF"
fi

# --- 4. render --------------------------------------------------------------
read -r -d '' render_body <<'JSON'
{"source":"def hello():\n    print(\"Hello\")\n    return 42\n","language":"python"}
JSON

request POST /api/highlight/render "$render_body" "$TOKEN"
if [ "$RESP_STATUS" != '200' ]; then
  fail 'POST /api/highlight/render' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" != *'"data"'* ]]; then
  fail 'POST /api/highlight/render' "response is missing the data envelope: ${RESP_BODY}"
else
  pass 'POST /api/highlight/render returns 200 with a data envelope'

  # Pygments-compatible CSS classes are the whole point of the service; check a
  # few rather than the exact HTML, which shifts with every Chroma release.
  # The quotes arrive backslash-escaped because html is a JSON string value.
  missing=()
  for cls in k nf nb mi; do
    [[ "$RESP_BODY" == *"class=\\\"${cls}\\\""* ]] || missing+=("$cls")
  done
  if [ ${#missing[@]} -eq 0 ]; then
    pass 'rendered HTML carries Pygments CSS classes (k, nf, nb, mi)'
  else
    fail 'rendered HTML carries Pygments CSS classes' "missing: ${missing[*]}"
  fi
fi

# --- 5. languages -----------------------------------------------------------
request GET /api/highlight/languages '' "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"data"'* ]] && [[ "$RESP_BODY" == *'"python"'* ]]; then
  pass 'GET /api/highlight/languages lists python'
else
  fail 'GET /api/highlight/languages' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
