package mailer

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// Adapter is one delivery backend. Send reports the backend's own message id
// when it has one; SMTP and sendmail do not, and return "".
//
// ctx is the request context. Adapters that speak HTTP hand it to the client so
// a caller that walks away stops the call in flight; net/smtp has no context
// support, so the SMTP adapter can only observe cancellation between attempts.
type Adapter interface {
	Type() string
	Send(ctx context.Context, msg *contracts.EmailMessage) (messageID string, err error)
}

// PermanentError marks a failure no amount of retrying will fix — a malformed
// recipient, a rejected sender domain, a revoked API key.
//
// This is the distinction the whole error path exists for. Phorge's worker
// queue is the authoritative retry loop, and it re-queues anything that is not
// reported as permanent; a mistyped recipient address that comes back as a
// generic failure is therefore retried forever. The handler turns this type
// into 422 ERR_PERMANENT_FAILURE, which the PHP client raises as
// PhabricatorMetaMTAPermanentFailureException.
//
// When in doubt, do not return this: a transient failure misreported as
// permanent drops mail silently, while a permanent one misreported as transient
// only wastes worker cycles.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// permanentf builds a PermanentError from a formatted message.
func permanentf(format string, args ...any) error {
	return &PermanentError{Err: fmt.Errorf(format, args...)}
}

// IsPermanent reports whether err is, or wraps, a PermanentError.
func IsPermanent(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}

// classifyProviderStatus turns an HTTP provider's response status into an error
// of the right durability, or nil when the call succeeded. SES, SendGrid,
// Mailgun and Postmark all share these semantics.
//
// A 4xx is the provider rejecting this particular message — bad address,
// unverified sender, bad credentials — and every retry produces the same
// answer. 429 is the exception, because rate limiting says "not now" rather
// than "not ever". Everything else, 5xx included, is the provider having a bad
// moment, which is what the retry loop and the failover to the next adapter
// exist for.
func classifyProviderStatus(provider string, status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	err := fmt.Errorf("%s: HTTP %d: %s", provider, status, string(body))
	if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
		return &PermanentError{Err: err}
	}
	return err
}

// NewAdapter builds the backend a spec names.
func NewAdapter(spec MailerSpec) (Adapter, error) {
	switch spec.Type {
	case "smtp":
		return newSMTPAdapter(spec.Options)
	case "sendmail":
		return newSendmailAdapter(spec.Options)
	case "ses":
		return newSESAdapter(spec.Options)
	case "sendgrid":
		return newSendGridAdapter(spec.Options)
	case "mailgun":
		return newMailgunAdapter(spec.Options)
	case "postmark":
		return newPostmarkAdapter(spec.Options)
	case "test":
		return newTestAdapter(spec.Options)
	default:
		return nil, fmt.Errorf("unknown mailer type: %s", spec.Type)
	}
}
