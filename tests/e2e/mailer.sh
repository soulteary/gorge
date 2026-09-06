#!/usr/bin/env bash
#
# End-to-end smoke test for gorge-mailer, run against an already-running
# instance. It starts nothing and cleans up nothing.
#
# The instance has to have at least one backend configured, or the readiness
# scenario fails by design. The "test" backend accepts every message and
# delivers none, which is what makes an assertion about the send path possible
# without an SMTP server:
#
#   GORGE_MAILER_CONFIG='[{"key":"test","type":"test"}]' \
#     make run SERVICE=gorge-mailer
#   bash tests/e2e/mailer.sh
#
#   BASE_URL=http://127.0.0.1:8110 TOKEN=dev bash tests/e2e/mailer.sh
#
# Exits non-zero on the first failed scenario.

set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8110}"
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

printf 'gorge-mailer e2e\n'
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
#
# This is the one probe that carries information here: a mailer with no backend
# configured is listening and useless, and /healthz cannot tell you that.
request GET /readyz
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"status"'*'"ok"'* ]]; then
  pass 'GET /readyz reports at least one configured backend'
else
  fail 'GET /readyz' "status=${RESP_STATUS} body=${RESP_BODY} (configure a backend, see the header of this script)"
fi

# --- 3. unauthorized --------------------------------------------------------
if [ -n "$TOKEN" ]; then
  request GET /api/mailer/mailers
  if [ "$RESP_STATUS" = '401' ] && [[ "$RESP_BODY" == *'ERR_UNAUTHORIZED'* ]]; then
    pass 'GET /api/mailer/mailers without a token gives 401 ERR_UNAUTHORIZED'
  else
    fail 'GET /api/mailer/mailers without a token' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  printf '%sSKIP%s GET /api/mailer/mailers without a token (set TOKEN to exercise auth)\n' \
    "$C_DIM" "$C_OFF"
fi

# --- 4. mailer list ---------------------------------------------------------
request GET /api/mailer/mailers '' "$TOKEN"
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"data"'* ]]; then
  fail 'GET /api/mailer/mailers' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" != *'"key"'* ]] || [[ "$RESP_BODY" != *'"type"'* ]]; then
  fail 'GET /api/mailer/mailers' "an entry must carry key and type: ${RESP_BODY}"
else
  pass 'GET /api/mailer/mailers lists the configured backends'
fi

# --- 5. send ----------------------------------------------------------------
#
# data.mailerKey is the assertion that matters: it names the backend that
# actually accepted the message, which is what Phorge records against the sent
# mail. A 200 alone would not distinguish "delivered" from "accepted by
# nothing".
read -r -d '' send_body <<'JSON'
{"message":{"from":{"name":"Gorge e2e","address":"noreply@example.com"},
"to":[{"address":"rcpt@example.com"}],
"subject":"gorge e2e","textBody":"Hello from tests/e2e/mailer.sh",
"headers":[{"name":"X-Phabricator-Sent-This-Message","value":"Yes"}]}}
JSON

request POST /api/mailer/send "$send_body" "$TOKEN"
if [ "$RESP_STATUS" != '200' ]; then
  fail 'POST /api/mailer/send' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" != *'"mailerKey"'* ]]; then
  fail 'POST /api/mailer/send' "response is missing data.mailerKey: ${RESP_BODY}"
elif [[ "$RESP_BODY" == *'"error"'* ]]; then
  fail 'POST /api/mailer/send' "a success must carry no error: ${RESP_BODY}"
else
  pass 'POST /api/mailer/send returns 200 and reports data.mailerKey'
fi

# --- 6. attachments ---------------------------------------------------------
#
# Attachments arrive base64 inside the JSON body, which is why this service
# raises the transport limit to 10M. A rejection here is usually that limit
# having been left at the platform default.
read -r -d '' attachment_body <<'JSON'
{"message":{"from":{"address":"noreply@example.com"},
"to":[{"address":"rcpt@example.com"}],
"subject":"gorge e2e attachment","textBody":"See attached",
"attachments":[{"filename":"note.txt","mimeType":"text/plain","data":"SGVsbG8sIGdvcmdlIQ=="}]}}
JSON

request POST /api/mailer/send "$attachment_body" "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"mailerKey"'* ]]; then
  pass 'POST /api/mailer/send accepts a base64 attachment'
else
  fail 'POST /api/mailer/send with an attachment' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 7. incomplete message --------------------------------------------------
#
# 400, not 422: nothing judged this message undeliverable, it never reached a
# backend. Phorge branches on the difference.
request POST /api/mailer/send \
  '{"message":{"from":{"address":"noreply@example.com"},"subject":"nobody"}}' "$TOKEN"
if [ "$RESP_STATUS" = '400' ] && [[ "$RESP_BODY" == *'ERR_BAD_REQUEST'* ]]; then
  pass 'POST /api/mailer/send with no recipient gives 400 ERR_BAD_REQUEST'
else
  fail 'POST /api/mailer/send with no recipient' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 8. unknown mailer key --------------------------------------------------
request POST /api/mailer/send \
  '{"message":{"from":{"address":"a@example.com"},"to":[{"address":"b@example.com"}],"subject":"x"},"mailerKeys":["no-such-mailer"]}' \
  "$TOKEN"
if [ "$RESP_STATUS" = '502' ] && [[ "$RESP_BODY" == *'ERR_SEND_FAILED'* ]]; then
  pass 'POST /api/mailer/send with an unknown mailerKey gives 502 ERR_SEND_FAILED'
else
  fail 'POST /api/mailer/send with an unknown mailerKey' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
