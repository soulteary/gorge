package gitea

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestConduitSinkSkipsDeliveredEvent(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("X-Service-Token") != "gateway-token" {
			t.Error("missing gateway token")
		}
		if !strings.HasSuffix(r.URL.Path, "/api/transaction.search") {
			t.Errorf("unexpected method path %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(r.Form.Get("params")), &params); err != nil {
			t.Fatal(err)
		}
		meta, _ := params["__conduit__"].(map[string]any)
		if meta["token"] != "api-token" {
			t.Error("missing Conduit API token")
		}
		_, _ = w.Write([]byte(`{"result":{"data":[{"comments":[{"content":{"raw":"Gitea-Delivery: delivery-1"}}]}]},"error_code":null,"error_info":null}`))
	}))
	defer server.Close()

	sink := NewConduitSink(server.URL, "api-token", "gateway-token", server.Client())
	posted, err := sink.Append(context.Background(), "T1", Event{DeliveryID: "delivery-1"})
	if err != nil {
		t.Fatal(err)
	}
	if posted || requests != 1 {
		t.Fatalf("posted=%v requests=%d", posted, requests)
	}
}

func TestConduitSinkWritesFormEncodedComment(t *testing.T) {
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.URL.Path)
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		if len(methods) == 1 {
			_, _ = w.Write([]byte(`{"result":{"data":[]},"error_code":null,"error_info":null}`))
			return
		}
		body, _ := url.ParseQuery(readRequestBody(t, r))
		if !strings.Contains(body.Get("params"), "Gitea-Delivery: delivery-2") {
			t.Error("comment does not carry delivery marker")
		}
		_, _ = w.Write([]byte(`{"result":{"object":{"id":1}},"error_code":null,"error_info":null}`))
	}))
	defer server.Close()

	sink := NewConduitSink(server.URL, "api-token", "", server.Client())
	posted, err := sink.Append(context.Background(), "T1", Event{DeliveryID: "delivery-2", Kind: "push"})
	if err != nil || !posted {
		t.Fatalf("posted=%v err=%v", posted, err)
	}
	if len(methods) != 2 || !strings.HasSuffix(methods[1], "/api/maniphest.edit") {
		t.Fatalf("methods = %#v", methods)
	}
}

func readRequestBody(t *testing.T, r *http.Request) string {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return r.Form.Encode()
}
