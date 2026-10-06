package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

// executeResult is the shape worker.execute returns: a classification plus,
// for a yield, how long to wait. It mirrors the branches
// PhabricatorWorkerActiveTask::executeTask takes, but reported rather than
// acted on, since gorge-worker owns the queue and does the bookkeeping.
type executeResult struct {
	Result           string                     `json:"result"`
	FailureReason    string                     `json:"failureReason"`
	Retry            int                        `json:"retry"`
	ExecutionVersion int                        `json:"executionVersion"`
	LeaseDuration    *int                       `json:"leaseDuration"`
	Followups        []contracts.EnqueueRequest `json:"followups"`
	Duration         int64                      `json:"duration"`
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
//   - "success"           => *worker.Completion (parent and children commit together),
//   - "yield"             => *worker.YieldError (retried after retry seconds),
//   - "permanent-failure" => *worker.PermanentError (failed, not retried),
//   - anything else / a transport error => a plain error (transient, retried).
func NewConduitDelegateHandler(conduit *ConduitClient, queues ...*worker.Client) worker.TaskHandler {
	return newConduitExecutionHandler(conduit, nil, queues...)
}

func newConduitExecutionHandler(conduit *ConduitClient, native worker.TaskHandler, queues ...*worker.Client) worker.TaskHandler {
	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		if task.LeaseExpires == nil || task.LeaseOwner == "" {
			return fmt.Errorf("missing execution lease")
		}
		base := worker.ExecutionContext(ctx)
		var queue *worker.Client
		if len(queues) > 0 {
			queue = queues[0]
		}
		if queue != nil {
			if err := queue.RequireExecutionProtocol(base); err != nil {
				return err
			}
		}
		params := map[string]any{
			"taskID": task.ID, "taskClass": task.TaskClass, "data": string(data),
			"executionVersion": 1, "phase": "prepare", "failureCount": task.FailureCount,
			"priority": task.Priority, "leaseOwner": task.LeaseOwner, "leaseExpires": *task.LeaseExpires,
		}
		probe := make(map[string]any, len(params))
		for key, value := range params {
			probe[key] = value
		}
		probe["phase"], probe["taskClass"] = "capabilities", ""
		capabilities, err := conduit.Call(ctx, "worker.execute", probe)
		if err != nil {
			return fmt.Errorf("negotiate worker execution: %w", err)
		}
		var supported executeResult
		if err := json.Unmarshal(capabilities.Result, &supported); err != nil {
			return err
		}
		if supported.ExecutionVersion != 1 || supported.Result != "capabilities" {
			return fmt.Errorf("Phorge does not support execution protocol 1")
		}
		prepared, err := conduit.Call(ctx, "worker.execute", params)
		if err != nil {
			return fmt.Errorf("prepare worker: %w", err)
		}
		var policy executeResult
		if err := json.Unmarshal(prepared.Result, &policy); err != nil {
			return err
		}
		if policy.ExecutionVersion != 1 {
			return fmt.Errorf("Phorge does not support execution protocol 1")
		}
		if policy.Result == "permanent-failure" {
			return &worker.PermanentError{Msg: policy.FailureReason}
		}
		if policy.Result == "skipped" {
			return &worker.Completion{}
		}
		if policy.Result != "prepared" {
			return fmt.Errorf("unexpected preparation result %q", policy.Result)
		}
		if policy.LeaseDuration != nil && *policy.LeaseDuration > 0 {
			if queue == nil {
				return fmt.Errorf("queue renewal client unavailable")
			}
			if err := queue.Renew(base, task, *policy.LeaseDuration); err != nil {
				return err
			}
		}
		executionCtx := base
		if native != nil {
			start := time.Now()
			if err := native(executionCtx, task, data); err != nil {
				return err
			}
			return &worker.Completion{Duration: time.Since(start).Microseconds()}
		}
		params["phase"] = "execute"
		params["leaseExpires"] = *task.LeaseExpires

		resp, err := conduit.Call(executionCtx, "worker.execute", params)
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

		if res.ExecutionVersion != 1 {
			return fmt.Errorf("invalid execution result version")
		}
		switch res.Result {
		case "success":
			return &worker.Completion{Followups: res.Followups, Duration: res.Duration}
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
			return &worker.RetryError{Message: fmt.Sprintf("%s failed: %s", task.TaskClass, res.FailureReason), Wait: res.Retry}
		default:
			// An unexpected/empty classification is treated as transient so the
			// task is retried rather than silently completed or lost.
			return fmt.Errorf("worker.execute for %s (id=%d) returned unexpected result %q",
				task.TaskClass, task.ID, res.Result)
		}
	}
}
