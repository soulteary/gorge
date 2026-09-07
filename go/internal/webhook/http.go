// Package webhook is the Herald webhook delivery domain: it takes the request
// rows Phorge queues in `{namespace}_herald.herald_webhookrequest`, POSTs each
// one to its hook's URI under an HMAC-SHA256 signature, and writes the result
// back to the row it came from.
//
// It is the only domain in the repository whose work is not driven by an
// inbound request. The HTTP endpoints here are read-only status: everything
// that matters happens in the poll loop, and nothing calls in to start it.
// See docs/modules/webhook.md.
package webhook

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// readyTimeout bounds the readiness probe. A probe that hangs is reported as
// unready by the orchestrator's own timeout too, but only after a much longer
// wait.
const readyTimeout = 5 * time.Second

// This domain defines no error code of its own, and that is a decision rather
// than an omission. Both endpoints do one thing — count rows — so their only
// failure is "the database did not answer", which the platform's
// ERR_INTERNAL already says. The state a Phorge setup check actually needs to
// distinguish, "the service is up but cannot reach the queue", is what /readyz
// reports, and it reports it with a reason string a new error code could not
// improve on. See docs/architecture.md section 4.2.

// Deps is everything the webhook routes need to serve a request.
type Deps struct {
	Store Store
	Token string
}

// RegisterRoutes mounts the webhook status endpoints.
//
// Both paths are part of the contract: PhabricatorGorgeWebhookClient calls
// them as written. /healthz, /readyz and / come from platform/health.
func RegisterRoutes(e *echo.Echo, deps *Deps) {
	g := e.Group("/api/webhook")
	g.Use(auth.Token(deps.Token))

	g.GET("/stats", stats(deps))
	g.GET("/hooks", listHooks(deps))
}

// ReadyProbe adapts a Store to the platform's readiness probe.
//
// Readiness here is a single condition — the database answers — because there
// is nothing else this service could be waiting for and nothing it can do
// without it. Unlike the file storage domain there is no "configured but
// empty" state to report: a webhook service with no queue to read is not
// degraded, it is idle in a way indistinguishable from broken.
func ReadyProbe(s Store) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
		defer cancel()
		return s.Ready(ctx)
	}
}

// stats answers the queue's current shape.
//
// The counts are read from the database on every call rather than kept in
// memory, so they describe the queue as Phorge's own UI sees it — including
// the work of every other instance, and including rows that were queued while
// this process was not running.
func stats(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		result, err := deps.Store.Stats(c.Request().Context())
		if err != nil {
			// Returned raw: the platform error handler answers 500
			// ERR_INTERNAL with a generic message and logs the cause. A 5xx
			// body in this repository never describes the service's
			// internals, and here that would have meant handing a caller the
			// SQL that failed.
			return err
		}
		return httpx.OK(c, result)
	}
}

// listHooks answers how many hooks are configured, disabled ones included.
//
// It is a separate endpoint from stats rather than another field on it because
// the two answer different questions: stats counts what is *deliverable*, so
// its ActiveWebhooks excludes disabled hooks, while this one is what a setup
// check reads to tell "no hooks yet" from "hooks that are all switched off".
func listHooks(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		total, err := deps.Store.CountHooks(c.Request().Context())
		if err != nil {
			return err
		}
		return httpx.OK(c, &contracts.HookSummary{Total: total})
	}
}
