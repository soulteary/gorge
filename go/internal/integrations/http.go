package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/conduitclient"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"net/http"
	"slices"
	"time"
)

type Service struct {
	Config   Config
	Store    Store
	progress *progress
	HTTP     *http.Client
}

func New(c Config, s Store) *Service {
	return &Service{Config: c, Store: s, progress: &progress{started: time.Now().Unix(), fact: ProjectionHealth{Enabled: c.FactDSN != ""}}, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (s *Service) Register(app fiber.Router) {
	g := app.Group("/api/integrations", auth.Token(s.Config.Token, auth.WithQueryToken(false)))
	g.Get("/capabilities", func(c fiber.Ctx) error {
		targets := map[string]string{}
		for k, v := range s.Config.Targets {
			targets[k] = v.Type
		}
		return httpx.OK(c, map[string]any{"version": 2, "authentication": []string{"asana", "jira", "github"}, "inboxRetentionDays": s.Config.InboxRetentionDays, "inbound": s.Config.Inbound, "targets": targets, "fact": s.Config.FactDSN != ""})
	})
	g.Post("/resolve", func(c fiber.Ctx) error {
		var r Resolution
		if c.Bind().Body(&r) != nil || r.Validate() != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "invalid reconciliation")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 40*time.Second)
		defer cancel()
		var reconcile func(json.RawMessage) error
		if r.Domain == "inbound" {
			reconcile = func(raw json.RawMessage) error {
				var m Inbound
				if json.Unmarshal(raw, &m) != nil {
					return errors.New("missing inbox payload")
				}
				client := conduitclient.NewBounded(s.Config.ConduitURI, s.Config.ConduitToken, 1024*1024)
				response, e := client.Call(ctx, "integration.inbound", map[string]any{"eventID": r.ID, "message": m, "phase": "reconcile", "evidence": r.Evidence})
				if e != nil {
					return e
				}
				var result struct {
					State string `json:"state"`
				}
				if json.Unmarshal(response.Result, &result) != nil || result.State != "done" {
					return ErrUnknown
				}
				return nil
			}
		}
		if e := s.Store.Resolve(ctx, r, reconcile); e != nil {
			return httpx.Fail(c, 409, "ERR_RECONCILIATION", "state, digest or evidence must be checked")
		}
		return httpx.OK(c, map[string]string{"state": r.State})
	})
	g.Get("/health", func(c fiber.Ctx) error {
		health, e := s.Store.Health(c.Context())
		if e != nil {
			return httpx.Fail(c, 503, "ERR_STORE", "integration health unavailable")
		}
		return httpx.OK(c, map[string]any{"delivery": health, "fact": s.FactHealth()})
	})
	g.Get("/usage", func(c fiber.Ctx) error {
		v, e := s.Store.Usage(c.Context())
		if e != nil {
			return httpx.Fail(c, 503, "ERR_STORE", "usage unavailable")
		}
		return httpx.OK(c, v)
	})
	g.Get("/effect", func(c fiber.Ctx) error {
		var o Outcome
		var raw []byte
		var hash string
		var updated int64
		if c.Query("id") == "" {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "identity required")
		}
		e := s.Store.DB.QueryRowContext(c.Context(), "SELECT state,result,httpStatus,digest,updated FROM gorge_integration_effect WHERE id=?", c.Query("id")).Scan(&o.State, &raw, &o.Status, &hash, &updated)
		if e != nil {
			return httpx.Fail(c, 404, "ERR_NOT_FOUND", "effect unavailable")
		}
		o.Result = raw
		return httpx.OK(c, map[string]any{"state": o.State, "result": o.Result, "status": o.Status, "digest": hash, "updated": updated})
	})
	g.Post("/auth", func(c fiber.Ctx) error {
		var r AuthRequest
		if c.Bind().Body(&r) != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "invalid authentication")
		}
		t, ok := s.Config.Targets[r.Target]
		if !ok {
			return httpx.Fail(c, 422, "ERR_DISABLED", "authentication target unavailable")
		}
		if r.Validate(t) != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "invalid authentication operation")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 35*time.Second)
		defer cancel()
		out, e := Authenticate(ctx, s.HTTP, t, r)
		if e != nil {
			return httpx.Fail(c, 502, "ERR_PROVIDER", "authentication result unavailable; restart the login flow")
		}
		c.Set("Cache-Control", "no-store")
		return httpx.OK(c, out)
	})
	g.Post("/read", func(c fiber.Ctx) error {
		var r Request
		if c.Bind().Body(&r) != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "invalid read")
		}
		t, ok := s.Config.Targets[r.Target]
		if !ok || r.Method != "GET" || (t.Type != "asana" && t.Type != "jira" && t.Type != "github") {
			return httpx.Fail(c, 422, "ERR_DISABLED", "read target unavailable")
		}
		r.ID = "read"
		if r.Validate(t) != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "invalid read")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 35*time.Second)
		defer cancel()
		out, e := SendRead(ctx, s.HTTP, t, r)
		if e != nil {
			return httpx.Fail(c, 502, "ERR_PROVIDER", "read unavailable")
		}
		return httpx.OK(c, out)
	})
	g.Post("/effect", func(c fiber.Ctx) error {
		var r Request
		if e := c.Bind().Body(&r); e != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "invalid effect")
		}
		t, ok := s.Config.Targets[r.Target]
		if !ok {
			return httpx.Fail(c, 422, "ERR_DISABLED", "target not configured")
		}
		if r.Method == "GET" || t.Type == "github" {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "use the fresh read endpoint")
		}
		if r.Validate(t) != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "invalid operation")
		}
		hash, e := r.Hash(t)
		if e != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "invalid operation")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 40*time.Second)
		defer cancel()
		o, e := s.Store.Effect(ctx, r.ID, hash, t.Type, func() (json.RawMessage, int, error) { return Send(ctx, s.HTTP, t, r) })
		if errors.Is(e, ErrConflict) {
			return httpx.Fail(c, 409, "ERR_IDENTITY_CONFLICT", "identity contains another payload")
		}
		if errors.Is(e, ErrUnknown) {
			return httpx.Fail(c, 409, "ERR_OUTCOME_UNKNOWN", "inspect identity before retrying")
		}
		if e != nil {
			return httpx.Fail(c, 503, "ERR_STORE", "effect unavailable")
		}
		return httpx.OK(c, o)
	})
	g.Get("/inbound", func(c fiber.Ctx) error {
		var state string
		var due int64
		var attempts int
		var hash string
		if c.Query("id") == "" {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "identity required")
		}
		e := s.Store.DB.QueryRowContext(c.Context(), "SELECT state,due,attempts,digest FROM gorge_integration_inbox WHERE id=?", c.Query("id")).Scan(&state, &due, &attempts, &hash)
		if e != nil {
			return httpx.Fail(c, 404, "ERR_NOT_FOUND", "inbound unavailable")
		}
		return httpx.OK(c, map[string]any{"state": state, "due": due, "attempts": attempts, "digest": hash})
	})
	g.Post("/inbound", func(c fiber.Ctx) error {
		var in Intake
		if c.Bind().Body(&in) != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", "invalid intake")
		}
		if !slices.Contains(s.Config.Inbound, in.Provider) {
			return httpx.Fail(c, 422, "ERR_DISABLED", "provider not enabled")
		}
		m, e := Normalize(in)
		if e != nil {
			return httpx.Fail(c, 400, "ERR_BAD_REQUEST", e.Error())
		}
		id, e := s.Store.Accept(c.Context(), m)
		if errors.Is(e, ErrConflict) {
			return httpx.Fail(c, 409, "ERR_IDENTITY_CONFLICT", "inbound identity conflict")
		}
		if e != nil {
			return httpx.Fail(c, 503, "ERR_STORE", "inbound not persisted")
		}
		return httpx.OK(c, map[string]string{"id": id, "state": "accepted"})
	})
}
