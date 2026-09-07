package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// timeouter is what the net and url errors implement when they know the
// failure was a timeout rather than a refusal.
type timeouter interface{ Timeout() bool }

// Dispatcher is the poll loop: it takes queued requests out of Phorge's herald
// database, POSTs each one to its hook's URI, and writes the result back to
// the row it came from.
//
// It is the reason this binary exists. The HTTP port only reports on it.
type Dispatcher struct {
	store  Store
	client *http.Client

	pollInterval    time.Duration
	maxConcurrent   int
	leaseSec        int
	retryBackoffSec int
	errorBackoffSec int
	errorThreshold  int64

	// now is the clock every window in this file is measured against: the
	// lease, the retry backoff and the circuit breaker's five minutes. It is a
	// field so the tests can hold it still — those three windows are the whole
	// of the claim's behaviour, and a test that had to wait real seconds for
	// them could only assert the parts that are not about time.
	//
	// It is not the only clock involved. The claim's own version bump uses the
	// database's UNIX_TIMESTAMP(), because the value has to advance whichever
	// process wrote it last.
	now func() int64

	// hookLock serialises deliveries per hook. Phorge does the same thing
	// with a global lock named `webhook(PHID)` and for the same reason it
	// gives there: a hook with a backlog would otherwise have every request in
	// it delivered at once, and the circuit breaker below cannot catch up with
	// a burst it is measured against.
	mu       sync.Mutex
	hookLock map[string]*sync.Mutex
}

// NewDispatcher builds the loop from the service configuration.
func NewDispatcher(store Store, cfg *Config) *Dispatcher {
	return &Dispatcher{
		store: store,
		client: &http.Client{
			Timeout: time.Duration(cfg.DeliveryTimeout) * time.Second,
		},
		pollInterval:    time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		maxConcurrent:   cfg.MaxConcurrent,
		leaseSec:        cfg.ClaimLease(),
		retryBackoffSec: cfg.RetryBackoffSec,
		errorBackoffSec: cfg.ErrorBackoffSec,
		errorThreshold:  int64(cfg.ErrorThreshold),
		now:             func() int64 { return time.Now().Unix() },
		hookLock:        make(map[string]*sync.Mutex),
	}
}

// Run polls until ctx is cancelled, then waits for the deliveries already in
// flight before returning.
//
// The wait is what lets the caller close the connection pool without pulling
// it out from under a result write. It does not make those writes succeed:
// they share this context, so cancelling it fails them, and a request whose
// terminal state was lost that way is delivered again after the lease. That is
// a known limitation rather than an oversight; see docs/findings.md.
func (d *Dispatcher) Run(ctx context.Context) {
	slog.Info("dispatcher started",
		"poll", d.pollInterval,
		"concurrency", d.maxConcurrent,
		"lease_sec", d.leaseSec,
		"retry_backoff_sec", d.retryBackoffSec,
		"error_backoff_sec", d.errorBackoffSec,
		"error_threshold", d.errorThreshold)

	// The semaphore caps deliveries in flight across every hook, so a queue
	// full of work cannot open an unbounded number of sockets.
	sem := make(chan struct{}, d.maxConcurrent)
	var inFlight sync.WaitGroup

	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("dispatcher stopping, draining deliveries in flight")
			inFlight.Wait()
			return
		case <-ticker.C:
			d.poll(ctx, sem, &inFlight)
		}
	}
}

// poll runs one tick.
//
// A failure here is logged and dropped rather than propagated: the database
// being briefly unreachable is not a reason to stop a loop whose next
// iteration will retry, and it is already visible through /readyz.
func (d *Dispatcher) poll(ctx context.Context, sem chan struct{}, inFlight *sync.WaitGroup) {
	query := newClaimQuery(d.now(), d.maxConcurrent, d.leaseSec, d.retryBackoffSec)

	requests, err := d.store.FetchClaimable(ctx, query)
	if err != nil {
		slog.Error("WEBHOOK_POLL_FAILED", "error", err)
		return
	}

	for _, req := range requests {
		// Acquiring the slot before starting the goroutine is what bounds the
		// goroutine count as well as the socket count. Waiting on ctx too
		// keeps a shutdown from being held up by a full semaphore.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		inFlight.Add(1)
		go func() {
			defer inFlight.Done()
			defer func() { <-sem }()
			d.processRequest(ctx, req)
		}()
	}
}

