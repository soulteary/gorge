// Command gorge-db-api serves the database-management domain: it reports on
// Phorge's MySQL cluster — the configured servers, each one's health and
// replication state, its schema and environment against what Phorge expects,
// and how far its migrations have run.
//
// It holds no state of its own; every answer is a live query against the
// cluster it is pointed at. Like gorge-file-storage it opens database
// connections lazily, so it starts before MySQL is up and reports the
// difference through /readyz rather than crash-looping.
package main

import (
	"fmt"
	"os"

	"github.com/soulteary/gorge/go/internal/dbapi"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func main() {
	cfg := dbapi.LoadFromEnv()

	cluster, err := cfg.BuildCluster()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-db-api: failed to load the cluster configuration: %v\n", err)
		os.Exit(1)
	}

	deps := dbapi.NewDeps(cluster, cfg.MySQLPass, cfg.ServiceToken)

	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// Starting with an unreachable database is not fatal — the database
		// container may not be up yet — but it must not read as healthy, or
		// orchestration keeps an instance in rotation that fails every query.
		Ready: deps.Ready,
	})

	dbapi.RegisterRoutes(srv.App(), deps)

	// Closed explicitly rather than deferred: os.Exit below would skip a
	// deferred close, and the connection pools are the one thing worth
	// releasing on the way out.
	runErr := srv.Run()
	if closeErr := deps.Close(); closeErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-db-api: failed to close the database pools: %v\n", closeErr)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-db-api: %v\n", runErr)
		os.Exit(1)
	}
}
