package mailer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestNewAdapterKnowsEveryBackend(t *testing.T) {
	cases := []struct {
		mailerType string
		options    map[string]string
	}{
		{"smtp", nil},
		{"sendmail", nil},
		{"ses", map[string]string{"access-key": "AK", "secret-key": "SK"}},
		{"sendgrid", map[string]string{"api-key": "SG.x"}},
		{"mailgun", map[string]string{"api-key": "k", "domain": "example.com"}},
		{"postmark", map[string]string{"access-token": "t"}},
		{"test", nil},
	}

	for _, tc := range cases {
		t.Run(tc.mailerType, func(t *testing.T) {
			a, err := NewAdapter(MailerSpec{Key: "k", Type: tc.mailerType, Options: tc.options})
			if err != nil {
				t.Fatal(err)
			}
			if a.Type() != tc.mailerType {
				t.Errorf("expected %s, got %s", tc.mailerType, a.Type())
			}
		})
	}
}

// TestNewAdapterRejectsMissingCredentials: a provider adapter built without its
// key would fail every message at send time instead of at startup, and /readyz
// would report a backend that cannot work.
func TestNewAdapterRejectsMissingCredentials(t *testing.T) {
	for _, mailerType := range []string{"ses", "sendgrid", "mailgun", "postmark"} {
		if mailerType == "ses" {
			// SES is the exception: it may run against an endpoint that
			// authenticates some other way, so it validates nothing here.
			continue
		}
		if _, err := NewAdapter(MailerSpec{Key: "k", Type: mailerType}); err == nil {
			t.Errorf("%s: expected an error when no credentials are configured", mailerType)
		}
	}
}

// TestClassifyProviderStatus is the shared rule behind SES, SendGrid, Mailgun
// and Postmark. 429 is the case worth reading twice: it is a 4xx that means
// "not now", so treating it like the other 4xx would drop mail whenever a
// provider throttled.
func TestClassifyProviderStatus(t *testing.T) {
	cases := []struct {
		status    int
		wantErr   bool
		permanent bool
	}{
		{200, false, false},
		{202, false, false},
		{400, true, true},
		{401, true, true},
		{403, true, true},
		{422, true, true},
		{429, true, false},
		{500, true, false},
		{503, true, false},
	}

	for _, tc := range cases {
		err := classifyProviderStatus("provider", tc.status, []byte("body"))
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("status %d: expected an error", tc.status)
		case !tc.wantErr && err != nil:
			t.Errorf("status %d: expected success, got %v", tc.status, err)
		case tc.wantErr && IsPermanent(err) != tc.permanent:
			t.Errorf("status %d: expected permanent=%v, got %v", tc.status, tc.permanent, err)
		}
	}
}

// TestClassifySMTPError applies RFC 5321's own distinction: a 5xx reply is a
// permanent negative completion, a 4xx explicitly means "try again later", and
// a failure with no reply code at all never reached the point of being judged.
func TestClassifySMTPError(t *testing.T) {
	if err := classifySMTPError(nil); err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	permanent := classifySMTPError(&textproto.Error{Code: 550, Msg: "No such user here"})
	if !IsPermanent(permanent) {
		t.Errorf("a 550 must be permanent, got %v", permanent)
	}

	for _, code := range []int{421, 450, 451} {
		err := classifySMTPError(&textproto.Error{Code: code, Msg: "try later"})
		if err == nil || IsPermanent(err) {
			t.Errorf("a %d must be transient, got %v", code, err)
		}
	}

	// A refused connection or a DNS failure carries no reply code: the server
	// never had a chance to reject the message.
	network := classifySMTPError(&net.OpError{Op: "dial", Err: errors.New("connection refused")})
	if network == nil || IsPermanent(network) {
		t.Errorf("a connection failure must be transient, got %v", network)
	}
}

