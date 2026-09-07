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

// Token returns middleware that rejects requests not presenting the expected
// shared secret. An empty token disables the check entirely, which is what
// makes local development and the compose defaults work without ceremony.
func Token(expected string) fiber.Handler {
	return func(c fiber.Ctx) error {
		if expected == "" {
			return c.Next()
		}
		presented := c.Get(HeaderName)
		if presented == "" {
			presented = c.Query(QueryParamName)
		}
		if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
			return httpx.Fail(c, http.StatusUnauthorized,
				httpx.CodeUnauthorized, "missing or invalid service token")
		}
		return c.Next()
	}
}
