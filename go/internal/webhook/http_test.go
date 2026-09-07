package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

const testToken = "test-token"

// newTestServer builds the routes the way cmd/gorge-webhook does, so the
// platform error handler and the health probes are in play.
func newTestServer(t *testing.T, store Store) *echo.Echo {
	t.Helper()
	quietLogs(t)

	srv := httpx.New(httpx.Config{Ready: ReadyProbe(store)})
	RegisterRoutes(srv.Echo(), &Deps{Store: store, Token: testToken})
	return srv.Echo()
}

// do issues an authenticated request.
func do(e *echo.Echo, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Service-Token", testToken)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

type testEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func envelope(t *testing.T, rec *httptest.ResponseRecorder) testEnvelope {
	t.Helper()

	var decoded testEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not an envelope: %v (body: %s)", err, rec.Body.String())
	}
	return decoded
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	if rec.Code != status {
		t.Errorf("expected %d, got %d (body: %s)", status, rec.Code, rec.Body.String())
	}
	env := envelope(t, rec)
	if env.Error == nil {
		t.Fatalf("expected an error envelope, got: %s", rec.Body.String())
	}
	if env.Error.Code != code {
		t.Errorf("expected %s, got %s", code, env.Error.Code)
	}
	if len(env.Data) != 0 {
		t.Errorf("an error response must carry no data, got: %s", env.Data)
	}
}

// seededStore is one hook, one disabled hook and a request in each state, so
// every count the endpoints report is a different number.
func seededStore(t *testing.T) *memStore {
	t.Helper()

	store := newMemStore(1_000_000).
		addHook(testHook()).
		addHook(&Hook{PHID: "PHID-HWBH-disabled0000000000", Status: HookStatusDisabled})

	store.addRequest(1, testHookPHID, RequestProperties{Retry: RetryForever})
	store.addRequest(2, testHookPHID, RequestProperties{Retry: RetryForever})

	sent := store.addRequest(3, testHookPHID, RequestProperties{})
	if err := store.UpdateResult(t.Context(), sent.ID,
		delivered(&RequestProperties{}, "200", 1_000_000)); err != nil {
		t.Fatal(err)
	}

	failed := store.addRequest(4, testHookPHID, RequestProperties{Retry: RetryNever})
	if err := store.UpdateResult(t.Context(), failed.ID,
		attemptFailed(&RequestProperties{Retry: RetryNever}, ErrorTypeHTTP, "500", 1_000_000)); err != nil {
		t.Fatal(err)
	}

	return store
}

func TestStats(t *testing.T) {
	e := newTestServer(t, seededStore(t))

	rec := do(e, http.MethodGet, "/api/webhook/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result struct {
		QueuedCount    int64 `json:"queuedCount"`
		SentCount      int64 `json:"sentCount"`
		FailedCount    int64 `json:"failedCount"`
		ActiveWebhooks int64 `json:"activeWebhooks"`
	}
	if err := json.Unmarshal(envelope(t, rec).Data, &result); err != nil {
		t.Fatal(err)
	}

	if result.QueuedCount != 2 || result.SentCount != 1 || result.FailedCount != 1 {
		t.Errorf("unexpected counts: %+v", result)
	}
	// Active excludes the disabled hook, which is the difference between this
	// field and what GET /api/webhook/hooks answers.
	if result.ActiveWebhooks != 1 {
		t.Errorf("activeWebhooks = %d, want the disabled hook excluded", result.ActiveWebhooks)
	}
}

func TestListHooks(t *testing.T) {
	e := newTestServer(t, seededStore(t))

	rec := do(e, http.MethodGet, "/api/webhook/hooks")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Every hook, disabled ones included: this is what tells "no hooks yet"
	// from "hooks that are all switched off".
	if !strings.Contains(rec.Body.String(), `"total":2`) {
		t.Errorf("expected both hooks to be counted, got %s", rec.Body.String())
	}
}

