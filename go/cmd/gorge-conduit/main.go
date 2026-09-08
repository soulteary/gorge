// Command gorge-conduit is the Conduit API gateway: it fronts Phorge's
// Conduit surface (`ANY /api/:method`) with shared-token auth, a per-IP rate
// limiter and a body limit, then relays the call to the upstream Phorge PHP
// app. It is the one gorge service whose callers are other Go services rather
// than Phorge itself; see docs/modules/conduit.md.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/soulteary/gorge/go/internal/conduit"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func main() {
	cfg, err := conduit.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-conduit: failed to load config: %v\n", err)
		os.Exit(1)
	}

	srv := httpx.New(httpx.Config{
		ListenAddr: cfg.ListenAddr,
		// The body limit is enforced by fasthttp before any handler runs, the
		// same spelling ("10M") the pre-monorepo gateway used.
		BodyLimit:       cfg.MaxBodySize,
		ShutdownTimeout: time.Duration(cfg.ProxyTimeoutSec) * time.Second,
		// The gateway holds no state of its own: it either can reach the
		// upstream on a given request or answers ERR-CONDUIT-PROXY for that one.
		// A readiness probe against the upstream would only add a second, slower
		// signal that says the same thing, so readiness equals liveness.
		Ready: nil,
	})

	proxy := conduit.NewProxy(cfg.UpstreamURL, cfg.ProxyTimeoutSec)

	deps := &conduit.Deps{
		Proxy: proxy,
		Token: cfg.ServiceToken,
	}
	if cfg.RateLimitRPS > 0 {
		rl := conduit.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst, cfg.RateLimitExempt)
		// The eviction loop is the only thing to unwind, and Run blocks until a
		// signal arrives, so a defer here ends it on the way out. If Run ever
		// returns through a listener error instead, the deferred Stop still
		// fires before the process exits.
		defer rl.Stop()
		deps.RateLimiter = rl
	}

	conduit.RegisterRoutes(srv.App(), deps)

	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gorge-conduit: %v\n", err)
		os.Exit(1)
	}
}
