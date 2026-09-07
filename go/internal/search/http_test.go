package search

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/engine"
)

const testToken = "test-token"

// newServerWith builds the routes the way cmd/gorge-search does, so the
// platform error handler and the health probes are in play.
//
// It takes a slice rather than a variadic list because "no backends at all" is
// a state under test here, and a variadic helper cannot tell it apart from
// "the caller wants the default".
func newServerWith(t *testing.T, defs []engine.BackendDef) *fiber.App {
	t.Helper()

	// Requests that fail on purpose log; keep the test output readable.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	se, err := NewEngine(defs)
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}

	srv := httpx.New(httpx.Config{Ready: se.Ready})
	RegisterRoutes(srv.App(), &Deps{Engine: se, Token: testToken})
	return srv.App()
}

// newTestServer is the common case: one healthy in-memory backend.
func newTestServer(t *testing.T) *fiber.App {
	t.Helper()
	return newServerWith(t, []engine.BackendDef{{Type: "test"}})
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

func request(t *testing.T, app *fiber.App, method, path, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", testToken)
	return do(t, app, req)
}

type testEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// envelope decodes the {data, error} response every /api/** endpoint answers
// with, and fails when the body is not one.
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

const validDoc = `{"phid":"PHID-TASK-1","type":"TASK","title":"Fix the parser",
"dateCreated":1700000000,"dateModified":1700000001,
"fields":[{"name":"body","corpus":"the parser drops trailing newlines"}],
"relationships":[{"name":"auth","relatedPHID":"PHID-USER-1","rtype":"auth"}]}`

// Phorge's PhabricatorGorgeFulltextStorageEngine calls these seven paths as
// written. Renaming one is not a compile error anywhere; it is a 404 the PHP
// side reports as a search failure.
func TestRoutePathsAreStable(t *testing.T) {
	app := newTestServer(t)

	routes := make(map[string]bool)
	for _, r := range app.GetRoutes(true) {
		routes[r.Method+" "+r.Path] = true
	}

	for _, want := range []string{
		"POST /api/search/index",
		"POST /api/search/query",
		"POST /api/search/init",
		"GET /api/search/exists",
		"GET /api/search/stats",
		"POST /api/search/sane",
		"GET /api/search/backends",
	} {
		if !routes[want] {
			t.Errorf("%s is no longer registered", want)
		}
	}
}

// The domain registers no probes of its own: httpx.New already did, and a
// second registration makes the router panic at startup. This asserts the
// platform's are the ones in play and that they stay outside the envelope.
func TestProbesComeFromThePlatform(t *testing.T) {
	app := newTestServer(t)

	for _, path := range []string{"/", "/healthz", "/readyz"} {
		resp, body := do(t, app, httptest.NewRequest(http.MethodGet, path, nil))

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, resp.StatusCode)
		}
		if strings.Contains(body, `"data"`) {
			t.Errorf("%s: probe payloads stay outside the envelope, got %s", path, body)
		}
	}
}

// /readyz is the whole reason this service has a Ready at all. Before the move
// into this repository it was a copy of /healthz, so a service with no backend
// answered healthy while failing every query.
func TestReadinessReportsAnUnusableConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		defs   []engine.BackendDef
		status int
	}{
		{"no backends", []engine.BackendDef{}, http.StatusServiceUnavailable},
		{"write only", []engine.BackendDef{{Type: "test", Roles: []string{"write"}}}, http.StatusServiceUnavailable},
		{"readable", []engine.BackendDef{{Type: "test"}}, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newServerWith(t, tc.defs)

			resp, body := do(t, app, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if resp.StatusCode != tc.status {
				t.Errorf("expected %d, got %d (body: %s)", tc.status, resp.StatusCode, body)
			}

			// Liveness stays 200 throughout: the process is serving, it just
			// has nothing to serve with. Conflating the two is what made the
			// broken state invisible.
			live, _ := do(t, app, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if live.StatusCode != http.StatusOK {
				t.Errorf("liveness must not depend on the backends, got %d", live.StatusCode)
			}
		})
	}
}

func TestIndexDocumentSucceeds(t *testing.T) {
	resp, body := request(t, newTestServer(t), http.MethodPost, "/api/search/index", validDoc)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	// The PHID is echoed so a caller batching documents can tell which one an
	// answer belongs to.
	if !strings.Contains(body, `"phid":"PHID-TASK-1"`) {
		t.Errorf("unexpected response: %s", body)
	}
	if env := envelope(t, body); env.Error != nil {
		t.Errorf("a success must carry no error: %+v", env.Error)
	}
}

