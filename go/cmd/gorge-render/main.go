// Command gorge-render serves the rendering domain: syntax highlighting today,
// diff rendering later, from a single process.
package main

import (
	"fmt"
	"os"
	"time"

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

	srv := httpx.New(httpx.Config{
		ListenAddr:      cfg.ListenAddr,
		ShutdownTimeout: time.Duration(cfg.TimeoutSec) * time.Second,
		// Render holds no external dependency, so readiness equals liveness.
		Ready: nil,
	})

	render.RegisterRoutes(srv.Echo(), &render.Deps{
		Highlighter: highlight.New(),
		Token:       cfg.ServiceToken,
		MaxBytes:    cfg.MaxBytes,
	})

	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gorge-render: %v\n", err)
		os.Exit(1)
	}
}
