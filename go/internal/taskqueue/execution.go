package taskqueue

import (
	"context"
	"errors"
	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"net/http"
)

var ErrLeaseConflict = errors.New("task lease no longer belongs to this execution")

// ExecutionStore is the versioned extension needed before delegated work runs.
// Legacy queue routes remain available for old PHP clients during migration.
type ExecutionStore interface {
	Finalize(context.Context, *contracts.FinalizeRequest) error
	Renew(context.Context, *contracts.RenewRequest) (*contracts.Task, error)
}

func registerExecutionRoutes(g fiber.Router, deps *Deps) {
	g.Get("/meta", func(c fiber.Ctx) error {
		version := 0
		if _, ok := deps.Store.(ExecutionStore); ok {
			version = 1
		}
		return httpx.OK(c, contracts.ExecutionCapabilities{ExecutionVersion: version})
	})
	g.Post("/finalize", func(c fiber.Ctx) error {
		var req contracts.FinalizeRequest
		if err := c.Bind().Body(&req); err != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid finalization")
		}
		if !validExecutionLease(req.ExecutionLease) || req.Duration < 0 || len(req.Followups) > 1000 {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid finalization")
		}
		for _, child := range req.Followups {
			if child.TaskClass == "" {
				return httpx.Fail(c, 400, httpx.CodeBadRequest, "followup taskClass is required")
			}
		}
		store, ok := deps.Store.(ExecutionStore)
		if !ok {
			return httpx.Fail(c, 503, httpx.CodeInternal, "execution protocol unavailable")
		}
		if err := store.Finalize(c.Context(), &req); err != nil {
			return executionError(c, err)
		}
		return httpx.OK(c, map[string]string{"status": "finalized"})
	})
	g.Post("/renew", func(c fiber.Ctx) error {
		var req contracts.RenewRequest
		if err := c.Bind().Body(&req); err != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid renewal")
		}
		if !validExecutionLease(req.ExecutionLease) || req.Duration <= 0 || req.Duration > 7*24*3600 {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid renewal")
		}
		store, ok := deps.Store.(ExecutionStore)
		if !ok {
			return httpx.Fail(c, 503, httpx.CodeInternal, "execution protocol unavailable")
		}
		task, err := store.Renew(c.Context(), &req)
		if err != nil {
			return executionError(c, err)
		}
		return httpx.OK(c, task)
	})
}
func validExecutionLease(l contracts.ExecutionLease) bool {
	return l.TaskID > 0 && l.LeaseOwner != "" && l.LeaseExpires > 0
}
func executionError(c fiber.Ctx, err error) error {
	if errors.Is(err, ErrLeaseConflict) {
		return httpx.Fail(c, http.StatusConflict, "ERR_LEASE_CONFLICT", "task lease no longer belongs to this execution")
	}
	return err
}