// TestAStoreFailureIsAnOpaque500 is why this domain defines no error code of
// its own. There is one failure mode — the database did not answer — and the
// platform already says that. What matters is what does *not* come back with
// it: the query, the host, the driver's message.
func TestAStoreFailureIsAnOpaque500(t *testing.T) {
	for _, tc := range []struct {
		path  string
		setup func(*memStore)
	}{
		{"/api/webhook/stats", func(m *memStore) { m.statsErr = errStoreDown }},
		{"/api/webhook/hooks", func(m *memStore) { m.countErr = errStoreDown }},
	} {
		t.Run(tc.path, func(t *testing.T) {
			store := newMemStore(1_000_000)
			tc.setup(store)
			e := newTestServer(t, store)

			rec := do(e, http.MethodGet, tc.path)
			assertErrorCode(t, rec, http.StatusInternalServerError, httpx.CodeInternal)

			if strings.Contains(rec.Body.String(), "3306") ||
				strings.Contains(rec.Body.String(), "herald") {
				t.Errorf("a 5xx body must not describe the service's internals: %s", rec.Body.String())
			}
		})
	}
}

func TestTokenAuth(t *testing.T) {
	e := newTestServer(t, seededStore(t))

	t.Run("rejected without a token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/webhook/stats", nil))
		assertErrorCode(t, rec, http.StatusUnauthorized, httpx.CodeUnauthorized)
	})

	t.Run("accepted in the header", func(t *testing.T) {
		if rec := do(e, http.MethodGet, "/api/webhook/stats"); rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("accepted in the query string", func(t *testing.T) {
		// The fallback for a caller that cannot set headers, which for a
		// read-only status endpoint is most of the ways a person looks at one.
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			"/api/webhook/stats?token="+testToken, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestHealthProbesReportTheDatabase: /healthz answers while the process is
// listening, /readyz answers whether it can read the queue. For a service
// whose work is a background loop that difference is the only thing an
// orchestrator can see — a delivery service with an unreachable database is
// listening and completely idle.
func TestHealthProbesReportTheDatabase(t *testing.T) {
	probe := func(e *echo.Echo, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	down := newMemStore(1_000_000)
	down.readyErr = errStoreDown
	e := newTestServer(t, down)

	if rec := probe(e, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("liveness must not depend on the database, got %d", rec.Code)
	}

	rec := probe(e, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	// Probe payloads stay outside the {data,error} envelope: orchestrators are
	// configured against the flat shape.
	if strings.Contains(rec.Body.String(), `"data"`) {
		t.Errorf("probe payload must not use the envelope: %s", rec.Body.String())
	}
	// Unlike a 5xx body, a probe's reason is for an operator and does carry
	// the cause.
	if !strings.Contains(rec.Body.String(), `"reason"`) {
		t.Errorf("readiness must say why it is unavailable: %s", rec.Body.String())
	}

	if rec := probe(newTestServer(t, newMemStore(1_000_000)), "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("expected 200 with a reachable database, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRoutePathsAreStable: PhabricatorGorgeWebhookClient calls these paths as
// written, so renaming any of them is a breaking change on the PHP side.
func TestRoutePathsAreStable(t *testing.T) {
	e := newTestServer(t, newMemStore(1_000_000))

	want := map[string]string{
		"GET /api/webhook/stats": "",
		"GET /api/webhook/hooks": "",
		"GET /healthz":           "",
		"GET /readyz":            "",
		"GET /":                  "",
	}
	for _, r := range e.Routes() {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("route %s is no longer registered", route)
	}
}

// TestTheEndpointsAreReadOnly: nothing here may queue, retry or cancel a
// delivery. Phorge owns the queue's contents — it writes the rows — and an
// endpoint that let a caller push one would be a second, unauthenticated way
// into a table this service is only supposed to drain.
func TestTheEndpointsAreReadOnly(t *testing.T) {
	e := newTestServer(t, seededStore(t))

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := do(e, method, "/api/webhook/stats")
		if rec.Code == http.StatusOK {
			t.Errorf("%s /api/webhook/stats answered 200; the API is read-only", method)
		}
	}
}

// TestUnknownPathKeepsTheEnvelope: everything the framework answers on its own
// is an envelope too, which is what the PHP client falls back to reading.
func TestUnknownPathKeepsTheEnvelope(t *testing.T) {
	e := newTestServer(t, newMemStore(1_000_000))

	assertErrorCode(t, do(e, http.MethodGet, "/api/webhook/nope"),
		http.StatusNotFound, httpx.CodeNotFound)
}
