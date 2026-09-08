package conduit

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// fixtureDir holds the language-neutral contract fixtures. They live at the
// repository root rather than under go/ so a PHP runner can read the same
// files; see tests/contract/conduit/README.md for what each one pins.
const fixtureDir = "../../../tests/contract/conduit"

// TestContractFixtures replays the conduit fixtures.
//
// It does not call contracttest.Run, which the other domains use, because that
// runner drives every fixture against one shared app and asserts the platform
// {data, error} envelope's dotted paths. The gateway needs neither: two of its
// fixtures require per-fixture configuration the request cannot carry — a stub
// upstream for the pass-through case and a limiter tuned to refuse for the
// rate-limited case — and its wire shape is the flat Conduit envelope
// {result, error_code, error_info}, whose keys are not dotted. So this test
// builds the right app per fixture and asserts the subset of the shared
// contracttest.Fixture vocabulary the conduit fixtures use (status, jsonHas,
// jsonEquals, jsonAbsent, bodyContains). The fixture files themselves stay in
// the shared schema so a PHP runner can replay them; only the driver differs.
func TestContractFixtures(t *testing.T) {
	// A stub upstream for the pass-through fixture. It answers a fixed Conduit
	// success body, which the gateway must relay unwrapped.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"result":{"pong":true},"error_code":null,"error_info":null}`)
	}))
	defer upstream.Close()

	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures found in %s", fixtureDir)
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fx contracttest.Fixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatalf("invalid fixture: %v", err)
			}
			if fx.Name == "" || fx.Request.Method == "" || fx.Request.Path == "" {
				t.Fatal("fixture must set name, request.method and request.path")
			}

			app := appForFixture(t, filepath.Base(path), upstream.URL)

			req := httptest.NewRequest(fx.Request.Method, fx.Request.Path,
				strings.NewReader(fx.Request.Body))
			for k, v := range fx.Request.Headers {
				req.Header.Set(k, v)
			}
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
			if err != nil {
				t.Fatalf("%s: app.Test failed: %v", fx.Name, err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("%s: reading response body: %v", fx.Name, err)
			}
			_ = resp.Body.Close()

			checkConduit(t, &fx, resp, body)
		})
	}
}

// appForFixture builds a gateway app configured for one fixture. The
// rate-limited fixture gets a limiter tuned to refuse its first request; every
// other fixture runs with the limiter off. All fixtures point the proxy at the
// stub upstream so the pass-through case can relay a real response.
func appForFixture(t *testing.T, fixture, upstreamURL string) *fiber.App {
	t.Helper()
	srv := httpx.New(httpx.Config{ListenAddr: ":0"})
	deps := &Deps{
		Proxy: NewProxy(upstreamURL, 5),
		Token: contracttest.Token,
	}
	if fixture == "rate-limited.json" {
		// RPS 1, burst 1: the bucket holds a single token, which the fixture's
		// own request consumes only if it were the first — but the limiter
		// starts a new visitor with a full burst, so a burst of 1 means one
		// request passes and the second is refused. Pre-warm by consuming the
		// token here so the fixture's request is the one that trips it.
		rl := NewRateLimiter(1, 1, nil)
		t.Cleanup(rl.Stop)
		// Consume the single token for the loopback IP app.Test uses (0.0.0.0),
		// so the fixture's request finds an empty bucket. app.Test sets the
		// remote address to 0.0.0.0; both requests share it.
		rl.Allow("0.0.0.0", "differential.query")
		deps.RateLimiter = rl
	}
	RegisterRoutes(srv.App(), deps)
	return srv.App()
}

// checkConduit asserts the subset of the shared fixture vocabulary the conduit
// fixtures use. The Conduit envelope has no dotted keys, so a flat map lookup
// is enough; JSONHas treats a null value as absent, matching the shared runner.
func checkConduit(t *testing.T, fx *contracttest.Fixture, resp *http.Response, body []byte) {
	t.Helper()

	if resp.StatusCode != fx.Expect.Status {
		t.Errorf("%s: expected status %d, got %d (body: %s)",
			fx.Name, fx.Expect.Status, resp.StatusCode, string(body))
	}

	rawBody := string(body)
	for _, want := range fx.Expect.BodyContains {
		if !strings.Contains(rawBody, want) {
			t.Errorf("%s: response body should contain %q (body: %s)", fx.Name, want, rawBody)
		}
	}
	for _, unwanted := range fx.Expect.BodyNotContains {
		if strings.Contains(rawBody, unwanted) {
			t.Errorf("%s: response body should not contain %q", fx.Name, unwanted)
		}
	}

	needsJSON := len(fx.Expect.JSONHas) > 0 || len(fx.Expect.JSONAbsent) > 0 ||
		len(fx.Expect.JSONEquals) > 0
	if !needsJSON {
		return
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("%s: response is not a JSON object: %v", fx.Name, err)
	}
	for _, key := range fx.Expect.JSONHas {
		v, ok := decoded[key]
		if !ok || v == nil {
			t.Errorf("%s: expected %s to be present and non-null", fx.Name, key)
		}
	}
	for _, key := range fx.Expect.JSONAbsent {
		if _, ok := decoded[key]; ok {
			t.Errorf("%s: expected %s to be absent", fx.Name, key)
		}
	}
	for key, want := range fx.Expect.JSONEquals {
		got, ok := decoded[key]
		if !ok {
			t.Errorf("%s: expected %s to be present", fx.Name, key)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: expected %s to be %q, got %q", fx.Name, key,
				fmt.Sprint(want), fmt.Sprint(got))
		}
	}
}
