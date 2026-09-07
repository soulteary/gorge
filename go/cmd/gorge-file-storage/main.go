// Command gorge-file-storage serves the file storage domain: the bytes of
// Phorge's files, in a MySQL blob, on local disk or in an S3 bucket.
//
// It gets a process of its own for the same reason the mailer does — it holds
// external state and has a readiness condition worth reporting — and it is the
// first binary in the repository to open a database connection.
package main

import (
	"fmt"
	"os"

	"github.com/soulteary/gorge/go/internal/filestorage"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func main() {
	cfg := filestorage.LoadFromEnv()

	router, err := filestorage.NewRouterFromConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-file-storage: failed to initialise the storage backends: %v\n", err)
		os.Exit(1)
	}

	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// The file arrives as the raw request body, so the platform's 2M
		// default would cap every upload at 2 MB. Phorge chunks anything over
		// 8 MB before it gets here.
		BodyLimit: filestorage.TransportBodyLimit,
		// Starting with no backend configured is not a fatal error — it is a
		// legitimate state to boot into while the configuration is still being
		// written — but it must not read as healthy, or orchestration keeps a
		// service in rotation that fails every file it is handed.
		Ready: router.Ready,
	})

	filestorage.RegisterRoutes(srv.Echo(), &filestorage.Deps{
		Router: router,
		Token:  cfg.ServiceToken,
	})

	// Closed explicitly rather than deferred: os.Exit below would skip a
	// deferred close, and the connection pool is the one thing here worth
	// releasing on the way out.
	runErr := srv.Run()
	if closeErr := router.Close(); closeErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-file-storage: failed to close the storage backends: %v\n", closeErr)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-file-storage: %v\n", runErr)
		os.Exit(1)
	}
}
