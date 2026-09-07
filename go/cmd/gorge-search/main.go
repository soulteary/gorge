// Command gorge-search serves the search domain: Phorge's fulltext index and
// queries, translated onto whichever store a deployment configured.
//
// Like gorge-mailer and unlike gorge-render it gets a process of its own,
// because it is the opposite of pure computation: it holds a per-host health
// table, it connects out to Elasticsearch or Meilisearch, and it has a
// readiness condition worth reporting.
package main

import (
	"fmt"
	"os"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search"
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

	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// Starting with no backends configured is not a fatal error — it is a
		// legitimate state to boot into while the configuration is still being
		// written — but it must not read as healthy. Before the move into this
		// repository /readyz was a copy of /healthz, so a service with nothing
		// behind it reported healthy to compose while failing every single
		// query. Ready is what tells those two states apart, and it does not
		// dial the store; see engine.SearchEngine.Ready.
		Ready: se.Ready,
	})

	search.RegisterRoutes(srv.App(), &search.Deps{
		Engine: se,
		Token:  cfg.ServiceToken,
	})

	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gorge-search: %v\n", err)
		os.Exit(1)
	}
}
