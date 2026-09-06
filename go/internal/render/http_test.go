package render

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

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

func newTestServer(deps *Deps) *echo.Echo {
	e := echo.New()
	RegisterRoutes(e, deps)
	return e
}

// newPlatformServer builds the routes the way cmd/gorge-render does, so the
// body-limit middleware and the platform error handler are in play. The bare
// newTestServer above stays for tests about the handlers themselves.
func newPlatformServer(t *testing.T, deps *Deps) *echo.Echo {
	t.Helper()

	// Requests that fail on purpose log a 5xx; keep the test output readable.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	e := httpx.New(httpx.Config{}).Echo()
	RegisterRoutes(e, deps)
	return e
}

func postRender(e *echo.Echo, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/highlight/render", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", "test-token")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestRenderSuccess(t *testing.T) {
	rec := postRender(newTestServer(newTestDeps()), `{"source":"x = 1","language":"python"}`)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "html") {
		t.Error("expected html in response")
	}
}

func TestRenderEmptySource(t *testing.T) {
	rec := postRender(newTestServer(newTestDeps()), `{"source":"","language":"python"}`)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestRenderTooLarge(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 10

	rec := postRender(newTestServer(deps),
		`{"source":"this is a very long source string","language":"python"}`)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ERR_TOO_LARGE") {
		t.Error("expected ERR_TOO_LARGE in response")
	}
}

func TestRenderBadRequest(t *testing.T) {
	rec := postRender(newTestServer(newTestDeps()), `{"source":`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ERR_BAD_REQUEST") {
		t.Error("expected ERR_BAD_REQUEST in response")
	}
}

func TestListLanguages(t *testing.T) {
	e := newTestServer(newTestDeps())

	req := httptest.NewRequest(http.MethodGet, "/api/highlight/languages", nil)
	req.Header.Set("X-Service-Token", "test-token")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "python") {
		t.Error("expected python in languages list")
	}
}

// chunkedReader hands out at most chunk bytes per Read, the way a socket does,
// and reports no length to httptest.NewRequest so the request goes out without
// a Content-Length.
//
// A strings.Reader would defeat the test twice over: it announces its length,
// and it delivers the whole body in a single Read, which lets the JSON decoder
// finish parsing before it ever looks at the error the body-limit reader
// returned alongside those bytes.
type chunkedReader struct {
	data  string
	chunk int
	read  int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.read >= len(r.data) {
		return 0, io.EOF
	}
	end := min(r.read+min(r.chunk, len(p)), len(r.data))
	n := copy(p, r.data[r.read:end])
	r.read += n
	return n, nil
}

