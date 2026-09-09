// Package auth provides the shared-secret middleware guarding service-to-service
// calls. It knows nothing about the routes it protects.
package auth

import (
	"crypto/subtle"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// HeaderName is the request header carrying the shared service token.
const HeaderName = "X-Service-Token"

// QueryParamName is the fallback query parameter, for callers that cannot set
// headers (browser-issued links, curl one-liners in runbooks).
const QueryParamName = "token"

// options holds the tunable behaviour of the middleware. The zero value is the
// historical default: the query-string fallback is accepted, because that is
// how the middleware behaved when every service shared it and runbooks relied
// on the `?token=` form.
type options struct {
	allowQueryToken bool
}

// Option tunes the middleware. It exists so a service can tighten the default
// without breaking the services that depend on it.
type Option func(*options)

// WithQueryToken sets whether the `?token=` query fallback is accepted.
//
// It is opt-in-shaped on purpose. A token in the URL is a token in access
// logs, in the browser history and in the Referer header of anything the page
// links to, so a service that has no runbook reason to accept it should not.
// The shared default keeps accepting it so the services that were written
// against that behaviour keep working; a service that wants only the header
// passes WithQueryToken(false), which is what gorge-db-api does — nothing
// links to `/api/db/**` from a browser, so the URL fallback is pure risk
// there.
func WithQueryToken(allow bool) Option {
	return func(o *options) { o.allowQueryToken = allow }
}

// Token returns middleware that rejects requests not presenting the expected
// shared secret. An empty token disables the check entirely, which is what
// makes local development and the compose defaults work without ceremony.
//
// By default the token may be presented in the X-Service-Token header or the
// `token` query parameter; pass WithQueryToken(false) to accept only the
// header. See WithQueryToken for why a service might want to.
func Token(expected string, opts ...Option) fiber.Handler {
	cfg := options{allowQueryToken: true}
	for _, opt := range opts {
		opt(&cfg)
	}
	return func(c fiber.Ctx) error {
		if expected == "" {
			return c.Next()
		}
		presented := c.Get(HeaderName)
		if presented == "" && cfg.allowQueryToken {
			presented = c.Query(QueryParamName)
		}
		if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
			return httpx.Fail(c, http.StatusUnauthorized,
				httpx.CodeUnauthorized, "missing or invalid service token")
		}
		return c.Next()
	}
}
