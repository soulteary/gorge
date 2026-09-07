package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// captureLogs redirects the platform logger into a buffer for the duration of a
// test. Tests that deliberately trigger a 5xx would otherwise print a stack
// trace, and the buffer is how they assert the detail withheld from the client
// really did reach the log.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// response is the status, headers and body of one answered request, decoupled
// from whether it came through app.Test in memory or a real listener.
type response struct {
	Code   int
	Header http.Header
	body   []byte
}

func (r response) BodyString() string { return string(r.body) }
func (r response) BodyBytes() []byte  { return r.body }

// send runs one request against a server built the way every gorge binary
// builds it, so the middleware stack and the error handler are the real ones.
func send(t *testing.T, cfg Config, req *http.Request, register func(*fiber.App)) response {
	t.Helper()

	srv := New(cfg)
	if register != nil {
		register(srv.App())
	}

	resp, err := srv.App().Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	_ = resp.Body.Close()
	return response{Code: resp.StatusCode, Header: resp.Header, body: body}
}

// sendLive serves the request over a real listener. It is needed for the one
// case app.Test cannot reproduce: a body over fasthttp's limit is rejected
// while the request line is still being read, which app.Test surfaces as a Go
// error rather than the 413 the server writes on a real connection.
func sendLive(t *testing.T, cfg Config, method, path, body string, headers map[string]string) response {
	t.Helper()

	srv := New(cfg)
	done := make(chan error, 1)
	go func() { done <- RunAll(srv) }()
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

	req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return response{Code: resp.StatusCode, Header: resp.Header, body: raw}
}

// envelopeOf decodes a response and fails unless it is an error envelope: no
// data key, an error object present, and nothing of a default {"message": ...}
// shape left at the top level.
func envelopeOf(t *testing.T, r response) Error {
	t.Helper()

	var body struct {
		Data    json.RawMessage `json:"data"`
		Error   *Error          `json:"error"`
		Message *string         `json:"message"`
	}
	decoder := json.NewDecoder(strings.NewReader(r.BodyString()))
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, r.BodyString())
	}
	if decoder.More() {
		t.Fatalf("response carries more than one JSON document: %q", r.BodyString())
	}
	if body.Message != nil {
		t.Errorf("response kept a default message shape: %q", r.BodyString())
	}
	if body.Data != nil {
		t.Errorf("error response must not populate data: %q", r.BodyString())
	}
	if body.Error == nil {
		t.Fatalf("response has no error object: %q", r.BodyString())
	}
	return *body.Error
}

// TestBodyOverLimitIsEnveloped covers the body limit, which rejects a request
// before any handler runs and so never passes through httpx.Fail. It runs over
// a real listener because that rejection happens at the fasthttp server layer,
// which app.Test does not exercise.
func TestBodyOverLimitIsEnveloped(t *testing.T) {
	r := sendLive(t, Config{BodyLimit: "1K"}, http.MethodPost, "/api/thing",
		strings.Repeat("x", 4096),
		map[string]string{fiber.HeaderContentType: fiber.MIMEApplicationJSON})

	if r.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (body %q)", r.Code, r.BodyString())
	}
	// The domain-level size check in internal/render reports the same code, so
	// a caller sees one code no matter which limit it tripped.
	if got := envelopeOf(t, r).Code; got != CodeTooLarge {
		t.Errorf("expected %s, got %s", CodeTooLarge, got)
	}
}

// TestUnknownPathIsEnveloped covers Fiber's router, which answers before the
// request reaches any of our code.
func TestUnknownPathIsEnveloped(t *testing.T) {
	r := send(t, Config{}, httptest.NewRequest(http.MethodGet, "/api/nope", nil), nil)

	if r.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", r.Code)
	}
	if got := envelopeOf(t, r).Code; got != CodeNotFound {
		t.Errorf("expected %s, got %s", CodeNotFound, got)
	}
}

func TestMethodNotAllowedIsEnveloped(t *testing.T) {
	r := send(t, Config{}, httptest.NewRequest(http.MethodDelete, "/healthz", nil), nil)

	if r.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", r.Code)
	}
	if got := envelopeOf(t, r).Code; got != CodeMethodNotAllowed {
		t.Errorf("expected %s, got %s", CodeMethodNotAllowed, got)
	}
}

// TestPanicIsEnvelopedWithoutLeakingDetail covers the Recover middleware, and
// pins the rule that a 5xx tells the client nothing beyond "it failed": the
// panic value and its stack are for the log only.
func TestPanicIsEnvelopedWithoutLeakingDetail(t *testing.T) {
	logs := captureLogs(t)

	const secret = "postgres://user:hunter2@db.internal/prod"
	r := send(t, Config{}, httptest.NewRequest(http.MethodGet, "/api/boom", nil),
		func(app *fiber.App) {
			app.Get("/api/boom", func(c fiber.Ctx) error { panic(secret) })
		})

	if r.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", r.Code)
	}
	envelope := envelopeOf(t, r)
	if envelope.Code != CodeInternal {
		t.Errorf("expected %s, got %s", CodeInternal, envelope.Code)
	}
	if strings.Contains(r.BodyString(), secret) {
		t.Errorf("panic value leaked to the client: %q", r.BodyString())
	}
	if strings.Contains(r.BodyString(), "goroutine") {
		t.Errorf("stack trace leaked to the client: %q", r.BodyString())
	}
	if !strings.Contains(logs.String(), secret) {
		t.Error("panic value did not reach the log, so the failure is now undiagnosable")
	}
}