// 400 rather than 502: a document this incomplete never reached a store, so
// nothing downstream judged anything.
func TestIndexDocumentRejectsIncompleteDocuments(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing phid", `{"type":"TASK","title":"x"}`},
		{"missing type", `{"phid":"PHID-TASK-1","title":"x"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := request(t, newTestServer(t), http.MethodPost, "/api/search/index", tc.body)
			assertErrorCode(t, resp, body, http.StatusBadRequest, httpx.CodeBadRequest)
		})
	}
}

func TestSearchRoundTrip(t *testing.T) {
	app := newTestServer(t)
	if resp, body := request(t, app, http.MethodPost, "/api/search/index", validDoc); resp.StatusCode != http.StatusOK {
		t.Fatalf("indexing failed: %s", body)
	}

	resp, body := request(t, app, http.MethodPost, "/api/search/query", `{"query":"parser"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var decoded struct {
		Data struct {
			PHIDs []string `json:"phids"`
			Count int      `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Data.Count != 1 || decoded.Data.PHIDs[0] != "PHID-TASK-1" {
		t.Errorf("unexpected results: %+v", decoded.Data)
	}
}

// Phorge's search UI opens on an unfiltered listing, which arrives here as a
// query with no text. Rejecting it would leave that page permanently broken.
func TestAnEmptyQueryIsNotAnError(t *testing.T) {
	resp, body := request(t, newTestServer(t), http.MethodPost, "/api/search/query", `{}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	// An empty result set is [] rather than null: the PHP side iterates the
	// value, and null would be a different shape to handle.
	if !strings.Contains(body, `"phids":[]`) {
		t.Errorf("expected an empty list, got %s", body)
	}
}

func TestIndexLifecycleEndpoints(t *testing.T) {
	app := newTestServer(t)

	resp, body := request(t, app, http.MethodGet, "/api/search/exists", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"exists":false`) {
		t.Errorf("a fresh index must not exist: %d %s", resp.StatusCode, body)
	}

	resp, body = request(t, app, http.MethodPost, "/api/search/init", `{"docTypes":["TASK","DREV"]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"initialized"`) {
		t.Fatalf("init failed: %d %s", resp.StatusCode, body)
	}

	_, body = request(t, app, http.MethodGet, "/api/search/exists", "")
	if !strings.Contains(body, `"exists":true`) {
		t.Errorf("after init the index must exist: %s", body)
	}

	_, body = request(t, app, http.MethodPost, "/api/search/sane", `{"docTypes":["TASK","DREV"]}`)
	if !strings.Contains(body, `"sane":true`) {
		t.Errorf("an index just built for these types must be sane: %s", body)
	}

	// A false is a normal answer, not an error: it is how a mapping change
	// announces that a reindex is due.
	resp, body = request(t, app, http.MethodPost, "/api/search/sane", `{"docTypes":["TASK","DREV","CMIT"]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"sane":false`) {
		t.Errorf("expected a plain false, got %d %s", resp.StatusCode, body)
	}
}

// /init and /sane both need the document type list, and leaving it out of
// /sane is the worse of the two: an empty type list builds an empty
// expectation, which any index at all satisfies. Answering "sane: true" to a
// question nobody asked properly is the silent success this domain has to
// avoid, so it is a 400 instead.
func TestDocTypesAreRequired(t *testing.T) {
	for _, path := range []string{"/api/search/init", "/api/search/sane"} {
		t.Run(path, func(t *testing.T) {
			resp, body := request(t, newTestServer(t), http.MethodPost, path, `{}`)
			assertErrorCode(t, resp, body, http.StatusBadRequest, httpx.CodeBadRequest)
			if !strings.Contains(body, "docTypes is required") {
				t.Errorf("the message should name the missing field: %s", body)
			}
		})
	}
}

func TestStatsAndBackends(t *testing.T) {
	app := newTestServer(t)

	resp, body := request(t, app, http.MethodGet, "/api/search/stats", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	// snake_case, unlike every other name on this service's wire, and kept
	// that way on purpose; see contracts.IndexStats.
	if !strings.Contains(body, `"storage_bytes"`) {
		t.Errorf("storage_bytes is part of the wire: %s", body)
	}

	resp, body = request(t, app, http.MethodGet, "/api/search/backends", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"type"`, `"index"`, `"roles"`} {
		if !strings.Contains(body, want) {
			t.Errorf("a backend entry must carry %s: %s", want, body)
		}
	}
}

// All five domain codes are 502, and all five have to be reachable — that is
// the whole reason the in-memory backend takes failure injection.
func TestEveryDomainErrorCodeIsReachable(t *testing.T) {
	cases := []struct {
		fail   string
		method string
		path   string
		body   string
		code   string
	}{
		{"index", http.MethodPost, "/api/search/index", validDoc, CodeIndexFailed},
		{"search", http.MethodPost, "/api/search/query", `{"query":"x"}`, CodeSearchFailed},
		{"init", http.MethodPost, "/api/search/init", `{"docTypes":["TASK"]}`, CodeInitFailed},
		{"exists", http.MethodGet, "/api/search/exists", "", CodeCheckFailed},
		{"sane", http.MethodPost, "/api/search/sane", `{"docTypes":["TASK"]}`, CodeCheckFailed},
		{"stats", http.MethodGet, "/api/search/stats", "", CodeStatsFailed},
	}

	for _, tc := range cases {
		t.Run(tc.code+" via "+tc.fail, func(t *testing.T) {
			app := newServerWith(t, []engine.BackendDef{{
				Type:    "test",
				Options: map[string]string{"fail": tc.fail},
			}})
			resp, body := request(t, app, tc.method, tc.path, tc.body)
			assertErrorCode(t, resp, body, http.StatusBadGateway, tc.code)
		})
	}
}

// A backend failure is never a 500. 500 in this domain means the service
// itself broke, and reporting a downstream outage as one sends whoever is
// debugging to the wrong logs.
func TestBackendFailuresAreNever500(t *testing.T) {
	app := newServerWith(t, []engine.BackendDef{{
		Type:    "test",
		Options: map[string]string{"fail": "index,search,init,exists,sane,stats"},
	}})

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/search/index", validDoc},
		{http.MethodPost, "/api/search/query", `{"query":"x"}`},
		{http.MethodPost, "/api/search/init", `{"docTypes":["TASK"]}`},
		{http.MethodGet, "/api/search/exists", ""},
		{http.MethodPost, "/api/search/sane", `{"docTypes":["TASK"]}`},
		{http.MethodGet, "/api/search/stats", ""},
	} {
		resp, _ := request(t, app, tc.method, tc.path, tc.body)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s %s: expected 502, got %d", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestMalformedBodyIsABadRequest(t *testing.T) {
	for _, path := range []string{"/api/search/index", "/api/search/query", "/api/search/init", "/api/search/sane"} {
		t.Run(path, func(t *testing.T) {
			resp, body := request(t, newTestServer(t), http.MethodPost, path, `{"phid":`)
			assertErrorCode(t, resp, body, http.StatusBadRequest, httpx.CodeBadRequest)
		})
	}
}

// A body over the transport limit surfaces as the platform's 413 and has to
// reach the platform error handler unchanged; catching it locally would report
// it as ERR_BAD_REQUEST and tell the caller to fix its JSON.
//
// fasthttp rejects an oversized body while reading the request, which app.Test
// surfaces as a Go error rather than the 413 a real connection receives, so
// this exercises the platform body limit over a real listener.
func TestOversizedBodyStaysTooLarge(t *testing.T) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	se, err := NewEngine([]engine.BackendDef{{Type: "test"}})
	if err != nil {
		t.Fatal(err)
	}

	srv := httpx.New(httpx.Config{ListenAddr: "127.0.0.1:0", BodyLimit: "64", Ready: se.Ready})
	RegisterRoutes(srv.App(), &Deps{Engine: se, Token: testToken})

	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	t.Cleanup(func() {
		_ = srv.App().ShutdownWithTimeout(2 * time.Second)
		<-done
	})

	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.ListenerAddr(); a != nil {
			addr = a.String()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("server never started listening")
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/search/index", strings.NewReader(validDoc))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected %d, got %d (body: %s)", http.StatusRequestEntityTooLarge, resp.StatusCode, string(raw))
	}
	body := string(raw)
	var decoded testEnvelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("response is not an envelope: %v (body: %s)", err, body)
	}
	if decoded.Error == nil {
		t.Fatalf("expected an error envelope, got: %s", body)
	}
	if decoded.Error.Code != httpx.CodeTooLarge {
		t.Errorf("expected %s, got %s", httpx.CodeTooLarge, decoded.Error.Code)
	}
	if len(decoded.Data) != 0 {
		t.Errorf("an error response must carry no data, got: %s", decoded.Data)
	}
}

func TestAuthentication(t *testing.T) {
	app := newTestServer(t)

	t.Run("no token", func(t *testing.T) {
		resp, body := do(t, app, httptest.NewRequest(http.MethodGet, "/api/search/backends", nil))
		assertErrorCode(t, resp, body, http.StatusUnauthorized, httpx.CodeUnauthorized)
	})

	t.Run("wrong token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/search/backends", nil)
		req.Header.Set("X-Service-Token", "nope")
		resp, body := do(t, app, req)
		assertErrorCode(t, resp, body, http.StatusUnauthorized, httpx.CodeUnauthorized)
	})

	t.Run("query parameter fallback", func(t *testing.T) {
		resp, body := do(t, app, httptest.NewRequest(http.MethodGet, "/api/search/backends?token="+testToken, nil))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", resp.StatusCode, body)
		}
	})

	// Authentication runs before the path is resolved, so an unauthenticated
	// request to a path that does not exist is a 401 rather than a 404. This
	// is not a bug: it keeps the route list from leaking to an unauthenticated
	// caller.
	t.Run("unknown path under the group", func(t *testing.T) {
		resp, body := do(t, app, httptest.NewRequest(http.MethodGet, "/api/search/nope", nil))
		assertErrorCode(t, resp, body, http.StatusUnauthorized, httpx.CodeUnauthorized)
	})
}

// An unbuildable backend fails startup rather than being skipped. A backend
// missing from the rotation is how an install ends up searching one index and
// writing to another without noticing.
func TestAnUnbuildableBackendFailsStartup(t *testing.T) {
	_, err := NewEngine([]engine.BackendDef{
		{Type: "test", Options: map[string]string{"fail": "nonsense"}},
	})
	if err == nil {
		t.Fatal("expected an error rather than a silently dropped backend")
	}
}
