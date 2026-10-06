package filestorage

import (
	"bytes"
	"github.com/gofiber/fiber/v3"
	"io"
	"net/http/httptest"
	"testing"
)

func TestUploadHTTPBoundary(t *testing.T) {
	u, _ := NewUploads(t.TempDir())
	app := fiber.New()
	registerUploadRoutes(app, u, "secret")
	id := "dddddddddddddddddddddddddddddddd"
	cases := []struct {
		method, path, body, token string
		status                    int
	}{
		{"GET", "/api/file/uploads/meta?token=secret", "", "", 401},
		{"POST", "/api/file/uploads/session", `{"id":"../bad","size":3}`, "secret", 400},
		{"POST", "/api/file/uploads/session", `{"id":"` + id + `","size":3}`, "secret", 200},
		{"POST", "/api/file/uploads/" + id + "/complete", "{}", "secret", 409},
		{"POST", "/api/file/uploads/" + id + "/verify", "{}", "secret", 409},
		{"PUT", "/api/file/uploads/" + id + "/chunk?start=1", "abc", "secret", 409},
		{"PUT", "/api/file/uploads/" + id + "/chunk?start=0", "ab", "secret", 409},
		{"PUT", "/api/file/uploads/" + id + "/chunk?start=0", "abc", "secret", 200},
		{"POST", "/api/file/uploads/" + id + "/complete", "{}", "secret", 200},
		{"POST", "/api/file/uploads/" + id + "/verify", "{}", "secret", 200},
		{"GET", "/api/file/uploads/" + id + "/data?start=0&end=3", "", "secret", 200},
		{"DELETE", "/api/file/uploads/" + id, "", "secret", 200},
		{"GET", "/api/file/uploads/" + id, "", "secret", 410},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, bytes.NewBufferString(c.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Token", c.token)
		res, e := app.Test(req)
		if e != nil {
			t.Fatal(e)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != c.status {
			t.Fatalf("%s %s: got %d %s", c.method, c.path, res.StatusCode, body)
		}
	}
}
