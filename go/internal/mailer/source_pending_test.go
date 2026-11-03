package mailer

import (
	"context"
	"fmt"
	"testing"
)

func TestRetryableSourcePendingStopsDispatchWithoutProviderFallback(t *testing.T) {
	a, b := &gatedAdapter{}, &gatedAdapter{}
	d := &Dispatcher{adapters: []namedAdapter{{key: "a", adapter: a}, {key: "b", adapter: b}}, retry: RetryPolicy{MaxRetries: 3}}
	gates := 0
	_, err := d.SendAuthorized(context.Background(), testMessage(), nil, func(context.Context) error { gates++; return &SafeRetryError{Err: fmt.Errorf("source pending")} })
	if !CanRetry(err) || IsPermanent(err) || !isSourceGateError(err) || gates != 1 || a.calls != 0 || b.calls != 0 {
		t.Fatalf("pending dispatched or lost retry semantics: err=%v gates=%d sends=%d/%d", err, gates, a.calls, b.calls)
	}
	result, err := d.SendAuthorized(context.Background(), testMessage(), nil, func(context.Context) error { gates++; return nil })
	if err != nil || result.MailerKey != "a" || gates != 2 || a.calls != 1 || b.calls != 0 {
		t.Fatalf("fresh durable retry did not reauthorize exactly once: %v", err)
	}
}
func TestSourcePendingOnPhysicalRetryNeverReachesFallback(t *testing.T) {
	a, b := &gatedAdapter{fail: true}, &gatedAdapter{}
	d := &Dispatcher{adapters: []namedAdapter{{key: "a", adapter: a}, {key: "b", adapter: b}}, retry: RetryPolicy{MaxRetries: 2}}
	gates := 0
	_, err := d.SendAuthorized(context.Background(), testMessage(), nil, func(context.Context) error {
		gates++
		if gates == 2 {
			return &SafeRetryError{Err: fmt.Errorf("source pending")}
		}
		return nil
	})
	if !CanRetry(err) || !isSourceGateError(err) || gates != 2 || a.calls != 1 || b.calls != 0 {
		t.Fatalf("retry gate failure traversed fallback: %v gates=%d sends=%d/%d", err, gates, a.calls, b.calls)
	}
}
func TestPermanentSourceDenialRetainsPermanentOutcome(t *testing.T) {
	a, b := &gatedAdapter{}, &gatedAdapter{}
	d := &Dispatcher{adapters: []namedAdapter{{key: "a", adapter: a}, {key: "b", adapter: b}}}
	gates := 0
	_, err := d.SendAuthorized(context.Background(), testMessage(), nil, func(context.Context) error { gates++; return permanentf("source denied") })
	if !IsPermanent(err) || CanRetry(err) || gates != 1 || a.calls != 0 || b.calls != 0 {
		t.Fatalf("source denial changed outcome or dispatched: %v", err)
	}
}
