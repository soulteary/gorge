package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

// FeedHTTPData is the task data of a FeedPublisherHTTPWorker task, spelled as
// Phorge serialises it.
type FeedHTTPData struct {
	URI             string `json:"uri"`
	DeliveryVersion int    `json:"deliveryVersion"`
	Body            string `json:"body"`
}

// NewFeedHTTPHandler returns a native handler for FeedPublisherHTTPWorker
// tasks: it POSTs a complete versioned story snapshot to the feed endpoint. It is the one
// class the worker runs itself rather than delegating, because it is a plain
// HTTP forward with no Phorge-side logic to reach back for.
func NewFeedHTTPHandler() worker.TaskHandler {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		var td FeedHTTPData
		if err := json.Unmarshal(data, &td); err != nil {
			return &worker.PermanentError{Msg: fmt.Sprintf("invalid task data: %v", err)}
		}
		if td.URI == "" || td.DeliveryVersion != 1 || td.Body == "" {
			return &worker.PermanentError{Msg: "missing URI in task data"}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, td.URI,
			strings.NewReader(td.Body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := client.Do(req)
		if err != nil {
			return &worker.RetryError{Message: fmt.Sprintf("feed HTTP request failed: %v", err), Wait: max(task.FailureCount+1, 1) * 60}
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		return &worker.RetryError{Message: fmt.Sprintf("feed HTTP hook returned status %d", resp.StatusCode), Wait: max(task.FailureCount+1, 1) * 60}
	}
}
