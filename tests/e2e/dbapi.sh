#!/usr/bin/env bash
#
# End-to-end smoke test for gorge-db-api, run against an already-running
# instance. It starts nothing and cleans up nothing.
#
# This service holds no state of its own: every answer is a live query against
# the MySQL cluster it is pointed at. So unlike a queue or a store, there is
# nothing to seed — what follows checks the probes, the token guard, and the
# shape of the seven read endpoints as PhabricatorGorgeDBClient parses them.
# The endpoints that must open a connection are exercised only for envelope
# shape and for the one invariant this suite can enforce against any install:
# a failure past the token check never leaks the host, database, query or
# password. The per-check MySQL algorithms (version gate, savepoint naming,
# charset choice) are pinned in go/internal/dbapi/*_test.go, where a test can
# drive an exact server state a live install cannot be made to hold.
#
#   GORGE_DB_MYSQL_HOST=127.0.0.1 GORGE_DB_MYSQL_USER=root \
#   GORGE_DB_MYSQL_PASS=... GORGE_DB_NAMESPACE=phorge \
#   GORGE_SERVICE_TOKEN=dev \
#     make run SERVICE=gorge-db-api
#   BASE_URL=http://127.0.0.1:8080 TOKEN=dev bash tests/e2e/dbapi.sh
#
# It does not need a reachable database: /api/db/servers reports an unreachable
# node in-band rather than failing, so every shape assertion below passes
# whether or not MySQL is up. The scenarios that require a reachable cluster
# announce that they were skipped rather than failing when it is down.
#
# Exits non-zero on the first failed scenario.

set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
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

# no_leak LABEL
#
# Fails LABEL if the current $RESP_BODY carries anything a caller past the
# token check must not see: the configured password, a fragment of SQL, or an
# INFORMATION_SCHEMA reference. The host and port (the refKey) are deliberately
# not checked — they are the public identifier the health report is built to
# hand out.
no_leak() {
  local label="$1"
  local pass="${GORGE_DB_MYSQL_PASS:-${MYSQL_PASS:-}}"
  for needle in 'SELECT ' 'INFORMATION_SCHEMA' 'patch_status' "$pass"; do
    [ -z "$needle" ] && continue
    if [[ "$RESP_BODY" == *"$needle"* ]]; then
      fail "$label leaks internal detail" "found '${needle}' in: ${RESP_BODY}"
      return 1
    fi
  done
  return 0
}

tmp_body="$(mktemp)"
tmp_err="$(mktemp)"
trap 'rm -f "$tmp_body" "$tmp_err"' EXIT

printf 'gorge-db-api e2e\n'
printf '  base url : %s\n' "$BASE_URL"
if [ -n "$TOKEN" ]; then
  printf '  token    : (set)\n'
else
  printf '  token    : (empty — auth disabled, the 401 scenario is skipped)\n'
fi
printf '  %snote    : the MySQL algorithms are pinned in go/internal/dbapi/*_test.go%s\n\n' \
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
# Liveness answers while the process is listening; readiness answers whether a
# master can be pinged. A db-api whose masters are all down is listening but
# not ready, and it says so with a 503 carrying a reason — never the envelope.
request GET /readyz
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"status"'*'"ok"'* ]]; then
  pass 'GET /readyz reports a reachable master'
elif [ "$RESP_STATUS" = '503' ] && [[ "$RESP_BODY" == *'"reason"'* ]]; then
  pass 'GET /readyz reports 503 with a reason (no master reachable)'
else
  fail 'GET /readyz' "status=${RESP_STATUS} body=${RESP_BODY}"
fi
if [[ "$RESP_BODY" == *'"data"'* ]]; then
  fail 'GET /readyz' "probe payload must not use the envelope: ${RESP_BODY}"
fi

# --- 3. unauthorized --------------------------------------------------------
#
# The token is checked by the group middleware before the path is resolved or
# a connection is opened, so what the guard protects is the cluster's shape:
# which servers exist, how they replicate, how far their migrations have run.
if [ -n "$TOKEN" ]; then
  request GET /api/db/servers
  if [ "$RESP_STATUS" = '401' ] && [[ "$RESP_BODY" == *'ERR_UNAUTHORIZED'* ]]; then
    pass 'GET /api/db/servers without a token gives 401 ERR_UNAUTHORIZED'
  else
    fail 'GET /api/db/servers without a token' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  skip 'GET /api/db/servers without a token' 'set TOKEN to exercise auth'
fi

# --- 4. the cluster's shape -------------------------------------------------
#
# /api/db/servers never fails on a dead node — an unreachable server is in the
# array with connectionStatus "fail" — so this is a 200 with a data array
# whether or not MySQL is up, and every configured node carries a refKey and a
# connectionStatus.
request GET /api/db/servers "$TOKEN"
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"data"'* ]]; then
  fail 'GET /api/db/servers' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" != *'"refKey"'* ]] || [[ "$RESP_BODY" != *'"connectionStatus"'* ]]; then
  fail 'GET /api/db/servers' "a server record must carry refKey and connectionStatus: ${RESP_BODY}"
else
  pass 'GET /api/db/servers reports each node with a refKey and connectionStatus'
fi

# --- 5. no credential or SQL on the wire ------------------------------------
#
# The one invariant this suite enforces against any install: a caller past the
# token check gets the cluster's public identifiers (host:port) but never the
# password it connects with or the queries it runs.
if no_leak 'GET /api/db/servers'; then
  pass 'GET /api/db/servers carries no password or SQL'
fi

