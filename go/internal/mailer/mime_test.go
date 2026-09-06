package mailer

import (
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestBuildMIMEPlainText(t *testing.T) {
	msg := &contracts.EmailMessage{
		From:     contracts.Address{Name: "Sender", Address: "sender@example.com"},
		To:       []contracts.Address{{Address: "to@example.com"}},
		Subject:  "Test Subject",
		TextBody: "Hello, World!",
	}

	raw := string(buildMIME(msg))

	for _, want := range []string{"Subject:", "From:", "To: to@example.com", "text/plain"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestBuildMIMEWithHTML(t *testing.T) {
	msg := &contracts.EmailMessage{
		From:     contracts.Address{Address: "sender@example.com"},
		To:       []contracts.Address{{Address: "to@example.com"}},
		Subject:  "HTML Test",
		TextBody: "Plain text",
		HTMLBody: "<h1>HTML</h1>",
	}

	raw := string(buildMIME(msg))

	for _, want := range []string{"multipart/alternative", "text/plain", "text/html"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q", want)
		}
	}
	// RFC 2046: a client picks the last alternative it understands, so the
	// plainest part has to come first or every client shows the text version.
	if strings.Index(raw, "text/plain") > strings.Index(raw, "text/html") {
		t.Error("the text alternative must precede the HTML one")
	}
}

func TestBuildMIMEWithAttachments(t *testing.T) {
	msg := &contracts.EmailMessage{
		From:     contracts.Address{Address: "sender@example.com"},
		To:       []contracts.Address{{Address: "to@example.com"}},
		Subject:  "Attachment Test",
		TextBody: "See attachment",
		Attachments: []contracts.Attachment{
			{Filename: "test.txt", MimeType: "text/plain", Data: "SGVsbG8="},
		},
	}

	raw := string(buildMIME(msg))

	if !strings.Contains(raw, "multipart/mixed") {
		t.Error("expected multipart/mixed")
	}
	if !strings.Contains(raw, "test.txt") {
		t.Error("missing the attachment filename")
	}
	// Attachment data arrives base64 and goes out base64; re-encoding it would
	// deliver a file of base64 text.
	if !strings.Contains(raw, "SGVsbG8=") {
		t.Error("the attachment payload should be copied through verbatim")
	}
}

func TestBuildMIMEWithCC(t *testing.T) {
	msg := &contracts.EmailMessage{
		From:     contracts.Address{Address: "sender@example.com"},
		To:       []contracts.Address{{Address: "to@example.com"}},
		CC:       []contracts.Address{{Address: "cc@example.com"}},
		Subject:  "CC Test",
		TextBody: "Hello",
	}

	if raw := string(buildMIME(msg)); !strings.Contains(raw, "Cc: cc@example.com") {
		t.Error("missing the Cc header")
	}
}

func TestBuildMIMEWithReplyTo(t *testing.T) {
	msg := &contracts.EmailMessage{
		From:     contracts.Address{Address: "sender@example.com"},
		To:       []contracts.Address{{Address: "to@example.com"}},
		ReplyTo:  &contracts.Address{Address: "reply@example.com"},
		Subject:  "Reply-To Test",
		TextBody: "Hello",
	}

	if raw := string(buildMIME(msg)); !strings.Contains(raw, "Reply-To: reply@example.com") {
		t.Error("missing the Reply-To header")
	}
}

func TestBuildMIMECustomHeaders(t *testing.T) {
	msg := &contracts.EmailMessage{
		From:    contracts.Address{Address: "sender@example.com"},
		To:      []contracts.Address{{Address: "to@example.com"}},
		Subject: "Header Test",
		Headers: []contracts.Header{
			{Name: "X-Custom-Header", Value: "custom-value"},
		},
		TextBody: "Hello",
	}

	if raw := string(buildMIME(msg)); !strings.Contains(raw, "X-Custom-Header: custom-value") {
		t.Error("missing the custom header")
	}
}

// TestBuildMIMEStripsHeaderInjection: Phorge's headers reach this builder
// verbatim, so a CR or LF inside one would let a caller append headers or a
// body of its own.
func TestBuildMIMEStripsHeaderInjection(t *testing.T) {
	msg := &contracts.EmailMessage{
		From:    contracts.Address{Address: "sender@example.com"},
		To:      []contracts.Address{{Address: "to@example.com"}},
		Subject: "Injection Test",
		Headers: []contracts.Header{
			{Name: "X-Evil", Value: "ok\r\nBcc: victim@example.com"},
		},
		TextBody: "Hello",
	}

	raw := string(buildMIME(msg))
	if strings.Contains(raw, "\r\nBcc:") {
		t.Errorf("a header value smuggled a header through:\n%s", raw)
	}
	if !strings.Contains(raw, "X-Evil: okBcc: victim@example.com") {
		t.Errorf("expected the value flattened onto one line, got:\n%s", raw)
	}
}

func TestFormatAddr(t *testing.T) {
	got := formatAddr(contracts.Address{Address: "user@example.com"})
	if got != "user@example.com" {
		t.Errorf("plain address: got %q", got)
	}

	got = formatAddr(contracts.Address{Name: "User", Address: "user@example.com"})
	if !strings.Contains(got, "<user@example.com>") || !strings.Contains(got, "User") {
		t.Errorf("named address should carry both parts: got %q", got)
	}
}

func TestRecipientsCombinesToAndCC(t *testing.T) {
	msg := &contracts.EmailMessage{
		To: []contracts.Address{{Address: "a@example.com"}, {Address: "b@example.com"}},
		CC: []contracts.Address{{Address: "c@example.com"}},
	}

	got := recipients(msg)
	want := []string{"a@example.com", "b@example.com", "c@example.com"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}
