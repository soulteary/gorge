package conduit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// stubUpstream answers a fixed Conduit success body, so a passing request has
// something to relay without a live Phorge.
func stubUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"result":{"ok":true},"error_code":null,"error_info":null}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func newAuthedGateway(t *testing.T, token string, rl *RateLimiter) *fiber.App {
	t.Helper()
	up := stubUpstream(t)
	srv := httpx.New(httpx.Config{ListenAddr: ":0"})
	RegisterRoutes(srv.App(), &Deps{
		Proxy:       NewProxy(up.URL, 5),
		RateLimiter: rl,
		Token:       token,
	})
	return srv.App()
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body is not JSON: %v (body: %s)", err, body)
	}
	return m
}

func TestAuthMissingTokenRejected(t *testing.T) {
	app := newAuthedGateway(t, "secret", nil)
	req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if m := decodeBody(t, resp); m["error_code"] != "ERR-CONDUIT-AUTH" {
		t.Errorf("error_code = %v, want ERR-CONDUIT-AUTH", m["error_code"])
	}
}

func TestAuthWrongTokenRejected(t *testing.T) {
	app := newAuthedGateway(t, "secret", nil)
	req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
	req.Header.Set(auth.HeaderName, "wrong")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthHeaderTokenAccepted(t *testing.T) {
	app := newAuthedGateway(t, "secret", nil)
	req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
	req.Header.Set(auth.HeaderName, "secret")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (relayed upstream)", resp.StatusCode)
	}
	if m := decodeBody(t, resp); m["result"] == nil {
		t.Errorf("a relayed success should carry result, got %v", m)
	}
}

func TestAuthQueryParamTokenAccepted(t *testing.T) {
	app := newAuthedGateway(t, "secret", nil)
	req := httptest.NewRequest("POST", "/api/conduit.ping?token=secret", strings.NewReader("{}"))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for ?token= fallback", resp.StatusCode)
	}
}

func TestAuthEmptyTokenDisablesCheck(t *testing.T) {
	app := newAuthedGateway(t, "", nil)
	req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200: an empty token disables auth", resp.StatusCode)
	}
}

func TestMissingMethodIsConduitCore(t *testing.T) {
	app := newAuthedGateway(t, "secret", nil)
	for _, p := range []string{"/api", "/api/"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set(auth.HeaderName, "secret")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", p, resp.StatusCode)
		}
		if m := decodeBody(t, resp); m["error_code"] != "ERR-CONDUIT-CORE" {
			t.Errorf("%s: error_code = %v, want ERR-CONDUIT-CORE", p, m["error_code"])
		}
	}
}

func TestRateLimitMiddlewareEngagesWhenMounted(t *testing.T) {
	rl := NewRateLimiter(1, 1, nil)
	defer rl.Stop()
	app := newAuthedGateway(t, "secret", rl)

	do := func() int {
		req := httptest.NewRequest("POST", "/api/differential.query", strings.NewReader("{}"))
		req.Header.Set(auth.HeaderName, "secret")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	// First request from this IP passes (burst 1), the second is limited.
	if got := do(); got != http.StatusOK {
		t.Errorf("first request status = %d, want 200", got)
	}
	if got := do(); got != http.StatusTooManyRequests {
		t.Errorf("second request status = %d, want 429", got)
	}
}

func TestRateLimitExemptMethodBypassesLimiter(t *testing.T) {
	rl := NewRateLimiter(1, 1, []string{"conduit.ping"})
	defer rl.Stop()
	app := newAuthedGateway(t, "secret", rl)

	// conduit.ping is exempt, so repeated calls all pass.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/api/conduit.ping", strings.NewReader("{}"))
		req.Header.Set(auth.HeaderName, "secret")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("exempt method call %d status = %d, want 200", i, resp.StatusCode)
		}
	}
}