// processRequest carries one request from candidate to result.
//
// The order of the steps is the contract. A configuration failure — properties
// that do not parse, a hook that is gone or disabled — retires the request
// without claiming it, because it does not matter which process records a
// terminal state that every process would compute identically. Everything
// after that is guarded: the claim is the last thing before the POST, so a
// process that spends time waiting for the hook lock or counting failures
// cannot deliver a request another process has since taken.
func (d *Dispatcher) processRequest(ctx context.Context, req *Request) {
	var props RequestProperties
	if err := json.Unmarshal([]byte(req.Properties), &props); err != nil {
		slog.Error("WEBHOOK_PROPERTIES_INVALID", "request", req.PHID, "error", err)
		d.record(ctx, req, configFailed(&props, ErrorTypeHook, ErrorCodeInvalidProperties))
		return
	}

	hook, err := d.store.GetWebhook(ctx, req.WebhookPHID)
	if err != nil {
		// Not a decision about the request, just a database that did not
		// answer. Left alone, it is a candidate again next tick.
		slog.Error("WEBHOOK_HOOK_LOOKUP_FAILED",
			"request", req.PHID, "hook", req.WebhookPHID, "error", err)
		return
	}
	if hook == nil {
		slog.Warn("WEBHOOK_HOOK_MISSING", "request", req.PHID, "hook", req.WebhookPHID)
		d.record(ctx, req, configFailed(&props, ErrorTypeHook, ErrorCodeHookNotFound))
		return
	}
	if hook.Disabled() {
		slog.Info("WEBHOOK_HOOK_DISABLED", "request", req.PHID, "hook", hook.PHID)
		d.record(ctx, req, configFailed(&props, ErrorTypeHook, ErrorCodeDisabled))
		return
	}

	unlock := d.lockHook(hook.PHID)
	defer unlock()

	if d.inErrorBackoff(ctx, hook.PHID) {
		return
	}

	claimed, err := d.store.Claim(ctx, req.ID, req.DateModified)
	if err != nil {
		slog.Error("WEBHOOK_CLAIM_FAILED", "request", req.PHID, "error", err)
		return
	}
	if !claimed {
		// Another attempt owns this row. Nothing is written: the owner will
		// record the outcome.
		slog.Debug("WEBHOOK_CLAIM_LOST", "request", req.PHID)
		return
	}

	d.deliver(ctx, req, hook, &props)
}

// lockHook returns the release function for this hook's serialisation lock.
func (d *Dispatcher) lockHook(hookPHID string) func() {
	d.mu.Lock()
	lk, ok := d.hookLock[hookPHID]
	if !ok {
		lk = &sync.Mutex{}
		d.hookLock[hookPHID] = lk
	}
	d.mu.Unlock()

	lk.Lock()
	return lk.Unlock
}

// inErrorBackoff reports whether the hook has failed too often too recently to
// be worth another attempt. It mirrors HeraldWebhook::isInErrorBackoff, down
// to the window and the threshold.
//
// The window slides, so recovery needs no reset: as the old failures age out
// of it the count falls back under the threshold and deliveries resume.
//
// A counting failure is reported as "in backoff", the conservative direction:
// a request left in the queue is delivered a tick later, while one delivered
// on a database error may be delivered to an endpoint that is already
// drowning.
func (d *Dispatcher) inErrorBackoff(ctx context.Context, hookPHID string) bool {
	since := d.now() - int64(d.errorBackoffSec)

	failures, err := d.store.CountRecentFailures(ctx, hookPHID, since)
	if err != nil {
		slog.Error("WEBHOOK_BACKOFF_CHECK_FAILED", "hook", hookPHID, "error", err)
		return true
	}
	if failures < d.errorThreshold {
		return false
	}

	slog.Warn("WEBHOOK_HOOK_IN_ERROR_BACKOFF",
		"hook", hookPHID, "failures", failures,
		"threshold", d.errorThreshold, "window_sec", d.errorBackoffSec)
	return true
}

