package search

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type projectionRebuilder interface {
	CreateRebuild(context.Context, string, string, projection.Target) (*projection.RebuildJob, error)
	RebuildJob(context.Context, string, string) (*projection.RebuildJob, error)
	CheckRebuild(context.Context, string, string) (*projection.RebuildCheck, error)
}

func rebuildResult(c fiber.Ctx, result any, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.Fail(c, 404, "ERR_REBUILD_NOT_FOUND", "rebuild job or bound generation not found")
	}
	if errors.Is(err, projection.ErrRebuildConflict) {
		return httpx.Fail(c, 409, "ERR_REBUILD_CONFLICT", "job ID already belongs to another generation")
	}
	if err != nil {
		return httpx.Fail(c, 503, "ERR_REBUILD_UNAVAILABLE", "rebuild control unavailable")
	}
	return httpx.OK(c, result)
}
func registerProjectionRebuild(g fiber.Router, in *ProjectionIngress) {
	if in.Rebuilder == nil {
		return
	}
	g.Post("/rebuilds", func(c fiber.Ctx) error {
		var req struct {
			JobID        string `json:"jobID"`
			BackendID    string `json:"backendID"`
			GenerationID string `json:"generationID"`
		}
		dec := json.NewDecoder(bytes.NewReader(c.Body()))
		dec.DisallowUnknownFields()
		if len(c.Body()) > 2048 || dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || projection.ValidateNamespace(req.JobID) != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "valid rebuild request required")
		}
		target := projection.Target{BackendID: req.BackendID, GenerationID: req.GenerationID}
		allowed := false
		for _, t := range in.Targets {
			if t == target {
				allowed = true
			}
		}
		if !allowed {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "configured delivery target required")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		result, err := in.Rebuilder.CreateRebuild(ctx, in.Namespace, req.JobID, target)
		return rebuildResult(c, result, err)
	})
	g.Get("/rebuilds/:jobID", func(c fiber.Ctx) error {
		id := c.Params("jobID")
		if projection.ValidateNamespace(id) != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "valid job ID required")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		result, err := in.Rebuilder.RebuildJob(ctx, in.Namespace, id)
		return rebuildResult(c, result, err)
	})
	g.Post("/rebuilds/:jobID/check", func(c fiber.Ctx) error {
		id := c.Params("jobID")
		if projection.ValidateNamespace(id) != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "valid job ID required")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		result, err := in.Rebuilder.CheckRebuild(ctx, in.Namespace, id)
		return rebuildResult(c, result, err)
	})
}
