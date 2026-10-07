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
// ctx bounds network submission. SMTP uses a cancellable connection; HTTP
// adapters pass it to their transport. Cancellation does not prove nonacceptance.
type Adapter interface {
	Type() string
	Send(ctx context.Context, msg *contracts.EmailMessage) (messageID string, err error)
}

// PermanentError marks a failure no amount of retrying will fix — a malformed
// recipient or a rejected sender domain. Credentials are backend errors.
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
// A 4xx explicitly rejects the message. Credentials (401/403) describe a
// backend configuration failure, and rate limiting (429) permits a later
// attempt. A 5xx or unexpected redirect cannot prove nonacceptance and must
// remain unknown, without automatic retries or provider failover.
func classifyProviderStatus(provider string, status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	err := fmt.Errorf("%s: HTTP %d: %s", provider, status, string(body))
	if status == 401 || status == 403 {
		return &SafeRetryError{Err: err, Backend: true}
	}
	if status == http.StatusTooManyRequests {
		return &SafeRetryError{Err: err}
	}
	if status >= 400 && status < 500 {
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
