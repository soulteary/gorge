// Command gorge-file-storage serves the file storage domain: the bytes of
// Phorge's files, in a MySQL blob, on local disk or in an S3 bucket.
//
// It gets a process of its own for the same reason the mailer does — it holds
// external state and has a readiness condition worth reporting — and it is the
// first binary in the repository to open a database connection.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"

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

	var uploads *filestorage.Uploads
	if cfg.UploadRoot != "" {
		if cfg.ServiceToken == "" {
			fmt.Fprintln(os.Stderr, "uploads require a service token")
			os.Exit(1)
		}
		uploads, err = filestorage.NewUploads(cfg.UploadRoot)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	var deletionDB *sql.DB
	if cfg.DeletionDSN != "" {
		if err = filestorage.ValidateDeletionDatabase(cfg.Namespace, cfg.DeletionDSN); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if cfg.ServiceToken == "" {
			fmt.Fprintln(os.Stderr, "deletion consumer requires a service token")
			os.Exit(1)
		}
		deletionDB, err = filestorage.OpenDB(cfg.DeletionDSN)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if deletionDB != nil {
					batch, c := context.WithTimeout(ctx, 30*time.Second)
					err := filestorage.ProcessDeletion(batch, deletionDB, router, uploads)
					c()
					if err != nil {
						slog.Error("file deletion retry failed", "error", err)
					}
				}
				if uploads != nil {
					if err := uploads.SweepExpired(ctx, 100); err != nil {
						slog.Error("upload expiry failed", "error", err)
					}
				}
			}
		}
	}()
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
		Ready: func() error {
			if e := router.Ready(); e != nil {
				return e
			}
			if deletionDB != nil {
				ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
				defer c()
				return deletionDB.PingContext(ctx)
			}
			return nil
		},
	})

	filestorage.RegisterRoutes(srv.App(), &filestorage.Deps{
		Router:          router,
		Token:           cfg.ServiceToken,
		Uploads:         uploads,
		DeletionEnabled: deletionDB != nil,
	})

	// Closed explicitly rather than deferred: os.Exit below would skip a
	// deferred close, and the connection pool is the one thing here worth
	// releasing on the way out.
	runErr := srv.Run()
	cancel()
	<-done
	if deletionDB != nil {
		_ = deletionDB.Close()
	}
	if closeErr := router.Close(); closeErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-file-storage: failed to close the storage backends: %v\n", closeErr)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "gorge-file-storage: %v\n", runErr)
		os.Exit(1)
	}
}
