package handlers

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

// NewNoopHandler returns a handler that logs and completes the task without
// doing anything else. It is a placeholder for classes that only need to be
// acknowledged, and the seam tests register to exercise the loop without a
// network call.
func NewNoopHandler(taskClass string) worker.TaskHandler {
	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		slog.Info("noop task completed", "taskClass", taskClass, "id", task.ID)
		return nil
	}
}
