package mailer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

type providerTransport func(*http.Request) (*http.Response, error)

func (f providerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countedBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *countedBody) Read(p []byte) (int, error) { n, e := b.Reader.Read(p); b.read += n; return n, e }
func (b *countedBody) Close() error               { b.closed = true; return nil }

func httpAdapterForTest(t *testing.T, name string, client *http.Client) Adapter {
	t.Helper()
	a, err := NewAdapter(MailerSpec{Type: name, Options: map[string]string{"api-key": "test", "domain": "example.com", "access-token": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	switch x := a.(type) {
	case *sendGridAdapter:
		x.client = client
	case *postmarkAdapter:
		x.client = client
	case *sesAdapter:
		x.client = client
	case *mailgunAdapter:
		x.client = client
	}
	return a
}

func TestProviderBodyBudgetPreservesAcceptanceSemantics(t *testing.T) {
	for _, name := range []string{"sendgrid", "ses", "mailgun", "postmark"} {
		t.Run(name, func(t *testing.T) {
			body := &countedBody{Reader: strings.NewReader(strings.Repeat("x", int(ProviderResponseLimit*2)))}
			client := &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
			})}
			_, err := httpAdapterForTest(t, name, client).Send(context.Background(), testMessage())
			confirmedByStatus := name == "sendgrid" || name == "ses"
			if confirmedByStatus && err != nil {
				t.Fatalf("accepted message lost its receipt: %v", err)
			}
			if !confirmedByStatus && (err == nil || CanRetry(err) || IsPermanent(err)) {
				t.Fatalf("incomplete receipt must remain unknown: %v", err)
			}
			if body.read != int(ProviderResponseLimit+1) || !body.closed {
				t.Fatalf("body budget not enforced: read=%d closed=%v", body.read, body.closed)
			}
		})
	}
}

func TestProviderTimeoutNeverRetriesUncertainSubmission(t *testing.T) {
	for _, name := range []string{"sendgrid", "ses", "mailgun", "postmark"} {
		t.Run(name, func(t *testing.T) {
			client := newProviderClient()
			client.Timeout = 15 * time.Millisecond
			calls := 0
			client.Transport = providerTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			primary := httpAdapterForTest(t, name, client)
			backup, _ := newTestAdapter(nil)
			d := &Dispatcher{adapters: []namedAdapter{{key: name, adapter: primary}, {key: "backup", adapter: backup}}, retry: RetryPolicy{MaxRetries: 5}}
			start := time.Now()
			_, err := d.Send(context.Background(), testMessage())
			if err == nil || CanRetry(err) || IsPermanent(err) {
				t.Fatalf("timeout must be unknown: %v", err)
			}
			if calls != 1 || backup.Attempts() != 0 || time.Since(start) > time.Second {
				t.Fatalf("timeout repeated/outlived submission: calls=%d backup=%d", calls, backup.Attempts())
			}
		})
	}
}

type interruptedReceipt struct{ body string }

func (r *interruptedReceipt) Read(p []byte) (int, error) {
	n := copy(p, r.body)
	r.body = r.body[n:]
	return n, io.ErrUnexpectedEOF
}
func (*interruptedReceipt) Close() error { return nil }

