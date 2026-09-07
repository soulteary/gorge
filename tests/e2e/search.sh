#!/usr/bin/env bash
#
# End-to-end smoke test for gorge-search, run against an already-running
# instance. It starts nothing and cleans up nothing.
#
# ⚠ THIS SCRIPT DESTROYS THE INDEX. Scenario 5 calls POST /api/search/init,
# which drops the index and everything in it before recreating it. Point it at
# a scratch deployment, never at one holding a real Phorge install's documents.
#
#   GORGE_SEARCH_BACKENDS='[{"type":"elasticsearch","hosts":["127.0.0.1:9200"],"index":"gorge-e2e","version":7,"roles":["read","write"]}]' \
#     make run SERVICE=gorge-search
#   bash tests/e2e/search.sh
#
#   BASE_URL=http://127.0.0.1:8120 TOKEN=dev bash tests/e2e/search.sh
#
# It runs against the in-memory "test" backend too, which is what the compose
# stack ships with, but two scenarios are skipped there rather than passed:
# 11 and 12 are statements about Elasticsearch's analyser chain, and the test
# backend matches substrings, so it would answer both correctly while proving
# nothing. Those two are the reason this script exists — see scenario 11.
#
# Exits non-zero on the first failed scenario.

set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8120}"
TOKEN="${TOKEN:-}"
CURL_TIMEOUT="${CURL_TIMEOUT:-10}"

# Elasticsearch's index refresh is near-real-time, not real-time: a document is
# durable the moment it is written but not searchable until the next refresh,
# which defaults to one second. A query fired immediately after a write
# therefore misses it, and a script that asserted once would fail about as
# often as it passed. Every content assertion below polls instead.
VISIBILITY_TIMEOUT="${VISIBILITY_TIMEOUT:-15}"

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

