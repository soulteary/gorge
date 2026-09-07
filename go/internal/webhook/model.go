package webhook

// The values below are Phorge's, spelled as HeraldWebhookRequest spells them.
// They are read and written by both implementations, so none of them may be
// renamed, and — more importantly — **the status set may not be extended**.
// Phorge's UI renders an icon per status and HeraldWebhookWorker::doWork
// refuses any request that is not `queued`, so a `claimed` value invented here
// would break the interface and the PHP fallback path at once. That is the
// constraint the whole claim mechanism in store.go is built around.
const (
	// RetryForever keeps a failed request in the queue; anything else — in
	// practice RetryNever — retires it on the first failure. Phorge writes
	// this into the request's properties when it creates the row.
	RetryNever   = "never"
	RetryForever = "forever"

	StatusQueued = "queued"
	StatusFailed = "failed"
	StatusSent   = "sent"

	// ResultNone means no delivery was attempted, which is what a request
	// failed for a configuration reason records. It is not the same as
	// ResultFail, and the circuit breaker counts only the latter.
	ResultNone = "none"
	ResultOkay = "okay"
	ResultFail = "fail"

	ErrorTypeHook    = "hook"
	ErrorTypeHTTP    = "http"
	ErrorTypeTimeout = "timeout"

	// Error codes for ErrorTypeHook. `disabled` is Phorge's own
	// HeraldWebhookRequest::ERROR_DISABLED and renders as "Hook Disabled" in
	// the UI. The other two have no PHP counterpart and render as themselves,
	// which is deliberate — inventing a display string would mean patching
	// Phorge to describe a state only this service can produce.
	ErrorCodeDisabled          = "disabled"
	ErrorCodeHookNotFound      = "not-found"
	ErrorCodeInvalidProperties = "invalid-properties"
	ErrorCodeRequestBuild      = "request-build-error"
	ErrorCodeTimeout           = "timeout"

	// HookStatusDisabled is herald_webhook.status for a hook an administrator
	// has turned off.
	HookStatusDisabled = "disabled"
)

// SignatureHeader carries the HMAC-SHA256 of the delivered payload.
//
// The name is Phorge's and is a hard compatibility boundary: every existing
// receiver validates this header, and one that stopped arriving under its old
// name would leave receivers rejecting deliveries — or, worse, accepting
// unsigned ones.
const SignatureHeader = "X-Phabricator-Webhook-Signature"

// Request is one row of herald_webhookrequest.
//
// DateModified is load-bearing rather than informational: it is the optimistic
// version this domain claims a row with. See Store.Claim.
type Request struct {
	ID                int64
	PHID              string
	WebhookPHID       string
	ObjectPHID        string
	Status            string
	Properties        string
	LastRequestResult string
	LastRequestEpoch  int64
	DateCreated       int64
	DateModified      int64
}

// RequestProperties is the JSON document in herald_webhookrequest.properties.
//
// Phorge writes it and reads it back, so this struct has to round-trip the
// keys it does not care about as well as the ones it sets: the result write
// replaces the whole column. The `omitempty` tags match how Lisk's JSON
// serialisation leaves absent properties out.
type RequestProperties struct {
	Retry            string   `json:"retry,omitempty"`
	ErrorType        string   `json:"errorType,omitempty"`
	ErrorCode        string   `json:"errorCode,omitempty"`
	TransactionPHIDs []string `json:"transactionPHIDs,omitempty"`
	TriggerPHIDs     []string `json:"triggerPHIDs,omitempty"`
	Silent           bool     `json:"silent,omitempty"`
	Test             bool     `json:"test,omitempty"`
	Secure           bool     `json:"secure,omitempty"`
}

// Hook is one row of herald_webhook. This table is Phorge's to manage; the
// service only reads it.
type Hook struct {
	ID         int64
	PHID       string
	Name       string
	WebhookURI string
	Status     string
	HmacKey    string
}

// Disabled reports whether an administrator has turned the hook off. A request
// for a disabled hook is failed permanently rather than held, which is what
// HeraldWebhookWorker does with it too.
func (h *Hook) Disabled() bool { return h.Status == HookStatusDisabled }

// Outcome is what one delivery attempt writes back to the request row.
//
// ErrorType and ErrorCode live in Properties rather than beside it, because
// that is where Phorge keeps them: they are properties of the request, not
// columns. Building the outcome through the helpers below is what keeps the
// two in step.
type Outcome struct {
	Status        string
	RequestResult string
	// Epoch is written to lastRequestEpoch. It is 0 for an attempt that never
	// happened, which is how Phorge's own failRequest records one.
	Epoch      int64
	Properties *RequestProperties
}

// delivered is the outcome of a 2xx response.
//
// It records errorType `http` and the status code as errorCode even though
// nothing failed, because that is what the PHP worker leaves behind: it sets
// both before branching on the result, so a successful request in Phorge's UI
// shows "HTTP Status Code / 200". Omitting them here would make a delivery by
// this service look different from one by Phorge.
func delivered(props *RequestProperties, statusCode string, epoch int64) Outcome {
	props.ErrorType = ErrorTypeHTTP
	props.ErrorCode = statusCode
	return Outcome{
		Status:        StatusSent,
		RequestResult: ResultOkay,
		Epoch:         epoch,
		Properties:    props,
	}
}

// attemptFailed is the outcome of a delivery that was made and did not
// succeed. The retry mode decides whether the request goes back into the queue
// or is retired.
func attemptFailed(props *RequestProperties, errorType, errorCode string, epoch int64) Outcome {
	props.ErrorType = errorType
	props.ErrorCode = errorCode

	status := StatusFailed
	if props.Retry == RetryForever {
		status = StatusQueued
	}
	return Outcome{
		Status:        status,
		RequestResult: ResultFail,
		Epoch:         epoch,
		Properties:    props,
	}
}

// configFailed is the outcome of a request that could not be attempted at all:
// its hook is gone, disabled, or its properties do not parse.
//
// It ignores the retry mode, and has to: no number of retries makes a deleted
// hook reappear. ResultNone with a zero epoch is how Phorge's failRequest
// records the same thing, which also keeps these rows out of the circuit
// breaker's count — a hook is not broken because a request naming it was.
func configFailed(props *RequestProperties, errorType, errorCode string) Outcome {
	props.ErrorType = errorType
	props.ErrorCode = errorCode
	return Outcome{
		Status:        StatusFailed,
		RequestResult: ResultNone,
		Epoch:         0,
		Properties:    props,
	}
}
