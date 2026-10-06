package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

type feedPolicy struct {
	Silent *bool     `json:"silent"`
	URIs   *[]string `json:"uris"`
}

func readFeedPolicy(path string) (*feedPolicy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("feed policy unavailable: %w", err)
	}
	var policy feedPolicy
	if err = json.Unmarshal(raw, &policy); err != nil || policy.Silent == nil || policy.URIs == nil {
		return nil, fmt.Errorf("invalid feed execution policy")
	}
	return &policy, nil
}
func ValidateFeedPolicy(path string) error { _, err := readFeedPolicy(path); return err }

// Policy is read for each delivery so atomic file replacement immediately
// revokes removed hooks and enables silent mode without calling PHP prepare.
func NewFeedPolicyHandler(path string) worker.TaskHandler {
	native := NewFeedHTTPHandler()
	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		policy, err := readFeedPolicy(path)
		if err != nil {
			return err
		}
		if *policy.Silent {
			return nil
		}
		var snapshot FeedHTTPData
		if err = json.Unmarshal(data, &snapshot); err != nil {
			return &worker.PermanentError{Msg: "invalid feed snapshot"}
		}
		allowed := false
		for _, uri := range *policy.URIs {
			if snapshot.URI == uri {
				allowed = true
				break
			}
		}
		if !allowed {
			return &worker.PermanentError{Msg: "feed hook is no longer configured"}
		}
		return native(ctx, task, data)
	}
}
