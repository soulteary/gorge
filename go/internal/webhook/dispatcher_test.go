package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testHookPHID = "PHID-HWBH-hook00000000000000"
	testHmacKey  = "0123456789abcdef0123456789abcdef"
)

func testHook() *Hook {
	return &Hook{
		ID:         1,
		PHID:       testHookPHID,
		Name:       "test hook",
		WebhookURI: "http://127.0.0.1:1/unused",
		Status:     "active",
		HmacKey:    testHmacKey,
	}
}

// testConfig is the configuration the dispatcher tests run against: one
// delivery at a time, a one-second delivery timeout so ClaimLease does not
// raise the lease past what the tests move the clock by.
func testConfig() *Config {
	cfg := LoadFromEnv()
	cfg.DeliveryTimeout = 1
	cfg.MaxConcurrent = 4
	cfg.ClaimLeaseSec = 30
	cfg.RetryBackoffSec = 60
	cfg.ErrorBackoffSec = 300
	cfg.ErrorThreshold = 10
	return cfg
}

// newTestDispatcher builds a dispatcher that shares the fake store's clock, so
// the lease, the retry backoff and the circuit breaker's window can be moved
// by hand instead of waited out.
func newTestDispatcher(store *memStore, cfg *Config) *Dispatcher {
	d := NewDispatcher(store, cfg)
	d.now = store.clock
	return d
}

// receiver is a webhook endpoint under test: it records what arrived and
// answers what the test tells it to.
type receiver struct {
	server *httptest.Server

	mu         sync.Mutex
	bodies     []string
	signatures []string

	// block, when non-nil, holds each request until it is closed. It is what
	// makes a delivery observably *in flight*, which is the state the claim
	// exists to protect.
	block chan struct{}

	status int
}

func newReceiver(t *testing.T, status int) *receiver {
	t.Helper()

	r := &receiver{status: status}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)

		r.mu.Lock()
		r.bodies = append(r.bodies, string(body))
		r.signatures = append(r.signatures, req.Header.Get(SignatureHeader))
		block := r.block
		r.mu.Unlock()

		if block != nil {
			<-block
		}
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *receiver) body(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodies[i]
}

func (r *receiver) signature(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.signatures[i]
}

// hookTo builds a hook pointing at the receiver.
func hookTo(r *receiver) *Hook {
	h := testHook()
	h.WebhookURI = r.server.URL + "/hook"
	return h
}

// runTick runs exactly one poll and waits for the deliveries it started. It
// drives the real loop body rather than a test-only path, so the claim, the
// backoff check and the result write are all in play.
func runTick(t *testing.T, d *Dispatcher) {
	t.Helper()

	sem := make(chan struct{}, d.maxConcurrent)
	var inFlight sync.WaitGroup
	d.poll(context.Background(), sem, &inFlight)
	inFlight.Wait()
}

// startTick runs one poll without waiting, returning the wait. It is for the
// tests that need a delivery to still be in flight.
func startTick(d *Dispatcher) func() {
	sem := make(chan struct{}, d.maxConcurrent)
	var inFlight sync.WaitGroup
	d.poll(context.Background(), sem, &inFlight)
	return inFlight.Wait
}

func TestDeliversAQueuedRequest(t *testing.T) {
	quietLogs(t)

	rcv := newReceiver(t, http.StatusOK)
	store := newMemStore(1_000_000).addHook(hookTo(rcv))
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	runTick(t, newTestDispatcher(store, testConfig()))

	if rcv.count() != 1 {
		t.Fatalf("expected exactly one delivery, got %d", rcv.count())
	}

	row := store.row(1)
	if row.Status != StatusSent || row.LastRequestResult != ResultOkay {
		t.Errorf("row = %s/%s, want %s/%s",
			row.Status, row.LastRequestResult, StatusSent, ResultOkay)
	}
	// Phorge's UI shows "HTTP Status Code / 200" for a success, because its
	// own worker sets both before branching on the result. A delivery by this
	// service has to look the same.
	updates := store.recorded()
	if len(updates) != 1 {
		t.Fatalf("expected one result write, got %d", len(updates))
	}
	props := updates[0].Outcome.Properties
	if props.ErrorType != ErrorTypeHTTP || props.ErrorCode != "200" {
		t.Errorf("recorded %s/%s, want http/200", props.ErrorType, props.ErrorCode)
	}
	if updates[0].Outcome.Epoch == 0 {
		t.Error("a successful attempt must record when it happened")
	}
}

