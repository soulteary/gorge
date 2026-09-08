package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
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
	leaseClasses []string
	canLease     bool

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
	leaseClasses := leaseableClasses(registry, filter)
	return &Consumer{
		client:       client,
		registry:     registry,
		leaseLimit:   cfg.LeaseLimit,
		pollInterval: time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		maxWorkers:   cfg.MaxWorkers,
		idleTimeout:  time.Duration(cfg.IdleTimeoutSec) * time.Second,
		filter:       filter,
		leaseClasses: leaseClasses,
		canLease:     registry.HasFallback() || len(leaseClasses) > 0,
	}
}

// leaseableClasses computes the filter sent to taskqueue. nil means all
// classes and is only returned for a registry with a fallback and no explicit
// allowlist. Without a fallback, the request is restricted to the intersection
// of registered handlers and the configured allowlist.
func leaseableClasses(registry *Registry, configured map[string]bool) []string {
	if registry.HasFallback() && len(configured) == 0 {
		return nil
	}

	var classes []string
	for _, taskClass := range registry.SupportedClasses() {
		if taskClass == "*" {
			continue
		}
		if len(configured) == 0 || configured[taskClass] {
			classes = append(classes, taskClass)
		}
	}
	if registry.HasFallback() {
		for taskClass := range configured {
			if !registry.Has(taskClass) {
				continue
			}
			found := false
			for _, existing := range classes {
				if existing == taskClass {
					found = true
					break
				}
			}
			if !found {
				classes = append(classes, taskClass)
			}
		}
	}
	sort.Strings(classes)
	return classes
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
			if !c.canLease {
				continue
			}
			tasks, err := c.client.Lease(ctx, c.leaseLimit, c.leaseClasses)
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
					c.reportOutcome(ctx, task, "unsupported class", func(reportCtx context.Context) error {
						return c.client.Fail(reportCtx, task.ID, false, &retryWait)
					})
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

	// A delegated task may legitimately run until the queue lease expires. The
	// HTTP client has no unrelated fixed timeout; this deadline is the actual
	// ownership window returned by taskqueue.
	taskCtx := ctx
	var cancel context.CancelFunc
	if task.LeaseExpires != nil {
		taskCtx, cancel = context.WithDeadline(ctx, time.Unix(*task.LeaseExpires, 0))
		defer cancel()
	}

	err := handler(taskCtx, task, data)
	durationUs := time.Since(start).Microseconds()

	if err == nil {
		slog.Info("task completed",
			"taskClass", task.TaskClass, "id", task.ID, "duration", time.Since(start).String())
		if c.reportOutcome(taskCtx, task, "complete", func(reportCtx context.Context) error {
			return c.client.Complete(reportCtx, task.ID, durationUs)
		}) {
			c.processed.Add(1)
		}
		return
	}

	var permErr *PermanentError
	var yieldErr *YieldError

	switch {
	case errors.As(err, &permErr):
		slog.Warn("permanent task failure",
			"taskClass", task.TaskClass, "id", task.ID, "error", err)
		if c.reportOutcome(taskCtx, task, "permanent failure", func(reportCtx context.Context) error {
			return c.client.Fail(reportCtx, task.ID, true, nil)
		}) {
			c.failed.Add(1)
		}
	case errors.As(err, &yieldErr):
		dur := yieldErr.Duration
		if dur < 5 {
			dur = 5
		}
		slog.Info("task yielded",
			"taskClass", task.TaskClass, "id", task.ID, "duration_sec", dur, "reason", err)
		c.reportOutcome(taskCtx, task, "yield", func(reportCtx context.Context) error {
			return c.client.Yield(reportCtx, task.ID, dur)
		})
	default:
		slog.Warn("temporary task failure",
			"taskClass", task.TaskClass, "id", task.ID, "error", err)
		if c.reportOutcome(taskCtx, task, "temporary failure", func(reportCtx context.Context) error {
			return c.client.Fail(reportCtx, task.ID, false, nil)
		}) {
			c.failed.Add(1)
		}
	}
}

const (
	resultReportAttempts = 5
	resultReportBackoff  = 200 * time.Millisecond
)

// reportOutcome does not let a transient taskqueue outage silently turn a
// completed side effect into an unacknowledged lease. Counters advance only
// after the queue records the outcome. After bounded retries, the row remains
// leased and the error is explicit; the queue's normal lease-expiry recovery
// remains the final fallback.
func (c *Consumer) reportOutcome(ctx context.Context, task *contracts.Task, outcome string, report func(context.Context) error) bool {
	for attempt := 1; attempt <= resultReportAttempts; attempt++ {
		err := report(ctx)
		if err == nil {
			return true
		}
		slog.Error("task outcome report failed",
			"taskClass", task.TaskClass,
			"id", task.ID,
			"outcome", outcome,
			"attempt", attempt,
			"error", err)
		if attempt == resultReportAttempts {
			break
		}
		timer := time.NewTimer(resultReportBackoff * time.Duration(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
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
