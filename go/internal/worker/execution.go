package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// Completion carries successful work with its staged children. It is a
// structured handler outcome, not a failure; Consumer commits it atomically.
type Completion struct {
	Followups []contracts.EnqueueRequest
	Duration  int64
}

func (c *Completion) Error() string { return "task completed with atomic finalization" }

type RetryError struct {
	Message string
	Wait    int
}

func (e *RetryError) Error() string { return e.Message }

type executionContextKey struct{}

// ExecutionContext returns the process cancellation context before a default
// lease deadline was attached. A handler may extend it only after queue renewal.
func ExecutionContext(ctx context.Context) context.Context {
	if base, ok := ctx.Value(executionContextKey{}).(context.Context); ok {
		return base
	}
	return ctx
}

func executionLease(task *contracts.Task) (contracts.ExecutionLease, error) {
	if task.LeaseExpires == nil || task.LeaseOwner == "" {
		return contracts.ExecutionLease{}, fmt.Errorf("task has no execution lease")
	}
	return contracts.ExecutionLease{TaskID: task.ID, LeaseOwner: task.LeaseOwner, LeaseExpires: *task.LeaseExpires}, nil
}
func (c *Client) RequireExecutionProtocol(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/queue/meta", nil)
	if err != nil {
		return err
	}
	var meta contracts.ExecutionCapabilities
	if err := c.doJSON(req, &meta); err != nil {
		return err
	}
	if meta.ExecutionVersion != 1 || !meta.LeaseOutcomes {
		return fmt.Errorf("taskqueue does not support execution protocol 1")
	}
	return nil
}
func (c *Client) Finalize(ctx context.Context, task *contracts.Task, result *Completion) error {
	lease, err := executionLease(task)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(contracts.FinalizeRequest{ExecutionLease: lease, Duration: result.Duration, Followups: result.Followups})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/queue/finalize", raw)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}
func (c *Client) Renew(ctx context.Context, task *contracts.Task, duration int) error {
	if session, ok := ctx.Value(leaseSessionKey{}).(*leaseSession); ok {
		if err := session.renew(ctx, duration); err != nil {
			return err
		}
		updated, _ := session.snapshot()
		task.LeaseExpires = updated.LeaseExpires
		return nil
	}
	return c.renew(ctx, task, duration)
}
func (c *Client) renew(ctx context.Context, task *contracts.Task, duration int) error {
	lease, err := executionLease(task)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(contracts.RenewRequest{ExecutionLease: lease, Duration: duration})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/queue/renew", raw)
	if err != nil {
		return err
	}
	var updated contracts.Task
	if err := c.doJSON(req, &updated); err != nil {
		return err
	}
	if updated.ID != task.ID || updated.LeaseOwner != task.LeaseOwner || updated.LeaseExpires == nil {
		return fmt.Errorf("invalid renewed lease")
	}
	task.LeaseExpires = updated.LeaseExpires
	return nil
}

func (c *Client) Resolve(ctx context.Context, task *contracts.Task, outcome string, wait *int, duration int) error {
	lease, err := executionLease(task)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(contracts.ResolveRequest{ExecutionLease: lease, Outcome: outcome, RetryWait: wait, Duration: duration})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/queue/resolve", raw)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

func (c *Client) EnqueueEvent(ctx context.Context, event *contracts.EnqueueEventRequest) (*contracts.Task, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/queue/enqueue-event", raw)
	if err != nil {
		return nil, err
	}
	task := new(contracts.Task)
	if err = c.doJSON(req, task); err != nil {
		return nil, err
	}
	if task.ID <= 0 {
		return nil, fmt.Errorf("queue omitted accepted event task")
	}
	return task, nil
}
