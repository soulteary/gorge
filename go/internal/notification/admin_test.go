package notification

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

	"github.com/soulteary/gorge/go/internal/notification/hub"
	"github.com/soulteary/gorge/go/internal/notification/peer"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// silenceLogs keeps the output of tests that fail requests on purpose readable.
func silenceLogs(t *testing.T) {
	t.Helper()

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
}

// result carries one app.Test response, the app.Test stand-in for
// httptest.ResponseRecorder.
type result struct {
	Code int
	Body string
}

// newAdminServer builds the admin port the way cmd/gorge-notification does, so
// the middleware stack and the global error handler in play are the real ones.
// That matters more here than in the other domains: the point of these tests is
// which responses escape the envelope the error handler would otherwise impose.
func newAdminServer(t *testing.T) (*fiber.App, *hub.Hub, *peer.List) {
	t.Helper()
	silenceLogs(t)

	messages := hub.New()
	peers := peer.NewList()

	app := httpx.New(httpx.Config{}).App()
	RegisterAdminRoutes(app, &AdminDeps{Hub: messages, Peers: peers})
	return app, messages, peers
}

func dispatch(t *testing.T, app *fiber.App, req *http.Request) result {
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
	return result{Code: resp.StatusCode, Body: string(body)}
}

func postTo(t *testing.T, app *fiber.App, target, body string) result {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return dispatch(t, app, req)
}

// postAsPhorge posts the way Phorge really does: a raw JSON body labelled with
// the form-urlencoded default curl puts on it, which HTTPSFuture leaves in
// place. Tests that mean to exercise the Content-Type constraint have to use
// this rather than postTo, whose application/json a binder would handle
// correctly.
func postAsPhorge(t *testing.T, app *fiber.App, target, body string) result {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return dispatch(t, app, req)
}

// illegalEscapeText is a message field carrying a percent sign that is not a
// valid escape sequence, and it is the single byte sequence that makes a
// form-labelled post fail if the handler ever binds on Content-Type.
//
// Nothing else in a payload does that job, which was measured rather than
// assumed: "&" and "=" only change the shape of the garbage keys form parsing
// produces, and a well-formed escape like "%20" decodes cleanly, so either way
// the request still answers 200. Do not "simplify" the percent sign out of a
// payload that has one — it is the whole guard.
const illegalEscapeText = "build 100% done"

func getFrom(t *testing.T, app *fiber.App, target string) result {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	return dispatch(t, app, req)
}

// decodeBare fails the test unless the body is a single JSON object with no
// envelope around it. Phorge indexes these keys off the top level, so an "data"
// or "error" wrapper here would make every field unreachable without any error
// being raised on either side.
func decodeBare(t *testing.T, rec result) map[string]any {
	t.Helper()

	decoder := json.NewDecoder(strings.NewReader(rec.Body))
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body)
	}
	if decoder.More() {
		t.Errorf("expected exactly one JSON document, got %q", rec.Body)
	}
	for _, key := range []string{"data", "error"} {
		if _, wrapped := body[key]; wrapped {
			t.Fatalf("response must not be wrapped in the %q envelope: %q", key, rec.Body)
		}
	}
	return body
}

