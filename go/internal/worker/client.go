package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// Client is the worker's HTTP client for the taskqueue service. It is the
// concrete reason this domain is a second binary rather than a package inside
// taskqueue: the worker reaches the queue the same way Phorge's own code does,
// over the /api/queue routes, so it scales and deploys independently of the
// service that owns the tables.
type Client struct {
	baseURL    string
	token      string
	leaseOwner string
	httpClient *http.Client
}

// NewClient builds the client and derives a stable lease owner for this
// process. The owner is written into worker_activetask.leaseOwner, which is
// Phorge's column read back by its daemon console, so it is shaped to identify
// this worker among many: host, pid, start time and the binary name.
func NewClient(baseURL, token string) *Client {
	hostname, _ := os.Hostname()
	pid := os.Getpid()
	leaseOwner := fmt.Sprintf("%s:%d:%d:gorge-worker", hostname, pid, time.Now().Unix())

	return &Client{
		baseURL:    baseURL,
		token:      token,
		leaseOwner: leaseOwner,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// LeaseOwner reports the identity this client leases under. Exposed for the
// stats endpoint and tests.
func (c *Client) LeaseOwner() string { return c.leaseOwner }

type apiResponse struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error *apiError       `json:"error,omitempty"`
}

var ErrLeaseConflict = errors.New("execution lease conflict")

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Lease asks the queue for up to limit tasks, presenting this client's owner.
func (c *Client) Lease(ctx context.Context, limit int, taskClasses []string) ([]*contracts.Task, error) {
	body, _ := json.Marshal(contracts.LeaseRequest{Limit: limit, TaskClasses: taskClasses})
	req, err := c.newRequest(ctx, http.MethodPost, "/api/queue/lease", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Lease-Owner", c.leaseOwner)

	var tasks []*contracts.Task
	if err := c.doJSON(req, &tasks); err != nil {
		return nil, fmt.Errorf("lease: %w", err)
	}
	return tasks, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("X-Service-Token", c.token)
	}
	return req, nil
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	var envelope apiResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("unmarshal (status %d): %w", resp.StatusCode, err)
	}
	if envelope.Error != nil {
		if envelope.Error.Code == "ERR_LEASE_CONFLICT" {
			return ErrLeaseConflict
		}
		return fmt.Errorf("api error [%s]: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("queue returned HTTP %d", resp.StatusCode)
	}
	if out != nil && envelope.Data != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("unmarshal data: %w", err)
		}
	}
	return nil
}
