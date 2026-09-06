package mailer

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"mime"
	"strings"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// The MIME builder lives here rather than in smtp.go because three backends
// need a complete RFC 5322 message: SMTP and sendmail transmit it, and SES
// takes it as the RawMessage of SendRawEmail. The provider APIs do not — they
// take fields and assemble the message themselves.

// buildMIME renders msg as a complete message ready for an SMTP transaction.
func buildMIME(msg *contracts.EmailMessage) []byte {
	var b strings.Builder
	boundary := "----=_PhorgeMailer_" + randomBoundary()

	b.WriteString("From: " + formatAddr(msg.From) + "\r\n")
	if len(msg.To) > 0 {
		b.WriteString("To: " + joinAddrs(msg.To) + "\r\n")
	}
	if len(msg.CC) > 0 {
		b.WriteString("Cc: " + joinAddrs(msg.CC) + "\r\n")
	}
	if msg.ReplyTo != nil {
		b.WriteString("Reply-To: " + formatAddr(*msg.ReplyTo) + "\r\n")
	}
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", msg.Subject) + "\r\n")

	for _, h := range msg.Headers {
		b.WriteString(sanitizeHeaderValue(h.Name) + ": " + sanitizeHeaderValue(h.Value) + "\r\n")
	}

	b.WriteString("MIME-Version: 1.0\r\n")

	switch {
	case len(msg.Attachments) > 0:
		b.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n")
		b.WriteString("\r\n")
		writeBodyParts(&b, msg, boundary)
		for _, att := range msg.Attachments {
			b.WriteString("--" + boundary + "\r\n")
			ct := att.MimeType
			if ct == "" {
				ct = "application/octet-stream"
			}
			b.WriteString("Content-Type: " + ct + "; name=\"" + att.Filename + "\"\r\n")
			b.WriteString("Content-Disposition: attachment; filename=\"" + att.Filename + "\"\r\n")
			b.WriteString("Content-Transfer-Encoding: base64\r\n")
			b.WriteString("\r\n")
			// Attachment data arrives base64-encoded, which is also how it goes
			// out, so it is copied through rather than decoded and re-encoded.
			b.WriteString(att.Data + "\r\n")
		}
		b.WriteString("--" + boundary + "--\r\n")

	case msg.HTMLBody != "":
		altBoundary := boundary + "_alt"
		b.WriteString("Content-Type: multipart/alternative; boundary=\"" + altBoundary + "\"\r\n")
		b.WriteString("\r\n")
		writeAlternative(&b, msg, altBoundary)

	default:
		writePlainBody(&b, msg.TextBody)
	}

	return []byte(b.String())
}

// writeBodyParts writes the message body as the first part of a multipart/mixed
// message, itself multipart/alternative when there is HTML.
func writeBodyParts(b *strings.Builder, msg *contracts.EmailMessage, boundary string) {
	altBoundary := boundary + "_alt"
	b.WriteString("--" + boundary + "\r\n")

	if msg.HTMLBody != "" {
		b.WriteString("Content-Type: multipart/alternative; boundary=\"" + altBoundary + "\"\r\n\r\n")
		writeAlternative(b, msg, altBoundary)
		return
	}
	writePlainBody(b, msg.TextBody)
}

// writeAlternative writes the text and HTML alternatives, plainest first, which
// is the order RFC 2046 requires: a client picks the last part it understands.
func writeAlternative(b *strings.Builder, msg *contracts.EmailMessage, boundary string) {
	b.WriteString("--" + boundary + "\r\n")
	writePlainBody(b, msg.TextBody)
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(base64.StdEncoding.EncodeToString([]byte(msg.HTMLBody)) + "\r\n")
	b.WriteString("--" + boundary + "--\r\n")
}

// writePlainBody writes the text part. Bodies are base64-encoded unconditionally
// rather than sent as 8bit: Phorge's mail carries UTF-8 subjects and bodies, and
// base64 is the one encoding no relay in the path can damage.
func writePlainBody(b *strings.Builder, text string) {
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(base64.StdEncoding.EncodeToString([]byte(text)) + "\r\n")
}

// formatAddr renders an address for a header, Q-encoding the display name so a
// non-ASCII name survives.
func formatAddr(a contracts.Address) string {
	if a.Name != "" {
		return mime.QEncoding.Encode("utf-8", a.Name) + " <" + a.Address + ">"
	}
	return a.Address
}

// joinAddrs renders an address list for a single header.
func joinAddrs(addrs []contracts.Address) string {
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		parts[i] = formatAddr(a)
	}
	return strings.Join(parts, ", ")
}

// recipients flattens To and Cc into the envelope recipient list. Bcc has no
// representation in the contract, so this is the whole set.
func recipients(msg *contracts.EmailMessage) []string {
	out := make([]string, 0, len(msg.To)+len(msg.CC))
	for _, r := range msg.To {
		out = append(out, r.Address)
	}
	for _, r := range msg.CC {
		out = append(out, r.Address)
	}
	return out
}

// sanitizeHeaderValue strips CR and LF, which is what stops a caller-supplied
// header from injecting headers or a body of its own.
func sanitizeHeaderValue(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

func randomBoundary() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}
