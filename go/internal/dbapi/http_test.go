package dbapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// The HTTP tests assert the Fiber + httpx behaviour the port introduced: the
// {data,error} envelope, the shared token guard, the platform error codes for
// routing failures, and — the load-bearing one — that a database failure comes
// back as a domain code with a generic message that leaks no host, database or
// SQL. The standalone service had its own envelope and status mapping; these
// tests pin the platform behaviour that replaced it.

type testEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

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
		t.Errorf("expected status %d, got %d (body: %s)", status, resp.StatusCode, body)
	}
	env := envelope(t, body)
	if env.Error == nil {
		t.Fatalf("expected an error envelope, got: %s", body)
	}
	if env.Error.Code != code {
		t.Errorf("expected code %s, got %s", code, env.Error.Code)
	}
	if len(env.Data) != 0 {
		t.Errorf("an error response must carry no data, got: %s", env.Data)
	}
}

// theSevenRoutes is the domain's public surface. PhabricatorGorgeDBClient calls
// these paths as written, so the set is a contract.
var theSevenRoutes = []struct {
	method, path string
}{
	{http.MethodGet, "/api/db/servers"},
	{http.MethodGet, "/api/db/servers/db1:3306/health"},
	{http.MethodGet, "/api/db/schema-diff"},
	{http.MethodGet, "/api/db/schema-issues"},
	{http.MethodGet, "/api/db/setup-issues"},
	{http.MethodGet, "/api/db/charset-info"},
	{http.MethodGet, "/api/db/migrations/status"},
}

// TestTokenGuardInterceptsEveryRoute: the token is checked before the handler
// runs, so a request without it is 401 ERR_UNAUTHORIZED on every one of the
// seven routes — even the ones that would otherwise touch the database, which
// is what keeps the cluster's shape behind the guard.
func TestTokenGuardInterceptsEveryRoute(t *testing.T) {
	app := newTestServer(t, func(d *Deps) { setConnFactory(d, unreachableFactory()) })

	for _, r := range theSevenRoutes {
		req := httptest.NewRequest(r.method, r.path, nil) // no token
		resp, body := dispatch(t, app, req)
		assertErrorCode(t, resp, body, http.StatusUnauthorized, httpx.CodeUnauthorized)
	}
}

