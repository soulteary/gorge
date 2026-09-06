package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
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

// send runs one request against a server built the way every gorge binary
// builds it, so the middleware stack and the error handler are the real ones.
func send(t *testing.T, cfg Config, req *http.Request, register func(*echo.Echo)) *httptest.ResponseRecorder {
	t.Helper()

	srv := New(cfg)
	if register != nil {
		register(srv.Echo())
	}

	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, req)
	return rec
}

// envelopeOf decodes a response and fails unless it is an error envelope: no
// data key, an error object present, and nothing of Echo's default
// {"message": "..."} shape left at the top level.
func envelopeOf(t *testing.T, rec *httptest.ResponseRecorder) Error {
	t.Helper()

	var body struct {
		Data    json.RawMessage `json:"data"`
		Error   *Error          `json:"error"`
		Message *string         `json:"message"`
	}
	decoder := json.NewDecoder(strings.NewReader(rec.Body.String()))
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if decoder.More() {
		t.Fatalf("response carries more than one JSON document: %q", rec.Body.String())
	}
	if body.Message != nil {
		t.Errorf("response kept Echo's default shape: %q", rec.Body.String())
	}
	if body.Data != nil {
		t.Errorf("error response must not populate data: %q", rec.Body.String())
	}
	if body.Error == nil {
		t.Fatalf("response has no error object: %q", rec.Body.String())
	}
	return *body.Error
}

// TestBodyOverLimitIsEnveloped covers the body-limit middleware, which rejects a
// request before any handler runs and so never passes through httpx.Fail.
func TestBodyOverLimitIsEnveloped(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/thing",
		strings.NewReader(strings.Repeat("x", 4096)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := send(t, Config{BodyLimit: "1K"}, req, func(e *echo.Echo) {
		e.POST("/api/thing", func(c echo.Context) error { return OK(c, "unreachable") })
	})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
	// The domain-level size check in internal/render reports the same code, so
	// a caller sees one code no matter which limit it tripped.
	if got := envelopeOf(t, rec).Code; got != CodeTooLarge {
		t.Errorf("expected %s, got %s", CodeTooLarge, got)
	}
}

// TestUnknownPathIsEnveloped covers Echo's router, which answers before the
// request reaches any of our code.
func TestUnknownPathIsEnveloped(t *testing.T) {
	rec := send(t, Config{}, httptest.NewRequest(http.MethodGet, "/api/nope", nil), nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if got := envelopeOf(t, rec).Code; got != CodeNotFound {
		t.Errorf("expected %s, got %s", CodeNotFound, got)
	}
}

func TestMethodNotAllowedIsEnveloped(t *testing.T) {
	rec := send(t, Config{}, httptest.NewRequest(http.MethodDelete, "/healthz", nil), nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
	if got := envelopeOf(t, rec).Code; got != CodeMethodNotAllowed {
		t.Errorf("expected %s, got %s", CodeMethodNotAllowed, got)
	}
}

// TestPanicIsEnvelopedWithoutLeakingDetail covers the Recover middleware, and
// pins the rule that a 5xx tells the client nothing beyond "it failed": the
// panic value and its stack are for the log only.
func TestPanicIsEnvelopedWithoutLeakingDetail(t *testing.T) {
	logs := captureLogs(t)

	const secret = "postgres://user:hunter2@db.internal/prod"
	rec := send(t, Config{}, httptest.NewRequest(http.MethodGet, "/api/boom", nil),
		func(e *echo.Echo) {
			e.GET("/api/boom", func(c echo.Context) error { panic(secret) })
		})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	envelope := envelopeOf(t, rec)
	if envelope.Code != CodeInternal {
		t.Errorf("expected %s, got %s", CodeInternal, envelope.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("panic value leaked to the client: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "goroutine") {
		t.Errorf("stack trace leaked to the client: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), secret) {
		t.Error("panic value did not reach the log, so the failure is now undiagnosable")
	}
}

// TestHandlerErrorIsEnveloped covers a handler returning a raw error instead of
// answering, which Echo also routes through the error handler.
func TestHandlerErrorIsEnveloped(t *testing.T) {
	logs := captureLogs(t)

	const detail = "dial tcp 10.0.0.7:5432: connection refused"
	rec := send(t, Config{}, httptest.NewRequest(http.MethodGet, "/api/thing", nil),
		func(e *echo.Echo) {
			e.GET("/api/thing", func(c echo.Context) error { return errors.New(detail) })
		})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if got := envelopeOf(t, rec).Code; got != CodeInternal {
		t.Errorf("expected %s, got %s", CodeInternal, got)
	}
	if strings.Contains(rec.Body.String(), detail) {
		t.Errorf("internal error string leaked to the client: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), detail) {
		t.Error("internal error string did not reach the log")
	}
}

// TestCommittedResponseIsNotOverwritten is the guard for domain error codes: a
// handler that already answered keeps its body, so internal/render's
// ERR_HIGHLIGHT_FAILED cannot be rewritten into ERR_INTERNAL and no response
// ends up with two JSON documents in it.
func TestCommittedResponseIsNotOverwritten(t *testing.T) {
	captureLogs(t)

	const domainCode = "ERR_DOMAIN_SPECIFIC"
	rec := send(t, Config{}, httptest.NewRequest(http.MethodGet, "/api/thing", nil),
		func(e *echo.Echo) {
			e.GET("/api/thing", func(c echo.Context) error {
				if err := Fail(c, http.StatusInternalServerError, domainCode, "domain detail"); err != nil {
					return err
				}
				return echo.NewHTTPError(http.StatusInternalServerError, "raised after answering")
			})
		})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	envelope := envelopeOf(t, rec)
	if envelope.Code != domainCode {
		t.Errorf("expected the domain code %s to survive, got %s", domainCode, envelope.Code)
	}
	if envelope.Message != "domain detail" {
		t.Errorf("expected the domain message to survive, got %q", envelope.Message)
	}
}

// TestProbesStayBareUnderTheErrorHandler is the companion to
// health.TestProbesAreNotEnveloped: that test builds a bare Echo, this one runs
// the probes through the full platform stack to prove the error handler did not
// start wrapping them.
func TestProbesStayBareUnderTheErrorHandler(t *testing.T) {
	for _, path := range []string{"/", "/healthz", "/readyz"} {
		rec := send(t, Config{}, httptest.NewRequest(http.MethodGet, path, nil), nil)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, rec.Code)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
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
	rec := send(t, Config{}, httptest.NewRequest(http.MethodHead, "/api/nope", nil), nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected an empty body, got %q", rec.Body.String())
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
		{"bad request", echo.ErrBadRequest, http.StatusBadRequest, CodeBadRequest, "Bad Request"},
		{"unauthorized", echo.ErrUnauthorized, http.StatusUnauthorized, CodeUnauthorized, "Unauthorized"},
		{"not found", echo.ErrNotFound, http.StatusNotFound, CodeNotFound, "Not Found"},
		{"method not allowed", echo.ErrMethodNotAllowed, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "Method Not Allowed"},
		{"too large", echo.ErrStatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge, CodeTooLarge, "Request Entity Too Large"},
		{"unmapped 4xx", echo.ErrUnsupportedMediaType, http.StatusUnsupportedMediaType, CodeBadRequest, "Unsupported Media Type"},
		{"bad gateway", echo.ErrBadGateway, http.StatusBadGateway, CodeInternal, internalMessage},
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
