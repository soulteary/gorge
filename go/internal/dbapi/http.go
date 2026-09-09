package dbapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// readyTimeout bounds the readiness probe: it dials the configured masters
// with the short probe timeout, and a probe that hangs is reported unready by
// the orchestrator's own timeout well after this one has returned.
const readyTimeout = 5 * time.Second

// Deps is everything the db-api routes need. The four services share the same
// cluster and password, and each opens its own short-lived connection per
// request or probe (through ConnFactory) and closes it within that scope.
// Password is here so the read endpoints can probe with it.
type Deps struct {
	Cluster        *ClusterConfig
	Health         *HealthService
	Schema         *DiffService
	Setup          *SetupService
	Migration      *MigrationService
	Password       string
	Token          string
	TopologySource string
}

// TopologySourceFile marks a cluster read from a GORGE_DB_CONFIG_FILE
// local.json; TopologySourceSingleNode marks one built from the scalar
// GORGE_DB_MYSQL_* settings. They are the values the capability endpoint
// reports so a consumer can tell a real cluster report from a single node.
const (
	TopologySourceFile       = "file"
	TopologySourceSingleNode = "single-node"
)

// NewDeps wires the services over a cluster. It opens no connection: like the
// file-storage and taskqueue services, the pools are lazy so the process can
// start before MySQL is up. topologySource records how the cluster was
// described (see TopologySource*), which the capability endpoint reports.
func NewDeps(cluster *ClusterConfig, password, token, topologySource string) *Deps {
	return &Deps{
		Cluster:        cluster,
		Health:         NewHealthService(cluster),
		Schema:         NewDiffService(cluster, password),
		Setup:          NewSetupService(cluster, password),
		Migration:      NewMigrationService(cluster, password),
		Password:       password,
		Token:          token,
		TopologySource: topologySource,
	}
}

// Ready backs /readyz. It reports the service usable once at least one
// configured master answers a ping.
//
// It pings and no more. It must NOT check that `{namespace}_meta_data` or any
// Phorge table exists: those are created by `bin/storage upgrade` inside the
// Phorge container, which is ordered *after* this service in the stack, so a
// readiness probe that required them would deadlock first boot — the same trap
// file-storage documents. The DSN the probe dials therefore names no database,
// so a ping succeeds against a server whose Phorge databases do not exist yet.
func (d *Deps) Ready() error {
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	if d.Health.anyReachable(ctx, d.Password) {
		return nil
	}
	return errors.New("no configured database server is reachable")
}

// Close releases any long-lived connection pools. There are none: unlike
// file-storage, whose Router owns a pool for the life of the process, every
// db-api answer is a live query on a connection the owning service opens and
// closes within the request or probe scope (see the ConnFactory each service
// holds). The one long-lived pool this package used to carry lived on a
// master/replica Router that no read endpoint ever drove; that write-path
// abstraction now lives in internal/dbproxy (see docs/adr/0001-isolate-db-proxy.md),
// so there is nothing here to release. Close is kept so main.go's shutdown
// contract is stable and a future pool has an obvious home to be released from.
func (d *Deps) Close() error { return nil }

// RegisterRoutes mounts the db-api endpoints. The seven /api/db/* routes are
// named after the domain and must not change: PhabricatorGorgeDBClient calls
// them as written. They sit behind the shared service-token middleware; the
// health probes (/healthz, /readyz, /) are registered by httpx and stay
// unauthenticated so container probes reach them.
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/db")
	// db-api accepts the token only in the X-Service-Token header, never in a
	// `?token=` query string. Nothing links to /api/db/** from a browser, so
	// the URL fallback the shared middleware allows by default is pure risk
	// here — a token in a URL lands in access logs and history. See
	// auth.WithQueryToken.
	g.Use(auth.Token(deps.Token, auth.WithQueryToken(false)))

	g.Get("/servers", listServers(deps))
	g.Get("/servers/:ref/health", serverHealth(deps))
	g.Get("/schema-diff", schemaDiff(deps))
	g.Get("/schema-issues", schemaIssues(deps))
	g.Get("/setup-issues", setupIssues(deps))
	g.Get("/charset-info", charsetInfo(deps))
	g.Get("/migrations/status", migrationStatus(deps))
	g.Get("/meta", meta(deps))
}

