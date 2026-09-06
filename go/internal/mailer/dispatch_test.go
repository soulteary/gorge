package mailer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// noRetry is what most of these tests want: one attempt per adapter, so a
// failure shows up immediately instead of after a wait.
var noRetry = RetryPolicy{}

func testMessage() *contracts.EmailMessage {
	return &contracts.EmailMessage{
		From:    contracts.Address{Address: "sender@example.com"},
		To:      []contracts.Address{{Address: "rcpt@example.com"}},
		Subject: "Hello",
	}
}

func newTestDispatcher(t *testing.T, specs ...MailerSpec) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(specs, noRetry)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNewDispatcherRejectsUnknownType(t *testing.T) {
	_, err := NewDispatcher([]MailerSpec{{Key: "bad", Type: "foobar"}}, noRetry)
	if err == nil || !strings.Contains(err.Error(), "unknown mailer type") {
		t.Fatalf("expected an unknown mailer type error, got: %v", err)
	}
}

func TestDispatcherSend(t *testing.T) {
	d := newTestDispatcher(t, MailerSpec{Key: "primary", Type: "test"})

	result, err := d.Send(context.Background(), testMessage())
	if err != nil {
		t.Fatal(err)
	}
	if result.MailerKey != "primary" {
		t.Errorf("expected primary, got %s", result.MailerKey)
	}
	if result.MessageID != "test-1" {
		t.Errorf("expected test-1, got %s", result.MessageID)
	}
}

func TestDispatcherSendWithKeys(t *testing.T) {
	d := newTestDispatcher(t,
		MailerSpec{Key: "a", Type: "test", Priority: 10},
		MailerSpec{Key: "b", Type: "test", Priority: 5},
	)

	result, err := d.SendWith(context.Background(), testMessage(), []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	if result.MailerKey != "b" {
		t.Errorf("expected b, got %s", result.MailerKey)
	}
}

func TestDispatcherSendWithUnknownKey(t *testing.T) {
	d := newTestDispatcher(t, MailerSpec{Key: "a", Type: "test"})

	_, err := d.SendWith(context.Background(), testMessage(), []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "no matching mailers") {
		t.Fatalf("expected a no-matching-mailers error, got: %v", err)
	}
}

func TestDispatcherPriorityOrder(t *testing.T) {
	d := newTestDispatcher(t,
		MailerSpec{Key: "low", Type: "test", Priority: 1},
		MailerSpec{Key: "high", Type: "test", Priority: 100},
		MailerSpec{Key: "mid", Type: "test", Priority: 50},
	)

	keys := d.MailerKeys()
	if keys[0] != "high" || keys[1] != "mid" || keys[2] != "low" {
		t.Fatalf("expected [high mid low], got %v", keys)
	}
}

func TestDispatcherMailerInfo(t *testing.T) {
	d := newTestDispatcher(t, MailerSpec{Key: "sg", Type: "test", Priority: 10})

	info := d.MailerInfo()
	if len(info) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(info))
	}
	if info[0].Key != "sg" || info[0].Type != "test" || info[0].Priority != 10 {
		t.Fatalf("unexpected info: %+v", info[0])
	}
}

// TestDispatcherReadyRequiresABackend is the whole of the /readyz contract. The
// state it catches — service listening, zero backends — used to report healthy
// while every message failed.
func TestDispatcherReadyRequiresABackend(t *testing.T) {
	empty := newTestDispatcher(t)
	if empty.Ready() == nil {
		t.Error("a dispatcher with no adapters must not report ready")
	}

	_, err := empty.Send(context.Background(), testMessage())
	if err == nil || !strings.Contains(err.Error(), "no mailers configured") {
		t.Fatalf("expected a no-mailers error, got: %v", err)
	}

	configured := newTestDispatcher(t, MailerSpec{Key: "t", Type: "test"})
	if err := configured.Ready(); err != nil {
		t.Errorf("one configured adapter is enough to be ready, got: %v", err)
	}
}

// TestDispatcherFailsOverOnTemporaryFailure is the reason several backends may
// be configured at once.
func TestDispatcherFailsOverOnTemporaryFailure(t *testing.T) {
	d := newTestDispatcher(t,
		MailerSpec{Key: "flaky", Type: "test", Priority: 10, Options: map[string]string{"fail": "temporary"}},
		MailerSpec{Key: "backup", Type: "test", Priority: 5},
	)

	result, err := d.Send(context.Background(), testMessage())
	if err != nil {
		t.Fatal(err)
	}
	if result.MailerKey != "backup" {
		t.Errorf("expected the failover to backup, got %s", result.MailerKey)
	}
}

