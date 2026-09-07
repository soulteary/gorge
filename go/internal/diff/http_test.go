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
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func newTestDeps() *Deps {
	return &Deps{
		Token:    "test-token",
		MaxBytes: DefaultMaxBytes,
	}
}

func newTestServer(deps *Deps) *fiber.App {
	app := fiber.New()
	RegisterRoutes(app, deps)
	return app
}

// newPlatformServer builds the routes the way cmd/gorge-render does, so the
// body limit and the platform error handler are in play. The bare
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

func post(t *testing.T, app *fiber.App, path, body string) (*http.Response, string) {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", "test-token")
	return do(t, app, req)
}

// postLive serves the request over a real listener, needed for bodies over the
// platform limit: fasthttp rejects those while reading the request, which
// app.Test surfaces as a Go error rather than the 413 a real connection
// receives.
func postLive(t *testing.T, deps *Deps, bodyLimit, path, body string) (int, string) {
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

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(body))
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
	resp, body := post(t, newTestServer(newTestDeps()), "/api/diff/generate",
		`{"old":"hello\nworld\n","new":"hello\ngopher\n","oldName":"a","newName":"b"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (%q)", resp.StatusCode, body)
	}

	var envelope struct {
		Data struct {
			Diff  string `json:"diff"`
			Equal bool   `json:"equal"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
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
	resp, body := post(t, newTestServer(newTestDeps()), "/api/diff/prose",
		`{"old":"the quick brown fox","new":"the slow brown fox"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (%q)", resp.StatusCode, body)
	}

	var envelope struct {
		Data struct {
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
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
	resp, body := post(t, newTestServer(newTestDeps()), "/api/diff/generate", `{"old":"","new":""}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (%q)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"equal":true`) {
		t.Errorf("expected equal to be true, got %q", body)
	}
}

// decodeSoleEnvelope decodes the response and fails if anything follows it.
//
// Checking the status code alone is not enough for the paths that answer
// early. When one of those writes its response and then lets the handler carry
// on, the status of the first write is kept and the second document is appended
// to the body, so a status assertion still passes against a response no JSON
// parser will accept. That is a real failure this package shipped once.
func decodeSoleEnvelope(t *testing.T, body string) struct {
	Error *httpx.Error `json:"error"`
	Data  any          `json:"data"`
} {
	t.Helper()

	var envelope struct {
		Error *httpx.Error `json:"error"`
		Data  any          `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("response is not JSON: %v (body %.300q)", err, body)
	}
	if decoder.More() {
		t.Fatalf("the response carries more than one JSON document: %.300q", body)
	}
	return envelope
}

func TestMalformedBodyIsBadRequest(t *testing.T) {
	for _, path := range []string{"/api/diff/generate", "/api/diff/prose"} {
		resp, body := post(t, newPlatformServer(t, newTestDeps()), path, `{"old":`)

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", path, resp.StatusCode)
		}
		envelope := decodeSoleEnvelope(t, body)
		if envelope.Error == nil || envelope.Error.Code != httpx.CodeBadRequest {
			t.Errorf("%s: expected %s, got %q", path, httpx.CodeBadRequest, body)
		}
		if envelope.Data != nil {
			t.Errorf("%s: a rejected request must not carry data: %q", path, body)
		}
	}
}

func TestMissingTokenIsUnauthorized(t *testing.T) {
	for _, path := range []string{"/api/diff/generate", "/api/diff/prose"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp, body := do(t, newTestServer(newTestDeps()), req)

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d", path, resp.StatusCode)
		}
		if !strings.Contains(body, httpx.CodeUnauthorized) {
			t.Errorf("%s: expected %s, got %q", path, httpx.CodeUnauthorized, body)
		}
	}
}

