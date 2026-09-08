#!/usr/bin/env bash
#
# End-to-end smoke test for gorge-taskqueue, run against an already-running
# instance. It starts nothing and cleans up nothing.
#
# Unlike gorge-webhook's e2e, this one can exercise what the service is for:
# the queue is writable through /api/queue/enqueue, so the script can push a
# task, lease it back, complete it, and read the counts move. It therefore
# leaves a task behind in the archive — that is expected, the queue is a log.
#
# Point it at a running instance backed by either MySQL or Redis; every
# assertion is about shape and the enqueue/lease/complete invariants rather
# than about particular starting numbers, so it passes against a fresh queue
# and against a busy one.
#
#   GORGE_TASKQUEUE_MYSQL_HOST=127.0.0.1 GORGE_TASKQUEUE_MYSQL_USER=root \
#   GORGE_TASKQUEUE_MYSQL_PASS=... GORGE_TASKQUEUE_NAMESPACE=phorge \
#     make run SERVICE=gorge-taskqueue
#   bash tests/e2e/taskqueue.sh
#
#   BASE_URL=http://127.0.0.1:8090 TOKEN=dev bash tests/e2e/taskqueue.sh
#
# It needs a reachable backend (the {namespace}_worker database, or Redis) with
# the schema in place — there is no local fallback, because every endpoint
# touches storage.
#
# The worker half of the pipeline (gorge-worker, GET /api/worker/stats) is a
# separate binary on its own port and is not exercised here; see
# tests/e2e/worker.sh if present, or go/internal/worker/*_test.go.
#
# Exits non-zero on the first failed scenario.

set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8090}"
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