// TestHandlerErrorIsEnveloped covers a handler returning a raw error instead of
// answering, which Fiber also routes through the error handler.
func TestHandlerErrorIsEnveloped(t *testing.T) {
	logs := captureLogs(t)

	const detail = "dial tcp 10.0.0.7:5432: connection refused"
	r := send(t, Config{}, httptest.NewRequest(http.MethodGet, "/api/thing", nil),
		func(app *fiber.App) {
			app.Get("/api/thing", func(c fiber.Ctx) error { return errors.New(detail) })
		})

	if r.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", r.Code)
	}
	if got := envelopeOf(t, r).Code; got != CodeInternal {
		t.Errorf("expected %s, got %s", CodeInternal, got)
	}
	if strings.Contains(r.BodyString(), detail) {
		t.Errorf("internal error string leaked to the client: %q", r.BodyString())
	}
	if !strings.Contains(logs.String(), detail) {
		t.Error("internal error string did not reach the log")
	}
}

// TestCommittedResponseIsNotOverwritten is the guard for domain error codes: a
// handler that already answered keeps its body, so internal/render's
// ERR_HIGHLIGHT_FAILED cannot be rewritten into ERR_INTERNAL and no response
// ends up with two JSON documents in it. Fiber has no Committed flag, so OK/Fail
// mark the context in Locals and errorHandler honours it.
func TestCommittedResponseIsNotOverwritten(t *testing.T) {
	captureLogs(t)

	const domainCode = "ERR_DOMAIN_SPECIFIC"
	r := send(t, Config{}, httptest.NewRequest(http.MethodGet, "/api/thing", nil),
		func(app *fiber.App) {
			app.Get("/api/thing", func(c fiber.Ctx) error {
				if err := Fail(c, http.StatusInternalServerError, domainCode, "domain detail"); err != nil {
					return err
				}
				return fiber.NewError(http.StatusInternalServerError, "raised after answering")
			})
		})

	if r.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", r.Code)
	}
	envelope := envelopeOf(t, r)
	if envelope.Code != domainCode {
		t.Errorf("expected the domain code %s to survive, got %s", domainCode, envelope.Code)
	}
	if envelope.Message != "domain detail" {
		t.Errorf("expected the domain message to survive, got %q", envelope.Message)
	}
}

// TestProbesStayBareUnderTheErrorHandler is the companion to
// health.TestProbesAreNotEnveloped: that test builds a bare app, this one runs
// the probes through the full platform stack to prove the error handler did not
// start wrapping them.
func TestProbesStayBareUnderTheErrorHandler(t *testing.T) {
	for _, path := range []string{"/", "/healthz", "/readyz"} {
		r := send(t, Config{}, httptest.NewRequest(http.MethodGet, path, nil), nil)

		if r.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, r.Code)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(r.BodyBytes(), &body); err != nil {
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

// TestHeadErrorCarriesNoBody keeps the one case where the envelope is
// deliberately absent from an error response: HTTP forbids a body on a HEAD
// reply, so only the status can carry the failure.
func TestHeadErrorCarriesNoBody(t *testing.T) {
	r := send(t, Config{}, httptest.NewRequest(http.MethodHead, "/api/nope", nil), nil)

	if r.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", r.Code)
	}
	if len(r.BodyBytes()) != 0 {
		t.Errorf("expected an empty body, got %q", r.BodyString())
	}
}

func TestClassifyMapsStatusesToCodes(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"bad request", fiber.ErrBadRequest, http.StatusBadRequest, CodeBadRequest, "Bad Request"},
		{"unauthorized", fiber.ErrUnauthorized, http.StatusUnauthorized, CodeUnauthorized, "Unauthorized"},
		{"not found", fiber.ErrNotFound, http.StatusNotFound, CodeNotFound, "Not Found"},
		{"method not allowed", fiber.ErrMethodNotAllowed, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "Method Not Allowed"},
		{"too large", fiber.ErrRequestEntityTooLarge, http.StatusRequestEntityTooLarge, CodeTooLarge, "Request Entity Too Large"},
		{"unmapped 4xx", fiber.ErrUnsupportedMediaType, http.StatusUnsupportedMediaType, CodeBadRequest, "Unsupported Media Type"},
		{"bad gateway", fiber.ErrBadGateway, http.StatusBadGateway, CodeInternal, internalMessage},
		{"raw error", errors.New("something internal"), http.StatusInternalServerError, CodeInternal, internalMessage},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, message := classify(tc.err)

			if status != tc.status {
				t.Errorf("status: expected %d, got %d", tc.status, status)
			}
			if code != tc.code {
				t.Errorf("code: expected %s, got %s", tc.code, code)
			}
			if message != tc.message {
				t.Errorf("message: expected %q, got %q", tc.message, message)
			}
		})
	}
}
