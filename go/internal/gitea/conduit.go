package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Sink interface {
	Append(context.Context, string, Event) (bool, error)
}

type ConduitSink struct {
	baseURL      string
	conduitToken string
	gatewayToken string
	client       *http.Client
}

func NewConduitSink(baseURL, conduitToken, gatewayToken string, client *http.Client) *ConduitSink {
	return &ConduitSink{strings.TrimRight(baseURL, "/"), conduitToken, gatewayToken, client}
}

type conduitEnvelope struct {
	Result    json.RawMessage `json:"result"`
	ErrorCode *string         `json:"error_code"`
	ErrorInfo *string         `json:"error_info"`
}

type transactionSearch struct {
	Data []struct {
		Comments []struct {
			Content struct {
				Raw string `json:"raw"`
			} `json:"content"`
		} `json:"comments"`
	} `json:"data"`
}

func (s *ConduitSink) Append(ctx context.Context, task string, event Event) (bool, error) {
	var history transactionSearch
	if err := s.call(ctx, "transaction.search", map[string]any{
		"objectIdentifier": task,
		"limit":            100,
	}, &history); err != nil {
		return false, err
	}
	for _, transaction := range history.Data {
		for _, comment := range transaction.Comments {
			if strings.Contains(comment.Content.Raw, event.Marker()) {
				return false, nil
			}
		}
	}

	return true, s.call(ctx, "maniphest.edit", map[string]any{
		"objectIdentifier": task,
		"transactions": []map[string]any{{
			"type":  "comment",
			"value": event.Comment(),
		}},
	}, nil)
}

func (s *ConduitSink) call(ctx context.Context, method string, params map[string]any, out any) error {
	params["__conduit__"] = map[string]any{"token": s.conduitToken}
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal conduit params: %w", err)
	}
	form := url.Values{"params": {string(raw)}, "output": {"json"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/api/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if s.gatewayToken != "" {
		req.Header.Set("X-Service-Token", s.gatewayToken)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("call conduit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var envelope conduitEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("conduit returned HTTP %d with an invalid envelope", resp.StatusCode)
	}
	if envelope.ErrorCode != nil {
		return fmt.Errorf("conduit rejected %s", method)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("conduit returned HTTP %d", resp.StatusCode)
	}
	if out != nil && len(envelope.Result) != 0 {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("decode conduit result: %w", err)
		}
	}
	return nil
}
