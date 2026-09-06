#!/usr/bin/env bash
#
# End-to-end smoke test for gorge-render's diff domain, run against an
# already-running instance. It starts nothing and cleans up nothing.
#
#   docker compose -f deploy/compose/docker-compose.yml up -d
#   bash tests/e2e/diff.sh
#
#   BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/diff.sh
#
# The diff domain shares a port with the render domain, so this hits the same
# base URL as render.sh and only exercises /api/diff/*.
#
# Where render.sh checks for a handful of CSS classes because Chroma's exact
# markup is not a contract, this compares whole diffs: the output is parsed by
# ArcanistDiffParser, which reads the hunk header to place every line after it.
# Going over the wire is the point — the "\ No newline at end of file" marker
# contains a backslash, so it is the one part of the payload that a JSON
# escaping mistake would corrupt without any unit test noticing.
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

# expect_diff LABEL REQUEST_BODY EXPECTED_JSON_FRAGMENT
#
# The expected fragment is written as it appears in the raw response, so
# newlines are the two characters \n and the marker's backslash is doubled.
# Comparing the escaped form rather than a decoded one keeps this script free
# of a JSON parser and still catches an escaping regression.
expect_diff() {
  local label="$1" body="$2" want="$3"

  request POST /api/diff/generate "$body" "$TOKEN"
  if [ "$RESP_STATUS" != '200' ]; then
    fail "$label" "status=${RESP_STATUS} body=${RESP_BODY}"
  elif [[ "$RESP_BODY" != *"$want"* ]]; then
    fail "$label" "want ${want} in ${RESP_BODY}"
  else
    pass "$label"
  fi
}

tmp_body="$(mktemp)"
tmp_err="$(mktemp)"
trap 'rm -f "$tmp_body" "$tmp_err"' EXIT

printf 'gorge diff e2e\n'
printf '  base url : %s\n' "$BASE_URL"
if [ -n "$TOKEN" ]; then
  printf '  token    : (set)\n\n'
else
  printf '  token    : (empty — auth disabled, the 401 scenario is skipped)\n\n'
fi

# --- 1. unauthorized --------------------------------------------------------
if [ -n "$TOKEN" ]; then
  request POST /api/diff/generate '{"old":"a\n","new":"b\n"}'
  if [ "$RESP_STATUS" = '401' ] && [[ "$RESP_BODY" == *'ERR_UNAUTHORIZED'* ]]; then
    pass 'POST /api/diff/generate without a token gives 401 ERR_UNAUTHORIZED'
  else
    fail 'POST /api/diff/generate without a token' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  printf '%sSKIP%s POST /api/diff/generate without a token (set TOKEN to exercise auth)\n' \
    "$C_DIM" "$C_OFF"
fi

# --- 2. a changed line ------------------------------------------------------
expect_diff 'POST /api/diff/generate renders a changed line' \
  '{"old":"hello\nworld\n","new":"hello\ngopher\n","oldName":"a","newName":"b"}' \
  '"diff":"--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,2 @@\n hello\n-world\n+gopher\n"'

# --- 3. a one line side omits the count -------------------------------------
expect_diff 'a single line hunk header omits the count' \
  '{"old":"hello\n","new":"gopher\n","oldName":"a","newName":"b"}' \
  '"diff":"--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1 +1 @@\n-hello\n+gopher\n"'

# --- 4. an empty side starts at zero ----------------------------------------
expect_diff 'an empty old side is 0,0' \
  '{"old":"","new":"x\ny\n","oldName":"a","newName":"b"}' \
  '"diff":"--- a 9999-99-99\n+++ b 9999-99-99\n@@ -0,0 +1,2 @@\n+x\n+y\n"'

# --- 5. the no-newline marker survives the wire -----------------------------
expect_diff 'the no-newline marker survives JSON encoding' \
  '{"old":"a\nb","new":"a\nc","oldName":"a","newName":"b"}' \
  '"diff":"--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n"'

# --- 6. identical input -----------------------------------------------------
# Not what diff would print: PHP synthesises this one, so "a\nb\n" counts as
# three lines and the body ends with a context line holding a single space.
expect_diff 'identical input returns the synthesised changeless diff' \
  '{"old":"a\nb\n","new":"a\nb\n","oldName":"a","newName":"b"}' \
  '"diff":"--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,3 +1,3 @@\n a\n b\n \n","equal":true'

# --- 7. normalize -----------------------------------------------------------
request POST /api/diff/generate \
  '{"old":"hello world\n","new":"helloworld\n","normalize":true}' "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"equal":true'* ]]; then
  pass 'normalize compares without spaces and tabs'
else
  fail 'normalize compares without spaces and tabs' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 8. prose ---------------------------------------------------------------
request POST /api/diff/prose \
  '{"old":"the quick brown fox","new":"the slow brown fox"}' "$TOKEN"
if [ "$RESP_STATUS" != '200' ]; then
  fail 'POST /api/diff/prose' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" != *'{"type":"=","text":"the "}'* ]] \
  || [[ "$RESP_BODY" != *'{"type":"-","text":"quick"}'* ]] \
  || [[ "$RESP_BODY" != *'{"type":"+","text":"slow"}'* ]]; then
  fail 'POST /api/diff/prose' "unexpected segmentation: ${RESP_BODY}"
else
  pass 'POST /api/diff/prose isolates the changed word'
fi

# --- 9. malformed body ------------------------------------------------------
request POST /api/diff/generate '{"old":' "$TOKEN"
if [ "$RESP_STATUS" = '400' ] && [[ "$RESP_BODY" == *'ERR_BAD_REQUEST'* ]]; then
  pass 'a malformed body gives 400 ERR_BAD_REQUEST'
else
  fail 'a malformed body gives 400 ERR_BAD_REQUEST' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
