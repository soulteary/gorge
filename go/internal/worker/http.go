package worker

import (
	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// Deps is everything the worker's status route needs.
type Deps struct {
	Consumer *Consumer
	Token    string
}

// RegisterRoutes mounts the worker status endpoint under /api/worker.
//
// The path is part of the contract: a Phorge setup check reads
// /api/worker/stats to confirm the consumer is alive and to see which classes
// it handles. /healthz, /readyz and / come from platform/health.
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/worker")
	g.Use(auth.Token(deps.Token))

	g.Get("/stats", stats(deps))
}

// stats answers this worker's lifetime counters and supported classes.
//
// It reads in-process counters rather than the queue, so it never fails and
// needs no error path: a worker that can answer at all can answer this.
func stats(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		s := deps.Consumer.Stats()
		return httpx.OK(c, &s)
	}
}
