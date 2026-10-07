package httpx

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestRedactURIPreservesNonsecretQuery(t *testing.T) {
	cases := map[string]string{
		"/path?x=a%2Fb&token=first&token=second&offset=1": "/path?x=a%2Fb&token=[REDACTED]&token=[REDACTED]&offset=1",
		"/path?ToKeN=secret&api%5Fkey=secret2":            "/path?ToKeN=[REDACTED]&api%5Fkey=[REDACTED]",
		"/path?token=%invalid;password=secret&flag":       "/path?token=[REDACTED]&flag",
		"/path?token=prefix;suffix&offset=1":              "/path?token=[REDACTED]&offset=1",
		"/path?token&access_token=x&normal=":              "/path?token=[REDACTED]&access_token=[REDACTED]&normal=",
		"/path?offset=5&":                                 "/path?offset=5&",
		"/path":                                           "/path",
	}
	for raw, want := range cases {
		if got, _ := redactURI(raw); got != want {
			t.Errorf("redactURI(%q)=%q want %q", raw, got, want)
		}
	}
}

func TestCredentialValuesAbsentFromAccessErrorAndPanicLogs(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)
	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	srv := New(Config{})
	srv.App().Get("/success", func(c fiber.Ctx) error { return OK(c, "accepted") })
	srv.App().Get("/error", func(c fiber.Ctx) error {
		return errors.New("failed at " + c.OriginalURL() + " decoded=" + c.Query("token"))
	})
	srv.App().Get("/panic", func(c fiber.Ctx) error { panic(c.OriginalURL() + " decoded=" + c.Query("token")) })
	for _, path := range []string{"/success", "/error", "/panic", "/missing"} {
		resp, err := srv.App().Test(httptest.NewRequest("GET", path+"?token=private%2Fcredential;private-suffix&offset=7", nil), fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	logs := output.String()
	for _, secret := range []string{"private%2Fcredential", "private/credential", "private-suffix"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("credential appeared in logs: %s", logs)
		}
	}
	for _, marker := range []string{"REQUEST", "REQUEST_FAILED", "PANIC_RECOVERED", "[REDACTED]", "offset=7"} {
		if !strings.Contains(logs, marker) {
			t.Fatalf("missing diagnostic %q: %s", marker, logs)
		}
	}
}
