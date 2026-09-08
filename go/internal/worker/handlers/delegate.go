package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

// executeResult is the shape worker.execute returns: a classification plus,
// for a yield, how long to wait. It mirrors the branches
// PhabricatorWorkerActiveTask::executeTask takes, but reported rather than
// acted on, since gorge-worker owns the queue and does the bookkeeping.
type executeResult struct {
	Result        string `json:"result"`
	FailureReason string `json:"failureReason"`
	Retry         int    `json:"retry"`
}

// NewConduitDelegateHandler returns a handler that runs a task on the Phorge
// side through the worker.execute Conduit method.
//
// This is what lets gorge-worker replace PhabricatorTaskmasterDaemon without
// reimplementing every PhabricatorWorker in Go: the worker leases and
// bookkeeps, PHP still runs the business logic. The handler translates the
// PHP-side classification back into the worker's error vocabulary so the
// consumer completes, fails, permanently-fails, or yields the task correctly:
//
//   - "success"           => nil (task is completed),
//   - "yield"             => *worker.YieldError (retried after retry seconds),
//   - "permanent-failure" => *worker.PermanentError (failed, not retried),
//   - anything else / a transport error => a plain error (transient, retried).
func NewConduitDelegateHandler(conduit *ConduitClient) worker.TaskHandler {
	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		params := map[string]any{
			"taskID":    task.ID,
			"taskClass": task.TaskClass,
			"data":      string(data),
		}

		resp, err := conduit.Call(ctx, "worker.execute", params)
		if err != nil {
			// A transport/protocol error (including the HTML-not-JSON case) is
			// transient: the task is failed for retry, not discarded.
			return fmt.Errorf("conduit worker.execute for %s (id=%d): %w",
				task.TaskClass, task.ID, err)
		}

		var res executeResult
		if len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, &res); err != nil {
				return fmt.Errorf("decode worker.execute result for %s (id=%d): %w",
					task.TaskClass, task.ID, err)
			}
		}

		switch res.Result {
		case "success":
			slog.Info("task delegated via conduit",
				"taskClass", task.TaskClass, "id", task.ID)
			return nil
		case "yield":
			return &worker.YieldError{
				Msg:      fmt.Sprintf("%s yielded", task.TaskClass),
				Duration: res.Retry,
			}
		case "permanent-failure":
			return &worker.PermanentError{
				Msg: fmt.Sprintf("%s permanently failed: %s",
					task.TaskClass, res.FailureReason),
			}
		case "failure":
			return fmt.Errorf("%s failed: %s", task.TaskClass, res.FailureReason)
		default:
			// An unexpected/empty classification is treated as transient so the
			// task is retried rather than silently completed or lost.
			return fmt.Errorf("worker.execute for %s (id=%d) returned unexpected result %q",
				task.TaskClass, task.ID, res.Result)
		}
	}
}
