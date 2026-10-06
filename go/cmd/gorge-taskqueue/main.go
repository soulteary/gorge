// Command gorge-taskqueue serves the task queue domain: Phorge's daemon work
// queue behind an HTTP API. It enqueues tasks, leases them to workers, and
// archives them when they finish.
//
// It is the service half of the two-binary task pipeline. gorge-worker is its
// client: the worker leases over the /api/queue routes this binary exposes, so
// the two scale and deploy independently — a deployment can run one queue and
// many workers, which is the whole point of keeping them apart.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/taskqueue"
)

func main() {
	cfg := taskqueue.LoadFromEnv()

	store, err := newStore(cfg)
	if err != nil {
		// Only a malformed configuration reaches here. An unreachable backend
		// does not: the pool is opened lazily, so the service starts, reports
		// itself unready and keeps retrying — which is what lets it be ordered
		// before the Phorge container that runs bin/storage upgrade.
		fmt.Fprintf(os.Stderr, "gorge-taskqueue: failed to open the %s backend: %v\n", cfg.Backend, err)
		os.Exit(1)
	}

	var scheduler *taskqueue.Scheduler
	if cfg.SchedulerEnabled {
		mysqlStore, ok := store.(*taskqueue.MySQLStore)
		if !ok || strings.TrimSpace(cfg.SchedulerConduitURI) == "" || strings.TrimSpace(cfg.SchedulerConduitToken) == "" {
			fmt.Fprintln(os.Stderr, "scheduler requires MySQL and an authenticated Conduit source")
			_ = store.Close()
			os.Exit(1)
		}
		scheduler = &taskqueue.Scheduler{Store: mysqlStore, Source: taskqueue.NewScheduleSource(cfg.SchedulerConduitURI, cfg.SchedulerConduitToken)}
	}
	ready := taskqueue.ReadyProbe(store)
	if scheduler != nil {
		ready = func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return scheduler.Ready(ctx)
		}
	}
	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// Optional scheduling adds migrated schema and clock-source readiness;
		// liveness remains independent so first-install migrations can run.
		Ready: ready,
	})

	var schedulerProbe func(context.Context) error
	if scheduler != nil {
		schedulerProbe = scheduler.Ready
	}
	taskqueue.RegisterRoutes(srv.App(), &taskqueue.Deps{
		Store:            store,
		Token:            cfg.ServiceToken,
		SchedulerEnabled: cfg.SchedulerEnabled,
		SchedulerReady:   schedulerProbe,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	if scheduler != nil {
		go func() { defer close(done); scheduler.Run(ctx) }()
	} else {
		close(done)
	}
	runErr := srv.Run()
	cancel()
	<-done

	// Closed explicitly rather than deferred: os.Exit below would skip a
	// deferred close, and the connection pool is the one thing worth releasing
	// on the way out.
	if closeErr := store.Close(); closeErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-taskqueue: failed to close the %s backend: %v\n", cfg.Backend, closeErr)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-taskqueue: %v\n", runErr)
		os.Exit(1)
	}
}

// newStore builds the store the configured backend selects. mysql is the
// default and reads Phorge's own worker tables; redis is for deployments that
// keep the queue off the primary database.
func newStore(cfg *taskqueue.Config) (taskqueue.Store, error) {
	switch cfg.Backend {
	case "redis":
		return taskqueue.NewRedisStore(cfg)
	default:
		return taskqueue.NewMySQLStore(cfg)
	}
}
