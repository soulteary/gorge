// Package publisher is the notification admin HTTP boundary. It does not load
// business objects or interpret browser subscription state.
package publisher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var ErrCredentials = errors.New("notification endpoint rejected credentials")

type Client struct{ http *http.Client }

func New() *Client {
	return &Client{http: &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Publish accepts HTTP success, matching the PHP boundary. It does not claim
// browser delivery, peer acknowledgment or durable notification persistence.
func (c *Client) Publish(ctx context.Context, endpoint, instance string, payload []byte) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid notification endpoint")
	}
	q := u.Query()
	q.Set("instance", instance)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("notification request unavailable")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("notification transport failed")
	}
	// Closing the read-only response is cleanup; request/read errors take precedence.
	defer func() { _ = resp.Body.Close() }()
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ErrCredentials
	}
	if readErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("notification endpoint failed")
}
