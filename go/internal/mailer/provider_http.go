package mailer

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	ProviderTimeout             = 20 * time.Second
	ProviderResponseLimit int64 = 64 << 10
)

var defaultProviderClient = newProviderClient()

func newProviderClient() *http.Client {
	return &http.Client{
		Timeout: ProviderTimeout,
		Transport: &http.Transport{
			Proxy:                  http.ProxyFromEnvironment,
			DialContext:            (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  10 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
			MaxIdleConns:           100,
			MaxIdleConnsPerHost:    10,
			IdleConnTimeout:        90 * time.Second,
			ForceAttemptHTTP2:      true,
		},
		// Redirects may forward the message or authentication to another host.
		// An unexpected redirect is an uncertain outcome, not permission to send.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// callProvider bounds the complete request and the response body. Only an
// explicit rejection or a failed dial proves the message was not accepted;
// timeout, 5xx and incomplete responses must never trigger automatic failover.
func callProvider(provider string, client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	if err := req.Context().Err(); err != nil {
		return nil, nil, &SafeRetryError{Err: err}
	}
	if client == nil {
		client = defaultProviderClient
	}
	resp, err := client.Do(req)
	if err != nil {
		err = fmt.Errorf("%s: request failed: %w", provider, err)
		var network *net.OpError
		if errors.As(err, &network) && network.Op == "dial" {
			return nil, nil, &SafeRetryError{Err: err, Backend: true}
		}
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, ProviderResponseLimit+1))
	if int64(len(body)) > ProviderResponseLimit {
		readErr = fmt.Errorf("response exceeds %d bytes", ProviderResponseLimit)
	}
	if readErr != nil {
		// A received 4xx rejection remains conclusive even if its diagnostic body
		// is unavailable. Successful statuses need provider-specific handling.
		statusErr := classifyProviderStatus(provider, resp.StatusCode, nil)
		if CanRetry(statusErr) || IsPermanent(statusErr) {
			return resp, nil, statusErr
		}
		return resp, nil, fmt.Errorf("%s: response outcome unknown: %w", provider, readErr)
	}
	if err := classifyProviderStatus(provider, resp.StatusCode, body); err != nil {
		return resp, body, err
	}
	return resp, body, nil
}