// TestTheSameRequestIsNotDeliveredTwice is the regression test for the defect
// this migration exists to fix. The standalone service polled every second,
// left the row `queued` for the whole attempt, and gave a delivery fifteen
// seconds to finish — so a single instance re-sent the same request up to
// fifteen times, to a receiver with no way to recognise the duplicates.
//
// The second tick here happens while the first delivery is still in flight,
// which is exactly the window that used to be open.
func TestTheSameRequestIsNotDeliveredTwice(t *testing.T) {
	quietLogs(t)

	rcv := newReceiver(t, http.StatusOK)
	rcv.block = make(chan struct{})

	store := newMemStore(1_000_000).addHook(hookTo(rcv))
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	d := newTestDispatcher(store, testConfig())

	waitFirst := startTick(d)

	// Wait for the delivery to actually be at the receiver, so the second
	// tick races the in-flight request rather than the goroutine start-up.
	waitFor(t, func() bool { return rcv.count() == 1 })

	// A tick one second later, as the real loop would.
	store.advance(1)
	runTick(t, d)

	close(rcv.block)
	waitFirst()

	if rcv.count() != 1 {
		t.Errorf("the request was delivered %d times; a claimed row must not be picked up again",
			rcv.count())
	}
	if got := store.claimCount(); got != 1 {
		t.Errorf("the second tick should not have reached the claim at all, got %d attempts", got)
	}
}

// TestPayloadIsByteExact pins the delivered document, and it is the assertion
// with the least room to move in this package: the signature is computed over
// these bytes, so a receiver validating against Phorge's own deliveries breaks
// if any of them change. Two-space indentation, key order, and the trailing
// newline all come from PhutilJSON::encodeFormatted.
func TestPayloadIsByteExact(t *testing.T) {
	quietLogs(t)

	rcv := newReceiver(t, http.StatusOK)
	store := newMemStore(1_700_000_000).addHook(hookTo(rcv))
	store.addRequest(1, testHookPHID, RequestProperties{
		Retry:            RetryForever,
		TransactionPHIDs: []string{"PHID-XACT-TASK-transaction01"},
		TriggerPHIDs:     []string{"PHID-HWTR-trigger0000000001"},
		Test:             true,
	})

	runTick(t, newTestDispatcher(store, testConfig()))

	const want = `{
  "object": {
    "type": "TASK",
    "phid": "PHID-TASK-abcdefghijklmnopqrst"
  },
  "triggers": [
    {
      "phid": "PHID-HWTR-trigger0000000001"
    }
  ],
  "action": {
    "test": true,
    "silent": false,
    "secure": false,
    "epoch": 1700000000
  },
  "transactions": [
    {
      "phid": "PHID-XACT-TASK-transaction01"
    }
  ]
}
`

	if got := rcv.body(0); got != want {
		t.Errorf("payload =\n%q\nwant\n%q", got, want)
	}
}

// TestPayloadCarriesEmptyListsRatherThanNull: a request with no transactions
// has to send `[]`, not `null`. A receiver iterating the list would fail on
// null, and Phorge's PHP always sends an array.
func TestPayloadCarriesEmptyListsRatherThanNull(t *testing.T) {
	quietLogs(t)

	rcv := newReceiver(t, http.StatusOK)
	store := newMemStore(1_700_000_000).addHook(hookTo(rcv))
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	runTick(t, newTestDispatcher(store, testConfig()))

	body := rcv.body(0)
	for _, want := range []string{`"triggers": []`, `"transactions": []`} {
		if !strings.Contains(body, want) {
			t.Errorf("payload must contain %s:\n%s", want, body)
		}
	}
}

// TestSignatureIsHMACOverTheExactBytes: the header is what a receiver
// validates, and it has to match the body as sent — not a re-serialisation of
// it, which is why the dispatcher signs the string it hands to the request.
func TestSignatureIsHMACOverTheExactBytes(t *testing.T) {
	quietLogs(t)

	rcv := newReceiver(t, http.StatusOK)
	store := newMemStore(1_700_000_000).addHook(hookTo(rcv))
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	runTick(t, newTestDispatcher(store, testConfig()))

	mac := hmac.New(sha256.New, []byte(testHmacKey))
	mac.Write([]byte(rcv.body(0)))
	want := hex.EncodeToString(mac.Sum(nil))

	if got := rcv.signature(0); got != want {
		t.Errorf("%s = %q, want the HMAC-SHA256 of the body, %q", SignatureHeader, got, want)
	}
}