// TestOversizedBodyReportsTooLargeFromEitherLimit covers the overlap between the
// two size limits guarding this endpoint.
//
// GORGE_RENDER_MAX_BYTES defaults to 1MiB, below the platform body limit, so a
// request normally trips the domain check inside renderHighlight —
// TestRenderTooLarge above covers that side. Raising it past the body limit,
// which deployments handling large files do, moves the rejection into the
// body-limit middleware, before any handler runs. Both must report
// ERR_TOO_LARGE, because Phorge's client branches on the code.
func TestOversizedBodyReportsTooLargeFromEitherLimit(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 8 << 20 // above the platform body limit of 2M

	// Larger than the body limit, smaller than deps.MaxBytes: the domain check
	// would accept this body if it ever saw it.
	body := `{"source":"` + strings.Repeat("x", 3<<20) + `","language":"python"}`
	rec := postRender(newPlatformServer(t, deps), body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (body %.200q)", rec.Code, rec.Body.String())
	}

	var envelope struct {
		Error   *httpx.Error `json:"error"`
		Message *string      `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if envelope.Message != nil {
		t.Errorf("framework rejection kept Echo's default shape: %q", rec.Body.String())
	}
	if envelope.Error == nil || envelope.Error.Code != httpx.CodeTooLarge {
		t.Errorf("expected %s, got %q", httpx.CodeTooLarge, rec.Body.String())
	}
}

// TestOversizedChunkedBodyIsNeverABadRequest covers the second half of the
// body-limit middleware: it compares Content-Length when there is one, and
// otherwise counts bytes as the handler reads them. A client streaming with
// Transfer-Encoding: chunked takes that second path, where the 413 surfaces out
// of c.Bind instead of before the handler runs, and renderHighlight has to hand
// it back to the platform rather than call it a bad request.
//
// Whether the limit fires at all on that path depends on the toolchain: the
// encoding/json decoder keeps reading past a read error as long as the reader
// returns bytes with it, and how far it gets changed between Go releases. So
// this asserts the part that is a contract — the code is never ERR_BAD_REQUEST —
// rather than that the request is rejected.
func TestOversizedChunkedBodyIsNeverABadRequest(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 8 << 20

	body := `{"source":"` + strings.Repeat("x", 3<<20) + `","language":"python"}`
	req := httptest.NewRequest(http.MethodPost, "/api/highlight/render",
		&chunkedReader{data: body, chunk: 16 << 10})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", "test-token")
	rec := httptest.NewRecorder()
	newPlatformServer(t, deps).ServeHTTP(rec, req)

	switch rec.Code {
	case http.StatusOK:
		// The decoder consumed the whole body before it looked at the limit
		// error, so the domain limit of 8MiB is what applied. Fine.
	case http.StatusRequestEntityTooLarge:
		if !strings.Contains(rec.Body.String(), httpx.CodeTooLarge) {
			t.Errorf("expected %s, got %q", httpx.CodeTooLarge, rec.Body.String())
		}
	default:
		t.Errorf("expected 200 or 413, got %d (%.200q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), httpx.CodeBadRequest) {
		t.Errorf("a body over the transport limit is not a bad request: %.200q", rec.Body.String())
	}
}

// TestHighlightFailedSurvivesTheErrorHandler guards the one domain error code
// this package owns. Phorge's client branches on ERR_HIGHLIGHT_FAILED, so the
// platform error handler must never rewrite it into ERR_INTERNAL, even when
// something raises an error after the handler has answered.
func TestHighlightFailedSurvivesTheErrorHandler(t *testing.T) {
	e := newPlatformServer(t, newTestDeps())

	// The shape of renderHighlight's failure path, with a late error added: a
	// real Highlight() failure needs a broken Chroma lexer to reproduce.
	e.GET("/test/highlight-failed", func(c echo.Context) error {
		if err := httpx.Fail(c, http.StatusInternalServerError,
			CodeHighlightFailed, "tokenisation failed"); err != nil {
			return err
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "raised after answering")
	})

	req := httptest.NewRequest(http.MethodGet, "/test/highlight-failed", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}

	var envelope struct {
		Error *httpx.Error `json:"error"`
	}
	decoder := json.NewDecoder(strings.NewReader(rec.Body.String()))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if decoder.More() {
		t.Fatalf("the error handler appended a second document: %q", rec.Body.String())
	}
	if envelope.Error == nil || envelope.Error.Code != CodeHighlightFailed {
		t.Fatalf("expected %s, got %q", CodeHighlightFailed, rec.Body.String())
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
	e := newPlatformServer(t, newTestDeps())

	req := httptest.NewRequest(http.MethodPost, "/api/highlight/rende", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", "test-token")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), httpx.CodeNotFound) {
		t.Errorf("expected %s, got %q", httpx.CodeNotFound, rec.Body.String())
	}
}

// TestRoutePathsAreStable pins the two paths Phorge's PhabricatorGoHighlightClient
// already calls. Renaming either is a breaking change on the PHP side.
func TestRoutePathsAreStable(t *testing.T) {
	e := newTestServer(newTestDeps())

	want := map[string]string{
		http.MethodPost + " /api/highlight/render":   "",
		http.MethodGet + " /api/highlight/languages": "",
	}
	for _, r := range e.Routes() {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("route %s is no longer registered", route)
	}
}
