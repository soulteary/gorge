package imagetransform

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"strconv"
)

func RegisterRoutes(app fiber.Router, s *Service, token string) {
	g := app.Group("/api/image")
	g.Use(auth.Token(token, auth.WithQueryToken(false)))
	g.Get("/stats", func(c fiber.Ctx) error {
		s.cache.mu.Lock()
		count, size := len(s.cache.entries), s.cache.bytes
		s.cache.mu.Unlock()
		return httpx.OK(c, map[string]any{"computations": s.computations.Load(), "cacheHits": s.cacheHits.Load(), "sharedWaits": s.sharedWaits.Load(), "cacheEntries": count, "cacheBytes": size})
	})
	g.Get("/capabilities", func(c fiber.Ctx) error {
		return httpx.OK(c, map[string]any{"protocolVersion": 1, "compose": map[string]any{"revision": ComposeRevision, "recipes": []string{"avatar", "icon", "favicon"}}, "meme": map[string]any{"revision": MemeRevision, "fontRevision": s.MemeFontRevision, "maxTextBytes": MemeMaxTextBytes, "maxPixels": MemeMaxPixels}, "recipeRevision": Revision, "backendRevision": s.BackendRevision, "recipes": Recipes, "inputFormats": []string{"image/jpeg", "image/png", "image/gif", "image/webp"}, "maxBytes": MaxBytes, "maxPixels": MaxPixels, "maxFrames": 100, "animationPolicies": []string{"legacy-static", "legacy-preserve"}})
	})
	check := func(c fiber.Ctx) error {
		n := c.Request().Header.ContentLength()
		if n < 0 || n != len(c.Body()) {
			return fail(400, "ERR_BAD_REQUEST", "a matching Content-Length is required")
		}
		return nil
	}
	report := func(c fiber.Ctx, e error) error {
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			return httpx.Fail(c, 504, "ERR_IMAGE_TIMEOUT", "image request canceled")
		}
		var f *Failure
		if errors.As(e, &f) {
			return httpx.Fail(c, f.Status, f.Code, f.Message)
		}
		if errors.Is(e, ErrBusy) {
			c.Set("Retry-After", "1")
			return httpx.Fail(c, 429, "ERR_IMAGE_BUSY", e.Error())
		}
		return httpx.Fail(c, 500, httpx.CodeInternal, "image execution failed")
	}
	g.Post("/probe", func(c fiber.Ctx) error {
		if e := check(c); e != nil {
			return report(c, e)
		}
		i, e := s.Probe(c.Context(), c.Body())
		if e != nil {
			return report(c, e)
		}
		return httpx.OK(c, i)
	})
	g.Post("/compose", func(c fiber.Ctx) error {
		var req ComposeRequest
		if e := c.Bind().Body(&req); e != nil {
			return report(c, fail(400, "ERR_BAD_REQUEST", "invalid compose request"))
		}
		result, e := s.Compose(c.Context(), req)
		if e != nil {
			return report(c, e)
		}
		c.Set("Content-Type", "image/png")
		c.Set("ETag", fmt.Sprintf("\"%s\"", result.Digest))
		c.Set("X-Gorge-Recipe-Revision", ComposeRevision)
		c.Set("X-Gorge-Backend-Revision", "go-compose-v1")
		c.Set("X-Gorge-Image-Width", strconv.Itoa(result.Info.Width))
		c.Set("X-Gorge-Image-Height", strconv.Itoa(result.Info.Height))
		c.Set("Cache-Control", "no-store")
		return c.Send(result.Data)
	})
	g.Post("/meme", func(c fiber.Ctx) error {
		if e := check(c); e != nil {
			return report(c, e)
		}
		if c.Query("revision") != MemeRevision {
			return report(c, fail(400, "ERR_BAD_REQUEST", "unsupported meme revision"))
		}
		decode := func(key string) (string, error) {
			raw := c.Get(key)
			if len(raw) > 8192 {
				return "", fmt.Errorf("text header too large")
			}
			b, e := base64.StdEncoding.Strict().DecodeString(raw)
			return string(b), e
		}
		above, e := decode("X-Gorge-Meme-Above")
		if e != nil {
			return report(c, fail(400, "ERR_BAD_REQUEST", "invalid meme text"))
		}
		below, e := decode("X-Gorge-Meme-Below")
		if e != nil {
			return report(c, fail(400, "ERR_BAD_REQUEST", "invalid meme text"))
		}
		result, e := s.Meme(c.Context(), c.Body(), above, below, c.Query("animation", "legacy-static"))
		if e != nil {
			return report(c, e)
		}
		c.Set("Content-Type", result.Info.MimeType)
		c.Set("ETag", fmt.Sprintf("\"%s\"", result.Digest))
		c.Set("X-Gorge-Recipe-Revision", MemeRevision)
		c.Set("X-Gorge-Font-Revision", s.MemeFontRevision)
		c.Set("X-Gorge-Backend-Revision", s.BackendRevision)
		c.Set("X-Gorge-Image-Width", strconv.Itoa(result.Info.Width))
		c.Set("X-Gorge-Image-Height", strconv.Itoa(result.Info.Height))
		c.Set("Cache-Control", "no-store")
		return c.Send(result.Data)
	})
	g.Post("/transform", func(c fiber.Ctx) error {
		if e := check(c); e != nil {
			return report(c, e)
		}
		if c.Query("revision") != Revision {
			return report(c, fail(400, "ERR_BAD_REQUEST", "unsupported recipe revision"))
		}
		result, e := s.Transform(c.Context(), c.Body(), c.Query("recipe"), c.Query("animation", "legacy-static"))
		if e != nil {
			return report(c, e)
		}
		c.Set("Content-Type", result.Info.MimeType)
		c.Set("ETag", fmt.Sprintf("\"%s\"", result.Digest))
		c.Set("X-Gorge-Recipe-Revision", Revision)
		c.Set("X-Gorge-Backend-Revision", s.BackendRevision)
		c.Set("X-Gorge-Image-Width", strconv.Itoa(result.Info.Width))
		c.Set("X-Gorge-Image-Height", strconv.Itoa(result.Info.Height))
		c.Set("X-Gorge-Animation", result.Info.AnimationStatus)
		c.Set("Cache-Control", "no-store")
		return c.Send(result.Data)
	})
}
