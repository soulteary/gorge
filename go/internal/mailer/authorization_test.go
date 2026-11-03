package mailer

import (
	"context"
	"fmt"
	"github.com/soulteary/gorge/go/internal/contracts"
	"testing"
)

type gatedAdapter struct {
	calls int
	fail  bool
}

func (a *gatedAdapter) Type() string { return "synthetic" }
func (a *gatedAdapter) Send(context.Context, *contracts.EmailMessage) (string, error) {
	a.calls++
	if a.fail {
		return "", &SafeRetryError{Err: fmt.Errorf("synthetic unavailable"), Backend: true}
	}
	return "synthetic", nil
}
func TestAuthorizationRunsBeforeEveryPhysicalRetry(t *testing.T) {
	a := &gatedAdapter{fail: true}
	d := &Dispatcher{adapters: []namedAdapter{{key: "a", adapter: a}}, retry: RetryPolicy{MaxRetries: 2}}
	gates := 0
	_, err := d.SendAuthorized(context.Background(), testMessage(), nil, func(context.Context) error {
		gates++
		if gates == 2 {
			return permanentf("revoked")
		}
		return nil
	})
	if !IsPermanent(err) || gates != 2 || a.calls != 1 {
		t.Fatalf("gate did not stop actual second send: %v gates%d sends%d", err, gates, a.calls)
	}
}
func TestAuthorizationRunsBeforeFallbackAndUnknownNeverSends(t *testing.T) {
	a := &gatedAdapter{fail: true}
	b := &gatedAdapter{}
	d := &Dispatcher{adapters: []namedAdapter{{key: "a", adapter: a}, {key: "b", adapter: b}}}
	gates := 0
	_, err := d.SendAuthorized(context.Background(), testMessage(), nil, func(context.Context) error {
		gates++
		if gates > 1 {
			return permanentf("revoked")
		}
		return nil
	})
	if !IsPermanent(err) || a.calls != 1 || b.calls != 0 || gates != 2 {
		t.Fatalf("fallback escaped authorization: %v", err)
	}
	a.calls = 0
	b.calls = 0
	_, err = d.SendAuthorized(context.Background(), testMessage(), nil, func(context.Context) error {
		return &SafeRetryError{Err: fmt.Errorf("source unavailable"), Backend: true}
	})
	if err == nil || a.calls != 0 || b.calls != 0 {
		t.Fatal("unknown authorization sent bytes")
	}
	_, err = d.SendAuthorized(context.Background(), testMessage(), nil, nil)
	if !IsPermanent(err) || a.calls != 0 || b.calls != 0 {
		t.Fatal("missing gate sent bytes")
	}
}
func TestAttemptAuthorizationRequiresBoundMessageAndFixedSource(t *testing.T) {
	for _, uri := range []string{"", "file:///tmp/private", "http://user:secret@source.invalid", "http://source.invalid/?token=x"} {
		if _, err := NewAttemptAuthorizer(uri, "synthetic"); err == nil {
			t.Fatal("unsafe callback configuration")
		}
	}
	authorizer, err := NewAttemptAuthorizer("http://source.invalid/", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range []*contracts.MailAuthorization{nil, {Version: 1, MailID: 2, DeliveryID: "x", MessageHash: "bad"}, {Version: 2, MailID: 1, DeliveryID: "x", MessageHash: "bad"}} {
		err = authorizer(context.Background(), contracts.MailDeliveryRequest{MailID: 1, DeliveryID: "x", Authorization: binding})
		if !IsPermanent(err) {
			t.Fatal("unbound callback did not fail before network")
		}
	}
}
