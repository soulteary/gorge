package handlers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/soulteary/gorge/go/internal/platform/conduitclient"
)

// ValidateExecutionService negotiates without preparing or executing a task.
// It must succeed before a delegated worker leases its first business task.
func ValidateExecutionService(ctx context.Context, uri, token string) error {
	conduit := conduitclient.NewBounded(uri, token, 65536)
	response, err := conduit.Call(ctx, "worker.execute", map[string]any{
		"executionVersion": 1, "phase": "capabilities", "taskClass": "",
		"taskID": 0, "data": "{}", "failureCount": 0, "priority": 0,
		"leaseOwner": "startup-probe", "leaseExpires": 0,
	})
	if err != nil {
		return fmt.Errorf("PHP execution capabilities unavailable; check gateway, service token and protocol")
	}
	var result executeResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return fmt.Errorf("invalid PHP execution capabilities")
	}
	if result.ExecutionVersion != 1 || result.Result != "capabilities" {
		return fmt.Errorf("PHP execution protocol 1 unavailable")
	}
	return nil
}
