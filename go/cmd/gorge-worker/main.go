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
	"encoding/json"
	"fmt"
	"github.com/soulteary/gorge/go/internal/contracts"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/worker"
	"github.com/soulteary/gorge/go/internal/worker/handlers"
	"github.com/soulteary/gorge/go/internal/worker/outbox"
)

func main() {
	cfg := worker.LoadFromEnv()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	client := worker.NewClient(cfg.TaskQueueURL, cfg.TaskQueueToken)

	registry := worker.NewRegistry()
	unavailableMail := func(context.Context, *contracts.Task, json.RawMessage) error {
		return &worker.YieldError{Duration: 60, Msg: "native mail handler unavailable"}
	}
	registry.Register("GorgeMailDeliveryWorker", unavailableMail)
	registry.Register("GorgeMailSubmitWorker", unavailableMail)
	handlers.RegisterWithFeedPolicy(registry, cfg.ConduitURL, cfg.ConduitToken, cfg.FeedPolicyFile, client)

	if err := handlers.RegisterNotificationMode(registry, cfg.NotificationPolicyFile, cfg.NotificationMode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if cfg.MailerURL != "" {
		if cfg.ConduitURL == "" || cfg.ConduitToken == "" || cfg.MailerToken == "" || cfg.FeedPolicyFile == "" {
			fmt.Fprintln(os.Stderr, "native mail requires conduit, nonempty service tokens and execution policy")
			os.Exit(1)
		}
		conduit := handlers.NewConduitClient(cfg.ConduitURL, cfg.ConduitToken)
		registry.Register("GorgeMailDeliveryWorker", handlers.NewMailPreparationHandler(conduit, cfg.MailerURL, cfg.MailerToken))
		registry.Register("GorgeMailSubmitWorker", handlers.NewMailSubmitHandler(conduit, cfg.MailerURL, cfg.MailerToken, cfg.FeedPolicyFile))
	}
	var mailDB *sql.DB
	if cfg.MailOutboxDSN != "" {
		if cfg.MailerURL == "" {
			fmt.Fprintln(os.Stderr, "mail outbox requires native handlers")
			os.Exit(1)
		}
		var err error
		mailDB, err = sql.Open("mysql", cfg.MailOutboxDSN)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid mail outbox DSN")
			os.Exit(1)
		}
		defer func() {
			if err := mailDB.Close(); err != nil {
				slog.Error("database close failed", "error", err)
			}
		}()
	}
	consumer := worker.NewConsumer(client, registry, cfg)
	checkExecution := func(ctx context.Context) error {
		if err := client.RequireExecutionProtocol(ctx); err != nil {
			return err
		}
		if cfg.ConduitURL != "" {
			return handlers.ValidateExecutionService(ctx, cfg.ConduitURL, cfg.ConduitToken)
		}
		return nil
	}

	var outboxDB *sql.DB
	if cfg.OutboxDSN != "" {
		var err error
		outboxDB, err = sql.Open("mysql", cfg.OutboxDSN)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid outbox configuration")
			os.Exit(1)
		}
		defer func() {
			if err := outboxDB.Close(); err != nil {
				slog.Error("database close failed", "error", err)
			}
		}()
	}
	checkReady := func(ctx context.Context) error {
		if err := checkExecution(ctx); err != nil {
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
			if err := rows.Close(); err != nil {
				return err
			}
		}
		if mailDB != nil {
			rows, err := mailDB.QueryContext(ctx, "SELECT eventID FROM metamta_gorgeoutbox LIMIT 1")
			if err != nil {
				return fmt.Errorf("mail outbox schema unavailable")
			}
			if err := rows.Close(); err != nil {
				return err
			}
			rows, err = mailDB.QueryContext(ctx, "SELECT deliveryID,projectionAttempts,projectionNextAttempt FROM gorge_mail_delivery LIMIT 1")
			if err != nil {
				return fmt.Errorf("mail projection schema unavailable")
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		if cfg.MailerURL != "" {
			if err := handlers.ValidateMailDeliveryService(ctx, cfg.MailerURL, cfg.MailerToken); err != nil {
				return err
			}
		}
		return nil
	}
	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		Ready: func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return checkReady(ctx)
		},
	})

	worker.RegisterRoutes(srv.App(), &worker.Deps{
		Consumer:          consumer,
		Token:             cfg.ServiceToken,
		NotificationStats: handlers.NotificationStats,
	})

	// Stop intake as soon as the signal arrives, while HTTP drains concurrently.
	// Waiting for srv.Run first would add its drain budget to the worker budget.
	ctx, stopLoop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopLoop()
	var relayWG sync.WaitGroup
	if outboxDB != nil {
		relayWG.Add(1)
		go func() { defer relayWG.Done(); (&outbox.Relay{DB: outboxDB, Queue: client}).Run(ctx) }()
	}

	if mailDB != nil {
		relayWG.Add(1)
		go func() {
			defer relayWG.Done()
			(&outbox.Relay{DB: mailDB, Queue: client, Table: "metamta_gorgeoutbox"}).Run(ctx)
		}()
	}
	if mailDB != nil {
		relayWG.Add(1)
		go func() {
			defer relayWG.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					report, cancel := context.WithTimeout(ctx, 30*time.Second)
					err := handlers.ProjectMailResultsOnce(report, mailDB, handlers.NewConduitClient(cfg.ConduitURL, cfg.ConduitToken))
					cancel()
					if err != nil {
						slog.Error("mail result projection pending", "error", err)
					}
				}
			}
		}()
	}
	looping := make(chan struct{})
	go func() {
		defer close(looping)
		if worker.WaitForExecution(ctx, 2*time.Second, checkReady) {
			consumer.Run(ctx)
		}
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
