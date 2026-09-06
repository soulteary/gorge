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

const postmarkEndpoint = "https://api.postmarkapp.com/email"

// postmarkPermanentErrorCodes are the API error codes that describe the message
// rather than a passing condition. Postmark reports these with HTTP 422, which
// classifyProviderStatus already treats as permanent; they are listed so the
// same verdict survives the day Postmark answers 200 with an error code, which
// its older endpoints did.
var postmarkPermanentErrorCodes = map[int]string{
	300: "invalid email request",
	406: "inactive recipient",
	409: "JSON required",
	422: "invalid email address",
}

type postmarkAdapter struct {
	accessToken string
}

func newPostmarkAdapter(opts map[string]string) (*postmarkAdapter, error) {
	a := &postmarkAdapter{accessToken: opts["access-token"]}
	if a.accessToken == "" {
		return nil, fmt.Errorf("postmark: access-token is required")
	}
	return a, nil
}

func (a *postmarkAdapter) Type() string { return "postmark" }

func (a *postmarkAdapter) Send(ctx context.Context, msg *contracts.EmailMessage) (string, error) {
	body, err := json.Marshal(a.buildPayload(msg))
	if err != nil {
		return "", fmt.Errorf("postmark: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postmarkEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("postmark: create request: %w", err)
	}
	req.Header.Set("X-Postmark-Server-Token", a.accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("postmark: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if err := classifyProviderStatus("postmark", resp.StatusCode, respBody); err != nil {
		return "", err
	}

	var result struct {
		MessageID string `json:"MessageID"`
		ErrorCode int    `json:"ErrorCode"`
		Message   string `json:"Message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("postmark: parse response: %w", err)
	}
	if result.ErrorCode != 0 {
		if name, ok := postmarkPermanentErrorCodes[result.ErrorCode]; ok {
			return "", permanentf("postmark: error %d (%s): %s", result.ErrorCode, name, result.Message)
		}
		return "", fmt.Errorf("postmark: error %d: %s", result.ErrorCode, result.Message)
	}

	return result.MessageID, nil
}

func (a *postmarkAdapter) buildPayload(msg *contracts.EmailMessage) map[string]any {
	payload := map[string]any{
		"From":    formatAddr(msg.From),
		"To":      joinAddrs(msg.To),
		"Subject": msg.Subject,
	}

	if len(msg.CC) > 0 {
		payload["Cc"] = joinAddrs(msg.CC)
	}
	if msg.ReplyTo != nil {
		payload["ReplyTo"] = formatAddr(*msg.ReplyTo)
	}
	if msg.TextBody != "" {
		payload["TextBody"] = msg.TextBody
	}
	if msg.HTMLBody != "" {
		payload["HtmlBody"] = msg.HTMLBody
	}

	if len(msg.Headers) > 0 {
		headers := make([]map[string]string, len(msg.Headers))
		for i, h := range msg.Headers {
			headers[i] = map[string]string{"Name": h.Name, "Value": h.Value}
		}
		payload["Headers"] = headers
	}

	if len(msg.Attachments) > 0 {
		atts := make([]map[string]string, len(msg.Attachments))
		for i, att := range msg.Attachments {
			ct := att.MimeType
			if ct == "" {
				ct = "application/octet-stream"
			}
			atts[i] = map[string]string{
				"Name":        att.Filename,
				"Content":     att.Data,
				"ContentType": ct,
			}
		}
		payload["Attachments"] = atts
	}

	return payload
}