// deliver POSTs the payload and records what came back.
func (d *Dispatcher) deliver(ctx context.Context, req *Request, hook *Hook, props *RequestProperties) {
	payload, err := buildPayload(req, props)
	if err != nil {
		// Unreachable in practice: the payload is PHIDs, booleans and an
		// integer. Recorded as a hook error rather than dropped, so a request
		// cannot sit in the queue forever with nothing to explain it.
		slog.Error("WEBHOOK_PAYLOAD_FAILED", "request", req.PHID, "error", err)
		d.record(ctx, req, attemptFailed(props, ErrorTypeHook,
			ErrorCodeRequestBuild, d.now()))
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.WebhookURI,
		strings.NewReader(payload))
	if err != nil {
		// A URI Phorge accepted but Go will not parse. It cannot improve with
		// a retry, but the retry mode is honoured anyway rather than special
		// cased: it is Phorge's setting and this is a delivery failure.
		slog.Error("WEBHOOK_REQUEST_BUILD_FAILED",
			"request", req.PHID, "uri", hook.WebhookURI, "error", err)
		d.record(ctx, req, attemptFailed(props, ErrorTypeHook,
			ErrorCodeRequestBuild, d.now()))
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(SignatureHeader, signPayload(payload, hook.HmacKey))

	start := time.Now()
	resp, err := d.client.Do(httpReq)
	duration := time.Since(start)
	now := d.now()

	if err != nil {
		errorType, errorCode := classifyTransportError(ctx, err)
		slog.Warn("WEBHOOK_DELIVERY_FAILED",
			"request", req.PHID, "hook", hook.PHID, "uri", hook.WebhookURI,
			"duration", duration, "error_type", errorType, "error", err)
		d.record(ctx, req, attemptFailed(props, errorType, errorCode, now))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// The body is read and discarded rather than ignored. Nothing here wants
	// it, but a body left unread keeps the connection from being reused, and
	// this service talks to the same few endpoints over and over.
	_, _ = io.Copy(io.Discard, resp.Body)

	statusCode := strconv.Itoa(resp.StatusCode)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		slog.Info("WEBHOOK_DELIVERED",
			"request", req.PHID, "hook", hook.PHID,
			"status", resp.StatusCode, "duration", duration)
		d.record(ctx, req, delivered(props, statusCode, now))
		return
	}

	slog.Warn("WEBHOOK_DELIVERY_REJECTED",
		"request", req.PHID, "hook", hook.PHID,
		"status", resp.StatusCode, "duration", duration)
	d.record(ctx, req, attemptFailed(props, ErrorTypeHTTP, statusCode, now))
}

// record writes an outcome back to the request row.
//
// A failed write is logged and nothing else: there is no second place to put
// the result, and the row is still queued or still claimed, so the next
// attempt after the lease expires is the recovery path.
func (d *Dispatcher) record(ctx context.Context, req *Request, out Outcome) {
	if err := d.store.UpdateResult(ctx, req.ID, out); err != nil {
		slog.Error("WEBHOOK_RESULT_WRITE_FAILED",
			"request", req.PHID, "status", out.Status, "error", err)
	}
}

// buildPayload serialises the delivered document.
//
// **The bytes are a contract, not a formatting preference.** Two spaces of
// indentation and a trailing newline are what Phorge's
// PhutilJSON::encodeFormatted produces, and the signature is computed over
// exactly these bytes — so a receiver that validates signatures against
// Phorge's own deliveries keeps working only as long as this function does not
// change. See contracts/webhook.go.
func buildPayload(req *Request, props *RequestProperties) (string, error) {
	triggers := make([]contracts.PayloadTrigger, 0, len(props.TriggerPHIDs))
	for _, phid := range props.TriggerPHIDs {
		triggers = append(triggers, contracts.PayloadTrigger{PHID: phid})
	}

	transactions := make([]contracts.PayloadTransaction, 0, len(props.TransactionPHIDs))
	for _, phid := range props.TransactionPHIDs {
		transactions = append(transactions, contracts.PayloadTransaction{PHID: phid})
	}

	payload := contracts.WebhookPayload{
		Object: contracts.PayloadObject{
			Type: phidType(req.ObjectPHID),
			PHID: req.ObjectPHID,
		},
		Triggers: triggers,
		Action: contracts.PayloadAction{
			Test:   props.Test,
			Silent: props.Silent,
			Secure: props.Secure,
			// The row's creation time, not this attempt's: a retry describes
			// the same event as the first try.
			Epoch: req.DateCreated,
		},
		Transactions: transactions,
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded) + "\n", nil
}

// signPayload is PhabricatorHash::digestHMACSHA256 — HMAC-SHA256 over the
// payload with the hook's key, lower-case hex.
func signPayload(payload, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// phidType is phid_get_type: the middle segment of `PHID-TASK-…`. An
// unrecognisable PHID yields an empty type rather than an error, which is what
// the PHP function does with one too.
func phidType(phid string) string {
	parts := strings.SplitN(phid, "-", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// classifyTransportError decides what a failure with no HTTP response was.
//
// The distinction is `timeout` versus `http`, which is what Phorge's UI shows
// as "Request Timeout" or "HTTP Status Code" — so a receiver that is slow has
// to be distinguishable from one that refused the connection. A cancelled
// context counts as a timeout because the only thing that cancels one here is
// the process shutting down mid-delivery, and the receiver did in fact not
// answer in time.
func classifyTransportError(ctx context.Context, err error) (string, string) {
	if ctx.Err() != nil {
		return ErrorTypeTimeout, ErrorCodeTimeout
	}
	// errors.As rather than a type assertion: the client wraps the transport's
	// error in a *url.Error, and a future wrap would silently turn every
	// timeout into an `http` error without it.
	var timeout timeouter
	if errors.As(err, &timeout) && timeout.Timeout() {
		return ErrorTypeTimeout, ErrorCodeTimeout
	}
	// The error string is the code, which is how the standalone service
	// recorded it and what Phorge's UI shows verbatim. It is a URL and a
	// syscall message, not an internal detail.
	return ErrorTypeHTTP, err.Error()
}
