#!/usr/bin/env bash
#
# End-to-end smoke test for gorge-webhook, run against an already-running
# instance. It starts nothing and cleans up nothing.
#
# Unlike every other script in this directory, this one cannot exercise what
# the service is for. gorge-webhook's work is a background loop draining a
# queue Phorge writes; no request starts a delivery, and there is no endpoint
# that reports one. So what follows checks the two read-only status endpoints,
# the probes, and the one invariant that spans both endpoints — and the
# delivery itself is checked in go/internal/webhook/dispatcher_test.go, where a
# test can hold the clock still and read the exact bytes that go out.
#
#   GORGE_WEBHOOK_MYSQL_HOST=127.0.0.1 GORGE_WEBHOOK_MYSQL_USER=root \
#   GORGE_WEBHOOK_MYSQL_PASS=... GORGE_WEBHOOK_NAMESPACE=phorge \
#     make run SERVICE=gorge-webhook
#   bash tests/e2e/webhook.sh
#
#   BASE_URL=http://127.0.0.1:8160 TOKEN=dev bash tests/e2e/webhook.sh
#
# It needs a reachable {namespace}_herald database with Phorge's schema in it —
# there is no local backend to fall back to, because both endpoints count rows.
# An install with no hooks and an empty queue is fine: every assertion below is
# about shape and invariants rather than about particular numbers, so it passes
# against a fresh install and against a busy one.
#
# Exits non-zero on the first failed scenario.

set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8160}"
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

skip() {
  printf '%sSKIP%s %s\n' "$C_DIM" "$C_OFF" "$1"
  if [ -n "${2:-}" ]; then
    printf '%s     %s%s\n' "$C_DIM" "$2" "$C_OFF"
  fi
}

