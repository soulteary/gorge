package webhook

import (
	"context"
	"strings"
	"testing"
)

// The tests in this file are of two kinds and they cover the claim from two
// sides. The first kind asserts the *statements* — which is the only thing a
// test without a database can say about SQL, and it is worth saying, because
// every clause pinned below is one a reasonable person would remove as
// redundant. The second kind asserts the *semantics*, through the in-memory
// store, which is what the dispatcher is actually written against.

// TestClaimStatementIsAnOptimisticCompareAndSet pins the three properties that
// make the claim work at all.
func TestClaimStatementIsAnOptimisticCompareAndSet(t *testing.T) {
	normalised := normaliseSQL(claimStmt)

	// The version bump has to be GREATEST(dateModified + 1, …) and not a bare
	// UNIX_TIMESTAMP(). dateModified has one-second resolution, so a row
	// created and claimed in the same second would be written back with the
	// value it already held — and the WHERE clause below would still match
	// for a second claimant, which is the whole failure this statement
	// prevents.
	if !strings.Contains(normalised, "SET dateModified = GREATEST(dateModified + 1, UNIX_TIMESTAMP())") {
		t.Errorf("the claim must advance dateModified past both its old value and the clock:\n%s", normalised)
	}

	// All three conditions are load-bearing. The id addresses the row, the
	// version is what makes the update a compare-and-set, and the status keeps
	// a row that reached a terminal state between the candidate query and here
	// from being claimed at all.
	if !strings.Contains(normalised, "WHERE id = ? AND status = ? AND dateModified = ?") {
		t.Errorf("the claim must be conditional on the id, the status and the version:\n%s", normalised)
	}

	// Claiming must not touch status. Phorge's UI renders an icon per status
	// and HeraldWebhookWorker::doWork refuses any request that is not
	// `queued`, so a `claimed` value would break the interface and the PHP
	// fallback path at once.
	if strings.Contains(normalised, "SET status") {
		t.Errorf("the claim must not write the status column:\n%s", normalised)
	}
}

// TestFetchClaimableStatementKeepsItsConditionsSeparate pins the candidate
// query's shape.
func TestFetchClaimableStatementKeepsItsConditionsSeparate(t *testing.T) {
	normalised := normaliseSQL(fetchClaimableStmt)

	// Equality on status and then ORDER BY id is what lets phase B's
	// key_status (status, id) serve both halves. Without that shape the query
	// is the full table scan it used to be, over a table whose `sent` rows
	// are kept for seven days.
	if !strings.Contains(normalised, "WHERE status = ?") {
		t.Errorf("the candidate query must filter on status by equality:\n%s", normalised)
	}
	if !strings.Contains(normalised, "ORDER BY id ASC") {
		t.Errorf("the candidate query must order by id, oldest request first:\n%s", normalised)
	}

	// The lease, with the escape hatch for a row nothing has touched yet.
	// Without the first half of the OR every delivery would wait out a lease
	// it was never under, which is delivery latency for every webhook in the
	// install.
	if !strings.Contains(normalised, "(dateModified = dateCreated OR dateModified <= ?)") {
		t.Errorf("the lease condition must admit a row that has never been modified:\n%s", normalised)
	}

	// The retry backoff, as its own condition. Merging it into the lease would
	// force one duration on two questions: how long a claim survives its
	// owner, and how long a failed request waits before it is retried.
	if !strings.Contains(normalised, "(lastRequestResult <> ? OR lastRequestEpoch <= ?)") {
		t.Errorf("the retry backoff must be a separate condition:\n%s", normalised)
	}
}

func TestNewClaimQueryDerivesBothCutoffsFromOneClock(t *testing.T) {
	q := newClaimQuery(1_000_000, 8, 30, 60)

	if q.Limit != 8 {
		t.Errorf("Limit = %d, want 8", q.Limit)
	}
	if q.LeaseCutoff != 999_970 {
		t.Errorf("LeaseCutoff = %d, want now - 30", q.LeaseCutoff)
	}
	if q.FailCutoff != 999_940 {
		t.Errorf("FailCutoff = %d, want now - 60", q.FailCutoff)
	}
}

// TestAFreshRequestIsClaimableImmediately is the case the lease's escape hatch
// exists for: Lisk writes dateCreated and dateModified as the same second, so
// without it a brand new request would wait out a full lease before its first
// attempt.
func TestAFreshRequestIsClaimableImmediately(t *testing.T) {
	store := newMemStore(1_000_000).addHook(testHook())
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	candidates, err := store.FetchClaimable(context.Background(),
		newClaimQuery(1_000_000, 8, 30, 60))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected the fresh request to be claimable at once, got %d candidates", len(candidates))
	}
}

// TestOnlyOneClaimWinsTheSameVersion is the duplicate delivery bug, reduced to
// its smallest form: two attempts that read the same row.
func TestOnlyOneClaimWinsTheSameVersion(t *testing.T) {
	store := newMemStore(1_000_000).addHook(testHook())
	req := store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})
	version := req.DateModified

	first, err := store.Claim(context.Background(), req.ID, version)
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("the first claim must win")
	}

	second, err := store.Claim(context.Background(), req.ID, version)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("a second claim on the same version must lose: it would be a duplicate delivery")
	}
}

// TestAClaimInTheSameSecondStillChangesTheVersion is what GREATEST buys.
//
// The clock does not move between the seeding and the claim, so a claim
// written as `dateModified = UNIX_TIMESTAMP()` would store the value the row
// already held — and the losing claimant's compare-and-set would still match.
func TestAClaimInTheSameSecondStillChangesTheVersion(t *testing.T) {
	const now = 1_000_000
	store := newMemStore(now).addHook(testHook())
	req := store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	if _, err := store.Claim(context.Background(), req.ID, req.DateModified); err != nil {
		t.Fatal(err)
	}

	claimed := store.row(req.ID)
	if claimed.DateModified <= req.DateModified {
		t.Fatalf("dateModified = %d, must advance past %d even within one second",
			claimed.DateModified, req.DateModified)
	}
}

