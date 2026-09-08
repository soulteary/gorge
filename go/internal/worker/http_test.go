package worker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

const testToken = "test-token"

func newStatsServer(t *testing.T) *fiber.App {
	t.Helper()
	registry := NewRegistry()
	registry.Register("A", NewNoop())
	registry.SetFallback(NewNoop())

	// A consumer with no client is fine for the stats endpoint: it reads only
	// the in-process counters and the registry, never leasing.
	consumer := NewConsumer(nil, registry, testConfig())

	srv := httpx.New(httpx.Config{Ready: nil})
	RegisterRoutes(srv.App(), &Deps{Consumer: consumer, Token: testToken})
	return srv.App()
}

func TestStatsReportsCountersAndSupported(t *testing.T) {
	app := newStatsServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/worker/stats", nil)
	req.Header.Set("X-Service-Token", testToken)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var env struct {
		Data contracts.ConsumerStats `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Supported == nil {
		t.Error("stats must report the supported classes")
	}
}

func TestStatsRequiresToken(t *testing.T) {
	app := newStatsServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/worker/stats", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without a token, got %d", resp.StatusCode)
	}
}

// TestReadinessIsLiveness: the worker holds no database and reaches the queue
// over HTTP, so /readyz answers 200 as long as the process is up — there is no
// external dependency whose absence should take it out of rotation.
func TestReadinessIsLiveness(t *testing.T) {
	app := newStatsServer(t)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("readiness must equal liveness for the worker, got %d", resp.StatusCode)
	}
}
