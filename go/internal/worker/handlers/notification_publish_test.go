package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

func notificationPolicyFile(t *testing.T, mode string, endpoints ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	if endpoints == nil {
		endpoints = []string{}
	}
	raw, _ := json.Marshal(NotificationPolicy{Version: 1, Mode: mode, Instance: "tenant a", Endpoints: endpoints})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestNotificationNativeCompatibilityAndStableID(t *testing.T) {
	var ids []string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("instance") != "tenant a" {
			t.Error("instance not encoded")
		}
		var message map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&message)
		var id string
		json.Unmarshal(message["uniqueID"], &id)
		ids = append(ids, id)
		if string(message["extension"]) != "42" {
			t.Error("unknown field lost")
		}
		if _, ok := message["deliveryVersion"]; ok {
			t.Error("internal envelope leaked")
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	path := notificationPolicyFile(t, "required", server.URL)
	registry := worker.NewRegistry()
	RegisterAll(registry, "", "", nil)
	RegisterNotification(registry, path)
	h, ok := registry.Get("PhabricatorNotificationPublishWorker")
	if !ok {
		t.Fatal("native registration requires PHP")
	}
	old := json.RawMessage(`{"message":{"type":"notification","key":"123","subscribers":["PHID-USER-a"],"extension":42}}`)
	for range 2 {
		if err := h(t.Context(), &contracts.Task{ID: 42}, old); err != nil {
			t.Fatal(err)
		}
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatal("retry ID is unstable")
	}
	v1 := json.RawMessage(`{"deliveryVersion":1,"eventID":"notification.publish/123","instance":"tenant a","message":{"type":"notification","key":"123","subscribers":["PHID-USER-a"],"uniqueID":"producer","extension":42}}`)
	if err := h(t.Context(), &contracts.Task{ID: 43}, v1); err != nil {
		t.Fatal(err)
	}
	if ids[2] != "producer" || calls != 3 {
		t.Fatal("producer identity changed")
	}
}
func TestNotificationPolicyFailuresAndRevocation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) }))
	defer server.Close()
	data := json.RawMessage(`{"message":{"type":"notification","key":"1","subscribers":["u"]}}`)
	for _, mode := range []string{"required", "fallback", "off"} {
		h := NewNotificationPublishHandler(notificationPolicyFile(t, mode, server.URL))
		err := h(t.Context(), &contracts.Task{ID: 1}, data)
		if (err != nil) != (mode == "required") {
			t.Fatalf("mode %s: %v", mode, err)
		}
	}
	if calls != 2 {
		t.Fatal("off sent a message")
	}
	path := notificationPolicyFile(t, "required", server.URL)
	h := NewNotificationPublishHandler(path)
	os.WriteFile(path, []byte(`{"version":1,"mode":"off","instance":"tenant a","endpoints":[]}`), 0600)
	if err := h(t.Context(), &contracts.Task{}, data); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(`{}`), 0600)
	if err := h(t.Context(), &contracts.Task{}, data); err == nil {
		t.Fatal("invalid policy silently disabled delivery")
	}
	if err := NewNotificationPublishHandler(notificationPolicyFile(t, "required"))(t.Context(), &contracts.Task{}, data); err != nil {
		t.Fatal("empty endpoints changed compatibility")
	}
}
func TestNotificationInvalidPayloadCannotBroadcast(t *testing.T) {
	h := NewNotificationPublishHandler(notificationPolicyFile(t, "required"))
	for _, raw := range []string{
		`{}`, `{"message":null}`, `{"message":{"type":"notification","key":"1","subscribers":[]}}`,
		`{"message":{"type":"notification","key":"1","subscribers":"u"}}`,
		`{"message":{"type":"notification","key":"1","subscribers":[""]}}`,
		`{"deliveryVersion":2,"message":{}}`,
		`{"deliveryVersion":1,"eventID":"e","instance":"other","message":{"type":"notification","key":"1","subscribers":["u"]}}`,
	} {
		var permanent *worker.PermanentError
		if err := h(t.Context(), &contracts.Task{}, json.RawMessage(raw)); !errors.As(err, &permanent) {
			t.Fatalf("unsafe payload accepted: %s / %v", raw, err)
		}
	}
}
func TestNotificationFailoverAndRedirect(t *testing.T) {
	goodCalls := 0
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { goodCalls++; w.WriteHeader(200) }))
	defer good.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, good.URL, 302) }))
	defer redirect.Close()
	data := json.RawMessage(`{"message":{"type":"notification","key":"1","subscribers":["u"]}}`)
	h := NewNotificationPublishHandler(notificationPolicyFile(t, "required", redirect.URL))
	if err := h(t.Context(), &contracts.Task{}, data); err == nil || goodCalls != 0 {
		t.Fatal("redirect followed")
	}
	h = NewNotificationPublishHandler(notificationPolicyFile(t, "required", redirect.URL, good.URL))
	if err := h(t.Context(), &contracts.Task{}, data); err != nil || goodCalls != 1 {
		t.Fatalf("failover: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h(ctx, &contracts.Task{}, data); err == nil || goodCalls != 1 {
		t.Fatal("canceled lease dispatched")
	}
}

func TestNotificationCredentialsNeverDegrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer server.Close()
	h := NewNotificationPublishHandler(notificationPolicyFile(t, "fallback", server.URL))
	data := json.RawMessage(`{"message":{"type":"notification","key":"1","subscribers":["u"]}}`)
	var retry *worker.RetryError
	if err := h(t.Context(), &contracts.Task{}, data); !errors.As(err, &retry) || retry.Wait != 60 {
		t.Fatalf("credential failure silently degraded: %v", err)
	}
}

func TestNotificationShadowNeverDoubleSends(t *testing.T) {
	nativeCalls, phpCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nativeCalls++; w.WriteHeader(200) }))
	defer server.Close()
	registry := worker.NewRegistry()
	registry.SetFallback(func(context.Context, *contracts.Task, json.RawMessage) error { phpCalls++; return nil })
	path := notificationPolicyFile(t, "required", server.URL)
	if err := RegisterNotificationMode(registry, path, "shadow"); err != nil {
		t.Fatal(err)
	}
	h, _ := registry.Get("PhabricatorNotificationPublishWorker")
	for _, data := range []json.RawMessage{json.RawMessage(`{"message":{"type":"notification","key":"1","subscribers":["u"]}}`), json.RawMessage(`{}`)} {
		if err := h(t.Context(), &contracts.Task{}, data); err != nil {
			t.Fatal(err)
		}
	}
	if nativeCalls != 0 || phpCalls != 2 {
		t.Fatalf("shadow sent or blocked delivery: native=%d php=%d", nativeCalls, phpCalls)
	}
	if err := RegisterNotificationMode(worker.NewRegistry(), path, "shadow"); err == nil {
		t.Fatal("shadow accepted without delegate")
	}
	if err := RegisterNotificationMode(worker.NewRegistry(), path, "unknown"); err == nil {
		t.Fatal("invalid rollout mode accepted")
	}
}

func TestExplicitDelegatedNotificationRequiresPHP(t *testing.T) {
	if err := RegisterNotificationMode(worker.NewRegistry(), "", "delegated"); err == nil {
		t.Fatal("delegated rollout silently accepted without PHP")
	}
	// Old standalone workers without notification configured remain supported.
	if err := RegisterNotificationMode(worker.NewRegistry(), "", "auto"); err != nil {
		t.Fatal(err)
	}
}
