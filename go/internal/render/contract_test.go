package render

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/render/highlight"
)

// fixtureDir holds the language-neutral contract fixtures. They live at the
// repository root rather than under go/ so a PHP runner can read the same
// files; see tests/contract/render/README.md for the format.
const fixtureDir = "../../../tests/contract/render"

// contractToken is the token the fixtures authenticate with.
const contractToken = "contract-token"

type contractFixture struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Request     struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Expect struct {
		Status              int            `json:"status"`
		JSONHas             []string       `json:"jsonHas"`
		JSONAbsent          []string       `json:"jsonAbsent"`
		JSONEquals          map[string]any `json:"jsonEquals"`
		HTMLContainsClasses []string       `json:"htmlContainsClasses"`
		HTMLContains        []string       `json:"htmlContains"`
		HTMLNotContains     []string       `json:"htmlNotContains"`
		BodyContains        []string       `json:"bodyContains"`
		BodyNotContains     []string       `json:"bodyNotContains"`
	} `json:"expect"`
}

func TestContractFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures found in %s", fixtureDir)
	}

	srv := httpx.New(httpx.Config{ListenAddr: ":0"})
	RegisterRoutes(srv.Echo(), &Deps{
		Highlighter: highlight.New(),
		Token:       contractToken,
		MaxBytes:    DefaultMaxBytes,
	})

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fx contractFixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatalf("invalid fixture: %v", err)
			}
			if fx.Name == "" || fx.Request.Method == "" || fx.Request.Path == "" {
				t.Fatal("fixture must set name, request.method and request.path")
			}

			var body *strings.Reader
			if fx.Request.Body != "" {
				body = strings.NewReader(fx.Request.Body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(fx.Request.Method, fx.Request.Path, body)
			for k, v := range fx.Request.Headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			srv.Echo().ServeHTTP(rec, req)

			checkFixture(t, &fx, rec)
		})
	}
}

func checkFixture(t *testing.T, fx *contractFixture, rec *httptest.ResponseRecorder) {
	t.Helper()

	if rec.Code != fx.Expect.Status {
		t.Errorf("%s: expected status %d, got %d (body: %s)",
			fx.Name, fx.Expect.Status, rec.Code, rec.Body.String())
	}

	rawBody := rec.Body.String()
	for _, want := range fx.Expect.BodyContains {
		if !strings.Contains(rawBody, want) {
			t.Errorf("%s: response body should contain %q", fx.Name, want)
		}
	}
	for _, unwanted := range fx.Expect.BodyNotContains {
		if strings.Contains(rawBody, unwanted) {
			t.Errorf("%s: response body should not contain %q", fx.Name, unwanted)
		}
	}

	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("%s: response is not a JSON object: %v", fx.Name, err)
	}

	for _, path := range fx.Expect.JSONHas {
		v, ok := lookupJSONPath(decoded, path)
		if !ok || v == nil {
			t.Errorf("%s: expected %s to be present", fx.Name, path)
			continue
		}
		if s, isStr := v.(string); isStr && s == "" {
			t.Errorf("%s: expected %s to be non-empty", fx.Name, path)
		}
	}
	for _, path := range fx.Expect.JSONAbsent {
		if _, ok := lookupJSONPath(decoded, path); ok {
			t.Errorf("%s: expected %s to be absent", fx.Name, path)
		}
	}
	for path, want := range fx.Expect.JSONEquals {
		got, ok := lookupJSONPath(decoded, path)
		if !ok {
			t.Errorf("%s: expected %s to be present", fx.Name, path)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: expected %s to be %v, got %v", fx.Name, path, want, got)
		}
	}

	needsHTML := len(fx.Expect.HTMLContainsClasses) > 0 ||
		len(fx.Expect.HTMLContains) > 0 || len(fx.Expect.HTMLNotContains) > 0
	if !needsHTML {
		return
	}

	htmlValue, ok := lookupJSONPath(decoded, "data.html")
	if !ok {
		t.Fatalf("%s: expected data.html for the html assertions", fx.Name)
	}
	html, ok := htmlValue.(string)
	if !ok {
		t.Fatalf("%s: data.html is not a string", fx.Name)
	}

	// Assert on class names rather than the exact markup: Chroma's output
	// shifts between releases, but the Pygments class names are the part
	// Phorge's stylesheet depends on and must not shift.
	for _, class := range fx.Expect.HTMLContainsClasses {
		if !strings.Contains(html, `class="`+class+`"`) {
			t.Errorf("%s: expected the Pygments class %q in the output", fx.Name, class)
		}
	}
	for _, want := range fx.Expect.HTMLContains {
		if !strings.Contains(html, want) {
			t.Errorf("%s: expected html to contain %q", fx.Name, want)
		}
	}
	for _, unwanted := range fx.Expect.HTMLNotContains {
		if strings.Contains(html, unwanted) {
			t.Errorf("%s: expected html not to contain %q", fx.Name, unwanted)
		}
	}
}

// lookupJSONPath walks a dot-separated path through a decoded JSON object.
func lookupJSONPath(root map[string]any, path string) (any, bool) {
	var current any = root
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
