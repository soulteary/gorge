// Command gorge-search serves the search domain: Phorge's fulltext index and
// queries, translated onto whichever store a deployment configured.
//
// Like gorge-mailer and unlike gorge-render it gets a process of its own,
// because it is the opposite of pure computation: it holds a per-host health
// table, it connects out to Elasticsearch or Meilisearch, and it has a
// readiness condition worth reporting.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

func main() {
	cfg, err := search.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-search: failed to load config: %v\n", err)
		os.Exit(1)
	}

	se, err := search.NewEngine(cfg.Backends)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-search: failed to initialise backends: %v\n", err)
		os.Exit(1)
	}

	ingress, controlDB, err := search.OpenProjection(context.Background(), cfg.Projection, cfg.ServiceToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-search: %v\n", err)
		os.Exit(1)
	}
	if controlDB != nil {
		defer func() { _ = controlDB.Close() }()
	}
	workers, err := search.PrepareProjectionDelivery(context.Background(), cfg.Projection, cfg.Backends, controlDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-search: %v\n", err)
		os.Exit(1)
	}
	if ingress != nil {
		ingress.BackendDelivery = len(workers) > 0
	}
	relayCtx, stopRelay := context.WithCancel(context.Background())
	var relayWG sync.WaitGroup
	defer func() { stopRelay(); relayWG.Wait() }()
	if cfg.Projection != nil && cfg.Projection.SourceOutboxDSN != "" {
		source, err := sql.Open("mysql", cfg.Projection.SourceOutboxDSN)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gorge-search: invalid search outbox database configuration")
			os.Exit(1)
		}
		defer func() { stopRelay(); relayWG.Wait(); _ = source.Close() }()
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		rows, err := source.QueryContext(checkCtx, "SELECT eventID,payload,attempts,nextAttempt,deliveredEpoch,lastError FROM search_gorgeoutbox LIMIT 0")
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "gorge-search: search outbox database or schema unavailable")
			os.Exit(1)
		}
		if err := rows.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "gorge-search: search outbox database or schema unavailable")
			os.Exit(1)
		}
		ingress.SourceOutbox = source
		relay := &projection.Relay{Source: source, Store: &projection.MySQLStore{DB: controlDB}, Namespace: ingress.Namespace, Targets: ingress.Targets}
		relayWG.Add(1)
		go func() { defer relayWG.Done(); relay.Run(relayCtx) }()
	}
	if cfg.Projection != nil && cfg.Projection.Rebuild {
		relayWG.Add(1)
		go func() {
			defer relayWG.Done()
			(&projection.MySQLStore{DB: controlDB}).RunRebuilds(relayCtx, ingress.Namespace, fmt.Sprintf("rebuild-%d", time.Now().UnixNano()), ingress.Targets)
		}()
	}
	for _, worker := range workers {
		relayWG.Add(1)
		go func() { defer relayWG.Done(); worker.Run(relayCtx) }()
	}
	ready := se.Ready
	if controlDB != nil {
		ready = func() error {
			if err := se.Ready(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := controlDB.PingContext(ctx); err != nil {
				return fmt.Errorf("projection database unavailable")
			}
			return nil
		}
	}
	bodyLimit := ""
	if ingress != nil {
		bodyLimit = "3M"
	}
	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		BodyLimit:  bodyLimit,
		// Starting with no backends configured is not a fatal error — it is a
		// legitimate state to boot into while the configuration is still being
		// written — but it must not read as healthy. Before the move into this
		// repository /readyz was a copy of /healthz, so a service with nothing
		// behind it reported healthy to compose while failing every single
		// query. Ready is what tells those two states apart, and it does not
		// dial the store; see engine.SearchEngine.Ready.
		Ready: ready,
	})

	search.RegisterRoutes(srv.App(), &search.Deps{
		Engine:     se,
		Projection: ingress,
		Token:      cfg.ServiceToken,
	})

	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gorge-search: %v\n", err)
		os.Exit(1)
	}
}
