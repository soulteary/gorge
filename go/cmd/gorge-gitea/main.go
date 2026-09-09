// Command gorge-gitea is the one-way Gitea-to-Phorge event bridge.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/soulteary/gorge/go/internal/gitea"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func main() {
	cfg := gitea.LoadFromEnv()
	srv := httpx.New(httpx.Config{
		ListenAddr:      cfg.ListenAddr,
		ShutdownTimeout: time.Duration(cfg.TimeoutSec) * time.Second,
		Ready:           cfg.Ready,
	})
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second}
	gitea.RegisterRoutes(srv.App(), &gitea.Deps{
		Sink:  gitea.NewConduitSink(cfg.ConduitURL, cfg.ConduitToken, cfg.GatewayToken, client),
		Token: cfg.ServiceToken, WebhookSecret: cfg.WebhookSecret, BaseURL: cfg.BaseURL,
	})
	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gorge-gitea: %v\n", err)
		os.Exit(1)
	}
}
