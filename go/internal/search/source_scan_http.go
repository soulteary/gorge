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

type sourceJobStore interface {
	CreateSourceJob(context.Context, string, string, projection.Target, *projection.SourceCatalog) (*projection.SourceJob, error)
	SourceJob(context.Context, string, string) (*projection.SourceJob, error)
	CheckSourceJob(context.Context, string, string) (*projection.SourceScanCheck, error)
}
type SourceScanRuntime struct {
	Store    sourceJobStore
	Provider projection.SourceProvider
}

func registerSourceScanRoutes(g fiber.Router, in *ProjectionIngress) {
	if in.SourceScanner == nil {
		return
	}
	g.Post("/source-scans", func(c fiber.Ctx) error {
		var req struct {
			JobID        string `json:"jobID"`
			BackendID    string `json:"backendID"`
			GenerationID string `json:"generationID"`
		}
		dec := json.NewDecoder(bytes.NewReader(c.Body()))
		dec.DisallowUnknownFields()
		if len(c.Body()) > 2048 || dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || projection.ValidateNamespace(req.JobID) != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "valid source scan request required")
		}
		target := projection.Target{BackendID: req.BackendID, GenerationID: req.GenerationID}
		allowed := false
		for _, t := range in.Targets {
			if t == target {
				allowed = true
			}
		}
		if !allowed {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "configured target required")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 20*time.Second)
		defer cancel()
		existing, err := in.SourceScanner.Store.SourceJob(ctx, in.Namespace, req.JobID)
		if err == nil {
			if existing.Target != target {
				return rebuildResult(c, nil, projection.ErrRebuildConflict)
			}
			return httpx.OK(c, existing)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return rebuildResult(c, nil, err)
		}
		catalog, err := in.SourceScanner.Provider.Catalog(ctx)
		if err != nil {
			return rebuildResult(c, nil, err)
		}
		if err = projection.ValidateCatalog(catalog, in.Namespace); err != nil {
			return rebuildResult(c, nil, err)
		}
		job, err := in.SourceScanner.Store.CreateSourceJob(ctx, in.Namespace, req.JobID, target, catalog)
		return rebuildResult(c, job, err)
	})
	g.Get("/source-scans/:jobID", func(c fiber.Ctx) error {
		id := c.Params("jobID")
		if projection.ValidateNamespace(id) != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "valid job ID required")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		job, err := in.SourceScanner.Store.SourceJob(ctx, in.Namespace, id)
		return rebuildResult(c, job, err)
	})
	g.Post("/source-scans/:jobID/check", func(c fiber.Ctx) error {
		id := c.Params("jobID")
		if projection.ValidateNamespace(id) != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "valid job ID required")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		result, err := in.SourceScanner.Store.CheckSourceJob(ctx, in.Namespace, id)
		return rebuildResult(c, result, err)
	})

}
