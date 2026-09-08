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
	"fmt"
	"os"

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

	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// The backend is the whole of this service's readiness: a queue that
		// cannot reach its store can answer /healthz but must not stay in
		// rotation, so /readyz reports the store.
		Ready: taskqueue.ReadyProbe(store),
	})

	taskqueue.RegisterRoutes(srv.App(), &taskqueue.Deps{
		Store: store,
		Token: cfg.ServiceToken,
	})

	runErr := srv.Run()

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