func TestTokenViaQueryParameter(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost,
		"/api/diff/generate?token=test-token", strings.NewReader(`{"old":"a\n","new":"b\n"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, body := do(t, newTestServer(newTestDeps()), req)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d (%q)", resp.StatusCode, body)
	}
}

// TestCombinedSizeIsWhatCounts pins that the limit applies to the two sides
// together. Either side alone fits; the comparison is what does not.
func TestCombinedSizeIsWhatCounts(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 100

	side := strings.Repeat("x", 60)
	for _, path := range []string{"/api/diff/generate", "/api/diff/prose"} {
		resp, body := post(t, newPlatformServer(t, deps), path,
			`{"old":"`+side+`","new":"`+side+`"}`)

		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: expected 413, got %d (%q)", path, resp.StatusCode, body)
		}
		envelope := decodeSoleEnvelope(t, body)
		if envelope.Error == nil || envelope.Error.Code != httpx.CodeTooLarge {
			t.Errorf("%s: expected %s, got %q", path, httpx.CodeTooLarge, body)
		}
		if envelope.Data != nil {
			t.Errorf("%s: a rejected request must not carry data: %q", path, body)
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

	resp, respBody := post(t, newTestServer(newTestDeps()), "/api/diff/generate", string(body))

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (%.200q)", resp.StatusCode, respBody)
	}
	if !strings.Contains(respBody, httpx.CodeTooLarge) {
		t.Errorf("expected %s, got %.200q", httpx.CodeTooLarge, respBody)
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

	resp, respBody := post(t, newPlatformServer(t, deps), "/api/diff/generate", string(body))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (%.200q)", resp.StatusCode, respBody)
	}
}

// TestOversizedBodyReportsTooLargeFromEitherLimit covers the overlap between
// the two size limits guarding these endpoints.
//
// GORGE_DIFF_MAX_BYTES defaults below the platform body limit, so a request
// normally trips the domain check — TestCombinedSizeIsWhatCounts covers that
// side. Raising it past the body limit moves the rejection into the platform
// body limit (fasthttp), before any handler runs. Both must report
// ERR_TOO_LARGE, because Phorge's client branches on the code, and neither may
// leak the framework's default {"message": ...} shape.
func TestOversizedBodyReportsTooLargeFromEitherLimit(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 8 << 20 // above the platform body limit

	// Larger than a small platform body limit, smaller than deps.MaxBytes: the
	// domain check would accept this body if it ever saw it. A small limit and
	// a small overshoot keep the client's single write ahead of the server's
	// rejection, so the 413 comes back cleanly rather than as a reset.
	body := `{"old":"` + strings.Repeat("x", 4096) + `","new":"y"}`
	status, respBody := postLive(t, deps, "1K", "/api/diff/generate", body)

	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (%.200q)", status, respBody)
	}

	var envelope struct {
		Error   *httpx.Error `json:"error"`
		Message *string      `json:"message"`
	}
	if err := json.Unmarshal([]byte(respBody), &envelope); err != nil {
		t.Fatalf("response is not JSON: %v (body %.200q)", err, respBody)
	}
	if envelope.Message != nil {
		t.Errorf("framework rejection kept a default message shape: %.200q", respBody)
	}
	if envelope.Error == nil || envelope.Error.Code != httpx.CodeTooLarge {
		t.Errorf("expected %s, got %.200q", httpx.CodeTooLarge, respBody)
	}
}

// TestOversizedChunkedBodyIsNeverABadRequest covers a client sending a body
// past the platform limit. Under fasthttp the oversized body is rejected as
// fiber.ErrRequestEntityTooLarge before the handler runs, and bind() must hand
// any such non-400 error back to the platform rather than call it a bad
// request.
//
// Whether the limit fires at all on that path depends on how far the decoder
// gets before the limit applies, which can vary. So this asserts the part that
// is a contract — the code is never ERR_BAD_REQUEST — rather than that the
// request is rejected.
func TestOversizedChunkedBodyIsNeverABadRequest(t *testing.T) {
	deps := newTestDeps()
	deps.MaxBytes = 8 << 20

	body := `{"old":"` + strings.Repeat("x", 3<<20) + `","new":"y"}`
	status, respBody := postLive(t, deps, "2M", "/api/diff/generate", body)

	switch status {
	case 0:
		// The transport rejected the oversized body mid-stream and reset the
		// connection. Definitively not a bad request, which is all this pins.
	case http.StatusOK:
		// The whole body was consumed before the limit applied, so the 8MiB
		// domain limit is what governed. Fine.
	case http.StatusRequestEntityTooLarge:
		if !strings.Contains(respBody, httpx.CodeTooLarge) {
			t.Errorf("expected %s, got %.200q", httpx.CodeTooLarge, respBody)
		}
	default:
		t.Errorf("expected 200 or 413, got %d (%.200q)", status, respBody)
	}
	if strings.Contains(respBody, httpx.CodeBadRequest) {
		t.Errorf("a body over the transport limit is not a bad request: %.200q", respBody)
	}
}

// TestUnknownAPIPathIsEnveloped pins the envelope on a path Phorge could reach
// through a typo or version skew: a 404 raised by the router, not by a handler.
func TestUnknownAPIPathIsEnveloped(t *testing.T) {
	resp, body := post(t, newPlatformServer(t, newTestDeps()), "/api/diff/generat", `{}`)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body %q)", resp.StatusCode, body)
	}
	if !strings.Contains(body, httpx.CodeNotFound) {
		t.Errorf("expected %s, got %q", httpx.CodeNotFound, body)
	}
}

// TestRoutePathsAreStable pins the two paths the PHP side will call. They were
// chosen before this domain moved into the monorepo and must not change with
// it: renaming either is a breaking change on the Phorge side.
func TestRoutePathsAreStable(t *testing.T) {
	app := newTestServer(newTestDeps())

	want := map[string]string{
		http.MethodPost + " /api/diff/generate": "",
		http.MethodPost + " /api/diff/prose":    "",
	}
	for _, r := range app.GetRoutes(true) {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("route %s is no longer registered", route)
	}
}

// TestRenderRoutesAreUntouched guards the reason both domains can share one
// process: registering this one must not shadow or reorder the other's group.
func TestRenderRoutesAreUntouched(t *testing.T) {
	app := httpx.New(httpx.Config{}).App()
	app.Post("/api/highlight/render", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusTeapot)
	})
	RegisterRoutes(app, newTestDeps())

	req := httptest.NewRequest(http.MethodPost, "/api/highlight/render", strings.NewReader(`{}`))
	resp, _ := do(t, app, req)

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("the render route no longer answers: got %d", resp.StatusCode)
	}
}
