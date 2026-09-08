package taskqueue

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

const testToken = "test-token"

func newTestServer(t *testing.T, store Store) *fiber.App {
	t.Helper()
	srv := httpx.New(httpx.Config{Ready: ReadyProbe(store)})
	RegisterRoutes(srv.App(), &Deps{Store: store, Token: testToken})
	return srv.App()
}

func dispatch(t *testing.T, app *fiber.App, req *http.Request) (*http.Response, string) {
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

func post(t *testing.T, app *fiber.App, path, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", testToken)
	return dispatch(t, app, req)
}

func get(t *testing.T, app *fiber.App, path string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Service-Token", testToken)
	return dispatch(t, app, req)
}

type testEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func envelope(t *testing.T, body string) testEnvelope {
	t.Helper()
	var decoded testEnvelope
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("response is not an envelope: %v (body: %s)", err, body)
	}
	return decoded
}

func assertErrorCode(t *testing.T, resp *http.Response, body string, status int, code string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Errorf("expected %d, got %d (body: %s)", status, resp.StatusCode, body)
	}
	env := envelope(t, body)
	if env.Error == nil {
		t.Fatalf("expected an error envelope, got: %s", body)
	}
	if env.Error.Code != code {
		t.Errorf("expected %s, got %s", code, env.Error.Code)
	}
	if len(env.Data) != 0 {
		t.Errorf("an error response must carry no data, got: %s", env.Data)
	}
}

func TestEnqueueReturnsTheTask(t *testing.T) {
	app := newTestServer(t, newMemStore(1_000_000))

	resp, body := post(t, app, "/api/queue/enqueue",
		`{"taskClass":"TestWorker","data":"{}","priority":2000}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var task contracts.Task
	if err := json.Unmarshal(envelope(t, body).Data, &task); err != nil {
		t.Fatal(err)
	}
	if task.ID == 0 || task.TaskClass != "TestWorker" {
		t.Errorf("unexpected task: %+v", task)
	}
}

func TestEnqueueRequiresTaskClass(t *testing.T) {
	app := newTestServer(t, newMemStore(1_000_000))
	resp, body := post(t, app, "/api/queue/enqueue", `{"data":"{}"}`)
	assertErrorCode(t, resp, body, http.StatusBadRequest, httpx.CodeBadRequest)
}

func TestLeaseUsesTheHeaderOwner(t *testing.T) {
	store := newMemStore(1_000_000)
	if _, err := store.Enqueue(t.Context(), &contracts.EnqueueRequest{TaskClass: "A", Data: "{}"}); err != nil {
		t.Fatal(err)
	}
	app := newTestServer(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/queue/lease", strings.NewReader(`{"limit":5}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", testToken)
	req.Header.Set(LeaseOwnerHeader, "worker-7")
	resp, body := dispatch(t, app, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var tasks []*contracts.Task
	if err := json.Unmarshal(envelope(t, body).Data, &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].LeaseOwner != "worker-7" {
		t.Errorf("lease must record the header owner, got %+v", tasks)
	}
}

func TestLeaseWithNoTasksReturnsEmptyArray(t *testing.T) {
	app := newTestServer(t, newMemStore(1_000_000))
	resp, body := post(t, app, "/api/queue/lease", `{"limit":5}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	// An empty lease is [] not null, so a JSON client can iterate it without a
	// nil check.
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("empty lease must be an empty array, got %s", body)
	}
}

func TestGetTaskNotFoundIs404(t *testing.T) {
	app := newTestServer(t, newMemStore(1_000_000))
	resp, body := get(t, app, "/api/queue/tasks/999")
	assertErrorCode(t, resp, body, http.StatusNotFound, httpx.CodeNotFound)
}

func TestGetTaskInvalidIDIs400(t *testing.T) {
	app := newTestServer(t, newMemStore(1_000_000))
	resp, body := get(t, app, "/api/queue/tasks/not-a-number")
	assertErrorCode(t, resp, body, http.StatusBadRequest, httpx.CodeBadRequest)
}

func TestCompleteRequiresTaskID(t *testing.T) {
	app := newTestServer(t, newMemStore(1_000_000))
	resp, body := post(t, app, "/api/queue/complete", `{"duration":1}`)
	assertErrorCode(t, resp, body, http.StatusBadRequest, httpx.CodeBadRequest)
}

func TestAwakenRequiresIDs(t *testing.T) {
	app := newTestServer(t, newMemStore(1_000_000))
	resp, body := post(t, app, "/api/queue/awaken", `{"taskIDs":[]}`)
	assertErrorCode(t, resp, body, http.StatusBadRequest, httpx.CodeBadRequest)
}

// TestAStoreFailureIsAnOpaque500 mirrors the webhook domain's: a backend
// failure is a generic 500 that never leaks the DSN or driver message.
func TestAStoreFailureIsAnOpaque500(t *testing.T) {
	store := newMemStore(1_000_000)
	store.statsErr = errStoreDown
	app := newTestServer(t, store)

	resp, body := get(t, app, "/api/queue/stats")
	assertErrorCode(t, resp, body, http.StatusInternalServerError, httpx.CodeInternal)
	if strings.Contains(body, "3306") || strings.Contains(body, "10.0.0.9") {
		t.Errorf("a 5xx body must not describe the service's internals: %s", body)
	}
}

func TestTokenAuthRejectsMissingToken(t *testing.T) {
	app := newTestServer(t, newMemStore(1_000_000))
	req := httptest.NewRequest(http.MethodGet, "/api/queue/stats", nil)
	resp, body := dispatch(t, app, req)
	assertErrorCode(t, resp, body, http.StatusUnauthorized, httpx.CodeUnauthorized)
}

// TestHealthProbesReportTheBackend: /healthz is liveness, /readyz reports
// whether the backend answers.
func TestHealthProbesReportTheBackend(t *testing.T) {
	down := newMemStore(1_000_000)
	down.readyErr = errStoreDown
	app := newTestServer(t, down)

	if resp, _ := dispatch(t, app, httptest.NewRequest(http.MethodGet, "/healthz", nil)); resp.StatusCode != http.StatusOK {
		t.Errorf("liveness must not depend on the backend, got %d", resp.StatusCode)
	}
	resp, body := dispatch(t, app, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with an unreachable backend, got %d: %s", resp.StatusCode, body)
	}

	up := newTestServer(t, newMemStore(1_000_000))
	if resp, body := dispatch(t, up, httptest.NewRequest(http.MethodGet, "/readyz", nil)); resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 with a reachable backend, got %d: %s", resp.StatusCode, body)
	}
}

// TestRoutePathsAreStable: Phorge's client and the worker consumer call these
// paths as written, so renaming any is a breaking change.
func TestRoutePathsAreStable(t *testing.T) {
	app := newTestServer(t, newMemStore(1_000_000))

	want := map[string]string{
		"POST /api/queue/enqueue":  "",
		"POST /api/queue/lease":    "",
		"POST /api/queue/complete": "",
		"POST /api/queue/fail":     "",
		"POST /api/queue/yield":    "",
		"POST /api/queue/cancel":   "",
		"POST /api/queue/awaken":   "",
		"GET /api/queue/stats":     "",
		"GET /api/queue/tasks":     "",
		"GET /api/queue/tasks/:id": "",
		"GET /healthz":             "",
		"GET /readyz":              "",
		"GET /":                    "",
	}
	for _, r := range app.GetRoutes(true) {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("route %s is no longer registered", route)
	}
}
