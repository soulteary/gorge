#!/usr/bin/env bash
#
# End-to-end smoke test for gorge-notification, run against an already-running
# instance. It starts nothing and cleans up nothing.
#
#   make build SERVICE=gorge-notification && ./bin/gorge-notification &
#   bash tests/e2e/notification.sh
#
#   ADMIN_URL=http://127.0.0.1:22281 CLIENT_URL=http://127.0.0.1:22280 \
#     bash tests/e2e/notification.sh
#
# Two base URLs rather than the single BASE_URL render.sh and diff.sh share:
# this service is one process on two listeners, and they are not
# interchangeable. Phorge posts to the admin port and browsers connect to the
# client port; the same request answers differently on each, which is the whole
# point of scenarios 4 and 5 below.
#
# There is no TOKEN. This domain has no auth, because Phorge's notification
# client sends no credentials — see compat/phorge/README.md.
#
# Exits non-zero on the first failed scenario.

set -uo pipefail

ADMIN_URL="${ADMIN_URL:-http://127.0.0.1:22281}"
CLIENT_URL="${CLIENT_URL:-http://127.0.0.1:22280}"
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

# request BASE METHOD PATH [BODY] [CONTENT_TYPE]
#
# Writes the response body to $RESP_BODY and the status code to $RESP_STATUS.
# The base URL is the first argument because every scenario has to say which
# listener it is talking to.
#
# CONTENT_TYPE defaults to the form-urlencoded label curl sends by default,
# which is exactly what Phorge's HTTPSFuture puts on a JSON payload; scenario 3
# depends on that being the default here too.
request() {
  local base="$1" method="$2" path="$3" body="${4:-}" ctype="${5:-}"
  local args=(-sS --max-time "$CURL_TIMEOUT" -o "$tmp_body" -w '%{http_code}'
              -X "$method" "${base}${path}")

  if [ -n "$body" ]; then
    args+=(--data "$body")
    if [ -n "$ctype" ]; then
      args+=(-H "Content-Type: ${ctype}")
    fi
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
tmp_head="$(mktemp)"
trap 'rm -f "$tmp_body" "$tmp_err" "$tmp_head"' EXIT

printf 'gorge-notification e2e\n'
printf '  admin url  : %s\n' "$ADMIN_URL"
printf '  client url : %s\n\n' "$CLIENT_URL"

# --- 1. liveness (admin port) -----------------------------------------------
request "$ADMIN_URL" GET /healthz
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"status"'*'"ok"'* ]]; then
  fail 'GET /healthz on the admin port' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" == *'"data"'* ]]; then
  # Probe payloads stay outside the {data,error} envelope on purpose.
  fail 'GET /healthz on the admin port' "probe payload must not use the envelope: ${RESP_BODY}"
else
  pass 'GET /healthz on the admin port returns a bare {"status":"ok"}'
fi

# --- 2. publish a message ---------------------------------------------------
# Sent with curl's default Content-Type, i.e. application/x-www-form-urlencoded
# on a body that is really JSON. That mislabelling is what Phorge does, and the
# handler has to decode the body regardless of the header rather than binding on
# it, which would form-parse the payload.
# tests/contract/notification/admin/post-form-content-type.json pins the same
# thing at the fixture level, with a payload chosen so that form-parsing fails.
# The percent sign in the payload is deliberate: it is an invalid escape
# sequence to a form parser, so this body only survives a handler that treats
# the bytes as JSON.
read -r -d '' message_body <<'JSON'
{"type":"message","data":{"type":"notification","title":"build 100% done"},"subscribers":[]}
JSON

request "$ADMIN_URL" POST / "$message_body"
if [ "$RESP_STATUS" != '200' ]; then
  fail 'POST / on the admin port' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" != *'"fingerprint"'* ]]; then
  fail 'POST / on the admin port' "response is missing the fingerprint: ${RESP_BODY}"
elif [[ "$RESP_BODY" == *'"data"'* ]]; then
  # The receipt must stay bare: Phorge indexes "fingerprint" off the top level.
  fail 'POST / on the admin port' "the receipt must not use the envelope: ${RESP_BODY}"
else
  pass 'POST / with a form-urlencoded label returns a bare fingerprint'
fi

# --- 3. status --------------------------------------------------------------
request "$ADMIN_URL" GET /status/
if [ "$RESP_STATUS" != '200' ]; then
  fail 'GET /status/ on the admin port' "status=${RESP_STATUS} body=${RESP_BODY}"
else
  # The dots in these key names are literal. Phorge's cluster panel reads them
  # with idx($details, 'clients.active'), so nesting them would leave it blank.
  missing=()
  for key in 'instance' 'clients.active' 'clients.total' 'messages.in' \
             'messages.out' 'history.size' 'version'; do
    [[ "$RESP_BODY" == *"\"${key}\""* ]] || missing+=("$key")
  done
  # Scenario 2 published at least one message into this process. Read the
  # counter rather than matching a literal 1, so running the script twice
  # against the same instance does not fail the second time.
  messages_in="$(printf '%s' "$RESP_BODY" | sed -n 's/.*"messages\.in":\([0-9]*\).*/\1/p')"

  if [ ${#missing[@]} -ne 0 ]; then
    fail 'GET /status/ reports the flat dotted keys' "missing: ${missing[*]}"
  elif [[ "$RESP_BODY" == *'"data"'* ]]; then
    fail 'GET /status/ on the admin port' "the status must not use the envelope: ${RESP_BODY}"
  elif [ "${messages_in:-0}" -lt 1 ]; then
    fail 'GET /status/ counts the published message' "messages.in=${messages_in:-<unset>} body=${RESP_BODY}"
  else
    pass 'GET /status/ returns a flat map with literal dotted keys'
  fi
fi

# --- 4. the client port refuses plain HTTP ----------------------------------
# 501 is the healthy answer, not a failure: testClient() reads anything else,
# 200 included, as a broken server.
request "$CLIENT_URL" GET /
if [ "$RESP_STATUS" = '501' ] && [[ "$RESP_BODY" == *'Use Websockets'* ]]; then
  pass 'GET / on the client port returns 501 Use Websockets'
else
  fail 'GET / on the client port' "expected 501 with Aphlict's body, got status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 5. the client port upgrades a real handshake ---------------------------
# The one scenario no unit test can reach: httptest's recorder does not
# implement http.Hijacker, so the 101 only happens on a real listener.
#
# curl has no WebSocket client here, so the handshake headers are sent by hand.
# The key is the example nonce from RFC 6455 — any valid base64 will do, the
# server only echoes it back. curl then holds the upgraded connection open with
# nothing to read, so it is expected to hit --max-time; the status line has
# already arrived by then, which is why the headers are read from a file rather
# than from curl's exit code.
curl -sS -i --max-time 3 \
  -H 'Connection: Upgrade' \
  -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' \
  -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  "${CLIENT_URL}/" >"$tmp_head" 2>/dev/null
handshake="$(head -n 1 "$tmp_head")"
if [[ "$handshake" == *'101'* ]] && grep -qi '^upgrade: *websocket' "$tmp_head"; then
  pass 'GET / with an Upgrade header switches protocols (101)'
else
  fail 'GET / with an Upgrade header' "expected 101, got: ${handshake:-<no response>}"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
