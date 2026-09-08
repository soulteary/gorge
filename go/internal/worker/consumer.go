package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// Consumer is the lease loop: the worker's whole reason to exist. It polls the
// queue, hands each leased task to its handler, and reports the result. It is
// the analogue of PhabricatorTaskmasterDaemon, moved out of Phorge's process
// and pointed at the taskqueue service over HTTP.
type Consumer struct {
	client       *Client
	registry     *Registry
	leaseLimit   int
	pollInterval time.Duration
	maxWorkers   int
	idleTimeout  time.Duration
	filter       map[string]bool

	processed atomic.Int64
	failed    atomic.Int64
	active    atomic.Int32
}

// NewConsumer builds the loop from the worker configuration.
func NewConsumer(client *Client, registry *Registry, cfg *Config) *Consumer {
	filter := make(map[string]bool, len(cfg.TaskClassFilter))
	for _, tc := range cfg.TaskClassFilter {
		filter[tc] = true
	}
	return &Consumer{
		client:       client,
		registry:     registry,
		leaseLimit:   cfg.LeaseLimit,
		pollInterval: time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		maxWorkers:   cfg.MaxWorkers,
		idleTimeout:  time.Duration(cfg.IdleTimeoutSec) * time.Second,
		filter:       filter,
	}
}

// Stats reports this worker's lifetime counters and the classes it supports.
// It is an in-process snapshot, so it resets on restart and describes only this
// process — unlike the queue's own stats, which are a database read.
func (c *Consumer) Stats() contracts.ConsumerStats {
	return contracts.ConsumerStats{
		Processed: c.processed.Load(),
		Failed:    c.failed.Load(),
		Active:    c.active.Load(),
		Supported: c.registry.SupportedClasses(),
	}
}

// Run drives the loop until ctx is cancelled, at which point it returns so the
// process can exit cleanly. Cancelling the context is how main stops it.
func (c *Consumer) Run(ctx context.Context) {
	slog.Info("worker consumer started",
		"lease_limit", c.leaseLimit,
		"poll", c.pollInterval.String(),
		"workers", c.maxWorkers,
		"supported", c.registry.SupportedClasses())

	sem := make(chan struct{}, c.maxWorkers)
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	var idleSince *time.Time

	for {
		select {
		case <-ctx.Done():
			slog.Info("worker consumer shutting down")
			return
		case <-ticker.C:
			tasks, err := c.client.Lease(ctx, c.leaseLimit)
			if err != nil {
				// The queue may not be up yet, or may have gone away. Logged
				// and retried on the next tick rather than fatal, the same
				// posture the webhook poll loop takes toward its database.
				slog.Warn("lease error", "error", err)
				continue
			}

			if len(tasks) == 0 {
				if idleSince == nil {
					now := time.Now()
					idleSince = &now
				} else if c.idleTimeout > 0 && time.Since(*idleSince) > c.idleTimeout {
					slog.Info("worker idle, hibernating", "idle_timeout", c.idleTimeout.String())
					c.hibernate(ctx)
					idleSince = nil
				}
				continue
			}

			idleSince = nil
			var wg sync.WaitGroup
			for _, task := range tasks {
				if !c.shouldProcess(task) {
					slog.Info("skipping unsupported task",
						"taskClass", task.TaskClass, "id", task.ID)
					// Returned to the queue temporarily rather than dropped:
					// another worker with the right handler may lease it.
					retryWait := 60
					_ = c.client.Fail(ctx, task.ID, false, &retryWait)
					continue
				}

				sem <- struct{}{}
				wg.Add(1)
				go func(t *contracts.Task) {
					defer func() {
						<-sem
						wg.Done()
					}()
					c.processTask(ctx, t)
				}(task)
			}
			wg.Wait()
		}
	}
}

func (c *Consumer) shouldProcess(task *contracts.Task) bool {
	if len(c.filter) > 0 && !c.filter[task.TaskClass] {
		return false
	}
	return c.registry.Has(task.TaskClass)
}

func (c *Consumer) processTask(ctx context.Context, task *contracts.Task) {
	c.active.Add(1)
	defer c.active.Add(-1)

	start := time.Now()

	handler, ok := c.registry.Get(task.TaskClass)
	if !ok {
		slog.Warn("no handler for task", "taskClass", task.TaskClass, "id", task.ID)
		_ = c.client.Fail(ctx, task.ID, true, nil)
		c.failed.Add(1)
		return
	}

	var data json.RawMessage
	if task.Data != "" {
		data = json.RawMessage(task.Data)
	}

	err := handler(ctx, task, data)
	durationUs := time.Since(start).Microseconds()

	if err == nil {
		slog.Info("task completed",
			"taskClass", task.TaskClass, "id", task.ID, "duration", time.Since(start).String())
		_ = c.client.Complete(ctx, task.ID, durationUs)
		c.processed.Add(1)
		return
	}

	var permErr *PermanentError
	var yieldErr *YieldError

	switch {
	case errors.As(err, &permErr):
		slog.Warn("permanent task failure",
			"taskClass", task.TaskClass, "id", task.ID, "error", err)
		_ = c.client.Fail(ctx, task.ID, true, nil)
		c.failed.Add(1)
	case errors.As(err, &yieldErr):
		dur := yieldErr.Duration
		if dur < 5 {
			dur = 5
		}
		slog.Info("task yielded",
			"taskClass", task.TaskClass, "id", task.ID, "duration_sec", dur, "reason", err)
		_ = c.client.Yield(ctx, task.ID, dur)
	default:
		slog.Warn("temporary task failure",
			"taskClass", task.TaskClass, "id", task.ID, "error", err)
		_ = c.client.Fail(ctx, task.ID, false, nil)
		c.failed.Add(1)
	}
}

// hibernate sleeps out a long idle stretch, waking on cancellation. It keeps an
// idle worker from polling a quiet queue every tick without adding a second
// timer to the hot path.
func (c *Consumer) hibernate(ctx context.Context) {
	timer := time.NewTimer(3 * time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