# --- 6. schema endpoints keep the envelope ----------------------------------
#
# schema-diff and charset-info must open a connection. When a master is
# reachable they answer 200 with data; when it is not they answer this domain's
# ERR_DB_UNREACHABLE at 503 with a generic message. Either way the body stays
# in the envelope and leaks nothing internal — that is what is asserted here,
# not the status, which depends on whether the cluster is up.
for path in /api/db/schema-diff /api/db/charset-info; do
  request GET "$path" "$TOKEN"
  if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"data"'* ]]; then
    if no_leak "GET ${path}"; then
      pass "GET ${path} answers 200 with data and no leak"
    fi
  elif [ "$RESP_STATUS" = '503' ] && [[ "$RESP_BODY" == *'ERR_DB_UNREACHABLE'* ]]; then
    if [[ "$RESP_BODY" == *'"data"'* ]]; then
      fail "GET ${path}" "an error response must carry no data: ${RESP_BODY}"
    elif no_leak "GET ${path}"; then
      pass "GET ${path} answers 503 ERR_DB_UNREACHABLE, generic and no leak"
    fi
  else
    fail "GET ${path}" "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
done

# --- 7. schema-issues / setup-issues / migrations ---------------------------
#
# schema-issues and setup-issues fold connection failures into in-band records,
# so both always answer a 200 array. migrations/status also answers 200 with
# initialized false when meta_data does not exist, but after a successful Ping
# a ledger permission or connection failure is an explicit 403/503.
for path in /api/db/schema-issues /api/db/setup-issues; do
  request GET "$path" "$TOKEN"
  if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"data"'* ]]; then
    if no_leak "GET ${path}"; then
      pass "GET ${path} answers a 200 data array"
    fi
  else
    fail "GET ${path}" "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
done

path=/api/db/migrations/status
request GET "$path" "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"data"'* ]]; then
  if no_leak "GET ${path}"; then
    pass "GET ${path} answers a 200 data array"
  fi
elif { [ "$RESP_STATUS" = '403' ] && [[ "$RESP_BODY" == *'ERR_DB_ACCESS_DENIED'* ]]; } ||
     { [ "$RESP_STATUS" = '503' ] && [[ "$RESP_BODY" == *'ERR_DB_UNREACHABLE'* ]]; }; then
  if [[ "$RESP_BODY" == *'"data"'* ]]; then
    fail "GET ${path}" "an error response must carry no data: ${RESP_BODY}"
  elif no_leak "GET ${path}"; then
    pass "GET ${path} reports its post-Ping ledger failure without leaking details"
  fi
else
  fail "GET ${path}" "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 8. one server by its ref key -------------------------------------------
#
# /api/db/servers/{ref}/health addresses one node by the refKey the array hands
# out, and an unknown key is a 404 rather than a database probe. Pull the first
# refKey out of the servers array to address a node that certainly exists.
request GET /api/db/servers "$TOKEN"
refkey="$(printf '%s' "$RESP_BODY" | tr -d ' \n' | sed -n 's/.*"refKey":"\([^"]*\)".*/\1/p')"
if [ -n "$refkey" ]; then
  request GET "/api/db/servers/${refkey}/health" "$TOKEN"
  if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *"\"refKey\":\"${refkey}\""* ]]; then
    pass "GET /api/db/servers/${refkey}/health returns that one node"
  else
    fail "GET /api/db/servers/${refkey}/health" "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  skip 'GET /api/db/servers/{ref}/health' 'no refKey was readable from /api/db/servers'
fi

request GET "/api/db/servers/nosuch-host:3306/health" "$TOKEN"
if [ "$RESP_STATUS" = '404' ] && [[ "$RESP_BODY" == *'ERR_NOT_FOUND'* ]]; then
  pass 'GET /api/db/servers/{unknown}/health is a 404 ERR_NOT_FOUND'
else
  fail 'GET /api/db/servers/{unknown}/health' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 9. token via query param -----------------------------------------------
#
# The fallback for callers that cannot set headers, checked only when the
# header is absent. The PHP client uses the header; this is for a browser or a
# runbook's curl.
if [ -n "$TOKEN" ]; then
  request GET "/api/db/servers?token=${TOKEN}"
  if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"data"'* ]]; then
    pass 'GET /api/db/servers?token= is accepted'
  else
    fail 'GET /api/db/servers?token=' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  skip 'GET /api/db/servers?token=' 'set TOKEN to exercise auth'
fi

# --- 10. read-only ----------------------------------------------------------
#
# Every endpoint is a read. A documented path called with a write method under
# /api/db/** answers a non-success, because the group matches every method in
# order to authenticate it first — the status is not asserted, only that it is
# not a 200.
readonly_ok=1
for method in POST PUT DELETE; do
  request "$method" /api/db/servers "$TOKEN"
  if [ "$RESP_STATUS" = '200' ]; then
    fail "${method} /api/db/servers" 'answered 200; the API is read-only'
    readonly_ok=0
  fi
done
if [ "$readonly_ok" -eq 1 ]; then
  pass 'POST, PUT and DELETE on /api/db/servers are all refused'
fi

# --- 11. unknown path -------------------------------------------------------
#
# A path the service does not serve goes through the framework's own error
# handler and has to come back in the {data,error} envelope too, because that
# is what the PHP client parses before it can report anything.
request GET /api/db/nope "$TOKEN"
if [ "$RESP_STATUS" = '404' ] && [[ "$RESP_BODY" == *'ERR_NOT_FOUND'* ]]; then
  pass 'an unknown path under /api/db/ keeps the {data,error} envelope'
else
  fail 'GET /api/db/nope' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
