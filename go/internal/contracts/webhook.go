package contracts

// The webhook domain has two wire contracts pointing in opposite directions,
// and only one of them is an HTTP API of this repository's own.
//
// DeliveryStats and HookSummary are the read-only status endpoints
// PhabricatorGorgeWebhookClient calls, so their field names are the contract.
//
// WebhookPayload is the other direction: it is the document delivered to a
// third-party endpoint, and it is the one structure in this package that is
// pinned **byte for byte** rather than field for field. The receiving end
// verifies an HMAC-SHA256 signature computed over the serialised bytes, so the
// key order below, the two-space indentation and the trailing newline added by
// the dispatcher are all part of it — a receiver written against Phorge's own
// PHP delivery must keep validating signatures unchanged. See
// compat/phorge/README.md.

// DeliveryStats is the payload of GET /api/webhook/stats.
//
// Every count is a live COUNT(*) over Phorge's herald tables rather than a
// counter this service keeps, so the numbers survive a restart and describe
// the queue as Phorge's own UI sees it. ActiveWebhooks counts hooks that are
// not disabled, which is why it is not simply "the number of hooks" —
// GET /api/webhook/hooks answers that.
type DeliveryStats struct {
	QueuedCount    int64 `json:"queuedCount"`
	SentCount      int64 `json:"sentCount"`
	FailedCount    int64 `json:"failedCount"`
	ActiveWebhooks int64 `json:"activeWebhooks"`
}

// HookSummary is the payload of GET /api/webhook/hooks: every configured hook,
// disabled ones included.
type HookSummary struct {
	Total int64 `json:"total"`
}

// WebhookPayload is the JSON document POSTed to a hook's URI.
//
// It carries identifiers and nothing else — no titles, no comment text, no
// field values. That is Phorge's design and not a simplification made here: a
// receiver is expected to call back through Conduit for anything it needs,
// which keeps the payload safe to send to an endpoint that may not be
// authorised to see the object.
type WebhookPayload struct {
	Object       PayloadObject        `json:"object"`
	Triggers     []PayloadTrigger     `json:"triggers"`
	Action       PayloadAction        `json:"action"`
	Transactions []PayloadTransaction `json:"transactions"`
}

// PayloadObject names the object whose change triggered the delivery. Type is
// the middle segment of the PHID (`TASK` for `PHID-TASK-…`), so a receiver can
// route on it without a Conduit round trip.
type PayloadObject struct {
	Type string `json:"type"`
	PHID string `json:"phid"`
}

// PayloadTrigger is one Herald rule that fired. A single-field object rather
// than a bare string because that is the shape Phorge emits.
type PayloadTrigger struct {
	PHID string `json:"phid"`
}

// PayloadAction describes the delivery itself.
//
// Epoch is the request row's dateCreated — when the change happened, not when
// this attempt was made — so a retried delivery carries the same value as the
// first attempt and a receiver can order events by it.
type PayloadAction struct {
	Test   bool  `json:"test"`
	Silent bool  `json:"silent"`
	Secure bool  `json:"secure"`
	Epoch  int64 `json:"epoch"`
}

// PayloadTransaction is one transaction on the object, by PHID only.
type PayloadTransaction struct {
	PHID string `json:"phid"`
}