// Even syntactically complete-looking JSON is not a complete HTTP receipt
// when its body reports truncation. The old adapters ignored ReadAll errors.
func TestProviderIncompleteReceiptNeverConfirmsAcceptance(t *testing.T) {
	for name, receipt := range map[string]string{
		"mailgun":  `{"id":"receipt","message":"Queued"}`,
		"postmark": `{"ErrorCode":0,"MessageID":"receipt"}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &interruptedReceipt{body: receipt}}, nil
			})}
			_, err := httpAdapterForTest(t, name, client).Send(context.Background(), testMessage())
			if err == nil || CanRetry(err) || IsPermanent(err) {
				t.Fatalf("incomplete receipt must remain unknown: %v", err)
			}
		})
	}
}

type stalledProviderBody struct {
	ctx    context.Context
	closed bool
}

func (b *stalledProviderBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *stalledProviderBody) Close() error { b.closed = true; return nil }

func TestProviderTimeoutIncludesResponseBody(t *testing.T) {
	client := newProviderClient()
	client.Timeout = 15 * time.Millisecond
	var body *stalledProviderBody
	client.Transport = providerTransport(func(r *http.Request) (*http.Response, error) {
		body = &stalledProviderBody{ctx: r.Context()}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})
	start := time.Now()
	_, err := httpAdapterForTest(t, "mailgun", client).Send(context.Background(), testMessage())
	if err == nil || CanRetry(err) || IsPermanent(err) || !body.closed || time.Since(start) > time.Second {
		t.Fatalf("response body timeout lost safety/budget: closed=%v err=%v", body.closed, err)
	}
}

func TestProviderExplicitRejectionSurvivesOversizedDiagnostics(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 503} {
		client := &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", int(ProviderResponseLimit*2))))}, nil
		})}
		req, _ := http.NewRequest("POST", "https://provider.invalid", nil)
		_, _, err := callProvider("provider", client, req)
		wantRetry := status == 401 || status == 403 || status == 429
		if err == nil || CanRetry(err) != wantRetry || IsPermanent(err) != (status == 400) {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}

func TestProviderDialFailureIsSafeButOtherTransportErrorsAreUnknown(t *testing.T) {
	for _, err := range []error{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, errors.New("response connection lost")} {
		client := &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) { return nil, err })}
		req, _ := http.NewRequest("POST", "https://provider.invalid", nil)
		_, _, got := callProvider("provider", client, req)
		var network *net.OpError
		if CanRetry(got) != errors.As(err, &network) {
			t.Fatalf("unexpected safety classification: %v", got)
		}
	}
}

func TestProviderRedirectDoesNotRepeatOrForwardMessage(t *testing.T) {
	client := newProviderClient()
	calls := 0
	client.Transport = providerTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://other.invalid"}}, Body: io.NopCloser(strings.NewReader("redirect"))}, nil
	})
	req, _ := http.NewRequest("POST", "https://provider.invalid", strings.NewReader("private message"))
	_, _, err := callProvider("provider", client, req)
	if err == nil || CanRetry(err) || calls != 1 {
		t.Fatalf("redirect forwarded or allowed retry: calls=%d err=%v", calls, err)
	}
}

func TestPostmarkMissingAcceptanceReceiptIsUnknown(t *testing.T) {
	for _, body := range []string{`{}`, `{"ErrorCode":0}`, `{"ErrorCode":0,"MessageID":"receipt"}`} {
		client := &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		_, err := httpAdapterForTest(t, "postmark", client).Send(context.Background(), testMessage())
		wantSuccess := strings.Contains(body, "receipt")
		if (err == nil) != wantSuccess || CanRetry(err) {
			t.Fatalf("body %s: %v", body, err)
		}
	}
}

type deadlineAdapter struct {
	calls    int
	deadline time.Time
}

func (a *deadlineAdapter) Type() string { return "deadline" }
func (a *deadlineAdapter) Send(ctx context.Context, _ *contracts.EmailMessage) (string, error) {
	a.calls++
	a.deadline, _ = ctx.Deadline()
	<-ctx.Done()
	return "", ctx.Err()
}

func TestSynchronousSendDeadlineReturnsUnknownWithoutFailover(t *testing.T) {
	a := &deadlineAdapter{}
	backup, _ := newTestAdapter(nil)
	deps := &Deps{Token: testToken, SendTimeout: 15 * time.Millisecond, Dispatcher: &Dispatcher{adapters: []namedAdapter{{key: "primary", adapter: a}, {key: "backup", adapter: backup}}, retry: RetryPolicy{MaxRetries: 5}}}
	start := time.Now()
	resp, body := postSend(t, newTestServer(t, deps), validSend)
	assertErrorCode(t, resp, body, 502, CodeOutcomeUnknown)
	if a.deadline.IsZero() || a.calls != 1 || backup.Attempts() != 0 || time.Since(start) > time.Second {
		t.Fatalf("unbounded/repeated send: deadline=%v calls=%d backup=%d", a.deadline, a.calls, backup.Attempts())
	}
}

func TestSynchronousDeadlineDuringSafeRetryWaitRemainsRetryable(t *testing.T) {
	d := newTestDispatcher(t, MailerSpec{Key: "down", Type: "test", Options: map[string]string{"fail": "temporary"}})
	d.retry = RetryPolicy{MaxRetries: 5, RetryWait: time.Hour}
	deps := &Deps{Token: testToken, SendTimeout: 15 * time.Millisecond, Dispatcher: d}
	resp, body := postSend(t, newTestServer(t, deps), validSend)
	assertErrorCode(t, resp, body, 502, CodeSendFailed)
	if d.adapters[0].adapter.(*testAdapter).Attempts() != 1 {
		t.Fatal("submission repeated after cancellation")
	}
}
