// Command gorge-mailer serves the mailer domain: outbound email for Phorge,
// fanned out over whichever of the seven backends a deployment configured.
//
// Unlike render and diff it gets a process of its own, because it is the
// opposite of pure computation: it holds adapter state, it talks to SMTP
// servers and provider APIs, and it has a readiness condition worth reporting.
package main

import (
	"fmt"
	"os"

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

	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// Attachments arrive base64-encoded inside the JSON body, so the
		// platform's 2M default is not enough.
		BodyLimit: mailer.TransportBodyLimit,
		// Starting with no backends configured is not a fatal error — it is a
		// legitimate state to boot into while the configuration is still being
		// written — but it must not read as healthy, or orchestration keeps a
		// service in rotation that fails every message it accepts.
		Ready: dispatcher.Ready,
	})

	mailer.RegisterRoutes(srv.Echo(), &mailer.Deps{
		Dispatcher: dispatcher,
		Token:      cfg.ServiceToken,
		BodyLimit:  cfg.BodyLimit,
	})

	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gorge-mailer: %v\n", err)
		os.Exit(1)
	}
}
