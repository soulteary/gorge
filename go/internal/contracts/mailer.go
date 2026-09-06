package contracts

// The mailer types are Phorge's view of an outbound email, not this service's.
// PhabricatorMailGorgeAdapter serialises a PhabricatorMailExternalMessage
// straight into SendRequest, so the camelCase field names below are the wire
// contract itself; see compat/phorge/README.md.
//
// Attachment payloads are base64 in this representation, which is a property of
// the contract rather than of any backend: the PHP adapter encodes at
// serialisation time and each adapter here decodes only if its provider wants
// raw bytes.

// Address is one mailbox. Name is the optional display name, which adapters
// RFC 2047-encode before it reaches a header.
type Address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// Header is one additional message header. Phorge uses these to carry its own
// threading and routing metadata (X-Phabricator-*, In-Reply-To, References).
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Attachment is one file. Data is base64-encoded, always: the field crosses the
// wire inside a JSON document, which cannot carry raw bytes.
type Attachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// EmailMessage is one outbound email, backend-independent.
type EmailMessage struct {
	From        Address      `json:"from"`
	ReplyTo     *Address     `json:"replyTo,omitempty"`
	To          []Address    `json:"to"`
	CC          []Address    `json:"cc,omitempty"`
	Subject     string       `json:"subject"`
	TextBody    string       `json:"textBody,omitempty"`
	HTMLBody    string       `json:"htmlBody,omitempty"`
	Headers     []Header     `json:"headers,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// SendRequest is the body of POST /api/mailer/send.
//
// MailerKeys restricts delivery to the named mailers, in the service's own
// priority order rather than the order given here. It is how Phorge's
// `bin/mail send-test --mailer <key>` reaches one specific backend; an empty
// list means "any configured mailer".
type SendRequest struct {
	Message    EmailMessage `json:"message"`
	MailerKeys []string     `json:"mailerKeys,omitempty"`
}

// SendResult is the payload returned inside the response envelope's data field.
// MailerKey names the backend that accepted the message, which is what Phorge
// records against the sent mail. MessageID is whatever the backend reported and
// is often empty: SMTP and sendmail have no id to give back.
type SendResult struct {
	MailerKey string `json:"mailerKey"`
	MessageID string `json:"messageId,omitempty"`
}

// MailerInfo is one entry of GET /api/mailer/mailers, in the order the
// dispatcher will try them.
type MailerInfo struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Priority int    `json:"priority"`
}
