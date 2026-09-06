// Package contracttest replays the language-neutral contract fixtures against
// a running handler.
//
// The fixtures live in tests/contract/<domain>/ at the repository root rather
// than under go/, because they describe the wire contract for two runners: this
// one and the PHP adapter that will read the same files. That is also why this
// is an ordinary package and not a _test.go file — each domain contributes a
// one-function test that points it at its own fixture directory, and the
// assertion vocabulary has to stay identical between them or the two domains
// will drift into describing their contracts differently.
//
// See tests/contract/README.md for the fixture format.
package contracttest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Token is the service token every fixture authenticates with. A runner must
// start the handler with exactly this value: the fixtures send it, and at
// least one of them asserts that a request without it is rejected.
const Token = "contract-token"

// Fixture is one recorded request and the expectations for its response.
type Fixture struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Request     struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Expect struct {
		Status     int            `json:"status"`
		JSONHas    []string       `json:"jsonHas"`
		JSONAbsent []string       `json:"jsonAbsent"`
		JSONEquals map[string]any `json:"jsonEquals"`
		// JSONStringContains asserts substrings inside a decoded string
		// value. Use it for a payload that is text rather than markup, such
		// as a unified diff, where a whole-value comparison through
		// JSONEquals would be too brittle to read in a failure message.
		JSONStringContains map[string][]string `json:"jsonStringContains"`
		// The html* assertions apply to data.html and only the render domain
		// uses them, but they stay in the shared vocabulary so both domains
		// describe their contracts with one set of names.
		HTMLContainsClasses []string `json:"htmlContainsClasses"`
		HTMLContains        []string `json:"htmlContains"`
		HTMLNotContains     []string `json:"htmlNotContains"`
		BodyContains        []string `json:"bodyContains"`
		BodyNotContains     []string `json:"bodyNotContains"`
	} `json:"expect"`
}

// Run replays every fixture in dir against handler, one subtest each.
func Run(t *testing.T, handler http.Handler, dir string) {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures found in %s", dir)
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fx Fixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatalf("invalid fixture: %v", err)
			}
			if fx.Name == "" || fx.Request.Method == "" || fx.Request.Path == "" {
				t.Fatal("fixture must set name, request.method and request.path")
			}

			req := httptest.NewRequest(fx.Request.Method, fx.Request.Path,
				strings.NewReader(fx.Request.Body))
			for k, v := range fx.Request.Headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			check(t, &fx, rec)
		})
	}
}

func check(t *testing.T, fx *Fixture, rec *httptest.ResponseRecorder) {
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
			t.Errorf("%s: expected %s to be %q, got %q", fx.Name, path,
				fmt.Sprint(want), fmt.Sprint(got))
		}
	}
	for path, wants := range fx.Expect.JSONStringContains {
		got, ok := lookupJSONPath(decoded, path)
		if !ok {
			t.Errorf("%s: expected %s to be present", fx.Name, path)
			continue
		}
		s, isStr := got.(string)
		if !isStr {
			t.Errorf("%s: %s is not a string", fx.Name, path)
			continue
		}
		for _, want := range wants {
			if !strings.Contains(s, want) {
				t.Errorf("%s: expected %s to contain %q, got %q", fx.Name, path, want, s)
			}
		}
	}

	checkHTML(t, fx, decoded)
}

func checkHTML(t *testing.T, fx *Fixture, decoded map[string]any) {
	t.Helper()

	if len(fx.Expect.HTMLContainsClasses) == 0 &&
		len(fx.Expect.HTMLContains) == 0 && len(fx.Expect.HTMLNotContains) == 0 {
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

// lookupJSONPath walks a dot-separated path through a decoded JSON document.
// A numeric segment indexes an array, so "data.parts.0.type" reaches into the
// segment lists the diff domain returns.
func lookupJSONPath(root map[string]any, path string) (any, bool) {
	var current any = root
	for _, segment := range strings.Split(path, ".") {
		switch container := current.(type) {
		case map[string]any:
			value, ok := container[segment]
			if !ok {
				return nil, false
			}
			current = value
		case []any:
			i, err := strconv.Atoi(segment)
			if err != nil || i < 0 || i >= len(container) {
				return nil, false
			}
			current = container[i]
		default:
			return nil, false
		}
	}
	return current, true
}
