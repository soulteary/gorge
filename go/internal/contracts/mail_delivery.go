package contracts

// MailDeliveryRequest is an immutable prepared email; generation belongs to
// the PHP producer. Reusing an identity with different content is a conflict.
type MailDeliveryRequest struct {
	AllowSend     *bool        `json:"allowSend,omitempty"`
	SchemaVersion int          `json:"schemaVersion"`
	DeliveryID    string       `json:"deliveryID"`
	MailID        int64        `json:"mailID"`
	Deadline      int64        `json:"deadline"`
	Message       EmailMessage `json:"message"`
	MailerURI     string       `json:"mailerURI,omitempty"`
	AdapterKey    string       `json:"adapterKey,omitempty"`
	MailerKeys    []string     `json:"mailerKeys,omitempty"`
}
type MailDeliveryResult struct {
	DeliveryID  string `json:"deliveryID"`
	State       string `json:"state"`
	Revision    int    `json:"revision"`
	Attempt     int    `json:"attempt"`
	NextAttempt int64  `json:"nextAttempt"`
	MailerKey   string `json:"mailerKey,omitempty"`
	MessageID   string `json:"messageId,omitempty"`
}