# query_until_found QUERY_BODY PHID
#
# Polls POST /api/search/query until the PHID appears or the visibility budget
# runs out. Returns 0 on success, and leaves the last response in $RESP_BODY
# either way so a failure message can show what did come back.
query_until_found() {
  local body="$1" phid="$2" deadline=$((SECONDS + VISIBILITY_TIMEOUT))

  while [ "$SECONDS" -lt "$deadline" ]; do
    request POST /api/search/query "$body" "$TOKEN"
    if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *"$phid"* ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

tmp_body="$(mktemp)"
tmp_err="$(mktemp)"
trap 'rm -f "$tmp_body" "$tmp_err"' EXIT

# Timestamps are epoch seconds, the same units PhabricatorSearchAbstractDocument
# uses. A fixed value would be fine for matching, but the newest-first ordering
# of an unfiltered listing is only meaningful with a plausible one.
now="$(date +%s)"

printf 'gorge-search e2e\n'
printf '  base url : %s\n' "$BASE_URL"
if [ -n "$TOKEN" ]; then
  printf '  token    : (set)\n'
else
  printf '  token    : (empty — auth disabled, the 401 scenario is skipped)\n'
fi
printf '  %swarning : scenario 5 drops and recreates the index%s\n\n' "$C_DIM" "$C_OFF"

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
# The one probe carrying information here: a search service with no readable
# backend is listening and answers every query with a 502, and /healthz reports
# it as perfectly fine. Note that readiness does not dial Elasticsearch — a 200
# means "this service can attempt a search", not that the cluster is up.
request GET /readyz
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"status"'*'"ok"'* ]]; then
  pass 'GET /readyz reports at least one readable backend'
else
  fail 'GET /readyz' "status=${RESP_STATUS} body=${RESP_BODY} (configure a backend, see the header of this script)"
fi

# --- 3. unauthorized --------------------------------------------------------
if [ -n "$TOKEN" ]; then
  request GET /api/search/backends
  if [ "$RESP_STATUS" = '401' ] && [[ "$RESP_BODY" == *'ERR_UNAUTHORIZED'* ]]; then
    pass 'GET /api/search/backends without a token gives 401 ERR_UNAUTHORIZED'
  else
    fail 'GET /api/search/backends without a token' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  printf '%sSKIP%s GET /api/search/backends without a token (set TOKEN to exercise auth)\n' \
    "$C_DIM" "$C_OFF"
fi

# --- 4. backend list --------------------------------------------------------
#
# The credential check is not decoration. This is a diagnostics endpoint —
# Phorge defines getBackends() but never calls it, and the cluster panel
# renders local cluster.search config instead — and diagnostic output is what
# gets pasted into tickets, logs and support threads, so an apiKey leaking
# into it leaks wherever those go.
request GET /api/search/backends '' "$TOKEN"
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"data"'* ]]; then
  fail 'GET /api/search/backends' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" != *'"type"'* ]] || [[ "$RESP_BODY" != *'"roles"'* ]]; then
  fail 'GET /api/search/backends' "an entry must carry type and roles: ${RESP_BODY}"
elif [[ "$RESP_BODY" == *'apiKey'* ]]; then
  fail 'GET /api/search/backends' "a credential must never appear here: ${RESP_BODY}"
else
  pass 'GET /api/search/backends lists the backends without credentials'
fi

# Which backend answered decides whether the analyser scenarios mean anything.
# The in-memory one matches substrings, so it would answer both of them
# correctly no matter what the mapping says — a pass there would be worse than
# a skip, because it looks like coverage.
backend_is_real=1
if [[ "$RESP_BODY" == *'"type":"test"'* ]]; then
  backend_is_real=0
fi

skip() {
  printf '%sSKIP%s %s\n' "$C_DIM" "$C_OFF" "$1"
  if [ -n "${2:-}" ]; then
    printf '%s     %s%s\n' "$C_DIM" "$2" "$C_OFF"
  fi
}

# --- 5. init ----------------------------------------------------------------
#
# This is the destructive one. The document types are the mapping the index is
# built from, which is why they are sent rather than assumed.
request POST /api/search/init \
  '{"docTypes":["TASK","DREV","CMIT","PSTE","WIKI","USER","PROJ"]}' "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'initialized'* ]]; then
  pass 'POST /api/search/init creates the index'
else
  fail 'POST /api/search/init' "status=${RESP_STATUS} body=${RESP_BODY}"
  printf '%s     nothing below can pass without an index; stopping%s\n' "$C_DIM" "$C_OFF"
  printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
  exit 1
fi

# --- 6. existence -----------------------------------------------------------
request GET /api/search/exists '' "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"exists"'*'true'* ]]; then
  pass 'GET /api/search/exists reports the index it just created'
else
  fail 'GET /api/search/exists' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 7. sanity --------------------------------------------------------------
#
# Immediately after an init this must be true, because the index was created
# from exactly the configuration the check compares against. A false here means
# the mapping this service writes and the mapping it expects have drifted apart
# — which no other test can see, since both halves live in the same file and
# agree with each other perfectly right up to the moment Elasticsearch
# normalises one of them.
request POST /api/search/sane \
  '{"docTypes":["TASK","DREV","CMIT","PSTE","WIKI","USER","PROJ"]}' "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"sane"'*'true'* ]]; then
  pass 'POST /api/search/sane is true for a freshly created index'
else
  fail 'POST /api/search/sane' "status=${RESP_STATUS} body=${RESP_BODY} (a freshly created index must be sane)"
fi

# --- 8. index an English document -------------------------------------------
read -r -d '' latin_doc <<JSON
{"phid":"PHID-TASK-e2elatin","type":"TASK",
"title":"Login redirect drops the query string",
"dateCreated":${now},"dateModified":${now},
"fields":[{"name":"titl","corpus":"Login redirect drops the query string"},
{"name":"body","corpus":"The redirect discards everything after the question mark."},
{"name":"cmnt","corpus":"Reproduced on staging.","aux":"PHID-USER-e2ealice"}],
"relationships":[{"name":"auth","relatedPHID":"PHID-USER-e2ealice","rtype":"USER"},
{"name":"ownr","relatedPHID":"PHID-USER-e2ebob","rtype":"USER"},
{"name":"proj","relatedPHID":"PHID-PROJ-e2eweb","rtype":"PROJ"},
{"name":"open","relatedPHID":"PHID-TASK-e2elatin","rtype":"TASK","timestamp":${now}}]}
JSON

request POST /api/search/index "$latin_doc" "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'PHID-TASK-e2elatin'* ]]; then
  pass 'POST /api/search/index accepts a document and echoes its PHID'
else
  fail 'POST /api/search/index' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 9. index a Chinese document --------------------------------------------
read -r -d '' cjk_doc <<JSON
{"phid":"PHID-TASK-e2ecjk","type":"TASK",
"title":"登录跳转丢失查询参数",
"dateCreated":${now},"dateModified":${now},
"fields":[{"name":"titl","corpus":"登录跳转丢失查询参数"},
{"name":"body","corpus":"用户报告说，问号后面的内容在跳转之后全部消失了。"}],
"relationships":[{"name":"auth","relatedPHID":"PHID-USER-e2ealice","rtype":"USER"}]}
JSON

request POST /api/search/index "$cjk_doc" "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'PHID-TASK-e2ecjk'* ]]; then
  pass 'POST /api/search/index accepts a Chinese document'
else
  fail 'POST /api/search/index with a Chinese corpus' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 10. English round trip -------------------------------------------------
if query_until_found '{"query":"redirect","types":["TASK"],"limit":10}' 'PHID-TASK-e2elatin'; then
  pass 'POST /api/search/query finds the document it just indexed'
else
  fail 'POST /api/search/query for "redirect"' "last response: ${RESP_BODY}"
fi

# --- 11. Chinese round trip -------------------------------------------------
#
# The reason this script exists. Everything above can be checked against the
# in-memory backend in go/internal/search/; this cannot, because it is a
# statement about Elasticsearch's analyser chain rather than about this
# service's code.
#
# 跳转 is two characters, which is the case the whole cjk subfield is for. The
# English analysers get it wrong in opposite directions and neither reports
# anything: the letter tokenizer behind letter_stop treats the entire run of
# Han characters as one indivisible token, so 跳转 matches nothing at all,
# while the standard tokenizer behind english_exact and english_stem splits it
# into 跳 and 转, so it matches every document containing either character.
# One is too strict and one is too loose. Both answer 200. A deployment whose
# mapping lost the cjk subfield therefore looks healthy on every probe, passes
# every contract fixture, and quietly stops finding Chinese text.
if [ "$backend_is_real" -eq 0 ]; then
  skip 'POST /api/search/query finds a two-character Chinese term' \
    'the in-memory backend matches substrings; only Elasticsearch can answer this'
elif query_until_found '{"query":"跳转","types":["TASK"],"limit":10}' 'PHID-TASK-e2ecjk'; then
  pass 'POST /api/search/query finds a two-character Chinese term'
else
  fail 'POST /api/search/query for 跳转' \
    "last response: ${RESP_BODY} (the cjk subfield or the cjk_bigram filter is missing from the mapping — reindex after POST /api/search/init)"
fi

# --- 12. precision ----------------------------------------------------------
#
# The other half of the CJK story. cjk_bigram is configured with
# output_unigrams, so single characters stay searchable; without a bigram index
# a query for 登录 would also match a document that merely contains 录, and a
# search that returns everything is as useless as one that returns nothing.
# The Latin document shares no character with the query, so it is the control.
if [ "$backend_is_real" -eq 0 ]; then
  skip 'POST /api/search/query for 登录跳转 matches only the Chinese document' \
    'the in-memory backend matches substrings; only Elasticsearch can answer this'
else
  request POST /api/search/query '{"query":"登录跳转","types":["TASK"],"limit":10}' "$TOKEN"
  if [ "$RESP_STATUS" != '200' ]; then
    fail 'POST /api/search/query for 登录跳转' "status=${RESP_STATUS} body=${RESP_BODY}"
  elif [[ "$RESP_BODY" != *'PHID-TASK-e2ecjk'* ]]; then
    fail 'POST /api/search/query for 登录跳转' "the Chinese document should match: ${RESP_BODY}"
  elif [[ "$RESP_BODY" == *'PHID-TASK-e2elatin'* ]]; then
    fail 'POST /api/search/query for 登录跳转' \
      "the English document shares no character with the query and must not match: ${RESP_BODY}"
  else
    pass 'POST /api/search/query for 登录跳转 matches only the Chinese document'
  fi
fi

# --- 13. unfiltered listing -------------------------------------------------
#
# Phorge's search UI opens on this exact request. Rejecting it would blank that
# page, so an empty query is a listing rather than an error.
request POST /api/search/query '{}' "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"phids"'* ]]; then
  pass 'POST /api/search/query with an empty body is a listing, not an error'
else
  fail 'POST /api/search/query with an empty body' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 14. relationship filters -----------------------------------------------
#
# authorPHIDs reaches the index as the four-character "auth" relationship. A
# filter that matched nothing would look identical to a correct filter over a
# corpus with no such author, which is why this asserts a document it knows is
# there.
if query_until_found \
  '{"query":"","types":["TASK"],"authorPHIDs":["PHID-USER-e2ealice"],"limit":10}' \
  'PHID-TASK-e2elatin'; then
  pass 'POST /api/search/query filters by authorPHIDs'
else
  fail 'POST /api/search/query with authorPHIDs' "last response: ${RESP_BODY}"
fi

# --- 15. exclude ------------------------------------------------------------
#
# Phorge passes the object being viewed as exclude so that "similar objects"
# does not list the object itself.
request POST /api/search/query \
  '{"query":"","types":["TASK"],"exclude":"PHID-TASK-e2elatin","limit":10}' "$TOKEN"
if [ "$RESP_STATUS" != '200' ]; then
  fail 'POST /api/search/query with exclude' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" == *'PHID-TASK-e2elatin'* ]]; then
  fail 'POST /api/search/query with exclude' "the excluded PHID came back: ${RESP_BODY}"
else
  pass 'POST /api/search/query honours exclude'
fi

# --- 16. statistics ---------------------------------------------------------
#
# storage_bytes is the single snake_case name on this wire. It predates the
# monorepo and Phorge's cluster panel reads it by that spelling; renaming it to
# storageBytes leaves the storage column blank with no error anywhere. Only
# Elasticsearch reports it, so its absence is a note rather than a failure.
request GET /api/search/stats '' "$TOKEN"
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"documents"'* ]]; then
  fail 'GET /api/search/stats' "status=${RESP_STATUS} body=${RESP_BODY}"
elif [[ "$RESP_BODY" == *'storageBytes'* ]]; then
  fail 'GET /api/search/stats' "storage_bytes was renamed; Phorge reads the snake_case spelling: ${RESP_BODY}"
else
  pass 'GET /api/search/stats reports the index statistics'
fi

# --- 17. incomplete document ------------------------------------------------
#
# 400, not 502: the document never reached a store, so nothing downstream
# judged anything.
request POST /api/search/index '{"type":"TASK","title":"no identity"}' "$TOKEN"
if [ "$RESP_STATUS" = '400' ] && [[ "$RESP_BODY" == *'ERR_BAD_REQUEST'* ]]; then
  pass 'POST /api/search/index without a PHID gives 400 ERR_BAD_REQUEST'
else
  fail 'POST /api/search/index without a PHID' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 18. empty docTypes -----------------------------------------------------
#
# On /sane this is the interesting one: an empty type list builds an empty
# expectation, which any index at all satisfies, so accepting it would answer a
# confident "sane: true" to a question nobody asked properly.
for path in /api/search/init /api/search/sane; do
  request POST "$path" '{"docTypes":[]}' "$TOKEN"
  if [ "$RESP_STATUS" = '400' ] && [[ "$RESP_BODY" == *'ERR_BAD_REQUEST'* ]]; then
    pass "POST ${path} with no docTypes gives 400 ERR_BAD_REQUEST"
  else
    fail "POST ${path} with no docTypes" "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
done

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
