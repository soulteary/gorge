package conduitclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransportDeadlineAndWire(t *testing.T) {
	if New("http://localhost", "token").httpClient.Timeout != 0 {
		t.Fatal("fixed timeout truncates caller lease")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search.source" || r.Header.Get("X-Service-Token") != "token" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("wire headers")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(r.Form.Get("params")), &p); err != nil {
			t.Error(err)
		}
		if p["afterID"] != "9007199254740993" || p["__conduit__"].(map[string]any)["token"] != "token" || r.Form.Get("output") != "json" {
			t.Error("wire params")
		}
		_, _ = w.Write([]byte(`{"result":{},"error_code":null,"error_info":null}`))
	}))
	defer srv.Close()
	client := NewBounded(srv.URL, "token", 1024)
	if _, err := client.Call(context.Background(), "search.source", map[string]any{"afterID": "9007199254740993"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Call(ctx, "search.source", nil); err == nil {
		t.Fatal("cancellation ignored")
	}
}
func TestBoundedTransportRejectsStatusSizeAndRedirect(t *testing.T) {
	for _, mode := range []string{"status", "size", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			followed := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/secret" {
					followed = true
				}
				switch mode {
				case "status":
					w.WriteHeader(503)
				case "redirect":
					w.Header().Set("Location", "/secret")
					w.WriteHeader(307)
				case "size":
					_, _ = w.Write([]byte(strings.Repeat("x", 1025)))
					return
				}
				_, _ = w.Write([]byte(`{"result":{},"error_code":null,"error_info":null}`))
			}))
			defer srv.Close()
			if _, err := NewBounded(srv.URL, "token", 1024).Call(context.Background(), "search.source", nil); err == nil {
				t.Fatal("response accepted")
			}
			if followed {
				t.Fatal("service credentials followed redirect")
			}
		})
	}
}

func TestBoundedTransportDoesNotExposeResponseInErrors(t *testing.T) {
	for _, body := range []string{
		`<html>private-response-marker</html>`,
		`{"error_code":"ERR-PRIVATE","error_info":"private-response-marker"}`,
		`{"error_code":"private-response-marker","error_info":null}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		_, err := NewBounded(srv.URL, "token", 1024).Call(t.Context(), "trigger.plan", nil)
		if err == nil || strings.Contains(err.Error(), "private-response-marker") {
			t.Fatalf("response exposed or accepted: %v", err)
		}
		// Keep the existing worker transport's diagnostics compatible.
		_, legacyErr := New(srv.URL, "token").Call(t.Context(), "trigger.plan", nil)
		if legacyErr == nil || !strings.Contains(legacyErr.Error(), "private-response-marker") {
			t.Fatalf("legacy diagnostics changed: %v", legacyErr)
		}
		srv.Close()
	}
}
