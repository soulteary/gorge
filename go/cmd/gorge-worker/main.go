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
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/worker"
	"github.com/soulteary/gorge/go/internal/worker/handlers"
	"github.com/soulteary/gorge/go/internal/worker/outbox"
)

func main() {
	cfg := worker.LoadFromEnv()

	client := worker.NewClient(cfg.TaskQueueURL, cfg.TaskQueueToken)

	registry := worker.NewRegistry()
	handlers.RegisterWithFeedPolicy(registry, cfg.ConduitURL, cfg.ConduitToken, cfg.FeedPolicyFile, client)

	if err := handlers.RegisterNotificationMode(registry, cfg.NotificationPolicyFile, cfg.NotificationMode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	consumer := worker.NewConsumer(client, registry, cfg)

	var outboxDB *sql.DB
	if cfg.OutboxDSN != "" {
		var err error
		outboxDB, err = sql.Open("mysql", cfg.OutboxDSN)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid outbox configuration")
			os.Exit(1)
		}
		defer outboxDB.Close()
	}
	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		Ready: func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.RequireExecutionProtocol(ctx); err != nil {
				return err
			}
			if cfg.NotificationPolicyFile != "" && cfg.NotificationMode != "delegated" && cfg.NotificationMode != "shadow" {
				if err := handlers.ValidateNotificationPolicy(cfg.NotificationPolicyFile); err != nil {
					return err
				}
			}
			if cfg.FeedPolicyFile != "" {
				if err := handlers.ValidateFeedPolicy(cfg.FeedPolicyFile); err != nil {
					return err
				}
			}
			if outboxDB != nil {
				rows, err := outboxDB.QueryContext(ctx, "SELECT eventID FROM feed_gorgeoutbox LIMIT 1")
				if err != nil {
					return fmt.Errorf("outbox schema unavailable")
				}
				rows.Close()
			}
			return nil
		},
	})

	worker.RegisterRoutes(srv.App(), &worker.Deps{
		Consumer:          consumer,
		Token:             cfg.ServiceToken,
		NotificationStats: handlers.NotificationStats,
	})

	// One signal stops both halves: srv.Run returns on SIGINT or SIGTERM, and
	// cancelling this context is what ends the lease loop.
	ctx, stopLoop := context.WithCancel(context.Background())
	var relayWG sync.WaitGroup
	if outboxDB != nil {
		relayWG.Add(1)
		go func() { defer relayWG.Done(); (&outbox.Relay{DB: outboxDB, Queue: client}).Run(ctx) }()
	}

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
	relayWG.Wait()

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-worker: %v\n", runErr)
		os.Exit(1)
	}
}
