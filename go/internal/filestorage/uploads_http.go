package filestorage

import (
	"context"
	"errors"
	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"os"
	"strconv"
	"time"
)

func registerUploadRoutes(app fiber.Router, u *Uploads, token string) {
	g := app.Group("/api/file/uploads")
	g.Use(auth.Token(token, auth.WithQueryToken(false)))
	g.Get("/meta", func(c fiber.Ctx) error {
		return httpx.OK(c, map[string]any{"protocolVersion": 1, "integrityVersion": 1, "enabled": u != nil, "chunkSize": UploadChunkSize, "maxSize": MaxUploadSize, "storageFormat": "raw", "durability": "posix-volume"})
	})
	if u == nil {
		return
	}
	g.Get("/usage", func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		size, parseErr := strconv.Atoi(c.Query("limit", "10000"))
		if parseErr != nil {
			return httpx.Fail(c, 400, "ERR_USAGE_PAGE", "invalid limit")
		}
		if size < 1 || size > 10000 {
			return httpx.Fail(c, 400, "ERR_USAGE_PAGE", "limit must be between 1 and 10000")
		}
		usage, err := u.UsagePage(ctx, c.Query("cursor"), size)
		if err != nil {
			return httpx.Fail(c, 503, "ERR_UPLOAD_USAGE", "upload inventory unavailable")
		}
		return httpx.OK(c, usage)
	})
	report := func(c fiber.Ctx, e error) error {
		switch {
		case errors.Is(e, ErrUploadConflict):
			return httpx.Fail(c, 409, "ERR_UPLOAD_CONFLICT", e.Error())
		case errors.Is(e, ErrBadHandle):
			return httpx.Fail(c, 400, httpx.CodeBadRequest, e.Error())
		case errors.Is(e, ErrUploadExpired):
			return httpx.Fail(c, 410, "ERR_UPLOAD_EXPIRED", e.Error())
		case errors.Is(e, os.ErrNotExist):
			return httpx.Fail(c, 404, httpx.CodeNotFound, "upload not found")
		}
		return e
	}
	g.Post("/session", func(c fiber.Ctx) error {
		var req struct {
			ID   string `json:"id"`
			Size int64  `json:"size"`
		}
		if e := c.Bind().Body(&req); e != nil || req.Size <= 0 || req.Size > MaxUploadSize {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid upload")
		}
		s, e := u.Create(c.Context(), req.ID, req.Size)
		if e != nil {
			return report(c, e)
		}
		return httpx.OK(c, s)
	})
	g.Get("/:id", func(c fiber.Ctx) error {
		s, e := u.Status(c.Context(), c.Params("id"))
		if e != nil {
			return report(c, e)
		}
		return httpx.OK(c, s)
	})
	g.Put("/:id/chunk", func(c fiber.Ctx) error {
		start, e := strconv.ParseInt(c.Query("start"), 10, 64)
		if e != nil || c.Request().Header.ContentLength() != len(c.Body()) {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid chunk offset or length")
		}
		s, e := u.Put(c.Context(), c.Params("id"), start, c.Body())
		if e != nil {
			return report(c, e)
		}
		return httpx.OK(c, s)
	})
	g.Post("/:id/complete", func(c fiber.Ctx) error {
		s, e := u.Complete(c.Context(), c.Params("id"))
		if e != nil {
			return report(c, e)
		}
		return httpx.OK(c, s)
	})
	g.Post("/:id/verify", func(c fiber.Ctx) error {
		s, e := u.Verify(c.Context(), c.Params("id"))
		if e != nil {
			return report(c, e)
		}
		return httpx.OK(c, s)
	})
	g.Delete("/:id", func(c fiber.Ctx) error {
		if e := u.Cancel(c.Context(), c.Params("id")); e != nil {
			return report(c, e)
		}
		return httpx.OK(c, map[string]string{"state": "cancelled"})
	})
	g.Get("/:id/data", func(c fiber.Ctx) error {
		start, e := strconv.ParseInt(c.Query("start"), 10, 64)
		if e != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid start")
		}
		end, e := strconv.ParseInt(c.Query("end"), 10, 64)
		if e != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "invalid end")
		}
		r, n, e := u.Read(c.Context(), c.Params("id"), start, end)
		if e != nil {
			return report(c, e)
		}
		c.Set("Content-Type", contentTypeBlob)
		return c.SendStream(r, int(n))
	})
}