// TestObjectTypeComesFromThePHID pins phid_get_type, including the answer for
// a PHID that has no type to extract.
func TestObjectTypeComesFromThePHID(t *testing.T) {
	for _, tc := range []struct{ phid, want string }{
		{"PHID-TASK-abcdefghijklmnopqrst", "TASK"},
		{"PHID-DREV-abcdefghijklmnopqrst", "DREV"},
		{"PHID-CMIT-a-b-c", "CMIT"},
		{"nonsense", ""},
		{"", ""},
	} {
		if got := phidType(tc.phid); got != tc.want {
			t.Errorf("phidType(%q) = %q, want %q", tc.phid, got, tc.want)
		}
	}
}

// TestAFailedDeliveryHonoursTheRetryMode: Phorge writes the mode into the
// request's properties and both implementations have to read it the same way,
// or a request Phorge would have retired is retried forever.
func TestAFailedDeliveryHonoursTheRetryMode(t *testing.T) {
	for _, tc := range []struct {
		retry      string
		wantStatus string
	}{
		{RetryForever, StatusQueued},
		{RetryNever, StatusFailed},
		// An unset mode is treated as "do not retry", which is what the PHP
		// comparison against RETRY_FOREVER does with it too.
		{"", StatusFailed},
	} {
		t.Run("retry="+tc.retry, func(t *testing.T) {
			quietLogs(t)

			rcv := newReceiver(t, http.StatusInternalServerError)
			store := newMemStore(1_000_000).addHook(hookTo(rcv))
			store.addRequest(1, testHookPHID, RequestProperties{Retry: tc.retry})

			runTick(t, newTestDispatcher(store, testConfig()))

			row := store.row(1)
			if row.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", row.Status, tc.wantStatus)
			}
			if row.LastRequestResult != ResultFail {
				t.Errorf("lastRequestResult = %q, want %q", row.LastRequestResult, ResultFail)
			}
			// The epoch is what the retry backoff measures from, so a failure
			// that did not record one would be retried on the next tick.
			if row.LastRequestEpoch == 0 {
				t.Error("a failed attempt must record its epoch")
			}
			if props := store.recorded()[0].Outcome.Properties; props.ErrorCode != "500" {
				t.Errorf("errorCode = %q, want the status code", props.ErrorCode)
			}
		})
	}
}

// TestATimeoutIsRecordedAsATimeout: Phorge's UI shows "Request Timeout"
// rather than "HTTP Status Code" for these, so a slow receiver has to be
// distinguishable from one that refused the connection.
func TestATimeoutIsRecordedAsATimeout(t *testing.T) {
	quietLogs(t)

	rcv := newReceiver(t, http.StatusOK)
	rcv.block = make(chan struct{})
	t.Cleanup(func() { close(rcv.block) })

	store := newMemStore(1_000_000).addHook(hookTo(rcv))
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	d := newTestDispatcher(store, testConfig())
	// Shortened here rather than in the config, whose smallest unit is a
	// second: the point is the classification, not the wait.
	d.client.Timeout = 50 * time.Millisecond

	runTick(t, d)

	props := store.recorded()[0].Outcome.Properties
	if props.ErrorType != ErrorTypeTimeout || props.ErrorCode != ErrorCodeTimeout {
		t.Errorf("recorded %s/%s, want %s/%s",
			props.ErrorType, props.ErrorCode, ErrorTypeTimeout, ErrorCodeTimeout)
	}
}

// TestAnUnreachableReceiverIsAnHTTPError: no response at all, but not a
// timeout either. The error string becomes the code, which is what Phorge's UI
// displays verbatim.
func TestAnUnreachableReceiverIsAnHTTPError(t *testing.T) {
	quietLogs(t)

	store := newMemStore(1_000_000).addHook(testHook()) // port 1, nothing listening
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	runTick(t, newTestDispatcher(store, testConfig()))

	props := store.recorded()[0].Outcome.Properties
	if props.ErrorType != ErrorTypeHTTP {
		t.Errorf("errorType = %q, want %q", props.ErrorType, ErrorTypeHTTP)
	}
	if props.ErrorCode == "" {
		t.Error("the transport error must be recorded as the code")
	}
}

// TestADisabledHookFailsTheRequestPermanently: ResultNone with a zero epoch,
// which is what Phorge's failRequest writes — and it keeps these rows out of
// the circuit breaker's count, since a hook is not broken because it is off.
func TestADisabledHookFailsTheRequestPermanently(t *testing.T) {
	quietLogs(t)

	rcv := newReceiver(t, http.StatusOK)
	hook := hookTo(rcv)
	hook.Status = HookStatusDisabled

	store := newMemStore(1_000_000).addHook(hook)
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	runTick(t, newTestDispatcher(store, testConfig()))

	if rcv.count() != 0 {
		t.Error("a disabled hook must not be delivered to")
	}
	assertConfigFailure(t, store, ErrorCodeDisabled)
}

