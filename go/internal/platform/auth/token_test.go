package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// newGuarded builds a router with a single dummy route behind the middleware.
// The middleware is domain-agnostic, so the tests are too.
func newGuarded(token string) *echo.Echo {
	e := echo.New()
	g := e.Group("/guarded")
	g.Use(Token(token))
	g.GET("/resource", func(c echo.Context) error {
		return c.String(http.StatusOK, "reached")
	})
	return e
}

func TestRenderUnauthorized(t *testing.T) {
	e := newGuarded("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "ERR_UNAUTHORIZED") {
		t.Errorf("expected ERR_UNAUTHORIZED in body, got %s", body)
	}
}

func TestTokenViaQueryParam(t *testing.T) {
	e := newGuarded("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource?token=test-token", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestTokenViaHeader(t *testing.T) {
	e := newGuarded("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource", nil)
	req.Header.Set(HeaderName, "test-token")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	e := newGuarded("test-token")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource", nil)
	req.Header.Set(HeaderName, "not-the-token")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestNoTokenRequired(t *testing.T) {
	e := newGuarded("")

	req := httptest.NewRequest(http.MethodGet, "/guarded/resource", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}
