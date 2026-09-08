// Command gorge-worker is Phorge's taskmaster daemon as a standalone process.
// It leases tasks from gorge-taskqueue over HTTP, runs them — natively or by
// delegating back to Phorge through Conduit — and reports each result.
//
// Like gorge-webhook its real work is a background loop rather than an inbound
// request; the HTTP listener exists so orchestration has something to probe and
// a Phorge setup check has somewhere to read the consumer's state from.
// Stopping the listener stops leasing, so a failed listener takes the process
// down instead of leaving the loop running blind.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/worker"
	"github.com/soulteary/gorge/go/internal/worker/handlers"
)

func main() {
	cfg := worker.LoadFromEnv()

	client := worker.NewClient(cfg.TaskQueueURL, cfg.TaskQueueToken)

	registry := worker.NewRegistry()
	handlers.RegisterAll(registry, cfg.ConduitURL, cfg.ConduitToken)

	consumer := worker.NewConsumer(client, registry, cfg)

	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// Readiness equals liveness: the worker holds no database and reaches
		// the queue over HTTP, retrying on its own, so there is no external
		// dependency whose absence should take it out of rotation. A queue
		// that is briefly down is a logged lease error, not an unready worker.
		Ready: nil,
	})

	worker.RegisterRoutes(srv.App(), &worker.Deps{
		Consumer: consumer,
		Token:    cfg.ServiceToken,
	})

	// One signal stops both halves: srv.Run returns on SIGINT or SIGTERM, and
	// cancelling this context is what ends the lease loop.
	ctx, stopLoop := context.WithCancel(context.Background())
	looping := make(chan struct{})
	go func() {
		defer close(looping)
		consumer.Run(ctx)
	}()

	runErr := srv.Run()

	// The loop is drained before exit so an in-flight task's result write is
	// not cut off. Cancelling the context is what makes Run return promptly.
	stopLoop()
	<-looping

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-worker: %v\n", runErr)
		os.Exit(1)
	}
}
