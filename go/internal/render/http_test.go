package render

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
	"github.com/soulteary/gorge/go/internal/render/highlight"
)

func newTestDeps() *Deps {
	return &Deps{
		Highlighter: highlight.New(),
		Token:       "test-token",
		MaxBytes:    DefaultMaxBytes,
	}
}

func newTestServer(deps *Deps) *fiber.App {
	app := fiber.New()
	RegisterRoutes(app, deps)
	return app
}

// newPlatformServer builds the routes the way cmd/gorge-render does, so the
// body-limit and the platform error handler are in play. The bare
// newTestServer above stays for tests about the handlers themselves.
func newPlatformServer(t *testing.T, deps *Deps) *fiber.App {
	t.Helper()

	// Requests that fail on purpose log a 5xx; keep the test output readable.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	app := httpx.New(httpx.Config{}).App()
	RegisterRoutes(app, deps)
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

func postRender(t *testing.T, app *fiber.App, body string) (*http.Response, string) {
	req := httptest.NewRequest(http.MethodPost, "/api/highlight/render", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", "test-token")
	return do(t, app, req)
}

// postRenderLive serves the request over a real listener, needed for bodies
// over the platform limit: fasthttp rejects those while reading the request,
// which app.Test surfaces as a Go error rather than the 413 a real connection
// receives.
func postRenderLive(t *testing.T, deps *Deps, bodyLimit, body string) (int, string) {
	t.Helper()

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	srv := httpx.New(httpx.Config{ListenAddr: "127.0.0.1:0", BodyLimit: bodyLimit})
	RegisterRoutes(srv.App(), deps)

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

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/highlight/render", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// fasthttp can reject an oversized body mid-stream and close the
		// connection before the client finishes writing, which surfaces as a
		// broken pipe / reset here. That is the transport refusing the body,
		// never a bad request: report it as such with an empty body.
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

func TestRenderSuccess(t *testing.T) {
	resp, body := postRender(t, newTestServer(newTestDeps()), `{"source":"x = 1","language":"python"}`)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "html") {
		t.Error("expected html in response")
	}
}

func TestRenderEmptySource(t *testing.T) {
	resp, _ := postRender(t, newTestServer(newTestDeps()), `{"source":"","language":"python"}`)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestRenderTooLarge(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 10

	resp, body := postRender(t, newTestServer(deps),
		`{"source":"this is a very long source string","language":"python"}`)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "ERR_TOO_LARGE") {
		t.Error("expected ERR_TOO_LARGE in response")
	}
}

func TestRenderBadRequest(t *testing.T) {
	resp, body := postRender(t, newTestServer(newTestDeps()), `{"source":`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "ERR_BAD_REQUEST") {
		t.Error("expected ERR_BAD_REQUEST in response")
	}
}

func TestListLanguages(t *testing.T) {
	app := newTestServer(newTestDeps())

	req := httptest.NewRequest(http.MethodGet, "/api/highlight/languages", nil)
	req.Header.Set("X-Service-Token", "test-token")
	resp, body := do(t, app, req)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "python") {
		t.Error("expected python in languages list")
	}
}

// TestOversizedBodyReportsTooLargeFromEitherLimit covers the overlap between the
// two size limits guarding this endpoint.
//
// GORGE_RENDER_MAX_BYTES defaults to 1MiB, below the platform body limit, so a
// request normally trips the domain check inside renderHighlight —
// TestRenderTooLarge above covers that side. Raising it past the body limit,
// which deployments handling large files do, moves the rejection into fasthttp's
// body-limit, before any handler runs. Both must report ERR_TOO_LARGE, because
// Phorge's client branches on the code.
func TestOversizedBodyReportsTooLargeFromEitherLimit(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 8 << 20 // above the platform body limit

	// Larger than a small platform body limit, smaller than deps.MaxBytes: the
	// domain check would accept this body if it ever saw it. A small limit and
	// a small overshoot keep the client's single write ahead of the server's
	// rejection, so the 413 comes back cleanly rather than as a reset.
	body := `{"source":"` + strings.Repeat("x", 4096) + `","language":"python"}`
	status, respBody := postRenderLive(t, deps, "1K", body)

	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (body %.200q)", status, respBody)
	}

	var envelope struct {
		Error   *httpx.Error `json:"error"`
		Message *string      `json:"message"`
	}
	if err := json.Unmarshal([]byte(respBody), &envelope); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, respBody)
	}
	if envelope.Message != nil {
		t.Errorf("framework rejection kept a default message shape: %q", respBody)
	}
	if envelope.Error == nil || envelope.Error.Code != httpx.CodeTooLarge {
		t.Errorf("expected %s, got %q", httpx.CodeTooLarge, respBody)
	}
}

