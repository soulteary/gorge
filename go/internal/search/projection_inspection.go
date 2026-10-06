package search

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type projectionInspector interface {
	EventStatus(context.Context, string, string) (*projection.EventStatus, error)
	DeliveryStats(context.Context, string, []projection.Target) (*projection.DeliveryStats, error)
}

func registerProjectionInspection(g fiber.Router, ingress *ProjectionIngress) {
	if ingress.Inspector == nil {
		return
	}
	g.Get("/status", func(c fiber.Ctx) error {
		id := c.Query("eventID")
		if err := projection.ValidateEventID(id); err != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "valid eventID required")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		result, err := ingress.Inspector.EventStatus(ctx, ingress.Namespace, id)
		if errors.Is(err, sql.ErrNoRows) {
			return httpx.Fail(c, 404, "ERR_PROJECTION_NOT_FOUND", "projection receipt not found")
		}
		if err != nil {
			return httpx.Fail(c, 503, "ERR_PROJECTION_UNAVAILABLE", "projection inspection unavailable")
		}
		return httpx.OK(c, result)
	})
	g.Get("/stats", func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		result, err := ingress.Inspector.DeliveryStats(ctx, ingress.Namespace, ingress.Targets)
		if err != nil {
			return httpx.Fail(c, 503, "ERR_PROJECTION_UNAVAILABLE", "projection inspection unavailable")
		}
		if ingress.SourceOutbox != nil {
			result.SourceOutbox, err = projection.InspectSourceOutbox(ctx, ingress.SourceOutbox)
			if err != nil {
				return httpx.Fail(c, 503, "ERR_PROJECTION_UNAVAILABLE", "source outbox inspection unavailable")
			}
		}
		return httpx.OK(c, result)
	})
}
