package conduit

import (
	"crypto/subtle"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
)

// Deps is everything the gateway routes need to serve a request. RateLimiter
// may be nil, which leaves the limiter unmounted (the RPS<=0 case).
type Deps struct {
	Proxy       *Proxy
	RateLimiter *RateLimiter
	Token       string
}

// RegisterRoutes mounts the gateway endpoint.
//
// The single route `ANY /api/:method` is the contract: it mirrors Phorge's own
// `/api/{method}` surface so a Conduit client can point at the gateway without
// rewriting call sites. It must not change.
//
// The middleware order still matters: authenticate first (group-level) so an
// unauthenticated flood is rejected before it can consume a rate-limit bucket,
// then rate-limit, then relay. The limiter is attached only when one was built
// (RPS>0), so a gateway with limiting disabled carries no per-request map
// lookup, and it is a route-level handler rather than group middleware because
// it needs the :method parameter, which a group-level Use does not yet see.
//
// The method-less paths /api and /api/ are registered explicitly so a request
// with no Conduit method answers ERR-CONDUIT-CORE (400) in the Conduit shape,
// rather than the platform's {data, error} 404 that an unmatched route would
// otherwise produce. Proxy.Handle sees an empty :method there and emits that
// code. They stay behind the same auth so a caller learns "no method" only
// after it has proven it may call the gateway at all.
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api")
	g.Use(tokenAuth(deps.Token))

	// The rate limiter is attached to the route rather than the group because
	// it keys on the :method path parameter, and a group-level Use runs before
	// the child route binds its parameters — c.Params("method") is empty there.
	// As a route handler it sees the bound value and can honour the exempt list.
	handlers := make([]any, 0, 2)
	if deps.RateLimiter != nil {
		handlers = append(handlers, deps.RateLimiter.Middleware())
	}
	proxy := func(c fiber.Ctx) error {
		return deps.Proxy.Handle(c)
	}
	handlers = append(handlers, proxy)

	g.All("/:method", handlers[0], handlers[1:]...)
	// The method-less paths carry no :method to rate-limit on, so they go
	// straight to the proxy handler, which answers ERR-CONDUIT-CORE.
	g.All("", proxy)
	g.All("/", proxy)
}

// tokenAuth is a thin rewrite of platform/auth.Token that answers a
// Conduit-protocol error envelope instead of httpx's {data, error}. It cannot
// reuse auth.Token directly for that reason alone: the comparison discipline is
// identical (constant-time, header-then-query, empty token disables the check),
// only the failure shape differs, because this endpoint speaks Conduit on both
// outcomes. See internal/contracts/conduit.go.
func tokenAuth(expected string) fiber.Handler {
	return func(c fiber.Ctx) error {
		if expected == "" {
			return c.Next()
		}
		presented := c.Get(auth.HeaderName)
		if presented == "" {
			presented = c.Query(auth.QueryParamName)
		}
		if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
			return conduitError(c, http.StatusUnauthorized, contracts.CodeConduitAuth,
				"Missing or invalid service token.")
		}
		return c.Next()
	}
}