# request METHOD PATH [TOKEN_HEADER_VALUE]
#
# Writes the response body to $RESP_BODY and the status code to $RESP_STATUS.
# An unset third argument means "send no token header at all", which is
# distinct from sending an empty one. Nothing here sends a body: the API is
# read-only.
request() {
  local method="$1" path="$2" token="${3-__unset__}"
  local args=(-sS --max-time "$CURL_TIMEOUT" -o "$tmp_body" -w '%{http_code}'
              -X "$method" "${BASE_URL}${path}")

  if [ "$token" != "__unset__" ]; then
    args+=(-H "X-Service-Token: ${token}")
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

# field NAME
#
# Pulls one integer out of $RESP_BODY. These responses are four fields deep at
# most and jq is not a dependency of this suite, so a match is enough.
field() {
  printf '%s' "$RESP_BODY" \
    | tr -d ' \n' \
    | sed -n "s/.*\"$1\":\(-\{0,1\}[0-9]\{1,\}\).*/\1/p"
}

tmp_body="$(mktemp)"
tmp_err="$(mktemp)"
trap 'rm -f "$tmp_body" "$tmp_err"' EXIT

printf 'gorge-webhook e2e\n'
printf '  base url : %s\n' "$BASE_URL"
if [ -n "$TOKEN" ]; then
  printf '  token    : (set)\n'
else
  printf '  token    : (empty — auth disabled, the 401 scenario is skipped)\n'
fi
printf '  %snote    : delivery itself is not reachable from here; see this script'"'"'s header%s\n\n' \
  "$C_DIM" "$C_OFF"

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
#
# The gap between the two probes is wider here than anywhere else in the
# repository. A gorge-webhook that cannot reach its database is listening,
# answers /healthz with a cheerful 200, and delivers absolutely nothing —
# and nothing fails anywhere, because Phorge keeps queueing rows and they
# simply sit there. Container healthchecks belong on this probe, not the one
# above.
request GET /readyz
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"status"'*'"ok"'* ]]; then
  pass 'GET /readyz reports a reachable herald database'
else
  fail 'GET /readyz' "status=${RESP_STATUS} body=${RESP_BODY} (the {namespace}_herald database must be reachable, see the header of this script)"
fi

# --- 3. unauthorized --------------------------------------------------------
#
# The guard is not protecting a mutation — everything here is read-only — but
# the queue's shape. How many deliveries are backing up, how many are failing
# and how many hooks an install has are exactly the numbers worth knowing
# before attacking one of their endpoints.
if [ -n "$TOKEN" ]; then
  request GET /api/webhook/stats
  if [ "$RESP_STATUS" = '401' ] && [[ "$RESP_BODY" == *'ERR_UNAUTHORIZED'* ]]; then
    pass 'GET /api/webhook/stats without a token gives 401 ERR_UNAUTHORIZED'
  else
    fail 'GET /api/webhook/stats without a token' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  skip 'GET /api/webhook/stats without a token' 'set TOKEN to exercise auth'
fi

# --- 4. queue state ---------------------------------------------------------
#
# All four fields, every time. A Phorge setup check reads them positionally in
# the sense that it renders each one under its own label, so a field that
# silently stopped being serialised would render as an empty cell rather than
# as an error.
request GET /api/webhook/stats "$TOKEN"
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"data"'* ]]; then
  fail 'GET /api/webhook/stats' "status=${RESP_STATUS} body=${RESP_BODY}"
else
  missing=''
  for f in queuedCount sentCount failedCount activeWebhooks; do
    if [ -z "$(field "$f")" ]; then
      missing="${missing} ${f}"
    fi
  done
  if [ -n "$missing" ]; then
    fail 'GET /api/webhook/stats' "missing or non-numeric:${missing} — body=${RESP_BODY}"
  else
    pass 'GET /api/webhook/stats reports all four counts'
  fi
fi

queued="$(field queuedCount)"
active="$(field activeWebhooks)"

# --- 5. the counts are counts -----------------------------------------------
#
# COUNT(*) cannot go negative, so a negative here means a count was replaced by
# a sentinel somewhere — a -1 for "I could not tell", most likely — and a setup
# check would render it as a number of pending deliveries.
if [ -n "$queued" ] && [ "$queued" -ge 0 ] 2>/dev/null && \
   [ -n "$active" ] && [ "$active" -ge 0 ] 2>/dev/null; then
  pass 'the counts are non-negative'
else
  fail 'the counts are non-negative' "queuedCount=${queued} activeWebhooks=${active}"
fi

# --- 6. hook count ----------------------------------------------------------
request GET /api/webhook/hooks "$TOKEN"
if [ "$RESP_STATUS" != '200' ] || [ -z "$(field total)" ]; then
  fail 'GET /api/webhook/hooks' "status=${RESP_STATUS} body=${RESP_BODY}"
else
  pass 'GET /api/webhook/hooks reports the hook total'
fi
total="$(field total)"

# --- 7. the two endpoints agree ---------------------------------------------
#
# The reason there are two endpoints at all, and the one thing here no unit
# test can check: hooks.total counts every hook and stats.activeWebhooks counts
# the ones that are not disabled, so the second can never exceed the first.
# Both are separate queries against a live database, and an install where they
# disagree has had one of them written against the wrong table — the request
# table instead of the hook table is the easy mistake, and against a busy
# queue it produces plausible-looking numbers.
#
# Equal is normal: it means no hook is disabled.
if [ -z "$total" ] || [ -z "$active" ]; then
  skip 'activeWebhooks never exceeds hooks.total' 'a count above was not readable'
elif [ "$active" -le "$total" ]; then
  if [ "$total" -eq 0 ]; then
    pass 'activeWebhooks never exceeds hooks.total (no hooks configured)'
  else
    pass "activeWebhooks never exceeds hooks.total (${active} of ${total} enabled)"
  fi
else
  fail 'activeWebhooks never exceeds hooks.total' \
    "stats says ${active} active but only ${total} hooks exist; one of the two is counting the wrong table"
fi

# --- 8. no hook details on the wire -----------------------------------------
#
# /api/webhook/hooks is a count and not a list, deliberately: behind that
# number are each hook's URI and its HMAC key, and the key is what makes a
# delivery trustworthy. Phorge's own interface is where hooks are inspected, by
# a user whose permissions it can check.
if [[ "$RESP_BODY" == *'hmac'* ]] || [[ "$RESP_BODY" == *'Key'* ]] || [[ "$RESP_BODY" == *'URI'* ]] \
   || [[ "$RESP_BODY" == *'http'* ]]; then
  fail 'GET /api/webhook/hooks leaks hook details' \
    "a hook's URI or HMAC key must never appear here: ${RESP_BODY}"
else
  pass 'GET /api/webhook/hooks is a count, with no hook details'
fi

# --- 9. token via query param -----------------------------------------------
#
# The fallback for callers that cannot set headers, checked only when the
# header is absent. The PHP client uses the header; this path is for a browser
# or a runbook's curl, which for read-only status is most of how it is reached.
if [ -n "$TOKEN" ]; then
  request GET "/api/webhook/stats?token=${TOKEN}"
  if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"data"'* ]]; then
    pass 'GET /api/webhook/stats?token= is accepted'
  else
    fail 'GET /api/webhook/stats?token=' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  skip 'GET /api/webhook/stats?token=' 'set TOKEN to exercise auth'
fi

# --- 10. read-only ----------------------------------------------------------
#
# Nothing may queue, retry or cancel a delivery. Phorge owns the queue's
# contents — it writes the rows — and an endpoint that let a caller push one
# would be a second way into a table this service is only supposed to drain.
#
# The status is not asserted, only that it is not a success: under
# /api/webhook/** a documented path called with an unregistered method answers
# 404 rather than 405, because the group matches every method in order to
# authenticate them first.
readonly_ok=1
for method in POST PUT DELETE; do
  request "$method" /api/webhook/stats "$TOKEN"
  if [ "$RESP_STATUS" = '200' ]; then
    fail "${method} /api/webhook/stats" "answered 200; the API is read-only"
    readonly_ok=0
  fi
done
if [ "$readonly_ok" -eq 1 ]; then
  pass 'POST, PUT and DELETE on /api/webhook/stats are all refused'
fi

# --- 11. unknown path -------------------------------------------------------
#
# What a caller that appends a trailing slash or joins the base URL wrongly
# receives. It goes through the framework's own error handler rather than a
# handler of ours, and it has to come back in the envelope too, because that is
# what the PHP client parses before it can report anything useful.
request GET /api/webhook/nope "$TOKEN"
if [ "$RESP_STATUS" = '404' ] && [[ "$RESP_BODY" == *'ERR_NOT_FOUND'* ]]; then
  pass 'an unknown path under /api/webhook/ keeps the {data,error} envelope'
else
  fail 'GET /api/webhook/nope' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