// TestAClaimedRequestIsNotACandidateUntilTheLeaseExpires is the other half of
// the mechanism. The status cannot say "being delivered", so the only thing
// keeping the next tick — one second later, against a delivery that may take
// fifteen — off the row is the lease.
func TestAClaimedRequestIsNotACandidateUntilTheLeaseExpires(t *testing.T) {
	const (
		now      = 1_000_000
		leaseSec = 30
	)
	store := newMemStore(now).addHook(testHook())
	req := store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	if _, err := store.Claim(context.Background(), req.ID, req.DateModified); err != nil {
		t.Fatal(err)
	}

	// The very next tick.
	store.advance(1)
	candidates, err := store.FetchClaimable(context.Background(),
		newClaimQuery(now+1, 8, leaseSec, 60))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Errorf("a claimed request must not be a candidate while its lease holds, got %d", len(candidates))
	}

	// And once the lease is over, it is: this is how a request claimed by a
	// process that then died gets delivered at all.
	store.advance(leaseSec)
	candidates, err = store.FetchClaimable(context.Background(),
		newClaimQuery(now+1+leaseSec, 8, leaseSec, 60))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Errorf("an abandoned claim must be retried once the lease expires, got %d candidates",
			len(candidates))
	}
}

// TestAFailedRequestWaitsTheRetryBackoff pins the 60 seconds Phorge waits.
// Before this the request went straight back into the queue and was retried on
// the next tick, so reaching the per-hook circuit breaker took ten failures a
// second apart rather than ten minutes of a broken endpoint.
func TestAFailedRequestWaitsTheRetryBackoff(t *testing.T) {
	const (
		now             = 1_000_000
		leaseSec        = 30
		retryBackoffSec = 60
	)
	store := newMemStore(now).addHook(testHook())
	req := store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	props := &RequestProperties{Retry: RetryForever}
	if err := store.UpdateResult(context.Background(), req.ID,
		attemptFailed(props, ErrorTypeHTTP, "500", now)); err != nil {
		t.Fatal(err)
	}
	if got := store.row(req.ID).Status; got != StatusQueued {
		t.Fatalf("a retry=forever failure must stay queued, got %q", got)
	}

	// Well past the lease, nowhere near the backoff. This is the case that
	// proves the two conditions are not interchangeable.
	store.advance(leaseSec + 1)
	candidates, err := store.FetchClaimable(context.Background(),
		newClaimQuery(now+leaseSec+1, 8, leaseSec, retryBackoffSec))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Errorf("a failed request must wait the retry backoff, not the lease, got %d candidates",
			len(candidates))
	}

	store.advance(retryBackoffSec)
	candidates, err = store.FetchClaimable(context.Background(),
		newClaimQuery(now+leaseSec+1+retryBackoffSec, 8, leaseSec, retryBackoffSec))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Errorf("expected the retry after the backoff, got %d candidates", len(candidates))
	}
}

// TestATerminalRequestIsNeverACandidate: `sent` and `failed` rows are kept for
// seven days by Phorge's garbage collection, so they are the bulk of the table
// and must never be looked at again.
func TestATerminalRequestIsNeverACandidate(t *testing.T) {
	store := newMemStore(1_000_000).addHook(testHook())

	for _, status := range []string{StatusSent, StatusFailed} {
		t.Run(status, func(t *testing.T) {
			req := store.addRequest(int64(len(store.requests)+1), testHookPHID,
				RequestProperties{Retry: RetryForever})
			props := &RequestProperties{}
			out := Outcome{Status: status, RequestResult: ResultOkay, Properties: props}
			if err := store.UpdateResult(context.Background(), req.ID, out); err != nil {
				t.Fatal(err)
			}

			candidates, err := store.FetchClaimable(context.Background(),
				newClaimQuery(store.now, 8, 30, 60))
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range candidates {
				if c.ID == req.ID {
					t.Errorf("a %s request must not be a candidate", status)
				}
			}

			// And a claim on it loses even if one is attempted, because the
			// statement's WHERE includes the status.
			won, err := store.Claim(context.Background(), req.ID, store.row(req.ID).DateModified)
			if err != nil {
				t.Fatal(err)
			}
			if won {
				t.Errorf("a %s request must not be claimable", status)
			}
		})
	}
}

// TestFetchClaimableHonoursTheLimit: the limit is MaxConcurrent, so a backlog
// is drained a tick at a time rather than read into memory whole.
func TestFetchClaimableHonoursTheLimit(t *testing.T) {
	store := newMemStore(1_000_000).addHook(testHook())
	for id := int64(1); id <= 5; id++ {
		store.addRequest(id, testHookPHID, RequestProperties{Retry: RetryForever})
	}

	candidates, err := store.FetchClaimable(context.Background(),
		newClaimQuery(1_000_000, 2, 30, 60))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected the limit to be honoured, got %d candidates", len(candidates))
	}
	// Oldest first, so a queue cannot starve its head.
	if candidates[0].ID != 1 || candidates[1].ID != 2 {
		t.Errorf("expected the two oldest requests, got %d and %d", candidates[0].ID, candidates[1].ID)
	}
}

// normaliseSQL collapses the indentation the statement constants carry, so the
// assertions above can be written as one line of SQL.
func normaliseSQL(stmt string) string {
	return strings.Join(strings.Fields(stmt), " ")
}