// TestAMissingHookFailsTheRequestPermanently: Phorge's garbage collection can
// retire a hook while requests naming it are still queued, so this is an
// expected state and not an error to retry.
func TestAMissingHookFailsTheRequestPermanently(t *testing.T) {
	quietLogs(t)

	store := newMemStore(1_000_000)
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	runTick(t, newTestDispatcher(store, testConfig()))

	assertConfigFailure(t, store, ErrorCodeHookNotFound)
}

func TestUnparseablePropertiesFailTheRequestPermanently(t *testing.T) {
	quietLogs(t)

	store := newMemStore(1_000_000).addHook(testHook())
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})
	store.setRawProperties(1, "{not json")

	runTick(t, newTestDispatcher(store, testConfig()))

	assertConfigFailure(t, store, ErrorCodeInvalidProperties)
}

// TestAConfigurationFailureIsRecordedWithoutClaiming: every process would
// compute the same terminal state, so there is nothing to serialise — and
// claiming would leave the row under a lease for a decision already made.
func TestAConfigurationFailureIsRecordedWithoutClaiming(t *testing.T) {
	quietLogs(t)

	store := newMemStore(1_000_000)
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	runTick(t, newTestDispatcher(store, testConfig()))

	if got := store.claimCount(); got != 0 {
		t.Errorf("expected no claim for a configuration failure, got %d attempts", got)
	}
}

// TestTheCircuitBreakerStopsDeliveryToABrokenHook is the per-hook backoff, at
// Phorge's own window and threshold: ten failures inside five minutes.
func TestTheCircuitBreakerStopsDeliveryToABrokenHook(t *testing.T) {
	quietLogs(t)

	const now = 1_000_000
	rcv := newReceiver(t, http.StatusOK)
	store := newMemStore(now).addHook(hookTo(rcv))

	// Ten rows already recorded as failed inside the window.
	for id := int64(1); id <= 10; id++ {
		req := store.addRequest(id, testHookPHID, RequestProperties{Retry: RetryNever})
		props := &RequestProperties{Retry: RetryNever}
		if err := store.UpdateResult(context.Background(), req.ID,
			attemptFailed(props, ErrorTypeHTTP, "500", now)); err != nil {
			t.Fatal(err)
		}
	}
	// And one still queued.
	store.addRequest(11, testHookPHID, RequestProperties{Retry: RetryForever})

	runTick(t, newTestDispatcher(store, testConfig()))

	if rcv.count() != 0 {
		t.Errorf("expected no delivery while the hook is in error backoff, got %d", rcv.count())
	}
	// Left completely alone: not claimed, so the request is a candidate again
	// on the next tick rather than sitting out a lease, and not written, so
	// nothing about it looks like an attempt that happened.
	if got := store.claimCount(); got != 0 {
		t.Errorf("a hook in backoff must not have its requests claimed, got %d attempts", got)
	}
	if row := store.row(11); row.Status != StatusQueued || row.LastRequestResult != ResultNone {
		t.Errorf("row = %s/%s, want it untouched", row.Status, row.LastRequestResult)
	}
}

// TestTheCircuitBreakerRecoversAsTheWindowSlides: the window is measured from
// now on every check, so a hook comes back on its own. Nothing resets it,
// deliberately — there is no state to reset.
func TestTheCircuitBreakerRecoversAsTheWindowSlides(t *testing.T) {
	quietLogs(t)

	const now = 1_000_000
	rcv := newReceiver(t, http.StatusOK)
	store := newMemStore(now).addHook(hookTo(rcv))

	for id := int64(1); id <= 10; id++ {
		req := store.addRequest(id, testHookPHID, RequestProperties{Retry: RetryNever})
		props := &RequestProperties{Retry: RetryNever}
		if err := store.UpdateResult(context.Background(), req.ID,
			attemptFailed(props, ErrorTypeHTTP, "500", now)); err != nil {
			t.Fatal(err)
		}
	}
	store.addRequest(11, testHookPHID, RequestProperties{Retry: RetryForever})

	cfg := testConfig()
	// Past the whole window, so every recorded failure has aged out of it.
	store.advance(int64(cfg.ErrorBackoffSec) + 1)

	runTick(t, newTestDispatcher(store, cfg))

	if rcv.count() != 1 {
		t.Errorf("expected delivery to resume once the failures aged out, got %d", rcv.count())
	}
}

