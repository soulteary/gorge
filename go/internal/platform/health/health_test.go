package health

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func serve(t *testing.T, ready ReadyFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	Register(e, ready, false)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestHealthPing(t *testing.T) {
	for _, path := range []string{"/", "/healthz"} {
		rec := serve(t, nil, path)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "ok") {
			t.Errorf("%s: expected ok in response", path)
		}
	}
}

func TestReadyz(t *testing.T) {
	rec := serve(t, nil, "/readyz")

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Error("expected ok in response")
	}
}

func TestReadyzUnavailable(t *testing.T) {
	rec := serve(t, func() error { return errors.New("upstream down") }, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upstream down") {
		t.Error("expected the failure reason in response")
	}
}

// TestProbesAreNotEnveloped guards the deliberate inconsistency documented on
// Register: probes stay flat because orchestrators are configured against that
// shape.
func TestProbesAreNotEnveloped(t *testing.T) {
	for _, path := range []string{"/", "/healthz", "/readyz"} {
		rec := serve(t, nil, path)

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if _, wrapped := body["data"]; wrapped {
			t.Errorf("%s: probe response must not be wrapped in the data envelope", path)
		}
		if body["status"] != "ok" {
			t.Errorf("%s: expected a bare status field, got %v", path, body)
		}
	}
}

// TestSkipRootLeavesRootToTheCaller guards the exemption Register documents:
// the notification client port answers GET / with 501 and Phorge reads that as
// the healthy answer, so a probe registered here would report the server broken.
func TestSkipRootLeavesRootToTheCaller(t *testing.T) {
	e := echo.New()
	Register(e, nil, true)

	const sentinel = "domain answered"
	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusNotImplemented, sentinel)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("expected the domain's 501, got %d", rec.Code)
	}
	if rec.Body.String() != sentinel {
		t.Errorf("expected %q, got %q", sentinel, rec.Body.String())
	}
}

// TestSkipRootKeepsContainerProbes pins the other half of the exemption: only
// GET / is given up, so Docker HEALTHCHECK and Kubernetes keep their endpoints.
func TestSkipRootKeepsContainerProbes(t *testing.T) {
	e := echo.New()
	Register(e, nil, true)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, rec.Code)
		}
	}
}
