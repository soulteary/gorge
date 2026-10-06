package taskqueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

var ErrEventConflict = errors.New("event ID already contains another payload")

type InboxStore interface {
	EnqueueEvent(context.Context, *contracts.EnqueueEventRequest) (*contracts.Task, error)
}

func eventDigest(req *contracts.EnqueueEventRequest) (string, []byte, error) {
	raw, err := json.Marshal(req.Task)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), raw, err
}
func registerInboxRoutes(g fiber.Router, deps *Deps) {
	g.Post("/enqueue-event", func(c fiber.Ctx) error {
		var req contracts.EnqueueEventRequest
		if err := c.Bind().Body(&req); err != nil || req.EventID == "" || len(req.EventID) > 128 || req.Task.TaskClass == "" {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid event")
		}
		store, ok := deps.Store.(InboxStore)
		if !ok {
			return httpx.Fail(c, 503, httpx.CodeInternal, "inbox unavailable")
		}
		task, err := store.EnqueueEvent(c.Context(), &req)
		if errors.Is(err, ErrEventConflict) {
			return httpx.Fail(c, 409, "ERR_EVENT_CONFLICT", err.Error())
		}
		if err != nil {
			return err
		}
		return httpx.OK(c, task)
	})
}