// TestSendmailClassifiesExitCodes runs a stub in place of sendmail so the exit
// code travels the real path: exec.ExitError, then the sysexits table.
func TestSendmailClassifiesExitCodes(t *testing.T) {
	cases := []struct {
		name      string
		exitCode  int
		permanent bool
	}{
		{"EX_NOUSER", 67, true},
		{"EX_DATAERR", 65, true},
		{"EX_NOHOST", 68, true},
		{"EX_TEMPFAIL", 75, false},
		{"EX_OSERR", 71, false},
		// An unrecognised code is transient on purpose: retrying needlessly
		// costs worker cycles, while a wrong "permanent" drops the mail.
		{"unknown", 3, false},
	}

	a, err := newSendmailAdapter(map[string]string{"path": stubSendmail(t)})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(stubExitVar, strconv.Itoa(tc.exitCode))

			_, sendErr := a.Send(context.Background(), testMessage())
			if sendErr == nil {
				t.Fatal("expected the send to fail")
			}
			if IsPermanent(sendErr) != tc.permanent {
				t.Errorf("expected permanent=%v, got %v", tc.permanent, sendErr)
			}
		})
	}
}

func TestSendmailSucceeds(t *testing.T) {
	a, err := newSendmailAdapter(map[string]string{"path": stubSendmail(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(stubExitVar, "0")

	messageID, err := a.Send(context.Background(), testMessage())
	if err != nil {
		t.Fatal(err)
	}
	if messageID != "" {
		t.Errorf("local delivery reports no message id, got %q", messageID)
	}
}

// stubExitVar tells the stub below what to exit with. The adapter runs the
// binary with the process environment, which is what makes one script enough
// for every case.
const stubExitVar = "GORGE_TEST_SENDMAIL_EXIT"

// stubSendmail writes a script that consumes the message on stdin, the way
// sendmail does, and exits with $GORGE_TEST_SENDMAIL_EXIT.
func stubSendmail(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sendmail")
	script := "#!/bin/sh\ncat >/dev/null\nexit \"${" + stubExitVar + ":-0}\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSESReportsStatusDurability drives a real HTTP round trip, so the signing,
// the request shape and the status classification are all in play. SES is the
// one provider whose endpoint is configurable, which is what makes this
// possible without a network.
func TestSESReportsStatusDurability(t *testing.T) {
	const success = `<SendRawEmailResponse><SendRawEmailResult>` +
		`<MessageId>0100-abc</MessageId></SendRawEmailResult></SendRawEmailResponse>`

	cases := []struct {
		name      string
		status    int
		body      string
		wantErr   bool
		permanent bool
		messageID string
	}{
		{"accepted", http.StatusOK, success, false, false, "0100-abc"},
		{"rejected", http.StatusBadRequest, "<Error>MessageRejected</Error>", true, true, ""},
		{"throttled", http.StatusTooManyRequests, "<Error>Throttling</Error>", true, false, ""},
		{"unavailable", http.StatusServiceUnavailable, "<Error>Unavailable</Error>", true, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth, gotAction string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				gotAuth = r.Header.Get("Authorization")
				gotAction = r.Form.Get("Action")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			a, err := newSESAdapter(map[string]string{
				"access-key": "AKIAEXAMPLE",
				"secret-key": "secret",
				"region":     "eu-west-1",
				"endpoint":   srv.URL,
			})
			if err != nil {
				t.Fatal(err)
			}

			messageID, sendErr := a.Send(context.Background(), testMessage())
			if tc.wantErr {
				if sendErr == nil {
					t.Fatal("expected an error")
				}
				if IsPermanent(sendErr) != tc.permanent {
					t.Errorf("expected permanent=%v, got %v", tc.permanent, sendErr)
				}
				return
			}
			if sendErr != nil {
				t.Fatal(sendErr)
			}
			if messageID != tc.messageID {
				t.Errorf("expected %q, got %q", tc.messageID, messageID)
			}
			// SendRawEmail, not SendEmail: the higher-level action would drop
			// the attachments and custom headers the contract carries.
			if gotAction != "SendRawEmail" {
				t.Errorf("expected SendRawEmail, got %q", gotAction)
			}
			if !strings.Contains(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") {
				t.Errorf("unexpected authorization header: %q", gotAuth)
			}
		})
	}
}

// TestSESCancelledRequestStopsInFlight: the HTTP backends take the request
// context, so a caller that walked away stops the call rather than the retry
// loop only noticing afterwards.
func TestSESCancelledRequestStopsInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	a, err := newSESAdapter(map[string]string{"endpoint": srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := a.Send(ctx, testMessage()); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a cancellation, got %v", err)
	}
}

// TestSendGridPayloadShape pins the field names SendGrid's v3 API requires.
// They are snake_case and nested in ways that do not match the contract, so a
// rename here is invisible until a real send fails.
func TestSendGridPayloadShape(t *testing.T) {
	a := &sendGridAdapter{apiKey: "SG.x"}
	msg := &contracts.EmailMessage{
		From:     contracts.Address{Name: "Phorge", Address: "noreply@example.com"},
		ReplyTo:  &contracts.Address{Address: "reply@example.com"},
		To:       []contracts.Address{{Address: "to@example.com"}},
		CC:       []contracts.Address{{Address: "cc@example.com"}},
		Subject:  "Subject",
		TextBody: "text",
		HTMLBody: "<p>html</p>",
		Headers:  []contracts.Header{{Name: "X-Custom", Value: "v"}},
		Attachments: []contracts.Attachment{
			{Filename: "a.txt", MimeType: "text/plain", Data: "SGVsbG8="},
		},
	}

	payload := a.buildPayload(msg)
	for _, key := range []string{"personalizations", "from", "subject", "content", "reply_to", "attachments"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("missing %q", key)
		}
	}

	personalizations := payload["personalizations"].([]any)
	p := personalizations[0].(map[string]any)
	for _, key := range []string{"to", "cc", "headers"} {
		if _, ok := p[key]; !ok {
			t.Errorf("missing personalizations[0].%q", key)
		}
	}

	// Attachments go out base64, exactly as they arrived.
	atts := payload["attachments"].([]map[string]string)
	if atts[0]["content"] != "SGVsbG8=" {
		t.Errorf("attachment content should pass through verbatim, got %q", atts[0]["content"])
	}
}

// TestPostmarkPayloadShape pins Postmark's PascalCase field names for the same
// reason.
func TestPostmarkPayloadShape(t *testing.T) {
	a := &postmarkAdapter{accessToken: "t"}
	msg := &contracts.EmailMessage{
		From:     contracts.Address{Address: "noreply@example.com"},
		To:       []contracts.Address{{Address: "to@example.com"}, {Address: "to2@example.com"}},
		Subject:  "Subject",
		TextBody: "text",
		Attachments: []contracts.Attachment{
			{Filename: "a.bin", Data: "SGVsbG8="},
		},
	}

	payload := a.buildPayload(msg)
	for _, key := range []string{"From", "To", "Subject", "TextBody", "Attachments"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("missing %q", key)
		}
	}
	// Postmark takes recipients as one comma-separated string, not a list.
	if to := payload["To"].(string); to != "to@example.com, to2@example.com" {
		t.Errorf("unexpected To: %q", to)
	}
	// An attachment with no declared type still needs one.
	atts := payload["Attachments"].([]map[string]string)
	if atts[0]["ContentType"] != "application/octet-stream" {
		t.Errorf("expected a fallback content type, got %q", atts[0]["ContentType"])
	}
}

// TestMailgunRejectsNonBase64Attachments: Mailgun is the one backend that
// decodes attachments, so it is the one that can catch a payload the contract
// says is base64 and is not. Re-sending it will not make it so.
func TestMailgunRejectsNonBase64Attachments(t *testing.T) {
	a, err := newMailgunAdapter(map[string]string{"api-key": "k", "domain": "example.com"})
	if err != nil {
		t.Fatal(err)
	}

	msg := testMessage()
	msg.Attachments = []contracts.Attachment{{Filename: "bad.txt", Data: "not base64!!"}}

	if _, err := a.Send(context.Background(), msg); !IsPermanent(err) {
		t.Fatalf("expected a permanent error, got %v", err)
	}
}

func TestSMTPAdapterDefaults(t *testing.T) {
	a, err := newSMTPAdapter(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if a.host != "localhost" || a.port != 25 {
		t.Errorf("expected localhost:25, got %s:%d", a.host, a.port)
	}

	a, err = newSMTPAdapter(map[string]string{"host": "mail.example.com", "port": "587"})
	if err != nil {
		t.Fatal(err)
	}
	if a.host != "mail.example.com" || a.port != 587 {
		t.Errorf("expected mail.example.com:587, got %s:%d", a.host, a.port)
	}

	// An unparseable port keeps the default rather than becoming 0, which would
	// fail to dial with a message naming no port at all.
	a, err = newSMTPAdapter(map[string]string{"port": "not-a-number"})
	if err != nil {
		t.Fatal(err)
	}
	if a.port != 25 {
		t.Errorf("expected the default port, got %d", a.port)
	}
}

func TestTestAdapterRejectsUnknownFailMode(t *testing.T) {
	if _, err := newTestAdapter(map[string]string{"fail": "sometimes"}); err == nil {
		t.Error("expected an error for an unknown fail mode")
	}
}

// TestPermanentErrorUnwrap: IsPermanent leans on errors.As reaching the wrapped
// error, so Unwrap has to hand back exactly what permanentf put in. errors.Is
// against a sentinel is the observable payoff.
func TestPermanentErrorUnwrap(t *testing.T) {
	sentinel := errors.New("bad recipient")
	err := &PermanentError{Err: sentinel}

	if unwrapped := errors.Unwrap(err); unwrapped != sentinel {
		t.Errorf("Unwrap() = %v, want the wrapped error", unwrapped)
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is must reach the wrapped sentinel through Unwrap")
	}
	// The message is the wrapped error's own, not a decorated one.
	if err.Error() != sentinel.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), sentinel.Error())
	}
}

