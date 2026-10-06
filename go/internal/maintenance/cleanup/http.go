package cleanup

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func RegisterRoutes(app fiber.Router, s *Store, token string) {
	g := app.Group("/api/maintenance", auth.Token(token, auth.WithQueryToken(false)))
	load := func(c fiber.Ctx) ([]State, error) {
		ctx, cancel := context.WithTimeout(c.Context(), 3*time.Second)
		defer cancel()
		states := []State{}
		for _, spec := range specs {
			if s.DBs[spec.Role] == nil {
				continue
			}
			st, err := s.Status(ctx, spec.ID)
			if err != nil {
				return nil, err
			}
			states = append(states, st)
		}
		return states, nil
	}
	g.Get("/collectors", func(c fiber.Ctx) error {
		states, err := load(c)
		if err != nil {
			return httpx.Fail(c, http.StatusServiceUnavailable, "ERR_CLEANUP_UNAVAILABLE", "cleanup state unavailable")
		}
		return httpx.OK(c, states)
	})
	g.Get("/metrics", func(c fiber.Ctx) error {
		states, err := load(c)
		if err != nil {
			return httpx.Fail(c, http.StatusServiceUnavailable, "ERR_CLEANUP_UNAVAILABLE", "cleanup state unavailable")
		}
		var b strings.Builder
		b.WriteString("# TYPE gorge_cleanup_deleted_rows_total counter\n# TYPE gorge_cleanup_last_success_seconds gauge\n# TYPE gorge_cleanup_failures gauge\n")
		for _, st := range states {
			fmt.Fprintf(&b, "gorge_cleanup_deleted_rows_total{collector=%q} %d\ngorge_cleanup_last_success_seconds{collector=%q} %d\ngorge_cleanup_failures{collector=%q} %d\n", st.ID, st.Deleted, st.ID, st.LastSuccess, st.ID, st.Failures)
		}
		c.Set("Content-Type", "text/plain; version=0.0.4")
		return c.SendString(b.String())
	})
}
