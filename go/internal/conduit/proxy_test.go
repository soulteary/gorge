package conduit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// newGateway builds an in-memory gateway app whose proxy points at upstreamURL,
// with no auth and no limiter, for exercising the relay directly.
func newGateway(upstreamURL string, timeoutSec int) *fiber.App {
	srv := httpx.New(httpx.Config{ListenAddr: ":0"})
	RegisterRoutes(srv.App(), &Deps{
		Proxy: NewProxy(upstreamURL, timeoutSec),
		Token: "",
	})
	return srv.App()
}

func TestProxyRelaysStatusHeaderAndBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The upstream is reached at /api/{method}.
		if r.URL.Path != "/api/conduit.ping" {
			t.Errorf("upstream path = %q, want /api/conduit.ping", r.URL.Path)
		}
		w.Header().Set("X-Upstream-Marker", "phorge")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, `{"result":{"pong":true},"error_code":null,"error_info":null}`)
	}))
	defer upstream.Close()

	app := newGateway(upstream.URL, 5)
	req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d (upstream status must be relayed)", resp.StatusCode, http.StatusTeapot)
	}
	if got := resp.Header.Get("X-Upstream-Marker"); got != "phorge" {
		t.Errorf("upstream header not relayed: X-Upstream-Marker = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("relayed body is not the upstream JSON: %v (body: %s)", err, body)
	}
	if _, ok := decoded["result"]; !ok {
		t.Errorf("relayed body should be the upstream Conduit envelope, got %s", body)
	}
}

func TestProxyInjectsForwardingHeaders(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = io.WriteString(w, `{"result":null}`)
	}))
	defer upstream.Close()

	app := newGateway(upstream.URL, 5)
	req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
	if _, err := app.Test(req); err != nil {
		t.Fatal(err)
	}
	if seen.Get("X-Conduit-Gateway") != "go-conduit" {
		t.Errorf("X-Conduit-Gateway = %q, want go-conduit", seen.Get("X-Conduit-Gateway"))
	}
	if seen.Get("X-Forwarded-For") == "" {
		t.Error("X-Forwarded-For should be set from the caller IP")
	}
	if seen.Get("X-Forwarded-Proto") == "" {
		t.Error("X-Forwarded-Proto should be set from the caller protocol")
	}
}

func TestProxyFiltersHopByHopHeaders(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = io.WriteString(w, `{"result":null}`)
	}))
	defer upstream.Close()

	app := newGateway(upstream.URL, 5)
	req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
	// A hop-by-hop header the caller set must not reach the upstream.
	req.Header.Set("X-Passthrough", "keep-me")
	req.Header.Set("Keep-Alive", "timeout=5")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp
	if seen.Get("Keep-Alive") != "" {
		t.Errorf("hop-by-hop Keep-Alive should be filtered, got %q", seen.Get("Keep-Alive"))
	}
	if seen.Get("X-Passthrough") != "keep-me" {
		t.Errorf("an ordinary header should pass through, X-Passthrough = %q", seen.Get("X-Passthrough"))
	}
}

func TestProxyPreservesRawQuery(t *testing.T) {
	var rawQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"result":null}`)
	}))
	defer upstream.Close()

	app := newGateway(upstream.URL, 5)
	req := httptest.NewRequest("GET", "/api/conduit.ping?a=1&b=two", nil)
	if _, err := app.Test(req); err != nil {
		t.Fatal(err)
	}
	if rawQuery != "a=1&b=two" {
		t.Errorf("upstream RawQuery = %q, want a=1&b=two", rawQuery)
	}
}

func TestProxyUpstreamFailureIsConduitError(t *testing.T) {
	// A proxy pointed at a closed port cannot reach any upstream.
	app := newGateway("http://127.0.0.1:1", 2)
	req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for an unreachable upstream", resp.StatusCode)
	}
	assertConduitCode(t, resp, "ERR-CONDUIT-PROXY")
}

func TestProxyUpstreamTimeoutIsConduitError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"result":null}`)
	}))
	defer upstream.Close()

	// Timeout shorter than the upstream's delay: the client aborts and the
	// gateway reports ERR-CONDUIT-PROXY rather than hanging.
	app := newGatewaySubsecondTimeout(upstream.URL)
	req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on upstream timeout", resp.StatusCode)
	}
	assertConduitCode(t, resp, "ERR-CONDUIT-PROXY")
}

// newGatewaySubsecondTimeout builds a gateway whose upstream client times out
// well under a second. NewProxy takes whole seconds, so the timeout is set on a
// proxy built directly for this test.
func newGatewaySubsecondTimeout(upstreamURL string) *fiber.App {
	srv := httpx.New(httpx.Config{ListenAddr: ":0"})
	p := NewProxy(upstreamURL, 0)
	p.client.Timeout = 100 * time.Millisecond
	RegisterRoutes(srv.App(), &Deps{Proxy: p, Token: ""})
	return srv.App()
}

func assertConduitCode(t *testing.T, resp *http.Response, wantCode string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("response is not JSON: %v (body: %s)", err, body)
	}
	if decoded["error_code"] != wantCode {
		t.Errorf("error_code = %v, want %s (body: %s)", decoded["error_code"], wantCode, body)
	}
	if v, ok := decoded["result"]; !ok || v != nil {
		t.Errorf("result should be present and null, got %v (ok=%v)", v, ok)
	}
}
