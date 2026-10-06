package cleanup

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestReadOnlyStatusAuth(t *testing.T) {
	app := fiber.New()
	RegisterRoutes(app, &Store{}, "secret")
	for _, path := range []string{"/api/maintenance/collectors", "/api/maintenance/metrics"} {
		for _, query := range []string{"", "?token=secret"} {
			req := httptest.NewRequest("GET", path+query, nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatalf("query/headerless auth accepted %d", resp.StatusCode)
			}
		}
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("X-Service-Token", "secret")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("authorized status rejected %d", resp.StatusCode)
		}
		if path == "/api/maintenance/collectors" && !strings.Contains(string(body), `"data":[]`) {
			t.Fatalf("missing platform envelope %s", body)
		}
	}
	req := httptest.NewRequest("POST", "/api/maintenance/collectors", nil)
	req.Header.Set("X-Service-Token", "secret")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 && resp.StatusCode != 404 {
		t.Fatal("status API accepted mutation")
	}
}
