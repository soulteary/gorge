package taskqueue

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// readyTimeout bounds the readiness probe, matching the webhook domain's.
const readyTimeout = 5 * time.Second

// LeaseOwnerHeader carries the identity a worker leases tasks under. It travels
// in a header rather than the body because it identifies the caller, not the
// request, and the same value is written into worker_activetask.leaseOwner —
// which is Phorge's column, read back by its daemon console, so the value a
// worker sends is part of the contract. See compat/phorge/README.md.
const LeaseOwnerHeader = "X-Lease-Owner"

// Deps is everything the task queue routes need to serve a request.
type Deps struct {
	Store Store
	Token string
}

// RegisterRoutes mounts the task queue endpoints under /api/queue.
//
// Every path here is part of the contract: Phorge's PhabricatorGorgeTaskQueue
// client and the gorge-worker consumer call them as written. /healthz, /readyz
// and / come from platform/health.
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/queue")
	g.Use(auth.Token(deps.Token))

	g.Post("/enqueue", enqueue(deps))
	g.Post("/lease", lease(deps))
	g.Post("/complete", complete(deps))
	g.Post("/fail", fail(deps))
	g.Post("/yield", yield(deps))
	g.Post("/cancel", cancel(deps))
	g.Post("/awaken", awaken(deps))
	g.Get("/stats", stats(deps))
	g.Get("/tasks", listTasks(deps))
	g.Get("/tasks/:id", getTask(deps))
}

// ReadyProbe adapts a Store to the platform's readiness probe. Readiness is a
// single condition — the backend answers — because there is nothing else this
// service could be waiting for and nothing it can do without it, exactly as in
// the webhook domain.
func ReadyProbe(s Store) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
		defer cancel()
		return s.Ready(ctx)
	}
}

func enqueue(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.EnqueueRequest
		if err := c.Bind().Body(&req); err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}
		if req.TaskClass == "" {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "taskClass is required")
		}
		task, err := deps.Store.Enqueue(c.Context(), &req)
		if err != nil {
			return err
		}
		return httpx.OK(c, task)
	}
}

func lease(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.LeaseRequest
		if err := c.Bind().Body(&req); err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}
		if req.Limit <= 0 {
			req.Limit = 1
		}

		// The owner identifies the worker that now holds the task. When a
		// caller sends none the client IP is used, which is enough to tell one
		// worker's leases from another's in a deployment that did not bother to
		// set the header.
		leaseOwner := c.Get(LeaseOwnerHeader)
		if leaseOwner == "" {
			leaseOwner = c.IP() + ":gorge-taskqueue"
		}

		tasks, err := deps.Store.Lease(c.Context(), req.Limit, leaseOwner, req.TaskClasses)
		if err != nil {
			return err
		}
		if tasks == nil {
			tasks = []*contracts.Task{}
		}
		return httpx.OK(c, tasks)
	}
}

func complete(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.CompleteRequest
		if err := c.Bind().Body(&req); err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}
		if req.TaskID <= 0 {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "taskID is required")
		}
		archived, err := deps.Store.Complete(c.Context(), req.TaskID, req.Duration)
		if err != nil {
			return err
		}
		return httpx.OK(c, archived)
	}
}

func fail(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.FailRequest
		if err := c.Bind().Body(&req); err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}
		if req.TaskID <= 0 {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "taskID is required")
		}
		if err := deps.Store.Fail(c.Context(), &req); err != nil {
			return err
		}
		return httpx.OK(c, fiber.Map{"status": "ok"})
	}
}

func yield(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.YieldRequest
		if err := c.Bind().Body(&req); err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}
		if req.TaskID <= 0 {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "taskID is required")
		}
		if err := deps.Store.Yield(c.Context(), req.TaskID, req.Duration); err != nil {
			return err
		}
		return httpx.OK(c, fiber.Map{"status": "ok"})
	}
}

func cancel(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.CancelRequest
		if err := c.Bind().Body(&req); err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}
		if req.TaskID <= 0 {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "taskID is required")
		}
		archived, err := deps.Store.Cancel(c.Context(), req.TaskID)
		if err != nil {
			return err
		}
		return httpx.OK(c, archived)
	}
}

func awaken(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.AwakenRequest
		if err := c.Bind().Body(&req); err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}
		if len(req.TaskIDs) == 0 {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "taskIDs is required")
		}
		affected, err := deps.Store.Awaken(c.Context(), req.TaskIDs)
		if err != nil {
			return err
		}
		return httpx.OK(c, fiber.Map{"awakened": affected})
	}
}

func stats(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		s, err := deps.Store.Stats(c.Context())
		if err != nil {
			return err
		}
		return httpx.OK(c, s)
	}
}

func listTasks(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		limit, _ := strconv.Atoi(c.Query("limit"))
		offset, _ := strconv.Atoi(c.Query("offset"))
		if limit <= 0 {
			limit = 100
		}
		tasks, err := deps.Store.ListActive(c.Context(), limit, offset)
		if err != nil {
			return err
		}
		if tasks == nil {
			tasks = []*contracts.Task{}
		}
		return httpx.OK(c, tasks)
	}
}

func getTask(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		id, err := strconv.ParseInt(c.Params("id"), 10, 64)
		if err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "invalid task ID")
		}
		task, err := deps.Store.GetTask(c.Context(), id)
		if err != nil {
			return err
		}
		if task == nil {
			return httpx.Fail(c, http.StatusNotFound, httpx.CodeNotFound, "task not found")
		}
		return httpx.OK(c, task)
	}
}
