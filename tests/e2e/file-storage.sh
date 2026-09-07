#!/usr/bin/env bash
#
# End-to-end smoke test for gorge-file-storage, run against an already-running
# instance. It starts nothing and cleans up nothing.
#
# The instance has to have at least one backend configured, or the readiness
# scenario fails by design. Local disk is the one that needs nothing external:
#
#   GORGE_FILE_LOCAL_DISK_PATH=/tmp/gorge-files \
#     make run SERVICE=gorge-file-storage
#   bash tests/e2e/file-storage.sh
#
#   BASE_URL=http://127.0.0.1:8100 TOKEN=dev bash tests/e2e/file-storage.sh
#
# This is the only layer that exercises the transport for real, and that is the
# point of it: this domain is the one place in the repository where a success
# answers raw bytes and a failure answers JSON, and no unit test can prove that
# an actual HTTP round trip returns the file byte for byte. The write-read-
# delete sequence below is therefore the scenario that matters — the ones after
# it are guards on the shapes.
#
# Exits non-zero on the first failed scenario.

set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8100}"
TOKEN="${TOKEN:-}"
CURL_TIMEOUT="${CURL_TIMEOUT:-10}"

# The engine the write-read-delete sequence runs against. It has to be one the
# instance actually has; local disk is what the header above tells you to
# configure.
ENGINE="${ENGINE:-local-disk}"

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