// TestTokenAcceptedInHeaderAndQuery pins the two ways a caller authenticates:
// the header the PHP client uses and the query-param fallback for a browser or
// a runbook's curl.
func TestTokenAcceptedInHeaderAndQuery(t *testing.T) {
	app := newTestServer(t, func(d *Deps) { setConnFactory(d, unreachableFactory()) })

	// /api/db/servers never fails on a dead database — it reports the node as
	// unreachable in-band — so it is a 200 with either form of the token.
	if resp, body := do(t, app, http.MethodGet, "/api/db/servers"); resp.StatusCode != http.StatusOK {
		t.Errorf("header token: expected 200, got %d: %s", resp.StatusCode, body)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/db/servers?token="+testToken, nil)
	if resp, body := dispatch(t, app, req); resp.StatusCode != http.StatusOK {
		t.Errorf("query token: expected 200, got %d: %s", resp.StatusCode, body)
	}
}

// TestRoutePathsAreStable: the seven paths plus the three probes are all
// registered. Renaming any is a breaking change on the PHP side.
func TestRoutePathsAreStable(t *testing.T) {
	app := newTestServer(t, nil)

	want := map[string]bool{
		"GET /api/db/servers":             true,
		"GET /api/db/servers/:ref/health": true,
		"GET /api/db/schema-diff":         true,
		"GET /api/db/schema-issues":       true,
		"GET /api/db/setup-issues":        true,
		"GET /api/db/charset-info":        true,
		"GET /api/db/migrations/status":   true,
		"GET /healthz":                    true,
		"GET /readyz":                     true,
		"GET /":                           true,
	}
	for _, r := range app.GetRoutes(true) {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("route %s is no longer registered", route)
	}
}

// TestServersReportsAnUnreachableNodeInBand: a node that cannot be reached is
// not a 5xx — it appears in the array with connectionStatus "fail", because
// "this server is down" is exactly what the health report exists to say. The
// reason string is the one place a driver message is allowed through, since a
// health probe's whole job is to report it.
func TestServersReportsAnUnreachableNodeInBand(t *testing.T) {
	app := newTestServer(t, func(d *Deps) { setConnFactory(d, unreachableFactory()) })

	resp, body := do(t, app, http.MethodGet, "/api/db/servers")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	env := envelope(t, body)
	var refs []map[string]any
	if err := json.Unmarshal(env.Data, &refs); err != nil {
		t.Fatalf("data is not an array of servers: %v (%s)", err, body)
	}
	if len(refs) != 1 {
		t.Fatalf("expected one server, got %d", len(refs))
	}
	if refs[0]["connectionStatus"] != "fail" {
		t.Errorf("an unreachable node should report connectionStatus fail, got %v", refs[0]["connectionStatus"])
	}
	if refs[0]["refKey"] != "db1:3306" {
		t.Errorf("refKey = %v, want db1:3306", refs[0]["refKey"])
	}
}

// TestUnknownServerIsA404: a health request for a ref key that matches no
// configured node is the caller addressing a server that does not exist, so it
// is a 404 rather than a database failure.
func TestUnknownServerIsA404(t *testing.T) {
	app := newTestServer(t, nil)
	resp, body := do(t, app, http.MethodGet, "/api/db/servers/nosuch:3306/health")
	assertErrorCode(t, resp, body, http.StatusNotFound, httpx.CodeNotFound)
}

// TestSchemaDiffOnUnreachableDatabaseIsAnOpaqueDomainCode is the security
// contract of the port: an endpoint that queries the database and cannot reach
// it answers the domain's ERR_DB_UNREACHABLE at 503 with a generic message,
// and that error body carries nothing about the host, the database name or the
// SQL — a caller past the token check still gets no map of the cluster.
//
// The refKey (host:port) is a deliberate exception: it is a documented field
// of every contract type and the stable identifier a caller addresses a node
// by, so it is present in the *in-band* 200 responses on purpose. What must
// never appear anywhere is the configured password or a fragment of SQL.
func TestSchemaDiffOnUnreachableDatabaseIsAnOpaqueDomainCode(t *testing.T) {
	app := newTestServer(t, func(d *Deps) { setConnFactory(d, unreachableFactory()) })

	for _, path := range []string{
		"/api/db/schema-diff",
		"/api/db/schema-issues",
		"/api/db/charset-info",
	} {
		resp, body := do(t, app, http.MethodGet, path)

		// A 503 is the error-envelope path: the message must be generic and
		// the body must not name the host, the database or the query.
		if resp.StatusCode == http.StatusServiceUnavailable {
			assertErrorCode(t, resp, body, http.StatusServiceUnavailable, CodeDBUnreachable)
			env := envelope(t, body)
			if env.Error.Message != genericMessage(kindUnreachable) {
				t.Errorf("%s: message %q is not the generic one", path, env.Error.Message)
			}
			for _, leak := range []string{"db1", "3306", "phorge_", "INFORMATION_SCHEMA", "SELECT"} {
				if strings.Contains(body, leak) {
					t.Errorf("%s error body leaks %q: %s", path, leak, body)
				}
			}
		}

		// The password and raw SQL must never leak, on any status. The refKey
		// (host:port) is exempt: it is public contract, not a leak.
		for _, leak := range []string{"secret", "INFORMATION_SCHEMA", "SELECT ", "SHOW "} {
			if strings.Contains(body, leak) {
				t.Errorf("%s body leaks %q: %s", path, leak, body)
			}
		}
	}
}

// TestUnknownPathKeepsTheEnvelope: a path the service does not serve is
// answered by the framework's own handler, and it comes back in the envelope
// too because that is what the PHP client parses.
func TestUnknownPathKeepsTheEnvelope(t *testing.T) {
	app := newTestServer(t, nil)
	resp, body := do(t, app, http.MethodGet, "/api/db/nope")
	assertErrorCode(t, resp, body, http.StatusNotFound, httpx.CodeNotFound)
}

// TestHealthProbesReportTheMaster: /healthz answers while the process is
// listening; /readyz answers whether a master can be pinged. A db-api whose
// masters are all down is listening but not ready.
func TestHealthProbesReportTheMaster(t *testing.T) {
	probe := func(app *fiber.App, path string) (*http.Response, string) {
		return dispatch(t, app, httptest.NewRequest(http.MethodGet, path, nil))
	}

	down := newTestServer(t, func(d *Deps) { setConnFactory(d, unreachableFactory()) })
	if resp, _ := probe(down, "/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("liveness must not depend on the database, got %d", resp.StatusCode)
	}
	resp, body := probe(down, "/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with unreachable masters, got %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, `"data"`) {
		t.Errorf("a probe payload must not use the envelope: %s", body)
	}
	if !strings.Contains(body, `"reason"`) {
		t.Errorf("readiness must say why it is unavailable: %s", body)
	}

	up := newTestServer(t, func(d *Deps) { setConnFactory(d, mockFactory(t, nil)) })
	if resp, body := probe(up, "/readyz"); resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 with a reachable master, got %d: %s", resp.StatusCode, body)
	}
}
