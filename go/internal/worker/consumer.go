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
	drainTimeout time.Duration
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
	drainSeconds := cfg.DrainTimeoutSec
	if drainSeconds <= 0 {
		drainSeconds = DefaultDrainTimeoutSec
	}
	return &Consumer{
		client:       client,
		registry:     registry,
		leaseLimit:   cfg.LeaseLimit,
		pollInterval: time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		maxWorkers:   cfg.MaxWorkers,
		drainTimeout: time.Duration(drainSeconds) * time.Second,
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

// Run leases until ctx is cancelled, then drains within an independent budget.
// Cancellation stops intake; draining tasks retain their fenced heartbeat until
// the budget expires, at which point their contexts are cancelled.
func (c *Consumer) Run(ctx context.Context) {
	slog.Info("worker consumer started",
		"lease_limit", c.leaseLimit,
		"poll", c.pollInterval.String(),
		"workers", c.maxWorkers,
		"supported", c.registry.SupportedClasses())

	sem := make(chan struct{}, c.maxWorkers)
	completed := make(chan struct{}, 1)
	var running sync.WaitGroup
	// Cancellation stops new leases. Accepted tasks keep their ownership
	// heartbeat while draining; ownership loss still cancels their task context.
	taskContext, cancelTasks := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancelTasks(nil)
	defer func() {
		drained := make(chan struct{})
		go func() { running.Wait(); close(drained) }()
		timer := time.NewTimer(c.drainTimeout)
		defer timer.Stop()
		select {
		case <-drained:
			return
		case <-timer.C:
			cancelTasks(errors.New("worker shutdown drain deadline exceeded"))
			slog.Warn("worker drain expired; active execution outcomes may be unknown", "active", c.active.Load())
		}
		// Context-aware HTTP/SQL handlers terminate promptly after cancellation.
		// A handler that ignores cancellation must not prevent process shutdown.
		grace := time.NewTimer(unknownArchiveTimeout + time.Second)
		defer grace.Stop()
		select {
		case <-drained:
		case <-grace.C:
			slog.Warn("worker exiting with handlers that did not honor cancellation", "active", c.active.Load())
		}
	}()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	var idleSince *time.Time

	for {
		select {
		case <-ctx.Done():
			slog.Info("worker consumer shutting down")
			return
		case <-ticker.C:
		case <-completed:
		}
		if ctx.Err() != nil {
			return
		}
		available := c.maxWorkers - len(sem)
		if !c.canLease || available == 0 {
			continue
		}
		tasks, err := c.client.Lease(ctx, min(c.leaseLimit, available), c.leaseClasses)
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
				slog.Info("worker idle, continuing to poll", "idle_timeout", c.idleTimeout.String())
				idleSince = nil
			}
			continue
		}

		idleSince = nil
		for _, task := range tasks {
			if !c.shouldProcess(task) {
				slog.Info("skipping unsupported task",
					"taskClass", task.TaskClass, "id", task.ID)
				// Returned to the queue temporarily rather than dropped:
				// another worker with the right handler may lease it.
				retryWait := 60
				c.reportOutcome(ctx, task, "unsupported class", func(reportCtx context.Context) error {
					return c.client.Resolve(reportCtx, task, "retry", &retryWait, 0)
				})
				continue
			}

			sem <- struct{}{}
			running.Add(1)
			go func(t *contracts.Task) {
				defer func() {
					<-sem
					running.Done()
					select {
					case completed <- struct{}{}:
					default:
					}
				}()
				c.processTask(taskContext, t)
			}(task)
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
		_ = c.client.Resolve(ctx, task, "failure", nil, 0)
		c.failed.Add(1)
		return
	}

	var data json.RawMessage
	if task.Data != "" {
		data = json.RawMessage(task.Data)
	}

	if err := c.client.RequireExecutionProtocol(ctx); err != nil {
		slog.Warn("execution protocol unavailable", "error", err)
		return
	}
	if _, err := executionLease(task); err != nil {
		slog.Warn("invalid execution lease", "error", err)
		return
	}
	// Confirm current ownership before executing any side effect.
	if err := c.client.Renew(ctx, task, 60); err != nil {
		slog.Warn("lease validation failed", "error", err)
		return
	}
	taskCtx, stopHeartbeat := c.client.startHeartbeat(ctx, task)
	handlerTask := *task
	handlerDone := make(chan error, 1)
	go func() { handlerDone <- handler(taskCtx, &handlerTask, data) }()
	var err error
	select {
	case err = <-handlerDone:
	case <-taskCtx.Done():
		err = taskCtx.Err()
	}
	current, leaseErr := stopHeartbeat()
	if ctx.Err() != nil {
		// An unresponsive remote worker may have started a side effect. The
		// existing permanent-failure archive prevents automatic redelivery;
		// this administrative quarantine does not establish business failure.
		c.archiveInterruptedTask(ctx, current)
		return
	}
	if leaseErr != nil {
		slog.Warn("execution ownership lost", "taskID", task.ID, "error", leaseErr)
		return
	}
	task = current
	durationUs := time.Since(start).Microseconds()
	reportCtx, reportCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reportCancel()
	taskCtx = reportCtx
	confirmed := false
	defer func() {
		// Drain can expire while finalization is in flight. A missing receipt
		// does not establish whether it committed; refresh the active row and
		// quarantine only if it still belongs to this execution.
		if ctx.Err() != nil && !confirmed {
			c.archiveInterruptedTask(ctx, task)
		}
	}()

	var completion *Completion
	if errors.As(err, &completion) {
		confirmed = c.reportOutcome(reportCtx, task, "finalize", func(reportCtx context.Context) error {
			return c.client.Finalize(reportCtx, task, completion)
		})
		if confirmed {
			c.processed.Add(1)
		}
		return
	}
	if err == nil {
		slog.Info("task completed",
			"taskClass", task.TaskClass, "id", task.ID, "duration", time.Since(start).String())
		confirmed = c.reportOutcome(taskCtx, task, "complete", func(reportCtx context.Context) error {
			return c.client.Finalize(reportCtx, task, &Completion{Duration: durationUs})
		})
		if confirmed {
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
		confirmed = c.reportOutcome(taskCtx, task, "permanent failure", func(reportCtx context.Context) error {
			return c.client.Resolve(reportCtx, task, "failure", nil, 0)
		})
		if confirmed {
			c.failed.Add(1)
		}
	case errors.As(err, &yieldErr):
		dur := yieldErr.Duration
		if dur < 5 {
			dur = 5
		}
		slog.Info("task yielded",
			"taskClass", task.TaskClass, "id", task.ID, "duration_sec", dur, "reason", err)
		confirmed = c.reportOutcome(taskCtx, task, "yield", func(reportCtx context.Context) error {
			return c.client.Resolve(reportCtx, task, "yield", nil, dur)
		})
	default:
		slog.Warn("temporary task failure",
			"taskClass", task.TaskClass, "id", task.ID, "error", err)
		confirmed = c.reportOutcome(taskCtx, task, "temporary failure", func(reportCtx context.Context) error {
			var retry *RetryError
			if errors.As(err, &retry) {
				return c.client.Resolve(reportCtx, task, "retry", &retry.Wait, 0)
			}
			return c.client.Resolve(reportCtx, task, "retry", nil, 0)
		})
		if confirmed {
			c.failed.Add(1)
		}
	}
}

const unknownArchiveTimeout = 10 * time.Second

func (c *Consumer) archiveInterruptedTask(ctx context.Context, task *contracts.Task) {
	slog.Warn("task interrupted; execution outcome unknown, manual review required", "taskID", task.ID)
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unknownArchiveTimeout)
	defer cancel()
	for attempt := 0; attempt < resultReportAttempts; attempt++ {
		// Renewal may have committed just as cancellation discarded its reply.
		// Refresh the exact token, but never take over another worker's lease.
		latest, err := c.client.ActiveTask(reportCtx, task.ID)
		if errors.Is(err, ErrTaskNotFound) {
			slog.Warn("interrupted task is no longer active; verify its recorded outcome", "taskID", task.ID)
			return
		}
		if err == nil {
			if latest.LeaseOwner != task.LeaseOwner || latest.LeaseExpires == nil || time.Now().Unix() >= *latest.LeaseExpires {
				slog.Error("unknown execution could not be archived: ownership changed or expired", "taskID", task.ID)
				return
			}
			err = c.client.Resolve(reportCtx, latest, "failure", nil, 0)
			if err == nil {
				slog.Warn("interrupted task permanently archived for manual review; business outcome remains unknown", "taskID", task.ID)
				return
			}
		}
		// A competing renewal/finalization may have changed the token; refresh
		// within this same bounded reporting window before any retry.
		timer := time.NewTimer(resultReportBackoff)
		select {
		case <-reportCtx.Done():
			timer.Stop()
			attempt = resultReportAttempts
		case <-timer.C:
		}
	}
	slog.Error("unknown execution archive unconfirmed; normal crash/lease recovery remains possible", "taskID", task.ID)
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
		if errors.Is(err, ErrLeaseConflict) || attempt == resultReportAttempts {
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
