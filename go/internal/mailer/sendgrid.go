package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/soulteary/gorge/go/internal/contracts"
)

const sendGridEndpoint = "https://api.sendgrid.com/v3/mail/send"

type sendGridAdapter struct {
	apiKey string
}

func newSendGridAdapter(opts map[string]string) (*sendGridAdapter, error) {
	a := &sendGridAdapter{apiKey: opts["api-key"]}
	if a.apiKey == "" {
		return nil, fmt.Errorf("sendgrid: api-key is required")
	}
	return a, nil
}

func (a *sendGridAdapter) Type() string { return "sendgrid" }

func (a *sendGridAdapter) Send(ctx context.Context, msg *contracts.EmailMessage) (string, error) {
	body, err := json.Marshal(a.buildPayload(msg))
	if err != nil {
		return "", fmt.Errorf("sendgrid: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sendGridEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("sendgrid: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sendgrid: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if err := classifyProviderStatus("sendgrid", resp.StatusCode, respBody); err != nil {
		return "", err
	}

	// SendGrid accepts asynchronously and answers 202 with an empty body; the
	// id is in a header.
	return resp.Header.Get("X-Message-Id"), nil
}

func (a *sendGridAdapter) buildPayload(msg *contracts.EmailMessage) map[string]any {
	personalization := map[string]any{"to": sendGridAddrs(msg.To)}
	if len(msg.CC) > 0 {
		personalization["cc"] = sendGridAddrs(msg.CC)
	}
	if len(msg.Headers) > 0 {
		headers := make(map[string]string, len(msg.Headers))
		for _, h := range msg.Headers {
			headers[h.Name] = h.Value
		}
		personalization["headers"] = headers
	}

	content := make([]map[string]string, 0, 2)
	if msg.TextBody != "" {
		content = append(content, map[string]string{"type": "text/plain", "value": msg.TextBody})
	}
	if msg.HTMLBody != "" {
		content = append(content, map[string]string{"type": "text/html", "value": msg.HTMLBody})
	}

	payload := map[string]any{
		"personalizations": []any{personalization},
		"from":             sendGridAddr(msg.From),
		"subject":          msg.Subject,
		"content":          content,
	}

	if msg.ReplyTo != nil {
		payload["reply_to"] = sendGridAddr(*msg.ReplyTo)
	}

	if len(msg.Attachments) > 0 {
		atts := make([]map[string]string, len(msg.Attachments))
		for i, att := range msg.Attachments {
			// SendGrid wants base64, which is how the attachment arrived.
			atts[i] = map[string]string{
				"content":  att.Data,
				"filename": att.Filename,
				"type":     att.MimeType,
			}
		}
		payload["attachments"] = atts
	}

	return payload
}

func sendGridAddr(a contracts.Address) map[string]string {
	out := map[string]string{"email": a.Address}
	if a.Name != "" {
		out["name"] = a.Name
	}
	return out
}

func sendGridAddrs(addrs []contracts.Address) []map[string]string {
	out := make([]map[string]string, len(addrs))
	for i, a := range addrs {
		out[i] = sendGridAddr(a)
	}
	return out
}
