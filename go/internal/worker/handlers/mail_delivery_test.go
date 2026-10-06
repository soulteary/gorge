package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

func TestMailPreparationAndSubmissionBoundary(t *testing.T) {
	var prepare, authorize, apply atomic.Int32
	php := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		var params map[string]any
		json.Unmarshal([]byte(r.FormValue("params")), &params)
		if r.Header.Get("X-Service-Token") != "internal" {
			t.Error("missing conduit authentication")
		}
		switch params["phase"] {
		case "prepare":
			prepare.Add(1)
			w.Write([]byte(`{"result":{"schemaVersion":1,"deliveryID":"mail/example/1","mailID":7,"deadline":9999999999,"message":{"from":{"address":"sender@example.test"},"to":[{"address":"to@example.test"}],"subject":"test"}}}`))
		case "authorize":
			authorize.Add(1)
			w.Write([]byte(`{"result":{"changed":false}}`))
		case "apply":
			n := apply.Add(1)
			if n == 1 {
				w.Write([]byte(`{"error_code":"projection-down","error_info":"temporary"}`))
			} else {
				w.Write([]byte(`{"result":{"applied":true}}`))
			}
		default:
			t.Errorf("unexpected PHP phase: %v", params["phase"])
		}
	}))
	defer php.Close()
	mailer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Service-Token") != "mail-token" {
			t.Error("missing mailer token")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/api/mailer/prepare" {
			w.Write([]byte(`{"data":{"deliveryID":"mail/example/1","state":"prepared","revision":0}}`))
			return
		}
		if r.URL.Path != "/api/mailer/execute" || body["allowSend"] != true || body["deliveryID"] != "mail/example/1" {
			t.Errorf("invalid submission request: %s %v", r.URL.Path, body)
		}
		if _, ok := body["message"]; ok {
			t.Error("submission task includes message content")
		}
		w.Write([]byte(`{"data":{"deliveryID":"mail/example/1","state":"accepted","revision":2}}`))
	}))
	defer mailer.Close()
	policy := filepath.Join(t.TempDir(), "policy.json")
	os.WriteFile(policy, []byte(`{"silent":false,"uris":[]}`), 0600)
	conduit := NewConduitClient(php.URL, "internal")
	task := &contracts.Task{Priority: 25}
	err := NewMailPreparationHandler(conduit, mailer.URL, "mail-token")(context.Background(), task, json.RawMessage(`7`))
	completion, ok := err.(*worker.Completion)
	if !ok || len(completion.Followups) != 1 {
		t.Fatalf("missing atomic followup: %v", err)
	}
	submit := NewMailSubmitHandler(conduit, mailer.URL, "mail-token", policy)
	if err = submit(context.Background(), task, json.RawMessage(completion.Followups[0].Data)); err == nil {
		t.Fatal("failed projection archived task")
	}
	if err = submit(context.Background(), task, json.RawMessage(completion.Followups[0].Data)); err != nil {
		t.Fatal(err)
	}
	if prepare.Load() != 1 || apply.Load() != 2 || authorize.Load() != 2 {
		t.Fatal("submission retries reran preparation")
	}
}

func TestMailReadinessRejectsMissingNativeProtocol(t *testing.T) {
	for _, body := range []string{`{"data":{"schemaVersion":1,"recovery":true}}`, `{"data":{"schemaVersion":1,"recovery":false}}`, `{"status":"ok"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Service-Token") != "token" {
				t.Error("missing token")
			}
			w.Write([]byte(body))
		}))
		err := ValidateMailDeliveryService(context.Background(), server.URL+"/", "token")
		server.Close()
		if (err == nil) != (body == `{"data":{"schemaVersion":1,"recovery":true}}`) {
			t.Fatalf("unexpected readiness result: %s %v", body, err)
		}
	}
}
func TestMailPreparationRejectsDifferentServiceRoute(t *testing.T) {
	var calls atomic.Int32
	php := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result":{"schemaVersion":1,"deliveryID":"mail/1","mailID":7,"deadline":9999999999,"mailerURI":"http://different-mailer","message":{"from":{"address":"from@example.test"},"to":[{"address":"to@example.test"}]}}}`))
	}))
	defer php.Close()
	mailer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer mailer.Close()
	err := NewMailPreparationHandler(NewConduitClient(php.URL, "token"), mailer.URL, "token")(context.Background(), &contracts.Task{}, json.RawMessage(`7`))
	if err == nil || calls.Load() != 0 {
		t.Fatal("snapshot was routed through a different service")
	}
}
