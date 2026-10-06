package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/integrations"
	"github.com/soulteary/gorge/go/internal/platform/conduitclient"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"log/slog"
	"os"
	"sync"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "gorge-integrations: startup/runtime failure")
		os.Exit(1)
	}
}
func run() error {
	cfg, e := integrations.Load(os.Getenv("GORGE_INTEGRATIONS_CONFIG_FILE"))
	if e != nil {
		return e
	}
	db, e := sql.Open("mysql", cfg.DSN)
	if e != nil {
		return e
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	store := integrations.Store{DB: db}
	var facts *sql.DB
	if cfg.FactDSN != "" {
		facts, e = sql.Open("mysql", cfg.FactDSN)
		if e != nil {
			return e
		}
		defer func() { _ = facts.Close() }()
		facts.SetMaxOpenConns(2)
	}
	ready := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := store.Ready(ctx); e != nil {
			return e
		}
		if facts != nil {
			return integrations.FactReady(ctx, facts)
		}
		return nil
	}
	if e = ready(); e != nil {
		return e
	}
	srv := httpx.New(httpx.Config{ListenAddr: cfg.Listen, BodyLimit: "10M", Ready: ready})
	service := integrations.New(cfg, store)
	service.Register(srv.App())
	c := conduitclient.NewBounded(cfg.ConduitURI, cfg.ConduitToken, 8*1024*1024)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	relayWorkers := cfg.RelayWorkers
	if len(cfg.Inbound) == 0 {
		relayWorkers = 0
	}
	for range relayWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if e := store.Relay(ctx, c); e != nil && !errors.Is(e, sql.ErrNoRows) && ctx.Err() == nil {
						slog.Error("inbound relay pending")
					}
				}
			}
		}()
	}
	if facts != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					batch, stop := context.WithTimeout(ctx, 60*time.Second)
					e := integrations.ProjectFacts(batch, facts, c)
					service.RecordFactResult(e)
					stop()
					if e != nil && ctx.Err() == nil {
						slog.Error("fact projection pending")
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(ctx, 10*time.Second)
				health, err := store.Health(check)
				factHealth := service.FactHealth()
				if err != nil && ctx.Err() == nil {
					slog.Error("integration health unavailable")
				} else if health.UnknownEffects > 0 || health.UnknownInbound > 0 || health.OldestOverdueSeconds > 300 || factHealth.Stale || factHealth.ConsecutiveFailures > 0 {
					slog.Warn("integration needs inspection", "unknownEffects", health.UnknownEffects, "unknownInbound", health.UnknownInbound, "pendingInbound", health.PendingInbound, "overdueSeconds", health.OldestOverdueSeconds, "factStale", factHealth.Stale, "factFailures", factHealth.ConsecutiveFailures)
				}
				if _, err := store.Purge(check, cfg.InboxRetentionDays); err != nil && ctx.Err() == nil {
					slog.Error("integration inbox cleanup pending")
				}
				stop()
			}
		}
	}()
	e = srv.Run()
	cancel()
	wg.Wait()
	return e
}