// permanentf builds a PermanentError whose wrapped error carries the formatted
// message, and IsPermanent recognises it — including when it is wrapped again
// with %w further up a call chain.
func TestPermanentfIsPermanent(t *testing.T) {
	err := permanentf("rejected %s", "sender@example.com")
	if !IsPermanent(err) {
		t.Fatal("permanentf must produce a permanent error")
	}
	if !strings.Contains(err.Error(), "sender@example.com") {
		t.Errorf("the formatted message is lost: %v", err)
	}

	wrapped := fmt.Errorf("dispatch: %w", err)
	if !IsPermanent(wrapped) {
		t.Error("IsPermanent must see through an outer wrap")
	}
}

// TestTestAdapterReset: Send records messages and counts attempts, and Reset
// puts both back to zero so a fixture can reuse one adapter across cases.
func TestTestAdapterReset(t *testing.T) {
	a, err := newTestAdapter(nil)
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if _, err := a.Send(context.Background(), testMessage()); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.Messages()) != 3 || a.Attempts() != 3 {
		t.Fatalf("before reset: messages=%d attempts=%d, want 3/3", len(a.Messages()), a.Attempts())
	}

	a.Reset()

	if len(a.Messages()) != 0 {
		t.Errorf("Reset must clear the recorded messages, got %d", len(a.Messages()))
	}
	if a.Attempts() != 0 {
		t.Errorf("Reset must zero the attempt counter, got %d", a.Attempts())
	}

	// The message id counter resets too: the next accepted send is test-1 again.
	id, err := a.Send(context.Background(), testMessage())
	if err != nil {
		t.Fatal(err)
	}
	if id != "test-1" {
		t.Errorf("expected the id counter reset to 1, got %q", id)
	}
}
