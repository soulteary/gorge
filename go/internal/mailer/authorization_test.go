package mailer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/soulteary/gorge/go/internal/contracts"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestAttemptAuthorizationPreservesSupportedLargeMessagePayloads(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > AttemptAuthorizationRequestLimit {
			t.Error("request exceeded transport budget")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		var params map[string]json.RawMessage
		if err := json.Unmarshal([]byte(r.Form.Get("params")), &params); err != nil {
			t.Error(err)
			return
		}
		var raw string
		if err := json.Unmarshal(params["message"], &raw); err != nil {
			t.Error(err)
			return
		}
		var msg contracts.EmailMessage
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Error(err)
			return
		}
		if len(msg.TextBody) <= 65536 && len(msg.Attachments) != 1 {
			t.Error("large payload did not survive callback")
		}
		calls++
		_, _ = w.Write([]byte(`{"result":{"allowed":true},"error_code":null,"error_info":null}`))
	}))
	defer srv.Close()
	gate, err := NewAttemptAuthorizer(srv.URL, "synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	request := contracts.MailDeliveryRequest{MailID: 1, DeliveryID: "synthetic", Authorization: &contracts.MailAuthorization{Version: 1, MailID: 1, DeliveryID: "synthetic", MessageHash: strings.Repeat("a", 64)}}
	for _, kind := range []string{"body", "attachment"} {
		msg := *testMessage()
		if kind == "body" {
			msg.TextBody = strings.Repeat("x", 70<<10)
		} else {
			msg.Attachments = []contracts.Attachment{{Filename: "synthetic.bin", MimeType: "application/octet-stream", Data: base64.StdEncoding.EncodeToString(make([]byte, 50<<10))}}
		}
		request.Message = msg
		if err := gate(context.Background(), request); err != nil {
			t.Fatalf("%s rejected: %v", kind, err)
		}
	}
	if calls != 2 {
		t.Fatal("large payloads did not consult source")
	}
	request.Message = *testMessage()
	request.Message.TextBody = strings.Repeat("x", AttemptAuthorizationMessageLimit+1)
	if err := gate(context.Background(), request); !IsPermanent(err) {
		t.Fatalf("oversized message not refused: %v", err)
	}
	if calls != 2 {
		t.Fatal("oversized message consulted source")
	}
	request.Message.TextBody = strings.Repeat("+", 4<<20)
	if err := gate(context.Background(), request); !IsPermanent(err) || !strings.Contains(err.Error(), "transport budget") {
		t.Fatalf("wire amplification not classified: %v", err)
	}
	if calls != 2 {
		t.Fatal("amplified request reached source")
	}
}

func TestAttemptAuthorizationMetadataBounds(t *testing.T) {
	if _, err := NewAttemptAuthorizer("http://source.invalid", strings.Repeat("x", 4097)); err == nil {
		t.Fatal("oversized callback token accepted")
	}
	gate, err := NewAttemptAuthorizer("http://source.invalid", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	req := contracts.MailDeliveryRequest{MailID: 1, DeliveryID: strings.Repeat("x", 129), Authorization: &contracts.MailAuthorization{Version: 1, MailID: 1, DeliveryID: strings.Repeat("x", 129), MessageHash: strings.Repeat("a", 64)}}
	if err := gate(context.Background(), req); !IsPermanent(err) {
		t.Fatal("oversized delivery binding did not stop before network")
	}
}
