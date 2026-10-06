// Command gorge-mailer serves the mailer domain: outbound email for Phorge,
// fanned out over whichever of the seven backends a deployment configured.
//
// Unlike render and diff it gets a process of its own, because it is the
// opposite of pure computation: it holds adapter state, it talks to SMTP
// servers and provider APIs, and it has a readiness condition worth reporting.
package main

import (
	"context"
	"database/sql"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/soulteary/gorge/go/internal/mailer"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func main() {
	cfg, err := mailer.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-mailer: failed to load config: %v\n", err)
		os.Exit(1)
	}

	dispatcher, err := mailer.NewDispatcher(cfg.Mailers, cfg.RetryPolicy())
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-mailer: failed to initialise mailers: %v\n", err)
		os.Exit(1)
	}

	var delivery *mailer.DeliveryService
	if dsn := os.Getenv("GORGE_MAILER_DELIVERY_DSN"); dsn != "" {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid delivery DSN")
			os.Exit(1)
		}
		defer db.Close()
		nativeDispatcher, err := mailer.NewDispatcher(cfg.Mailers, mailer.RetryPolicy{})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if cfg.ServiceToken == "" {
			fmt.Fprintln(os.Stderr, "native delivery requires nonempty service token")
			os.Exit(1)
		}
		limit := 4
		if raw := os.Getenv("GORGE_MAILER_DELIVERY_CONCURRENCY"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 64 {
				fmt.Fprintln(os.Stderr, "delivery concurrency must be 1..64")
				os.Exit(1)
			}
			limit = n
		}
		delivery = &mailer.DeliveryService{DB: db, Dispatcher: nativeDispatcher, Slots: make(chan struct{}, limit)}
	}
	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// Attachments arrive base64-encoded inside the JSON body, so the
		// platform's 2M default is not enough.
		BodyLimit: mailer.TransportBodyLimit,
		// Starting with no backends configured is not a fatal error — it is a
		// legitimate state to boot into while the configuration is still being
		// written — but it must not read as healthy, or orchestration keeps a
		// service in rotation that fails every message it accepts.
		Ready: func() error {
			if err := dispatcher.Ready(); err != nil {
				return err
			}
			if delivery != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return delivery.Ready(ctx)
			}
			return nil
		},
	})

	mailer.RegisterRoutes(srv.App(), &mailer.Deps{
		Dispatcher: dispatcher,
		Token:      cfg.ServiceToken,
		BodyLimit:  cfg.BodyLimit,
		Delivery:   delivery,
	})

	recoveryCtx, stopRecovery := context.WithCancel(context.Background())
	var recoveryWG sync.WaitGroup
	if delivery != nil {
		recoveryWG.Add(1)
		go func() {
			defer recoveryWG.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-recoveryCtx.Done():
					return
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(recoveryCtx, 5*time.Second)
					err := delivery.Recover(ctx)
					cancel()
					if err != nil && recoveryCtx.Err() == nil {
						slog.Error("mail delivery recovery pending", "error", err)
					}
				}
			}
		}()
	}
	runErr := srv.Run()
	stopRecovery()
	recoveryWG.Wait()
	if err := runErr; err != nil {
		fmt.Fprintf(os.Stderr, "gorge-mailer: %v\n", err)
		os.Exit(1)
	}
}
