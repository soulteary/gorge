package taskqueue

import (
	"context"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// Store is everything this domain does with the task queue's backing storage.
//
// It is an interface for the same reason the webhook domain's is: every
// endpoint here touches the store, so there is no configuration of the service
// that answers a request without one, and requiring a live MySQL in the test
// suite would make the contract fixtures non-neutral and un-runnable in CI.
// The in-memory implementation the tests inject is hand-written; see
// memstore_test.go.
//
// Two concrete backends satisfy it — MySQLStore over Phorge's own tables and
// RedisStore for deployments that keep the queue off the primary database.
// Both are exercised by the same store tests through this interface.
type Store interface {
	// Enqueue inserts a new task and returns it. It is what Phorge's
	// scheduleTask reaches over HTTP.
	Enqueue(ctx context.Context, req *contracts.EnqueueRequest) (*contracts.Task, error)

	// Lease takes ownership of up to limit runnable tasks for leaseOwner and
	// returns them with their data. Unleased tasks are preferred over ones
	// whose lease expired, and both are ordered by priority then id.
	Lease(ctx context.Context, limit int, leaseOwner string, taskClasses []string) ([]*contracts.Task, error)

	// Complete archives a task as succeeded, recording its runtime.
	Complete(ctx context.Context, taskID int64, duration int64) (*contracts.ArchivedTask, error)

	// Fail either archives a task as permanently failed or returns it to the
	// queue with an incremented failureCount and a retry backoff, depending on
	// req.Permanent.
	Fail(ctx context.Context, req *contracts.FailRequest) error

	// Yield returns a task to the queue under the YieldOwner sentinel for at
	// least duration seconds.
	Yield(ctx context.Context, taskID int64, duration int) error

	// Cancel archives a task as cancelled.
	Cancel(ctx context.Context, taskID int64) (*contracts.ArchivedTask, error)

	// Awaken pulls yielded tasks forward, reporting how many it moved.
	Awaken(ctx context.Context, taskIDs []int64) (int64, error)

	// Stats reports the queue's current shape.
	Stats(ctx context.Context) (*contracts.QueueStats, error)

	// GetTask loads one active task by id. A task that does not exist is
	// (nil, nil): it may already have been archived, which is an expected
	// answer and not an error.
	GetTask(ctx context.Context, taskID int64) (*contracts.Task, error)

	// ListActive lists active tasks, ordered by priority then id.
	ListActive(ctx context.Context, limit, offset int) ([]*contracts.Task, error)

	// Ready reports whether the backend can be reached. It backs /readyz.
	Ready(ctx context.Context) error

	// Close releases the connection pool.
	Close() error
}