// TestPostMessageAnswersABareReceipt is one half of the envelope exemption:
// Phorge reads the fingerprint straight off the top level of this body.
func TestPostMessageAnswersABareReceipt(t *testing.T) {
	e, messages, peers := newAdminServer(t)

	rec := postTo(t, e, "/?instance=default",
		`{"type":"notification","key":"123","subscribers":["PHID-USER-aaa"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := decodeBare(t, rec)
	if body["fingerprint"] != peers.Fingerprint() {
		t.Errorf("fingerprint = %v, want %q", body["fingerprint"], peers.Fingerprint())
	}

	history := messages.GetHistory(time.Now().Add(-time.Second))
	if len(history) != 1 {
		t.Fatalf("expected the message to be published, got %d history entries", len(history))
	}
	if history[0]["key"] != "123" {
		t.Errorf("expected the message to travel unchanged, got %v", history[0])
	}
}

// TestStatusIsBareAndFlat is the other half. The dotted keys have to survive as
// literal top-level keys: Phorge's cluster panel reads idx($details,
// 'clients.active'), which nested objects would not satisfy.
func TestStatusIsBareAndFlat(t *testing.T) {
	e, _, _ := newAdminServer(t)

	rec := getFrom(t, e, "/status/")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := decodeBare(t, rec)
	for _, key := range []string{
		"instance", "uptime", "version",
		"clients.active", "clients.total",
		"messages.in", "messages.out",
		"history.size", "history.age",
	} {
		if _, present := body[key]; !present {
			t.Errorf("missing key %q in %q", key, rec.Body)
		}
	}
	if body["instance"] != defaultInstance {
		t.Errorf("instance = %v, want %q", body["instance"], defaultInstance)
	}
}

func TestStatusReportsTheRequestedInstance(t *testing.T) {
	e, _, _ := newAdminServer(t)

	body := decodeBare(t, getFrom(t, e, "/status/?instance=prod"))
	if body["instance"] != "prod" {
		t.Errorf("instance = %v, want prod", body["instance"])
	}
}

func TestStatusCountsAPublishedMessage(t *testing.T) {
	e, _, _ := newAdminServer(t)

	postTo(t, e, "/", `{"type":"notification"}`)

	body := decodeBare(t, getFrom(t, e, "/status/"))
	if body["messages.in"] != float64(1) {
		t.Errorf("messages.in = %v, want 1", body["messages.in"])
	}
}

// TestContentTypeIsIgnored is why the handler decodes the body itself instead of
// calling c.Bind: Phorge posts a raw JSON payload through HTTPSFuture, which
// labels it with curl's form-urlencoded default.
//
// A content-type-driven binder dispatches on that header, and for this label it does not
// refuse the request — it takes the label at its word, form-parses the JSON and
// answers 200 with the message turned into garbage keys. The 415 the binder can
// produce comes from BindBody's default branch, which catches mediatypes it does
// not recognise, the empty one included; Phorge never sends an empty header, so
// 415 is not what this constraint is about. See compat/phorge/README.md
// section 5.4.
//
// That is why the published entry is inspected rather than counted: a
// form-parsed body lands in the history too, and only its contents tell the two
// apart. The constraint's other half — that the label Phorge really sends is
// accepted *and* the content survives — is held together in
// TestMessagePostedToAdminReachesASubscribedBrowser.
func TestContentTypeIsIgnored(t *testing.T) {
	for _, contentType := range []string{
		"application/json",
		"application/x-www-form-urlencoded",
		"",
	} {
		t.Run("content type "+contentType, func(t *testing.T) {
			e, messages, _ := newAdminServer(t)

			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"type":"notification"}`))
			if contentType != "" {
				req.Header.Set("Content-Type", contentType)
			}
			rec := dispatch(t, e, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body)
			}

			history := messages.GetHistory(time.Now().Add(-time.Second))
			if len(history) != 1 {
				t.Fatalf("expected the message to be published, got %d history entries", len(history))
			}
			if history[0]["type"] != "notification" {
				t.Errorf("the message reached the hub mangled: %v", history[0])
			}
		})
	}
}

// TestMessageAlreadyStampedIsNotRepublished covers the cluster loop guard: a
// message carrying our own fingerprint has come back to us, and republishing it
// would deliver it twice and relay it onward forever.
func TestMessageAlreadyStampedIsNotRepublished(t *testing.T) {
	e, messages, peers := newAdminServer(t)

	rec := postTo(t, e, "/", `{"type":"notification","touched":["`+peers.Fingerprint()+`"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := decodeBare(t, rec); body["fingerprint"] != peers.Fingerprint() {
		t.Errorf("expected the receipt to be answered anyway, got %v", body)
	}
	if history := messages.GetHistory(time.Now().Add(-time.Second)); len(history) != 0 {
		t.Errorf("expected nothing published, got %d history entries", len(history))
	}
}

// TestMalformedBodiesFailInTheEnvelope pins the other side of the exemption: only
// success answers bare. Failures keep the envelope, which is safe because the PHP
// client raises on any non-2xx without reading the body.
func TestMalformedBodiesFailInTheEnvelope(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"truncated json", "{not json"},
		{"json array", `["not","an","object"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _, _ := newAdminServer(t)

			rec := postTo(t, e, "/", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body)
			}

			var body struct {
				Error *httpx.Error `json:"error"`
			}
			if err := json.Unmarshal([]byte(rec.Body), &body); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if body.Error == nil {
				t.Fatalf("expected an error envelope, got %q", rec.Body)
			}
			if body.Error.Code != httpx.CodeBadRequest {
				t.Errorf("code = %q, want %q", body.Error.Code, httpx.CodeBadRequest)
			}
		})
	}
}

func TestStatusWithoutTrailingSlashIsNotFound(t *testing.T) {
	e, _, _ := newAdminServer(t)

	// Phorge always calls getURI('/status/'), so the trailing slash is part of
	// the contract rather than a convenience.
	if rec := getFrom(t, e, "/status"); rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for /status, got %d", rec.Code)
	}
}

func TestUnknownAdminPathIsNotFound(t *testing.T) {
	e, _, _ := newAdminServer(t)

	if rec := getFrom(t, e, "/something"); rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// TestAdminProbesAnswer records a deliberate difference from Aphlict, which
// answered 405 on GET /: the admin port keeps the platform's root probe, since
// POST / and GET / differ by method and can coexist. Phorge only ever probes
// /status/, so nothing observes the change.
func TestAdminProbesAnswer(t *testing.T) {
	e, _, _ := newAdminServer(t)

	for _, path := range []string{"/", "/healthz", "/readyz"} {
		rec := getFrom(t, e, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, rec.Code)
		}
		if body := decodeBare(t, rec); body["status"] != "ok" {
			t.Errorf("%s: expected a bare status, got %q", path, rec.Body)
		}
	}
}
