package mailer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func TestNativeMailRoutesRequireAuthentication(t *testing.T) {
	app := httpx.New(httpx.Config{}).App()
	RegisterRoutes(app, &Deps{Token: "internal", Delivery: &DeliveryService{}})
	for _, route := range []string{"prepare", "execute", "deliver", "cancel"} {
		req := httptest.NewRequest(http.MethodPost, "/api/mailer/"+route, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		response, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatalf("%s unauthenticated: %d", route, response.StatusCode)
		}
	}
}
