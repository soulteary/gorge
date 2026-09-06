// Command gorge-render serves two domains from a single process: render
// (syntax highlighting) and diff (unified and prose diffs). Both are pure
// computation with no external dependency, so splitting them across processes
// would buy nothing but a second thing to deploy.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/soulteary/gorge/go/internal/diff"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/render"
	"github.com/soulteary/gorge/go/internal/render/highlight"
)

func main() {
	cfg, err := render.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-render: failed to load config: %v\n", err)
		os.Exit(1)
	}
	diffCfg := diff.LoadFromEnv()

	srv := httpx.New(httpx.Config{
		ListenAddr:      cfg.ListenAddr,
		ShutdownTimeout: time.Duration(cfg.TimeoutSec) * time.Second,
		// Neither domain holds an external dependency, so readiness equals
		// liveness.
		Ready: nil,
	})

	render.RegisterRoutes(srv.Echo(), &render.Deps{
		Highlighter: highlight.New(),
		Token:       cfg.ServiceToken,
		MaxBytes:    cfg.MaxBytes,
	})

	// One token guards both domains: it authenticates the caller to this
	// process, not to a particular route group.
	diff.RegisterRoutes(srv.Echo(), &diff.Deps{
		Token:    cfg.ServiceToken,
		MaxBytes: diffCfg.MaxBytes,
	})

	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gorge-render: %v\n", err)
		os.Exit(1)
	}
}
