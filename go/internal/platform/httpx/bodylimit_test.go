package httpx

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// TestOKWritesTheEnvelope pins httpx.OK directly. Every domain answers success
// through it, but those calls live in other packages and do not count toward
// this package's coverage, so the success envelope is exercised here where it
// is defined: status 200, the payload under "data", and no error object.
func TestOKWritesTheEnvelope(t *testing.T) {
	app := fiber.New()
	app.Get("/ok", func(c fiber.Ctx) error {
		return OK(c, map[string]string{"hello": "world"})
	})

	req, err := http.NewRequest(http.MethodGet, "/ok", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body struct {
		Data  map[string]string `json:"data"`
		Error *Error            `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Error != nil {
		t.Errorf("success response must not carry an error: %+v", body.Error)
	}
	if body.Data["hello"] != "world" {
		t.Errorf("data not carried through the envelope: %+v", body.Data)
	}
}

// TestParseBodyLimit covers the human-readable size parser that replaced echo's
// body-limit middleware. It must accept the same spellings the existing config
// strings use and fall back to the default rather than failing the boot on a
// value it cannot read.
func TestParseBodyLimit(t *testing.T) {
	defaultBytes := mustParseBodyLimit(defaultBodyLimit)

	cases := []struct {
		in   string
		want int
	}{
		{"", defaultBytes},           // empty falls back to the default
		{"512", 512},                 // a bare byte count
		{"1B", 1},                    // explicit bytes
		{"1K", 1 << 10},              // kibibytes, short and long spelling
		{"1KB", 1 << 10},             //
		{"2M", 2 << 20},              // mebibytes
		{"2MB", 2 << 20},             //
		{"1G", 1 << 30},              // gibibytes
		{"1GB", 1 << 30},             //
		{"  1M  ", 1 << 20},          // surrounding space is ignored
		{"not-a-size", defaultBytes}, // an unparseable value falls back
	}
	for _, tc := range cases {
		if got := parseBodyLimit(tc.in); got != tc.want {
			t.Errorf("parseBodyLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestParseSizeRejectsGarbage covers the error path of the parser directly: a
// value with no numeric part is an error, not a silent zero.
func TestParseSizeRejectsGarbage(t *testing.T) {
	if _, err := parseSize("abcM"); err == nil {
		t.Fatal("expected an error for a non-numeric size, got nil")
	}
}

// TestMustParseBodyLimitPanicsOnGarbage covers the must-variant's panic path,
// which guards the compiled-in default and so should never be reached with a
// bad value in production, but is asserted here to keep the contract explicit.
func TestMustParseBodyLimitPanicsOnGarbage(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a non-numeric size, got none")
		}
	}()
	mustParseBodyLimit("abcM")
}