# request METHOD PATH [BODY_FILE] [TOKEN_HEADER_VALUE]
#
# Writes the response body to $RESP_BODY and the status code to $RESP_STATUS.
# The body of a request is passed as a *file* rather than a string, because
# this endpoint takes raw bytes and --data-binary @file is what sends them
# without curl reinterpreting anything.
#
# An unset fourth argument means "send no token header at all", which is
# distinct from sending an empty one.
request() {
  local method="$1" path="$2" body_file="${3:-}" token="${4-__unset__}"
  local args=(-sS --max-time "$CURL_TIMEOUT" -o "$tmp_body" -w '%{http_code}'
              -X "$method" "${BASE_URL}${path}")

  if [ "$token" != "__unset__" ]; then
    args+=(-H "X-Service-Token: ${token}")
  fi
  if [ -n "$body_file" ]; then
    args+=(-H 'Content-Type: application/octet-stream' --data-binary "@${body_file}")
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

# json_field NAME — pulls one string or number value out of a flat JSON object,
# so the script needs no jq. Good enough for the two fields it reads.
json_field() {
  printf '%s' "$RESP_BODY" | sed -n "s/.*\"$1\":\"\{0,1\}\([^,\"}]*\)\"\{0,1\}.*/\1/p"
}

tmp_body="$(mktemp)"
tmp_err="$(mktemp)"
tmp_upload="$(mktemp)"
tmp_download="$(mktemp)"
trap 'rm -f "$tmp_body" "$tmp_err" "$tmp_upload" "$tmp_download"' EXIT

printf 'gorge-file-storage e2e\n'
printf '  base url : %s\n' "$BASE_URL"
printf '  engine   : %s\n' "$ENGINE"
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
# A file storage service with no backend configured is listening and useless,
# and /healthz cannot tell you that. With a database configured this is also
# where an unreachable database shows up.
request GET /readyz
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"status"'*'"ok"'* ]]; then
  pass 'GET /readyz reports at least one usable backend'
else
  fail 'GET /readyz' "status=${RESP_STATUS} body=${RESP_BODY} (configure a backend, see the header of this script)"
fi

# --- 3. unauthorized --------------------------------------------------------
#
# The read endpoint, because that is the one where a missing guard leaks file
# contents rather than metadata.
if [ -n "$TOKEN" ]; then
  request GET "/api/file/blob?engine=${ENGINE}&handle=whatever"
  if [ "$RESP_STATUS" = '401' ] && [[ "$RESP_BODY" == *'ERR_UNAUTHORIZED'* ]]; then
    pass 'GET /api/file/blob without a token gives 401 ERR_UNAUTHORIZED'
  else
    fail 'GET /api/file/blob without a token' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  printf '%sSKIP%s GET /api/file/blob without a token (set TOKEN to exercise auth)\n' \
    "$C_DIM" "$C_OFF"
fi

# --- 4. engine list ---------------------------------------------------------
request GET /api/file/engines '' "$TOKEN"
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"data"'* ]]; then
  fail 'GET /api/file/engines' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" != *'"identifier"'* ]] || [[ "$RESP_BODY" != *'"priority"'* ]]; then
  fail 'GET /api/file/engines' "an entry must carry identifier and priority: ${RESP_BODY}"
elif [[ "$RESP_BODY" != *"\"${ENGINE}\""* ]]; then
  fail 'GET /api/file/engines' "the ${ENGINE} engine is not configured: ${RESP_BODY}"
else
  pass "GET /api/file/engines lists ${ENGINE}"
fi

# --- 5. write ---------------------------------------------------------------
#
# The bytes deliberately include a NUL and a newline: they are what would be
# lost or mangled if either side ever put this payload through a string, a JSON
# encoder or a text-mode transport. A round trip of printable ASCII would pass
# under all three mistakes.
printf 'hello gorge\000\n\xff\xfe binary' > "$tmp_upload"
upload_size="$(wc -c < "$tmp_upload" | tr -d ' ')"

request POST "/api/file/blob?name=e2e.bin&mimeType=application%2Foctet-stream" "$tmp_upload" "$TOKEN"
handle=''
# The engine that actually took the bytes, which is not necessarily $ENGINE:
# the write above names none, so the router picked by priority and may have
# fallen through. A handle only means anything to the engine that minted it,
# so every request below uses this rather than $ENGINE.
wrote_to="$ENGINE"
if [ "$RESP_STATUS" != '200' ]; then
  fail 'POST /api/file/blob' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" == *'"error"'* ]]; then
  fail 'POST /api/file/blob' "a success must carry no error: ${RESP_BODY}"
else
  handle="$(json_field handle)"
  written_engine="$(json_field engine)"
  written_size="$(json_field size)"
  if [ -z "$handle" ] || [ -z "$written_engine" ]; then
    fail 'POST /api/file/blob' "response is missing data.handle or data.engine: ${RESP_BODY}"
  elif [ "$written_size" != "$upload_size" ]; then
    fail 'POST /api/file/blob' "reported size ${written_size}, sent ${upload_size}"
  else
    wrote_to="$written_engine"
    pass "POST /api/file/blob stored ${upload_size} bytes in ${written_engine}"
  fi
fi

# --- 6. read --------------------------------------------------------------
#
# Two assertions, and the second is the one this whole script exists for: the
# response is the file itself, byte for byte, not a JSON document carrying it.
if [ -z "$handle" ]; then
  printf '%sSKIP%s GET /api/file/blob (nothing was written)\n' "$C_DIM" "$C_OFF"
else
  read_status="$(curl -sS --max-time "$CURL_TIMEOUT" -o "$tmp_download" \
    -w '%{http_code} %{content_type}' \
    -H "X-Service-Token: ${TOKEN}" \
    "${BASE_URL}/api/file/blob?engine=${wrote_to}&handle=${handle}" 2>"$tmp_err")"
  status="${read_status%% *}"
  content_type="${read_status#* }"

  if [ "$status" != '200' ]; then
    fail 'GET /api/file/blob' "status=${status} body=$(cat "$tmp_download")"
  elif [ "${content_type%%;*}" != 'application/octet-stream' ]; then
    # This header is what the PHP client branches on to tell the binary shape
    # from the envelope.
    fail 'GET /api/file/blob' "Content-Type=${content_type}, want application/octet-stream"
  elif ! cmp -s "$tmp_upload" "$tmp_download"; then
    fail 'GET /api/file/blob' "the file did not survive the round trip byte for byte"
  else
    pass 'GET /api/file/blob returns the raw bytes, unwrapped and unchanged'
  fi
fi

# --- 7. delete --------------------------------------------------------------
if [ -z "$handle" ]; then
  printf '%sSKIP%s DELETE /api/file/blob (nothing was written)\n' "$C_DIM" "$C_OFF"
else
  request DELETE "/api/file/blob?engine=${wrote_to}&handle=${handle}" '' "$TOKEN"
  if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"deleted"'* ]]; then
    pass 'DELETE /api/file/blob reports data.status deleted'
  else
    fail 'DELETE /api/file/blob' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi

  # --- 8. read after delete -----------------------------------------------
  #
  # 404 with the envelope: the other half of the mixed contract. A success is
  # bytes, a failure is JSON, and both have to hold or the client cannot branch
  # on the status code.
  request GET "/api/file/blob?engine=${wrote_to}&handle=${handle}" '' "$TOKEN"
  if [ "$RESP_STATUS" = '404' ] && [[ "$RESP_BODY" == *'ERR_NOT_FOUND'* ]]; then
    pass 'GET /api/file/blob after delete gives 404 ERR_NOT_FOUND in the envelope'
  else
    fail 'GET /api/file/blob after delete' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi

  # --- 9. delete again ----------------------------------------------------
  #
  # Idempotent on purpose. Phorge removes the bytes and the row pointing at
  # them in one sequence, so a 404 here would leave a row it can never retire.
  request DELETE "/api/file/blob?engine=${wrote_to}&handle=${handle}" '' "$TOKEN"
  if [ "$RESP_STATUS" = '200' ]; then
    pass 'DELETE /api/file/blob is idempotent'
  else
    fail 'DELETE /api/file/blob a second time' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
fi

# --- 10. empty file ---------------------------------------------------------
#
# Zero bytes is a real file, and it is the case that makes "branch on the
# status code" load-bearing: a successful read of it is a 200 with an empty
# body, which is indistinguishable from a failure to anyone checking whether
# the body is empty.
: > "$tmp_upload"
request POST /api/file/blob "$tmp_upload" "$TOKEN"
if [ "$RESP_STATUS" != '200' ]; then
  fail 'POST /api/file/blob with an empty file' "status=${RESP_STATUS} body=${RESP_BODY}"
else
  empty_handle="$(json_field handle)"
  empty_engine="$(json_field engine)"
  read_status="$(curl -sS --max-time "$CURL_TIMEOUT" -o "$tmp_download" -w '%{http_code}' \
    -H "X-Service-Token: ${TOKEN}" \
    "${BASE_URL}/api/file/blob?engine=${empty_engine}&handle=${empty_handle}" 2>"$tmp_err")"
  if [ "$read_status" = '200' ] && [ ! -s "$tmp_download" ]; then
    pass 'a zero-byte file round trips as 200 with an empty body'
  else
    fail 'a zero-byte file round trip' "status=${read_status} bytes=$(wc -c < "$tmp_download" | tr -d ' ')"
  fi
  request DELETE "/api/file/blob?engine=${empty_engine}&handle=${empty_handle}" '' "$TOKEN"
fi

# --- 11. unknown engine -----------------------------------------------------
#
# 400, not 404: a handle is meaningless without the engine that minted it, so
# there is nothing to substitute. 404 here would look like a base URL joined
# wrongly, which is the diagnosis ERR_NOT_FOUND is reserved for.
printf 'x' > "$tmp_upload"
request POST '/api/file/blob?engine=no-such-engine' "$tmp_upload" "$TOKEN"
if [ "$RESP_STATUS" = '400' ] && [[ "$RESP_BODY" == *'ERR_BAD_REQUEST'* ]]; then
  pass 'POST /api/file/blob with an unknown engine gives 400 ERR_BAD_REQUEST'
else
  fail 'POST /api/file/blob with an unknown engine' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 12. missing parameters -------------------------------------------------
request GET '/api/file/blob' '' "$TOKEN"
if [ "$RESP_STATUS" = '400' ] && [[ "$RESP_BODY" == *'ERR_BAD_REQUEST'* ]]; then
  pass 'GET /api/file/blob with no engine or handle gives 400 ERR_BAD_REQUEST'
else
  fail 'GET /api/file/blob with no parameters' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 13. a handle no engine could have minted -------------------------------
#
# Read and delete answer this differently on purpose, and the unit tests use
# stub engines — so this is the only place the real engines' own handle
# validation is exercised over HTTP. For local disk that validation is also
# what stops a `..` from escaping the storage root.
request DELETE "/api/file/blob?engine=${ENGINE}&handle=../../etc/passwd" '' "$TOKEN"
if [ "$RESP_STATUS" = '400' ] && [[ "$RESP_BODY" == *'ERR_BAD_REQUEST'* ]]; then
  # Not 500: with an already-gone object reported as success, a malformed
  # handle is the only delete failure that is not a backend fault, and a 500
  # would blame the service for what the caller sent.
  pass 'DELETE /api/file/blob with a malformed handle gives 400 ERR_BAD_REQUEST'
else
  fail 'DELETE /api/file/blob with a malformed handle' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

request GET "/api/file/blob?engine=${ENGINE}&handle=../../etc/passwd" '' "$TOKEN"
if [ "$RESP_STATUS" = '404' ] && [[ "$RESP_BODY" == *'ERR_NOT_FOUND'* ]]; then
  pass 'GET /api/file/blob with a malformed handle stays 404 ERR_NOT_FOUND'
else
  fail 'GET /api/file/blob with a malformed handle' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
