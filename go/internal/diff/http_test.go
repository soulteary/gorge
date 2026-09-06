package diff

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func newTestDeps() *Deps {
	return &Deps{
		Token:    "test-token",
		MaxBytes: DefaultMaxBytes,
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

func post(e *echo.Echo, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", "test-token")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// lines builds n newline-terminated lines with the given prefix.
func lines(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(prefix)
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestGenerateSuccess(t *testing.T) {
	rec := post(newTestServer(newTestDeps()), "/api/diff/generate",
		`{"old":"hello\nworld\n","new":"hello\ngopher\n","oldName":"a","newName":"b"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%q)", rec.Code, rec.Body.String())
	}

	var envelope struct {
		Data struct {
			Diff  string `json:"diff"`
			Equal bool   `json:"equal"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if envelope.Data.Equal {
		t.Error("expected equal to be false")
	}
	want := "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,2 @@\n hello\n-world\n+gopher\n"
	if envelope.Data.Diff != want {
		t.Errorf("diff mismatch\n got: %q\nwant: %q", envelope.Data.Diff, want)
	}
}

func TestProseSuccess(t *testing.T) {
	rec := post(newTestServer(newTestDeps()), "/api/diff/prose",
		`{"old":"the quick brown fox","new":"the slow brown fox"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%q)", rec.Code, rec.Body.String())
	}

	var envelope struct {
		Data struct {
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(envelope.Data.Parts) == 0 {
		t.Fatal("expected segments")
	}

	var oldSide, newSide strings.Builder
	for _, p := range envelope.Data.Parts {
		if p.Type == "=" || p.Type == "-" {
			oldSide.WriteString(p.Text)
		}
		if p.Type == "=" || p.Type == "+" {
			newSide.WriteString(p.Text)
		}
	}
	if oldSide.String() != "the quick brown fox" || newSide.String() != "the slow brown fox" {
		t.Errorf("segments do not reconstruct both sides: %q / %q",
			oldSide.String(), newSide.String())
	}
}

// TestEmptyInputIsNotAnError mirrors render's empty-source behaviour, but for
// a different reason: two empty texts really are equal, so the changeless diff
// PHP synthesises for that case is the correct answer rather than a shortcut.
func TestEmptyInputIsNotAnError(t *testing.T) {
	rec := post(newTestServer(newTestDeps()), "/api/diff/generate", `{"old":"","new":""}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"equal":true`) {
		t.Errorf("expected equal to be true, got %q", rec.Body.String())
	}
}

// decodeSoleEnvelope decodes the response and fails if anything follows it.
//
// Checking the status code alone is not enough for the paths that answer
// early. When one of those writes its response and then lets the handler carry
// on, Echo keeps the status of the first write and appends the second document
// to the body, so a status assertion still passes against a response no JSON
// parser will accept. That is a real failure this package shipped once.
func decodeSoleEnvelope(t *testing.T, rec *httptest.ResponseRecorder) struct {
	Error *httpx.Error `json:"error"`
	Data  any          `json:"data"`
} {
	t.Helper()

	var envelope struct {
		Error *httpx.Error `json:"error"`
		Data  any          `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(rec.Body.String()))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("response is not JSON: %v (body %.300q)", err, rec.Body.String())
	}
	if decoder.More() {
		t.Fatalf("the response carries more than one JSON document: %.300q", rec.Body.String())
	}
	return envelope
}

func TestMalformedBodyIsBadRequest(t *testing.T) {
	for _, path := range []string{"/api/diff/generate", "/api/diff/prose"} {
		rec := post(newPlatformServer(t, newTestDeps()), path, `{"old":`)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", path, rec.Code)
		}
		envelope := decodeSoleEnvelope(t, rec)
		if envelope.Error == nil || envelope.Error.Code != httpx.CodeBadRequest {
			t.Errorf("%s: expected %s, got %q", path, httpx.CodeBadRequest, rec.Body.String())
		}
		if envelope.Data != nil {
			t.Errorf("%s: a rejected request must not carry data: %q", path, rec.Body.String())
		}
	}
}

func TestMissingTokenIsUnauthorized(t *testing.T) {
	for _, path := range []string{"/api/diff/generate", "/api/diff/prose"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		newTestServer(newTestDeps()).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), httpx.CodeUnauthorized) {
			t.Errorf("%s: expected %s, got %q", path, httpx.CodeUnauthorized, rec.Body.String())
		}
	}
}

func TestTokenViaQueryParameter(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost,
		"/api/diff/generate?token=test-token", strings.NewReader(`{"old":"a\n","new":"b\n"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	newTestServer(newTestDeps()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (%q)", rec.Code, rec.Body.String())
	}
}

// TestCombinedSizeIsWhatCounts pins that the limit applies to the two sides
// together. Either side alone fits; the comparison is what does not.
func TestCombinedSizeIsWhatCounts(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 100

	side := strings.Repeat("x", 60)
	for _, path := range []string{"/api/diff/generate", "/api/diff/prose"} {
		rec := post(newPlatformServer(t, deps), path,
			`{"old":"`+side+`","new":"`+side+`"}`)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: expected 413, got %d (%q)", path, rec.Code, rec.Body.String())
		}
		envelope := decodeSoleEnvelope(t, rec)
		if envelope.Error == nil || envelope.Error.Code != httpx.CodeTooLarge {
			t.Errorf("%s: expected %s, got %q", path, httpx.CodeTooLarge, rec.Body.String())
		}
		if envelope.Data != nil {
			t.Errorf("%s: a rejected request must not carry data: %q", path, rec.Body.String())
		}
	}
}

// TestTooManyLinesIsTooLargeNotInternal covers the second guard, which the
// byte-count check cannot stand in for: 2001 short lines on each side is only
// a few kilobytes but asks for a four-million-cell table. Reporting it as 500
// would send Phorge looking for a service fault instead of trimming the input.
func TestTooManyLinesIsTooLargeNotInternal(t *testing.T) {
	body, err := json.Marshal(map[string]string{
		"old": lines("o", 2001),
		"new": lines("n", 2001),
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := post(newTestServer(newTestDeps()), "/api/diff/generate", string(body))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (%.200q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), httpx.CodeTooLarge) {
		t.Errorf("expected %s, got %.200q", httpx.CodeTooLarge, rec.Body.String())
	}
}

// TestSkewedLineCountsAreAccepted is the other half of that guard: a lopsided
// comparison allocates a small table and must go through. A line-count ceiling
// instead of a cell-count one would reject this.
func TestSkewedLineCountsAreAccepted(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 8 << 20

	body, err := json.Marshal(map[string]string{
		"old": lines("o", 10),
		"new": lines("n", 50000),
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := post(newPlatformServer(t, deps), "/api/diff/generate", string(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%.200q)", rec.Code, rec.Body.String())
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

// TestOversizedBodyReportsTooLargeFromEitherLimit covers the overlap between
// the two size limits guarding these endpoints.
//
// GORGE_DIFF_MAX_BYTES defaults below the platform body limit, so a request
// normally trips the domain check — TestCombinedSizeIsWhatCounts covers that
// side. Raising it past the body limit moves the rejection into the body-limit
// middleware, before any handler runs. Both must report ERR_TOO_LARGE, because
// Phorge's client branches on the code, and neither may leak Echo's default
// {"message": ...} shape.
func TestOversizedBodyReportsTooLargeFromEitherLimit(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 8 << 20 // above the platform body limit of 2M

	body := `{"old":"` + strings.Repeat("x", 3<<20) + `","new":"y"}`
	rec := post(newPlatformServer(t, deps), "/api/diff/generate", body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (%.200q)", rec.Code, rec.Body.String())
	}

	var envelope struct {
		Error   *httpx.Error `json:"error"`
		Message *string      `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not JSON: %v (body %.200q)", err, rec.Body.String())
	}
	if envelope.Message != nil {
		t.Errorf("framework rejection kept Echo's default shape: %.200q", rec.Body.String())
	}
	if envelope.Error == nil || envelope.Error.Code != httpx.CodeTooLarge {
		t.Errorf("expected %s, got %.200q", httpx.CodeTooLarge, rec.Body.String())
	}
}

// TestOversizedChunkedBodyIsNeverABadRequest covers the second half of the
// body-limit middleware: it compares Content-Length when there is one, and
// otherwise counts bytes as the handler reads them. A client streaming with
// Transfer-Encoding: chunked takes that second path, where the 413 surfaces out
// of c.Bind instead of before the handler runs, and bind() has to hand it back
// to the platform rather than call it a bad request.
//
// Whether the limit fires at all on that path depends on the toolchain: the
// encoding/json decoder keeps reading past a read error as long as the reader
// returns bytes with it, and how far it gets changed between Go releases. So
// this asserts the part that is a contract — the code is never
// ERR_BAD_REQUEST — rather than that the request is rejected.
func TestOversizedChunkedBodyIsNeverABadRequest(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 8 << 20

	body := `{"old":"` + strings.Repeat("x", 3<<20) + `","new":"y"}`
	req := httptest.NewRequest(http.MethodPost, "/api/diff/generate",
		&chunkedReader{data: body, chunk: 16 << 10})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", "test-token")
	rec := httptest.NewRecorder()
	newPlatformServer(t, deps).ServeHTTP(rec, req)

	switch rec.Code {
	case http.StatusOK, http.StatusRequestEntityTooLarge:
		// Either the decoder consumed the whole body before it looked at the
		// limit error, in which case the 8MiB domain limit applied, or the
		// middleware won. Both are acceptable.
	default:
		t.Errorf("expected 200 or 413, got %d (%.200q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), httpx.CodeBadRequest) {
		t.Errorf("a body over the transport limit is not a bad request: %.200q", rec.Body.String())
	}
}

// TestUnknownAPIPathIsEnveloped pins the envelope on a path Phorge could reach
// through a typo or version skew: a 404 raised by the router, not by a handler.
func TestUnknownAPIPathIsEnveloped(t *testing.T) {
	rec := post(newPlatformServer(t, newTestDeps()), "/api/diff/generat", `{}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), httpx.CodeNotFound) {
		t.Errorf("expected %s, got %q", httpx.CodeNotFound, rec.Body.String())
	}
}

// TestRoutePathsAreStable pins the two paths the PHP side will call. They were
// chosen before this domain moved into the monorepo and must not change with
// it: renaming either is a breaking change on the Phorge side.
func TestRoutePathsAreStable(t *testing.T) {
	e := newTestServer(newTestDeps())

	want := map[string]string{
		http.MethodPost + " /api/diff/generate": "",
		http.MethodPost + " /api/diff/prose":    "",
	}
	for _, r := range e.Routes() {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("route %s is no longer registered", route)
	}
}

// TestRenderRoutesAreUntouched guards the reason both domains can share one
// process: registering this one must not shadow or reorder the other's group.
func TestRenderRoutesAreUntouched(t *testing.T) {
	e := httpx.New(httpx.Config{}).Echo()
	e.POST("/api/highlight/render", func(c echo.Context) error {
		return c.NoContent(http.StatusTeapot)
	})
	RegisterRoutes(e, newTestDeps())

	req := httptest.NewRequest(http.MethodPost, "/api/highlight/render", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("the render route no longer answers: got %d", rec.Code)
	}
}
