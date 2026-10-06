package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExecutionReadinessNegotiatesWithoutBusinessWork(t *testing.T) {
	for _, response := range []string{
		`{"result":{"executionVersion":1,"result":"capabilities"},"error_code":null}`,
		`{"result":{"executionVersion":0,"result":"capabilities"},"error_code":null}`,
		`{"result":{"executionVersion":1,"result":"success"},"error_code":null}`,
		`{"result":null,"error_code":"ERR-WORKER-AUTH","error_info":"invalid token"}`,
	} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/worker.execute" || r.Header.Get("X-Service-Token") != "test-token" {
					t.Error("wrong execution probe or service token")
				}
				_ = r.ParseForm()
				var params map[string]any
				_ = json.Unmarshal([]byte(r.FormValue("params")), &params)
				if params["phase"] != "capabilities" || params["taskClass"] != "" || params["taskID"] != float64(0) {
					t.Error("startup probe must not prepare a business task")
				}
				if _, err := fmt.Fprint(w, response); err != nil {
					t.Errorf("write capabilities response: %v", err)
				}
			}))
			defer server.Close()
			err := ValidateExecutionService(context.Background(), server.URL, "test-token")
			wantOK := response == `{"result":{"executionVersion":1,"result":"capabilities"},"error_code":null}`
			if (err == nil) != wantOK {
				t.Fatalf("readiness=%v, wantOK=%v", err, wantOK)
			}
		})
	}
}

func TestExecutionReadinessDoesNotFollowRedirects(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
		if _, err := fmt.Fprint(w, `{"result":{"executionVersion":1,"result":"capabilities"}}`); err != nil {
			t.Errorf("write capabilities response: %v", err)
		}
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if ValidateExecutionService(context.Background(), redirect.URL, "test-token") == nil || followed {
		t.Fatal("redirected capabilities must not become ready or forward service credentials")
	}
}