// TestDispatcherDoesNotFailOverOnPermanentFailure is the counterpart: a message
// no backend will accept must not be walked through every backend, and must
// reach the caller as a PermanentError so Phorge stops re-queueing it.
func TestDispatcherDoesNotFailOverOnPermanentFailure(t *testing.T) {
	d := newTestDispatcher(t,
		MailerSpec{Key: "rejects", Type: "test", Priority: 10, Options: map[string]string{"fail": "permanent"}},
		MailerSpec{Key: "backup", Type: "test", Priority: 5},
	)

	_, err := d.Send(context.Background(), testMessage())
	if !IsPermanent(err) {
		t.Fatalf("expected a permanent error, got: %v", err)
	}

	backup := d.adapters[1].adapter.(*testAdapter)
	if backup.Attempts() != 0 {
		t.Errorf("the backup must not be tried after a permanent rejection, got %d attempts",
			backup.Attempts())
	}
}

func TestDispatcherRetriesOneAdapterBeforeFailingOver(t *testing.T) {
	d, err := NewDispatcher([]MailerSpec{
		{Key: "flaky", Type: "test", Priority: 10,
			Options: map[string]string{"fail": "temporary", "fail-times": "2"}},
		{Key: "backup", Type: "test", Priority: 5},
	}, RetryPolicy{MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}

	result, err := d.Send(context.Background(), testMessage())
	if err != nil {
		t.Fatal(err)
	}
	// Two failures then a success, all on the first adapter: retrying the
	// backend that just failed comes before reaching for a different one.
	if result.MailerKey != "flaky" {
		t.Errorf("expected the retried adapter to win, got %s", result.MailerKey)
	}
	if attempts := d.adapters[0].adapter.(*testAdapter).Attempts(); attempts != 3 {
		t.Errorf("expected 3 attempts (1 + 2 retries), got %d", attempts)
	}
	if attempts := d.adapters[1].adapter.(*testAdapter).Attempts(); attempts != 0 {
		t.Errorf("the backup should not have been reached, got %d attempts", attempts)
	}
}

// TestDispatcherRetriesAreBoundedByMaxRetries pins the count itself: MaxRetries
// counts retries, not attempts, so N means N+1 calls.
func TestDispatcherRetriesAreBoundedByMaxRetries(t *testing.T) {
	d, err := NewDispatcher([]MailerSpec{
		{Key: "down", Type: "test", Options: map[string]string{"fail": "temporary"}},
	}, RetryPolicy{MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := d.Send(context.Background(), testMessage()); err == nil {
		t.Fatal("expected the send to fail")
	}
	if attempts := d.adapters[0].adapter.(*testAdapter).Attempts(); attempts != 4 {
		t.Errorf("expected 4 attempts (1 + 3 retries), got %d", attempts)
	}
}

// TestDispatcherDoesNotRetryPermanentFailures: retrying a rejected message
// wastes the caller's whole timeout budget producing the same answer.
func TestDispatcherDoesNotRetryPermanentFailures(t *testing.T) {
	d, err := NewDispatcher([]MailerSpec{
		{Key: "rejects", Type: "test", Options: map[string]string{"fail": "permanent"}},
	}, RetryPolicy{MaxRetries: 5, RetryWait: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	// The hour-long wait is the assertion: reaching it at all would hang here.
	if _, err := d.Send(context.Background(), testMessage()); !IsPermanent(err) {
		t.Fatalf("expected a permanent error, got: %v", err)
	}
	if attempts := d.adapters[0].adapter.(*testAdapter).Attempts(); attempts != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", attempts)
	}
}

// TestDispatcherRetryLoopStopsWithTheRequestContext is what keeps a disconnected
// client from leaving a retry loop running behind it.
func TestDispatcherRetryLoopStopsWithTheRequestContext(t *testing.T) {
	d, err := NewDispatcher([]MailerSpec{
		{Key: "down", Type: "test", Options: map[string]string{"fail": "temporary"}},
	}, RetryPolicy{MaxRetries: 1000, RetryWait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := d.Send(ctx, testMessage()); err == nil {
		t.Fatal("expected the send to fail")
	}
	// 1000 retries at 50ms would be just under a minute.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the retry loop outlived its context by %v", elapsed)
	}
	if attempts := d.adapters[0].adapter.(*testAdapter).Attempts(); attempts > 10 {
		t.Errorf("expected the loop to stop within a few attempts, got %d", attempts)
	}
}