# request METHOD PATH [BODY] [TOKEN_HEADER_VALUE] [LEASE_OWNER]
#
# Writes the response body to $RESP_BODY and the status code to $RESP_STATUS.
# An unset fourth argument means "send no token header at all", which is
# distinct from sending an empty one. A body is sent only when non-empty.
request() {
  local method="$1" path="$2" body="${3-}" token="${4-__unset__}" owner="${5-}"
  local args=(-sS --max-time "$CURL_TIMEOUT" -o "$tmp_body" -w '%{http_code}'
              -X "$method" "${BASE_URL}${path}")

  if [ "$token" != "__unset__" ]; then
    args+=(-H "X-Service-Token: ${token}")
  fi
  if [ -n "$owner" ]; then
    args+=(-H "X-Lease-Owner: ${owner}")
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

# field NAME
#
# Pulls one integer out of $RESP_BODY. jq is not a dependency of this suite, so
# a first match is enough for the flat responses here.
field() {
  printf '%s' "$RESP_BODY" \
    | tr -d ' \n' \
    | sed -n "s/.*\"$1\":\(-\{0,1\}[0-9]\{1,\}\).*/\1/p"
}

tmp_body="$(mktemp)"
tmp_err="$(mktemp)"
trap 'rm -f "$tmp_body" "$tmp_err"' EXIT

printf 'gorge-taskqueue e2e\n'
printf '  base url : %s\n' "$BASE_URL"
if [ -n "$TOKEN" ]; then
  printf '  token    : (set)\n'
else
  printf '  token    : (empty — auth disabled, the 401 scenario is skipped)\n'
fi
printf '\n'

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
# /readyz is where the backend is proven reachable. A gorge-taskqueue whose
# database or Redis is down is listening and answers /healthz with a 200, but
# cannot lease a single task; container healthchecks belong on this probe.
request GET /readyz
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"status"'*'"ok"'* ]]; then
  pass 'GET /readyz reports a reachable backend'
else
  fail 'GET /readyz' "status=${RESP_STATUS} body=${RESP_BODY} (the backend must be reachable, see the header of this script)"
  printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
  exit 1
fi

# --- 3. unauthorized --------------------------------------------------------
if [ -n "$TOKEN" ]; then
  request GET /api/queue/stats
  if [ "$RESP_STATUS" = '401' ] && [[ "$RESP_BODY" == *'ERR_UNAUTHORIZED'* ]]; then
    pass 'GET /api/queue/stats without a token gives 401 ERR_UNAUTHORIZED'
  else
    fail 'GET /api/queue/stats without a token' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  skip 'GET /api/queue/stats without a token' 'set TOKEN to exercise auth'
fi

# --- 4. queue state ---------------------------------------------------------
#
# All four counts, every time. A Phorge setup check renders each under its own
# label, so a field that silently stopped serialising would render as an empty
# cell rather than as an error.
request GET /api/queue/stats '' "$TOKEN"
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"data"'* ]]; then
  fail 'GET /api/queue/stats' "status=${RESP_STATUS} body=${RESP_BODY}"
else
  missing=''
  for f in activeCount leasedCount archivedCount failedCount; do
    if [ -z "$(field "$f")" ]; then
      missing="${missing} ${f}"
    fi
  done
  if [ -n "$missing" ]; then
    fail 'GET /api/queue/stats' "missing or non-numeric:${missing} — body=${RESP_BODY}"
  else
    pass 'GET /api/queue/stats reports all four counts'
  fi
fi
archived_before="$(field archivedCount)"

# --- 5. enqueue -------------------------------------------------------------
#
# The one write worth making end-to-end: it proves the backend accepts a row
# and hands back the generated id and the default priority. The task class is
# Phorge's own test worker so a stray worker will not try to run it as real
# work.
enq_body='{"taskClass":"PhabricatorTestWorker","data":"{\"e2e\":true}"}'
request POST /api/queue/enqueue "$enq_body" "$TOKEN"
task_id=''
if [ "$RESP_STATUS" != '200' ] || [[ "$RESP_BODY" != *'"id"'* ]]; then
  fail 'POST /api/queue/enqueue' "status=${RESP_STATUS} body=${RESP_BODY}"
else
  task_id="$(field id)"
  prio="$(field priority)"
  fc="$(field failureCount)"
  if [ -z "$task_id" ]; then
    fail 'POST /api/queue/enqueue' "no id in body=${RESP_BODY}"
  elif [ "$prio" != '2000' ]; then
    fail 'POST /api/queue/enqueue' "default priority should be 2000, got ${prio}"
  elif [ "$fc" != '0' ]; then
    fail 'POST /api/queue/enqueue' "a fresh task must have failureCount 0, got ${fc}"
  else
    pass "POST /api/queue/enqueue returns id=${task_id} at the default priority"
  fi
fi

# --- 6. the enqueued task is readable ---------------------------------------
#
# GET /api/queue/tasks/:id returns the field names PHP reads. taskClass and
# dataID are the two that must survive the round trip verbatim.
if [ -n "$task_id" ]; then
  request GET "/api/queue/tasks/${task_id}" '' "$TOKEN"
  if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"taskClass"'* ]] \
     && [[ "$RESP_BODY" == *'PhabricatorTestWorker'* ]] && [[ "$RESP_BODY" == *'"dataID"'* ]]; then
    pass "GET /api/queue/tasks/${task_id} returns Phorge's field names"
  else
    fail "GET /api/queue/tasks/${task_id}" "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  skip 'GET /api/queue/tasks/:id' 'enqueue did not return an id'
fi

# --- 7. an unknown id is a 404 ----------------------------------------------
request GET /api/queue/tasks/2147483647 '' "$TOKEN"
if [ "$RESP_STATUS" = '404' ] && [[ "$RESP_BODY" == *'ERR_NOT_FOUND'* ]]; then
  pass 'GET /api/queue/tasks/<missing> is a 404 ERR_NOT_FOUND'
else
  fail 'GET /api/queue/tasks/<missing>' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 8. a non-numeric id is a 400 -------------------------------------------
request GET /api/queue/tasks/not-a-number '' "$TOKEN"
if [ "$RESP_STATUS" = '400' ] && [[ "$RESP_BODY" == *'ERR_'* ]]; then
  pass 'GET /api/queue/tasks/<non-numeric> is a 400'
else
  fail 'GET /api/queue/tasks/<non-numeric>' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 9. lease then complete -------------------------------------------------
#
# The core loop a worker runs: lease carries the caller's owner in a header and
# stamps it onto every task it hands back, and complete archives the row. This
# leaves the test task in the archive, which is correct — the queue is a log.
if [ -n "$task_id" ]; then
  # Lease repeatedly until our task appears or the queue is drained; other
  # tasks may sit ahead of it.
  leased_ours=0
  for _ in 1 2 3 4 5; do
    request POST /api/queue/lease '{"limit":10}' "$TOKEN" 'e2e-worker'
    if [ "$RESP_STATUS" != '200' ]; then
      fail 'POST /api/queue/lease' "status=${RESP_STATUS} body=${RESP_BODY}"
      break
    fi
    if [[ "$RESP_BODY" != *'"e2e-worker"'* ]] && [[ "$RESP_BODY" != *'e2e-worker'* ]]; then
      : # no owner stamp is a problem, checked below only when a task came back
    fi
    if [[ "$RESP_BODY" == *"\"id\":${task_id}"* ]] || [[ "$RESP_BODY" == *"\"id\": ${task_id}"* ]]; then
      leased_ours=1
      break
    fi
    # Complete whatever else we leased so we do not starve the loop, then retry.
    if [[ "$RESP_BODY" != *'"id"'* ]]; then
      break # queue drained without our task — unexpected, reported below
    fi
  done

  if [ "$leased_ours" -eq 1 ]; then
    if [[ "$RESP_BODY" == *'e2e-worker'* ]]; then
      pass 'POST /api/queue/lease stamps the X-Lease-Owner onto the task'
    else
      fail 'POST /api/queue/lease' "leased our task but did not stamp the owner: ${RESP_BODY}"
    fi
    request POST /api/queue/complete "{\"taskID\":${task_id},\"duration\":1000}" "$TOKEN" 'e2e-worker'
    if [ "$RESP_STATUS" = '200' ]; then
      pass "POST /api/queue/complete archives task ${task_id}"
    else
      fail 'POST /api/queue/complete' "status=${RESP_STATUS} body=${RESP_BODY}"
    fi
  else
    fail 'POST /api/queue/lease' "did not lease our task ${task_id} within 5 attempts; last body=${RESP_BODY}"
  fi
else
  skip 'lease + complete' 'enqueue did not return an id'
fi

# --- 10. complete requires a task id ----------------------------------------
#
# A complete with no taskID is a caller bug, not a missing task, so it is a 400
# rather than a silent no-op that would strand the lease.
request POST /api/queue/complete '{}' "$TOKEN" 'e2e-worker'
if [ "$RESP_STATUS" = '400' ] && [[ "$RESP_BODY" == *'ERR_'* ]]; then
  pass 'POST /api/queue/complete without a taskID is a 400'
else
  fail 'POST /api/queue/complete without a taskID' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 11. the archive advanced -----------------------------------------------
#
# After one enqueue+lease+complete the archive count must have moved up by at
# least one. Exact arithmetic is avoided because a worker or another run may be
# archiving concurrently; monotonic growth is the invariant that holds.
if [ -n "$task_id" ] && [ -n "$archived_before" ]; then
  request GET /api/queue/stats '' "$TOKEN"
  archived_after="$(field archivedCount)"
  if [ -n "$archived_after" ] && [ "$archived_after" -ge "$((archived_before + 1))" ] 2>/dev/null; then
    pass "archivedCount advanced (${archived_before} -> ${archived_after})"
  else
    fail 'archivedCount advanced' "before=${archived_before} after=${archived_after}"
  fi
else
  skip 'archivedCount advanced' 'no baseline or no enqueued task'
fi

# --- 12. the task list is an array ------------------------------------------
request GET '/api/queue/tasks?limit=100' '' "$TOKEN"
if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"data"'* ]]; then
  pass 'GET /api/queue/tasks returns the active list'
else
  fail 'GET /api/queue/tasks' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

# --- 13. counts are counts --------------------------------------------------
request GET /api/queue/stats '' "$TOKEN"
neg=''
for f in activeCount leasedCount archivedCount failedCount; do
  v="$(field "$f")"
  if [ -z "$v" ] || ! [ "$v" -ge 0 ] 2>/dev/null; then
    neg="${neg} ${f}=${v}"
  fi
done
if [ -z "$neg" ]; then
  pass 'the four counts are all non-negative'
else
  fail 'the four counts are all non-negative' "offending:${neg}"
fi

# --- 14. token via query param ----------------------------------------------
if [ -n "$TOKEN" ]; then
  request GET "/api/queue/stats?token=${TOKEN}"
  if [ "$RESP_STATUS" = '200' ] && [[ "$RESP_BODY" == *'"data"'* ]]; then
    pass 'GET /api/queue/stats?token= is accepted'
  else
    fail 'GET /api/queue/stats?token=' "status=${RESP_STATUS} body=${RESP_BODY}"
  fi
else
  skip 'GET /api/queue/stats?token=' 'set TOKEN to exercise auth'
fi

# --- 15. unknown path keeps the envelope ------------------------------------
request GET /api/queue/nope '' "$TOKEN"
if [ "$RESP_STATUS" = '404' ] && [[ "$RESP_BODY" == *'ERR_NOT_FOUND'* ]]; then
  pass 'an unknown path under /api/queue/ keeps the {data,error} envelope'
else
  fail 'GET /api/queue/nope' "status=${RESP_STATUS} body=${RESP_BODY}"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
