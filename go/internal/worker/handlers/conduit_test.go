package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

// TestConduitCallWireFormat pins the on-the-wire contract Phorge's Conduit API
// requires: a form-encoded POST to /api/<method> carrying a single `params`
// field (JSON), Conduit metadata under __conduit__ with the token, and
// output=json. Sending application/json here is what produced the original
// HTML "invalid character '<'" failure, so this guards the fix.
func TestConduitCallWireFormat(t *testing.T) {
	var (
		gotPath        string
		gotContentType string
		gotToken       string
		gotParams      map[string]any
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotToken = r.Header.Get("X-Service-Token")

		if err := r.ParseForm(); err != nil {
			t.Errorf("upstream could not parse form body: %v", err)
		}
		if out := r.PostFormValue("output"); out != "json" {
			t.Errorf("output = %q, want json", out)
		}
		raw := r.PostFormValue("params")
		if raw == "" {
			t.Error("params field missing from form body")
		}
		if err := json.Unmarshal([]byte(raw), &gotParams); err != nil {
			t.Errorf("params was not JSON: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"result":{"ok":true},"error_code":null,"error_info":null}`)
	}))
	defer srv.Close()

	c := NewConduitClient(srv.URL, "svc-token")
	resp, err := c.Call(context.Background(), "worker.execute", map[string]any{
		"taskID":    42,
		"taskClass": "SomeWorker",
	})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}

	if gotPath != "/api/worker.execute" {
		t.Errorf("path = %q, want /api/worker.execute", gotPath)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", gotContentType)
	}
	if gotToken != "svc-token" {
		t.Errorf("X-Service-Token = %q, want svc-token", gotToken)
	}

	meta, ok := gotParams["__conduit__"].(map[string]any)
	if !ok {
		t.Fatalf("__conduit__ missing or wrong type: %#v", gotParams["__conduit__"])
	}
	if meta["token"] != "svc-token" {
		t.Errorf("__conduit__.token = %v, want svc-token", meta["token"])
	}
	if gotParams["taskClass"] != "SomeWorker" {
		t.Errorf("taskClass = %v, want SomeWorker", gotParams["taskClass"])
	}

	var got struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !got.OK {
		t.Error("expected result.ok = true")
	}
}

// TestConduitCallHTMLResponse verifies a misrouted call that returns HTML
// yields a bounded, diagnosable error instead of the bare
// "invalid character '<'" JSON decode error.
func TestConduitCallHTMLResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<!DOCTYPE html><html><body>Not Found</body></html>")
	}))
	defer srv.Close()

	c := NewConduitClient(srv.URL, "svc-token")
	_, err := c.Call(context.Background(), "worker.execute", nil)
	if err == nil {
		t.Fatal("expected an error for an HTML response")
	}
	if !strings.Contains(err.Error(), "non-JSON") {
		t.Errorf("error should describe a non-JSON response, got: %v", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Errorf("error should not surface raw decoder noise, got: %v", err)
	}
}

// TestConduitCallErrorEnvelope verifies a Conduit error envelope becomes a
// Go error carrying the code and info.
func TestConduitCallErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"result":null,"error_code":"ERR-CONDUIT-CORE","error_info":"boom"}`)
	}))
	defer srv.Close()

	c := NewConduitClient(srv.URL, "")
	_, err := c.Call(context.Background(), "worker.execute", nil)
	if err == nil {
		t.Fatal("expected an error for an error envelope")
	}
	if !strings.Contains(err.Error(), "ERR-CONDUIT-CORE") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry code and info, got: %v", err)
	}
}

// conduitResultServer returns a Conduit server that answers worker.execute with
// the given result envelope body.
func conduitResultServer(t *testing.T, resultJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"result":%s,"error_code":null,"error_info":null}`, resultJSON))
	}))
}

func runDelegate(t *testing.T, resultJSON string) error {
	t.Helper()
	srv := conduitResultServer(t, resultJSON)
	defer srv.Close()

	h := NewConduitDelegateHandler(NewConduitClient(srv.URL, "tok"))
	task := &contracts.Task{ID: 7, TaskClass: "SomeWorker"}
	return h(context.Background(), task, json.RawMessage(`{"k":"v"}`))
}

// TestDelegateSuccess: a "success" classification completes the task (nil).
func TestDelegateSuccess(t *testing.T) {
	if err := runDelegate(t, `{"result":"success","duration":12}`); err != nil {
		t.Fatalf("success should map to nil, got: %v", err)
	}
}

// TestDelegatePermanentFailure maps to *worker.PermanentError so the consumer
// fails the task permanently instead of retrying forever.
func TestDelegatePermanentFailure(t *testing.T) {
	err := runDelegate(t,
		`{"result":"permanent-failure","failureReason":"bad data"}`)
	var perm *worker.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("expected *worker.PermanentError, got: %v", err)
	}
	if !strings.Contains(perm.Error(), "bad data") {
		t.Errorf("permanent error should carry the reason, got: %v", perm)
	}
}

// TestDelegateYield maps to *worker.YieldError carrying the retry delay.
func TestDelegateYield(t *testing.T) {
	err := runDelegate(t, `{"result":"yield","retry":30}`)
	var y *worker.YieldError
	if !errors.As(err, &y) {
		t.Fatalf("expected *worker.YieldError, got: %v", err)
	}
	if y.Duration != 30 {
		t.Errorf("yield duration = %d, want 30", y.Duration)
	}
}

// TestDelegateTransientFailure: a "failure" classification is a plain error, so
// the consumer treats it as a transient failure and retries.
func TestDelegateTransientFailure(t *testing.T) {
	err := runDelegate(t, `{"result":"failure","failureReason":"flaky"}`)
	if err == nil {
		t.Fatal("expected an error for a transient failure")
	}
	var perm *worker.PermanentError
	var y *worker.YieldError
	if errors.As(err, &perm) || errors.As(err, &y) {
		t.Errorf("transient failure must not be permanent or yield, got: %v", err)
	}
}

// TestDelegateUnexpectedResult: an unknown classification is transient, never
// a silent success.
func TestDelegateUnexpectedResult(t *testing.T) {
	err := runDelegate(t, `{"result":"wat"}`)
	if err == nil {
		t.Fatal("expected an error for an unexpected classification")
	}
}

// TestDelegateHTMLIsTransient: the original bug — an HTML response — surfaces
// as a transient error (retryable), not a permanent failure or a false success.
func TestDelegateHTMLIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>login</body></html>")
	}))
	defer srv.Close()

	h := NewConduitDelegateHandler(NewConduitClient(srv.URL, "tok"))
	task := &contracts.Task{ID: 7, TaskClass: "SomeWorker"}
	err := h(context.Background(), task, nil)
	if err == nil {
		t.Fatal("expected an error for an HTML response")
	}
	var perm *worker.PermanentError
	if errors.As(err, &perm) {
		t.Errorf("HTML response should be transient, not permanent: %v", err)
	}
}