// TestADatabaseFailureDuringTheBackoffCheckHoldsTheRequest: the conservative
// direction. A request left in the queue is delivered a tick later; one
// delivered on a failed check may be delivered to an endpoint already
// drowning.
func TestADatabaseFailureDuringTheBackoffCheckHoldsTheRequest(t *testing.T) {
	quietLogs(t)

	rcv := newReceiver(t, http.StatusOK)
	store := newMemStore(1_000_000).addHook(hookTo(rcv))
	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})
	store.failuresErr = errStoreDown

	runTick(t, newTestDispatcher(store, testConfig()))

	if rcv.count() != 0 {
		t.Error("a failed backoff check must not be read as 'not in backoff'")
	}
	if row := store.row(1); row.Status != StatusQueued {
		t.Errorf("status = %q, want the request left queued", row.Status)
	}
}

// TestALostClaimDeliversNothing: the row belongs to whoever won it, and the
// loser must write nothing at all — a result write here would overwrite the
// winner's.
func TestALostClaimDeliversNothing(t *testing.T) {
	quietLogs(t)

	rcv := newReceiver(t, http.StatusOK)
	store := newMemStore(1_000_000).addHook(hookTo(rcv))
	req := store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})

	// Someone else claims it between the candidate query and this process's
	// claim, which is what a second instance racing on one row looks like.
	if _, err := store.Claim(context.Background(), req.ID, req.DateModified); err != nil {
		t.Fatal(err)
	}

	d := newTestDispatcher(store, testConfig())
	// The stale version this process is holding.
	d.processRequest(context.Background(), req)

	if rcv.count() != 0 {
		t.Error("a lost claim must not deliver")
	}
	if len(store.recorded()) != 0 {
		t.Error("a lost claim must not write a result")
	}
}

// TestAPollFailureIsSurvived: the database being briefly unreachable is not a
// reason to stop a loop whose next iteration retries, and it is already
// visible through /readyz.
func TestAPollFailureIsSurvived(t *testing.T) {
	logs := quietLogs(t)

	store := newMemStore(1_000_000).addHook(testHook())
	store.fetchErr = errStoreDown

	runTick(t, newTestDispatcher(store, testConfig()))

	if !strings.Contains(logs.String(), "WEBHOOK_POLL_FAILED") {
		t.Errorf("a failed poll must be logged:\n%s", logs.String())
	}
}

// TestRunStopsWithTheContext: the loop is what the process is for, so it has
// to end promptly enough for the caller to close the connection pool behind
// it.
func TestRunStopsWithTheContext(t *testing.T) {
	quietLogs(t)

	store := newMemStore(1_000_000)
	cfg := testConfig()
	cfg.PollIntervalMs = 10

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		newTestDispatcher(store, cfg).Run(ctx)
	}()

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestClaimLeaseCoversAWholeDeliveryAttempt: a lease shorter than the delivery
// timeout would reopen the duplicate-delivery window, since the row stays
// `queued` for the whole attempt.
func TestClaimLeaseCoversAWholeDeliveryAttempt(t *testing.T) {
	cfg := testConfig()
	cfg.DeliveryTimeout = 60
	cfg.ClaimLeaseSec = 5

	if got := cfg.ClaimLease(); got != 60 {
		t.Errorf("ClaimLease() = %d, want it raised to the delivery timeout", got)
	}

	d := NewDispatcher(newMemStore(1_000_000), cfg)
	if d.leaseSec != 60 {
		t.Errorf("the dispatcher took the unclamped lease: %d", d.leaseSec)
	}
}

func assertConfigFailure(t *testing.T, s *memStore, wantCode string) {
	t.Helper()

	updates := s.recorded()
	if len(updates) != 1 {
		t.Fatalf("expected one result write, got %d", len(updates))
	}
	out := updates[0].Outcome
	if out.Status != StatusFailed {
		t.Errorf("status = %q, want %q", out.Status, StatusFailed)
	}
	// ResultNone, not ResultFail: no delivery was attempted, and the circuit
	// breaker counts only attempts.
	if out.RequestResult != ResultNone {
		t.Errorf("lastRequestResult = %q, want %q", out.RequestResult, ResultNone)
	}
	if out.Epoch != 0 {
		t.Errorf("epoch = %d, want 0 for an attempt that never happened", out.Epoch)
	}
	if out.Properties.ErrorType != ErrorTypeHook || out.Properties.ErrorCode != wantCode {
		t.Errorf("recorded %s/%s, want %s/%s",
			out.Properties.ErrorType, out.Properties.ErrorCode, ErrorTypeHook, wantCode)
	}
}

// waitFor polls a condition rather than sleeping for a guessed duration, so
// the tests that need a delivery to be in flight are not timing-dependent.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the delivery to reach the receiver")
		}
		time.Sleep(time.Millisecond)
	}
}
