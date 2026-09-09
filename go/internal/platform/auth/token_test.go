package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// newGuarded builds a router with a single dummy route behind the middleware.
// The middleware is domain-agnostic, so the tests are too.
func newGuarded(token string) *fiber.App {
	app := fiber.New()
	g := app.Group("/guarded")
	g.Use(Token(token))
	g.Get("/resource", func(c fiber.Ctx) error {
		return c.Status(http.StatusOK).SendString("reached")
	})
	return app
}

// do runs one request against app and returns the response and its body. It is
// the app.Test stand-in for the ServeHTTP+ResponseRecorder pattern.
func do(t *testing.T, app *fiber.App, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestRenderUnauthorized(t *testing.T) {
	app := newGuarded("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource", nil)
	resp, body := do(t, app, req)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "ERR_UNAUTHORIZED") {
		t.Errorf("expected ERR_UNAUTHORIZED in body, got %s", body)
	}
}

func TestTokenViaQueryParam(t *testing.T) {
	app := newGuarded("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource?token=test-token", nil)
	resp, _ := do(t, app, req)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestTokenViaHeader(t *testing.T) {
	app := newGuarded("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource", nil)
	req.Header.Set(HeaderName, "test-token")
	resp, _ := do(t, app, req)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	app := newGuarded("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource", nil)
	req.Header.Set(HeaderName, "not-the-token")
	resp, _ := do(t, app, req)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestNoTokenRequired(t *testing.T) {
	app := newGuarded("")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource", nil)
	resp, _ := do(t, app, req)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// newGuardedHeaderOnly builds a router whose middleware refuses the query
// fallback, mirroring how gorge-db-api mounts it.
func newGuardedHeaderOnly(token string) *fiber.App {
	app := fiber.New()
	g := app.Group("/guarded")
	g.Use(Token(token, WithQueryToken(false)))
	g.Get("/resource", func(c fiber.Ctx) error {
		return c.Status(http.StatusOK).SendString("reached")
	})
	return app
}

// TestQueryTokenRejectedWhenDisabled pins the db-api tightening: with the query
// fallback disabled, a correct token in the URL must NOT authenticate the
// request, so a token that leaked into an access log or browser history can
// not be replayed as a URL.
func TestQueryTokenRejectedWhenDisabled(t *testing.T) {
	app := newGuardedHeaderOnly("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource?token=test-token", nil)
	resp, body := do(t, app, req)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("URL token must be rejected when the query fallback is off, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "ERR_UNAUTHORIZED") {
		t.Errorf("expected ERR_UNAUTHORIZED in body, got %s", body)
	}
}

// TestHeaderTokenAcceptedWhenQueryDisabled: disabling the query fallback must
// not disturb the header path.
func TestHeaderTokenAcceptedWhenQueryDisabled(t *testing.T) {
	app := newGuardedHeaderOnly("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource", nil)
	req.Header.Set(HeaderName, "test-token")
	resp, _ := do(t, app, req)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for a valid header token, got %d", resp.StatusCode)
	}
}

// TestQueryTokenAcceptedByDefault pins the backward-compatible default the
// other services rely on: without the option, the query fallback still works.
func TestQueryTokenAcceptedByDefault(t *testing.T) {
	app := newGuarded("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource?token=test-token", nil)
	resp, _ := do(t, app, req)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("default middleware should still accept the query token, got %d", resp.StatusCode)
	}
}
