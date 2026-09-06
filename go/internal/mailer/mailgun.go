package mailer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/soulteary/gorge/go/internal/contracts"
)

type mailgunAdapter struct {
	apiKey      string
	domain      string
	apiHostname string
}

func newMailgunAdapter(opts map[string]string) (*mailgunAdapter, error) {
	a := &mailgunAdapter{
		apiKey:      opts["api-key"],
		domain:      opts["domain"],
		apiHostname: opts["api-hostname"],
	}
	if a.apiKey == "" {
		return nil, fmt.Errorf("mailgun: api-key is required")
	}
	if a.domain == "" {
		return nil, fmt.Errorf("mailgun: domain is required")
	}
	if a.apiHostname == "" {
		// api.eu.mailgun.net for EU-region accounts; the default is the US one.
		a.apiHostname = "api.mailgun.net"
	}
	return a, nil
}

func (a *mailgunAdapter) Type() string { return "mailgun" }

func (a *mailgunAdapter) Send(ctx context.Context, msg *contracts.EmailMessage) (string, error) {
	endpoint := fmt.Sprintf("https://%s/v2/%s/messages", a.apiHostname, a.domain)

	buf, contentType, err := a.buildForm(msg)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, buf)
	if err != nil {
		return "", fmt.Errorf("mailgun: create request: %w", err)
	}
	req.SetBasicAuth("api", a.apiKey)
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("mailgun: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if err := classifyProviderStatus("mailgun", resp.StatusCode, respBody); err != nil {
		return "", err
	}

	var result struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("mailgun: parse response: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("mailgun: no message id in response: %s", string(respBody))
	}

	return result.ID, nil
}

// buildForm renders the message as the multipart form Mailgun's messages
// endpoint takes. Attachments are decoded back to raw bytes here: this is the
// one backend that wants the file itself rather than base64.
func (a *mailgunAdapter) buildForm(msg *contracts.EmailMessage) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	_ = w.WriteField("from", formatAddr(msg.From))
	for _, to := range msg.To {
		_ = w.WriteField("to", formatAddr(to))
	}
	for _, cc := range msg.CC {
		_ = w.WriteField("cc", formatAddr(cc))
	}
	if msg.ReplyTo != nil {
		_ = w.WriteField("h:Reply-To", formatAddr(*msg.ReplyTo))
	}
	_ = w.WriteField("subject", msg.Subject)

	if msg.TextBody != "" {
		_ = w.WriteField("text", msg.TextBody)
	}
	if msg.HTMLBody != "" {
		_ = w.WriteField("html", msg.HTMLBody)
	}
	for _, h := range msg.Headers {
		_ = w.WriteField("h:"+h.Name, h.Value)
	}

	for _, att := range msg.Attachments {
		part, err := w.CreateFormFile("attachment", att.Filename)
		if err != nil {
			return nil, "", fmt.Errorf("mailgun: create attachment: %w", err)
		}
		decoded, err := base64.StdEncoding.DecodeString(att.Data)
		if err != nil {
			// The caller sent something that is not base64, and sending it
			// again will not make it so.
			return nil, "", permanentf("mailgun: attachment %q is not base64: %v", att.Filename, err)
		}
		if _, err := part.Write(decoded); err != nil {
			return nil, "", fmt.Errorf("mailgun: write attachment: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("mailgun: close multipart writer: %w", err)
	}

	return &buf, w.FormDataContentType(), nil
}
