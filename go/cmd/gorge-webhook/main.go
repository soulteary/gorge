// Command gorge-webhook serves the Herald webhook delivery domain: it drains
// the request queue Phorge fills and POSTs each request to its hook.
//
// It is the first binary in the repository whose real work is a background
// loop rather than an inbound request. The HTTP listener exists so that
// orchestration has something to probe and Phorge has somewhere to read the
// queue's state from; stopping it stops delivery, which is why a failed
// listener takes the process down instead of leaving the loop running blind.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/webhook"
)

func main() {
	cfg := webhook.LoadFromEnv()

	store, err := webhook.NewMySQLStore(cfg)
	if err != nil {
		// Only a malformed DSN reaches here. An unreachable database does
		// not: the pool is opened lazily, so the service starts, reports
		// itself unready and keeps retrying its poll — which is what lets it
		// be ordered before the database it reads.
		fmt.Fprintf(os.Stderr, "gorge-webhook: failed to open the herald database: %v\n", err)
		os.Exit(1)
	}

	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// The database is the whole of this service's readiness. /healthz
		// answers 200 while the process is listening, which for a delivery
		// service that cannot read its queue would be a lie orchestration
		// keeps in rotation.
		Ready: webhook.ReadyProbe(store),
	})

	webhook.RegisterRoutes(srv.App(), &webhook.Deps{
		Store: store,
		Token: cfg.ServiceToken,
	})

	// One signal stops both halves: srv.Run returns on SIGINT or SIGTERM, and
	// cancelling this context is what ends the poll loop.
	ctx, stopPolling := context.WithCancel(context.Background())
	polling := make(chan struct{})
	go func() {
		defer close(polling)
		webhook.NewDispatcher(store, cfg).Run(ctx)
	}()

	runErr := srv.Run()

	// Drained before the pool is closed, so a result write is not cut off by
	// the connection disappearing under it. Run returns once the deliveries in
	// flight are done, and cancelling the context is what makes that prompt.
	stopPolling()
	<-polling

	// Closed explicitly rather than deferred: os.Exit below would skip a
	// deferred close, and the connection pool is the one thing here worth
	// releasing on the way out.
	if closeErr := store.Close(); closeErr != nil {
		slog.Error("failed to close the herald database", "error", closeErr)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-webhook: %v\n", runErr)
		os.Exit(1)
	}
}
