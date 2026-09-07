package mailer

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

const testToken = "test-token"

func newHTTPDeps(t *testing.T, specs ...MailerSpec) *Deps {
	t.Helper()
	if len(specs) == 0 {
		specs = []MailerSpec{{Key: "test-mailer", Type: "test"}}
	}
	return &Deps{
		Dispatcher: newTestDispatcher(t, specs...),
		Token:      testToken,
		BodyLimit:  DefaultBodyLimit,
	}
}

// newTestServer builds the routes the way cmd/gorge-mailer does, so the
// platform error handler and the health probes are in play.
func newTestServer(t *testing.T, deps *Deps) *fiber.App {
	t.Helper()

	// Requests that fail on purpose log; keep the test output readable.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	srv := httpx.New(httpx.Config{
		BodyLimit: TransportBodyLimit,
		Ready:     deps.Dispatcher.Ready,
	})
	RegisterRoutes(srv.App(), deps)
	return srv.App()
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

func postSend(t *testing.T, app *fiber.App, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mailer/send", strings.NewReader(body))
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

const validSend = `{"message":{"from":{"address":"sender@example.com"},` +
	`"to":[{"address":"rcpt@example.com"}],"subject":"Test","textBody":"Hello"}}`

func TestSendSuccess(t *testing.T) {
	resp, body := postSend(t, newTestServer(t, newHTTPDeps(t)), validSend)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"mailerKey":"test-mailer"`) {
		t.Errorf("unexpected response: %s", body)
	}
	if env := envelope(t, body); env.Error != nil {
		t.Errorf("a success must carry no error: %+v", env.Error)
	}
}

func TestSendRejectsIncompleteMessages(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing from", `{"message":{"to":[{"address":"rcpt@example.com"}],"subject":"Test"}}`},
		{"missing recipients", `{"message":{"from":{"address":"a@b.com"},"subject":"Test"}}`},
		{"missing subject", `{"message":{"from":{"address":"a@b.com"},"to":[{"address":"c@d.com"}]}}`},
	}

	app := newTestServer(t, newHTTPDeps(t))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 400 rather than 422: nothing judged this message undeliverable,
			// it never reached a backend.
			resp, body := postSend(t, app, tc.body)
			assertErrorCode(t, resp, body, http.StatusBadRequest, httpx.CodeBadRequest)
		})
	}
}

func TestSendMalformedBody(t *testing.T) {
	resp, body := postSend(t, newTestServer(t, newHTTPDeps(t)), `{"message":`)
	assertErrorCode(t, resp, body, http.StatusBadRequest, httpx.CodeBadRequest)
}

// TestSendPermanentFailureIs422 is the mapping Phorge's worker depends on: the
// PHP client turns this code into PhabricatorMetaMTAPermanentFailureException,
// which is what stops it re-queueing a message no backend will ever accept.
func TestSendPermanentFailureIs422(t *testing.T) {
	deps := newHTTPDeps(t, MailerSpec{
		Key: "rejects", Type: "test", Options: map[string]string{"fail": "permanent"},
	})

	resp, body := postSend(t, newTestServer(t, deps), validSend)
	assertErrorCode(t, resp, body, http.StatusUnprocessableEntity, CodePermanentFailure)
}

// TestSendTemporaryFailureIs502 is the other half: anything that might work
// later must not come back as permanent, or the mail is dropped silently.
func TestSendTemporaryFailureIs502(t *testing.T) {
	deps := newHTTPDeps(t, MailerSpec{
		Key: "down", Type: "test", Options: map[string]string{"fail": "temporary"},
	})

	resp, body := postSend(t, newTestServer(t, deps), validSend)
	assertErrorCode(t, resp, body, http.StatusBadGateway, CodeSendFailed)
}

func TestSendUnknownMailerKeyIs502(t *testing.T) {
	body := `{"message":{"from":{"address":"a@b.com"},"to":[{"address":"c@d.com"}],` +
		`"subject":"Test"},"mailerKeys":["nope"]}`

	resp, respBody := postSend(t, newTestServer(t, newHTTPDeps(t)), body)
	assertErrorCode(t, resp, respBody, http.StatusBadGateway, CodeSendFailed)
}

func TestSendTruncatesOversizedBodies(t *testing.T) {
	deps := newHTTPDeps(t)
	deps.BodyLimit = 16

	body := `{"message":{"from":{"address":"a@b.com"},"to":[{"address":"c@d.com"}],` +
		`"subject":"Test","textBody":"` + strings.Repeat("x", 100) + `"}}`

	if resp, respBody := postSend(t, newTestServer(t, deps), body); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
	}

	adapter := deps.Dispatcher.adapters[0].adapter.(*testAdapter)
	messages := adapter.Messages()
	if len(messages) != 1 {
		t.Fatalf("expected 1 delivered message, got %d", len(messages))
	}
	if got := len(messages[0].TextBody); got != 16 {
		t.Errorf("expected the body truncated to 16 bytes, got %d", got)
	}
}

func TestListMailers(t *testing.T) {
	deps := newHTTPDeps(t,
		MailerSpec{Key: "first", Type: "test", Priority: 10},
		MailerSpec{Key: "second", Type: "test", Priority: 5},
	)

	req := httptest.NewRequest(http.MethodGet, "/api/mailer/mailers", nil)
	req.Header.Set("X-Service-Token", testToken)
	resp, body := do(t, newTestServer(t, deps), req)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	env := envelope(t, body)
	var info []struct {
		Key      string `json:"key"`
		Type     string `json:"type"`
		Priority int    `json:"priority"`
	}
	if err := json.Unmarshal(env.Data, &info); err != nil {
		t.Fatal(err)
	}
	if len(info) != 2 || info[0].Key != "first" || info[1].Key != "second" {
		t.Fatalf("expected the priority order, got %+v", info)
	}
}

func TestTokenAuth(t *testing.T) {
	app := newTestServer(t, newHTTPDeps(t))

	t.Run("rejected without a token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/mailer/mailers", nil)
		resp, body := do(t, app, req)
		assertErrorCode(t, resp, body, http.StatusUnauthorized, httpx.CodeUnauthorized)
	})

	t.Run("accepted in the header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/mailer/mailers", nil)
		req.Header.Set("X-Service-Token", testToken)
		resp, body := do(t, app, req)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("accepted in the query string", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/mailer/mailers?token="+testToken, nil)
		resp, body := do(t, app, req)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", resp.StatusCode, body)
		}
	})
}

// TestReadyzReportsUnconfiguredBackends is the probe half of the readiness
// contract: the process is alive, but it can deliver nothing, and orchestration
// has to be able to tell those apart.
func TestReadyzReportsUnconfiguredBackends(t *testing.T) {
	probe := func(deps *Deps, path string) (*http.Response, string) {
		return do(t, newTestServer(t, deps), httptest.NewRequest(http.MethodGet, path, nil))
	}

	unconfigured := &Deps{Dispatcher: newTestDispatcher(t), Token: testToken}

	if resp, _ := probe(unconfigured, "/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("liveness must not depend on the backends, got %d", resp.StatusCode)
	}

	resp, body := probe(unconfigured, "/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", resp.StatusCode, body)
	}
	// Probe payloads stay outside the {data,error} envelope: orchestrators are
	// configured against the flat shape.
	if strings.Contains(body, `"data"`) {
		t.Errorf("probe payload must not use the envelope: %s", body)
	}

	configured := &Deps{
		Dispatcher: newTestDispatcher(t, MailerSpec{Key: "t", Type: "test"}),
		Token:      testToken,
	}
	if resp, body := probe(configured, "/readyz"); resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 once a backend is configured, got %d: %s", resp.StatusCode, body)
	}
}

// TestRoutePathsAreStable: Phorge's PhabricatorGorgeMailerClient calls these two
// paths as written, so renaming either is a breaking change on the PHP side.
func TestRoutePathsAreStable(t *testing.T) {
	app := newTestServer(t, newHTTPDeps(t))

	want := map[string]string{
		"POST /api/mailer/send":   "",
		"GET /api/mailer/mailers": "",
		"GET /healthz":            "",
		"GET /readyz":             "",
	}
	for _, r := range app.GetRoutes(true) {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("route %s is no longer registered", route)
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := "Hello, 世界" // 7 + 3 + 3 bytes

	if got := truncateUTF8(s, 10); got != "Hello, 世" {
		t.Errorf("limit=10: got %q", got)
	}
	// 9 and 8 both land inside the 3-byte rune and must back up rather than
	// emit half of it: a backend would reject or mangle invalid UTF-8.
	if got := truncateUTF8(s, 9); got != "Hello, " {
		t.Errorf("limit=9: got %q", got)
	}
	if got := truncateUTF8(s, 8); got != "Hello, " {
		t.Errorf("limit=8: got %q", got)
	}
	if got := truncateUTF8("Hi", 100); got != "Hi" {
		t.Errorf("short strings must pass through, got %q", got)
	}
}