// TestOversizedChunkedBodyIsNeverABadRequest covers a client sending a body
// past the platform limit. Under fasthttp the oversized body is rejected as
// fiber.ErrRequestEntityTooLarge before the handler runs, and renderHighlight
// must hand any such non-400 error back to the platform rather than call it a
// bad request.
//
// The part that is a contract is that the code is never ERR_BAD_REQUEST; this
// asserts that rather than a specific status, since how far a decoder gets
// before the limit fires can vary.
func TestOversizedChunkedBodyIsNeverABadRequest(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 8 << 20

	body := `{"source":"` + strings.Repeat("x", 3<<20) + `","language":"python"}`
	status, respBody := postRenderLive(t, deps, "2M", body)

	switch status {
	case 0:
		// The transport rejected the oversized body mid-stream and reset the
		// connection. Definitively not a bad request, which is all this pins.
	case http.StatusOK:
		// The whole body was consumed before the limit applied, so the domain
		// limit of 8MiB is what governed. Fine.
	case http.StatusRequestEntityTooLarge:
		if !strings.Contains(respBody, httpx.CodeTooLarge) {
			t.Errorf("expected %s, got %q", httpx.CodeTooLarge, respBody)
		}
	default:
		t.Errorf("expected 200 or 413, got %d (%.200q)", status, respBody)
	}
	if strings.Contains(respBody, httpx.CodeBadRequest) {
		t.Errorf("a body over the transport limit is not a bad request: %.200q", respBody)
	}
}

// TestHighlightFailedSurvivesTheErrorHandler guards the one domain error code
// this package owns. Phorge's client branches on ERR_HIGHLIGHT_FAILED, so the
// platform error handler must never rewrite it into ERR_INTERNAL, even when
// something raises an error after the handler has answered. Fiber has no
// Committed flag; httpx.Fail records the answer in Locals and the error handler
// reads it.
func TestHighlightFailedSurvivesTheErrorHandler(t *testing.T) {
	app := newPlatformServer(t, newTestDeps())

	// The shape of renderHighlight's failure path, with a late error added: a
	// real Highlight() failure needs a broken Chroma lexer to reproduce.
	app.Get("/test/highlight-failed", func(c fiber.Ctx) error {
		if err := httpx.Fail(c, http.StatusInternalServerError,
			CodeHighlightFailed, "tokenisation failed"); err != nil {
			return err
		}
		return fiber.NewError(http.StatusInternalServerError, "raised after answering")
	})

	req := httptest.NewRequest(http.MethodGet, "/test/highlight-failed", nil)
	resp, respBody := do(t, app, req)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	var envelope struct {
		Error *httpx.Error `json:"error"`
	}
	decoder := json.NewDecoder(strings.NewReader(respBody))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, respBody)
	}
	if decoder.More() {
		t.Fatalf("the error handler appended a second document: %q", respBody)
	}
	if envelope.Error == nil || envelope.Error.Code != CodeHighlightFailed {
		t.Fatalf("expected %s, got %q", CodeHighlightFailed, respBody)
	}
	if envelope.Error.Message != "tokenisation failed" {
		t.Errorf("expected the domain message, got %q", envelope.Error.Message)
	}
}

// TestUnknownAPIPathIsEnveloped pins the envelope on a path Phorge could reach
// through a typo or version skew: a 404 raised by the router, not by a handler.
//
// The token is required even though the path does not exist: the group's auth
// middleware covers everything under /api/highlight, so an unauthenticated
// caller gets 401 rather than a map of which paths exist.
func TestUnknownAPIPathIsEnveloped(t *testing.T) {
	app := newPlatformServer(t, newTestDeps())

	req := httptest.NewRequest(http.MethodPost, "/api/highlight/rende", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", "test-token")
	resp, respBody := do(t, app, req)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body %q)", resp.StatusCode, respBody)
	}
	if !strings.Contains(respBody, httpx.CodeNotFound) {
		t.Errorf("expected %s, got %q", httpx.CodeNotFound, respBody)
	}
}

// TestRoutePathsAreStable pins the two paths Phorge's PhabricatorGoHighlightClient
// already calls. Renaming either is a breaking change on the PHP side.
func TestRoutePathsAreStable(t *testing.T) {
	app := newTestServer(newTestDeps())

	want := map[string]string{
		http.MethodPost + " /api/highlight/render":   "",
		http.MethodGet + " /api/highlight/languages": "",
	}
	for _, r := range app.GetRoutes(true) {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("route %s is no longer registered", route)
	}
}
