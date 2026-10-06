package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type ProjectionConfig struct {
	Deliveries      []ProjectionBackend `json:"deliveries,omitempty"`
	SourceOutboxDSN string              `json:"sourceOutboxDSN,omitempty"`
	ControlDSN      string              `json:"controlDSN"`
	Namespace       string              `json:"namespace"`
	Targets         []projection.Target `json:"targets"`
	invalid         bool
}
type projectionAcceptor interface {
	Accept(context.Context, *contracts.SearchProjection, []projection.Target) (*projection.Receipt, error)
}
type ProjectionIngress struct {
	SourceOutbox    *sql.DB
	Inspector       projectionInspector
	BackendDelivery bool
	Store           projectionAcceptor
	Namespace       string
	Targets         []projection.Target
}

// OpenProjection verifies the operator-applied schema without creating tables.
// The returned ingress only queues deliveries; it does not claim backend apply.
func OpenProjection(ctx context.Context, cfg *ProjectionConfig, token string) (*ProjectionIngress, *sql.DB, error) {
	if cfg == nil {
		return nil, nil, nil
	}
	if cfg.invalid || token == "" || cfg.ControlDSN == "" {
		return nil, nil, fmt.Errorf("projection requires valid configuration, control DSN and service token")
	}
	if err := projection.ValidateNamespace(cfg.Namespace); err != nil {
		return nil, nil, err
	}
	if err := projection.ValidateTargets(cfg.Targets); err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("mysql", cfg.ControlDSN)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid projection database configuration")
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxLifetime(5 * time.Minute)
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Probe every persisted column used by acceptance. Empty tables are valid.
	probes := []string{
		"SELECT namespace,eventID,envelopeHash,envelope,response FROM search_projection_inbox LIMIT 0",
		"SELECT namespace,phid,revision,contentHash FROM search_projection_revision LIMIT 0",
		"SELECT namespace,phid,revision,envelope FROM search_projection_head LIMIT 0",
		"SELECT namespace,backendID,generationID,phid,revision,eventID,status,createdEpoch,leaseOwner,leaseEpoch,leaseExpires,leaseRenewals,attempts,nextAttempt,lastError,appliedEpoch FROM search_projection_delivery LIMIT 0",
	}
	for _, query := range probes {
		rows, e := db.QueryContext(checkCtx, query)
		if e != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("projection control database or schema unavailable")
		}
		if err := rows.Close(); err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("projection control database or schema unavailable")
		}
	}
	return &ProjectionIngress{Inspector: &projection.MySQLStore{DB: db}, Store: &projection.MySQLStore{DB: db}, Namespace: cfg.Namespace, Targets: append([]projection.Target(nil), cfg.Targets...)}, db, nil
}

func registerProjectionRoutes(app fiber.Router, deps *Deps) {
	if deps.Projection == nil {
		return
	}
	g := app.Group("/api/search/projections", auth.Token(deps.Token, auth.WithQueryToken(false)))
	g.Get("/capabilities", func(c fiber.Ctx) error {
		return httpx.OK(c, fiber.Map{"projectionVersion": 1, "namespace": deps.Projection.Namespace, "maxDocumentBytes": projection.MaxDocumentBytes, "batch": false, "durableAcceptance": true, "backendDelivery": deps.Projection.BackendDelivery, "inspection": deps.Projection.Inspector != nil, "receiptStatus": []string{"accepted", "superseded"}, "targets": deps.Projection.Targets})
	})
	registerProjectionInspection(g, deps.Projection)
	g.Post("", func(c fiber.Ctx) error {
		event, err := projection.Decode(c.Body())
		if err != nil {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, err.Error())
		}
		if event.Namespace != deps.Projection.Namespace {
			return httpx.Fail(c, 400, httpx.CodeBadRequest, "projection namespace mismatch")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
		defer cancel()
		receipt, err := deps.Projection.Store.Accept(ctx, event, deps.Projection.Targets)
		if errors.Is(err, projection.ErrEventConflict) || errors.Is(err, projection.ErrRevisionConflict) {
			return httpx.Fail(c, http.StatusConflict, "ERR_PROJECTION_CONFLICT", err.Error())
		}
		if err != nil {
			return httpx.Fail(c, http.StatusServiceUnavailable, "ERR_PROJECTION_UNAVAILABLE", "projection acceptance unavailable")
		}
		// A successful receipt is returned only after the acceptance transaction commits.
		return httpx.OK(c, receipt)
	})
}
