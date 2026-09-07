package health

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

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

func serve(t *testing.T, ready ReadyFunc, path string) (*http.Response, string) {
	t.Helper()
	app := fiber.New()
	Register(app, ready, false)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	return do(t, app, req)
}

func TestHealthPing(t *testing.T) {
	for _, path := range []string{"/", "/healthz"} {
		resp, body := serve(t, nil, path)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, resp.StatusCode)
		}
		if !strings.Contains(body, "ok") {
			t.Errorf("%s: expected ok in response", path)
		}
	}
}

func TestReadyz(t *testing.T) {
	resp, body := serve(t, nil, "/readyz")

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "ok") {
		t.Error("expected ok in response")
	}
}

func TestReadyzUnavailable(t *testing.T) {
	resp, body := serve(t, func() error { return errors.New("upstream down") }, "/readyz")

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "upstream down") {
		t.Error("expected the failure reason in response")
	}
}

// TestProbesAreNotEnveloped guards the deliberate inconsistency documented on
// Register: probes stay flat because orchestrators are configured against that
// shape.
func TestProbesAreNotEnveloped(t *testing.T) {
	for _, path := range []string{"/", "/healthz", "/readyz"} {
		_, respBody := serve(t, nil, path)

		var body map[string]any
		if err := json.Unmarshal([]byte(respBody), &body); err != nil {
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
	app := fiber.New()
	Register(app, nil, true)

	const sentinel = "domain answered"
	app.Get("/", func(c fiber.Ctx) error {
		return c.Status(http.StatusNotImplemented).SendString(sentinel)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp, body := do(t, app, req)

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("expected the domain's 501, got %d", resp.StatusCode)
	}
	if body != sentinel {
		t.Errorf("expected %q, got %q", sentinel, body)
	}
}

// TestSkipRootKeepsContainerProbes pins the other half of the exemption: only
// GET / is given up, so Docker HEALTHCHECK and Kubernetes keep their endpoints.
func TestSkipRootKeepsContainerProbes(t *testing.T) {
	app := fiber.New()
	Register(app, nil, true)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		resp, _ := do(t, app, req)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, resp.StatusCode)
		}
	}
}