// fail turns a domain error into the platform envelope. A DBError with a
// caller-actionable kind answers its own code and status with a generic
// message; anything else is handed back to the platform error handler as a 500
// ERR_INTERNAL. The detail never enters the body — a service-to-service caller
// that passed the token check must still not be handed the cluster's host or
// database names — so the specific error is left for the log via the returned
// error on the ERR_INTERNAL path.
func fail(c fiber.Ctx, err error) error {
	var dbErr *DBError
	if errors.As(err, &dbErr) {
		if code, status, ok := codeForKind(dbErr.Kind); ok {
			return httpx.Fail(c, status, code, genericMessage(dbErr.Kind))
		}
	}
	// Not caller-actionable: let httpx render a 500 with its generic message
	// and log the real error.
	return err
}

// genericMessage is the safe, code-appropriate message a domain failure
// answers with. It says what kind of thing went wrong without naming a host,
// database or query.
func genericMessage(kind errorKind) string {
	switch kind {
	case kindUnreachable:
		return "a configured database server is unreachable"
	case kindReadonly:
		return "the target is read-only"
	case kindAccessDenied:
		return "the database denied access for this operation"
	default:
		return http.StatusText(http.StatusInternalServerError)
	}
}

func listServers(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		refs, err := deps.Health.QueryAll(c.Context(), deps.Password)
		if err != nil {
			return fail(c, err)
		}
		return httpx.OK(c, refs)
	}
}

func serverHealth(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ref, err := deps.Health.QueryOne(c.Context(), c.Params("ref"), deps.Password)
		if err != nil {
			// A key that matches no configured node is the caller addressing a
			// server that does not exist: 404, not a database failure.
			return httpx.Fail(c, http.StatusNotFound, httpx.CodeNotFound, "no such database server")
		}
		return httpx.OK(c, ref)
	}
}

func schemaDiff(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx := c.Context()
		requested := parseRequestedDatabases(c.Query("databases"))
		nodes := make([]any, 0)
		for _, ref := range deps.Schema.CollectRefs() {
			tree, err := deps.Schema.LoadActualSchema(ctx, ref, requested...)
			if err != nil {
				return fail(c, err)
			}
			nodes = append(nodes, tree)
		}
		return httpx.OK(c, nodes)
	}
}

// parseRequestedDatabases turns the optional `databases` query parameter into a
// bounded, sanitized list of expected database names. It caps both the count
// and each name's length so a caller cannot turn the accessDenied probe into an
// unbounded fan-out of SHOW TABLES statements, and drops empty or over-long
// entries rather than erroring, since the parameter is an optional hint.
func parseRequestedDatabases(raw string) []string {
	if raw == "" {
		return nil
	}
	const maxNames = 64
	const maxNameLen = 128
	var out []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" || len(name) > maxNameLen {
			continue
		}
		out = append(out, name)
		if len(out) >= maxNames {
			break
		}
	}
	return out
}

func schemaIssues(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		issues, err := deps.Schema.CollectIssues(c.Context())
		if err != nil {
			return fail(c, err)
		}
		return httpx.OK(c, issues)
	}
}

func setupIssues(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		issues, err := deps.Setup.CollectIssues(c.Context())
		if err != nil {
			return fail(c, err)
		}
		return httpx.OK(c, issues)
	}
}

func charsetInfo(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		info, err := deps.Schema.GetCharsetInfo(c.Context())
		if err != nil {
			return fail(c, err)
		}
		return httpx.OK(c, info)
	}
}

func migrationStatus(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		statuses, err := deps.Migration.Status(c.Context())
		if err != nil {
			return fail(c, err)
		}
		return httpx.OK(c, statuses)
	}
}

// meta backs GET /api/db/meta. It answers the capability description the PHP
// consumer reads before it routes the database console through this service:
// the wire-contract version, the configured namespace, where the topology came
// from, and the capability endpoints this build serves. It runs no query and
// touches no host — every value is known at boot — so it is the one /api/db
// route that can not fail on the cluster.
func meta(deps *Deps) fiber.Handler {
	// Built once: the answer is fixed for the life of the process.
	caps := contracts.Capabilities{
		ContractVersion: contracts.ContractVersion,
		Namespace:       deps.Cluster.Namespace,
		TopologySource:  deps.TopologySource,
		Capabilities: []string{
			"servers",
			"server-health",
			"schema-diff",
			"schema-issues",
			"setup-issues",
			"charset-info",
			"migrations-status",
		},
	}
	return func(c fiber.Ctx) error {
		return httpx.OK(c, caps)
	}
}
