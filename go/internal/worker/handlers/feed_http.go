package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

// FeedHTTPData is the task data of a FeedPublisherHTTPWorker task, spelled as
// Phorge serialises it.
type FeedHTTPData struct {
	URI     string `json:"uri"`
	StoryID int64  `json:"storyID"`
}

// NewFeedHTTPHandler returns a native handler for FeedPublisherHTTPWorker
// tasks: it POSTs the story id to the configured feed endpoint. It is the one
// class the worker runs itself rather than delegating, because it is a plain
// HTTP forward with no Phorge-side logic to reach back for.
func NewFeedHTTPHandler() worker.TaskHandler {
	client := &http.Client{Timeout: 15 * time.Second}

	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		var td FeedHTTPData
		if err := json.Unmarshal(data, &td); err != nil {
			return &worker.PermanentError{Msg: fmt.Sprintf("invalid task data: %v", err)}
		}
		if td.URI == "" {
			return &worker.PermanentError{Msg: "missing URI in task data"}
		}

		formData := url.Values{}
		formData.Set("storyID", fmt.Sprintf("%d", td.StoryID))

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, td.URI,
			strings.NewReader(formData.Encode()))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("http request to %s failed: %w", td.URI, err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			slog.Info("feed story delivered",
				"storyID", td.StoryID, "uri", td.URI, "status", resp.StatusCode)
			return nil
		}
		return fmt.Errorf("feed HTTP hook %s returned status %d", td.URI, resp.StatusCode)
	}
}
