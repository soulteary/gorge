package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

func TestNotificationRoomMessageRetry(t *testing.T) {
	fail := true
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("instance") != "tenant a" {
			t.Error("room retry lost its instance")
		}
		var message map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Error(err)
			return
		}
		if string(message["type"]) != `"message"` || string(message["threadPHID"]) != `"PHID-CONP-contract"` || string(message["messageID"]) != "42" || string(message["uniqueID"]) != `"stable-room-event"` {
			t.Error("room message identity changed")
		}
		if _, ok := message["deliveryVersion"]; ok {
			t.Error("internal envelope leaked")
		}
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	h := NewNotificationPublishHandler(notificationPolicyFile(t, "required", server.URL))
	data := json.RawMessage(`{"deliveryVersion":1,"eventID":"notification.publish/conpherence/PHID-CONP-contract/42","instance":"tenant a","message":{"type":"message","threadPHID":"PHID-CONP-contract","messageID":42,"subscribers":["PHID-CONP-contract"],"uniqueID":"stable-room-event"}}`)
	var retry *worker.RetryError
	if err := h(t.Context(), &contracts.Task{ID: 42}, data); !errors.As(err, &retry) {
		t.Fatalf("required room outage did not retry: %v", err)
	}
	fail = false
	if err := h(t.Context(), &contracts.Task{ID: 42}, data); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected failed attempt and successful retry, got %d", calls)
	}
}

func TestNotificationInvalidRoomMessages(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	h := NewNotificationPublishHandler(notificationPolicyFile(t, "required", server.URL))
	for _, message := range []string{
		`{"type":"message","threadPHID":"PHID-CONP-a","messageID":42,"subscribers":[]}`,
		`{"type":"message","threadPHID":"PHID-CONP-a","messageID":42,"subscribers":["PHID-CONP-b"]}`,
		`{"type":"message","threadPHID":"PHID-CONP-a","messageID":42,"subscribers":["PHID-CONP-a","PHID-CONP-b"]}`,
		`{"type":"message","threadPHID":"PHID-USER-a","messageID":42,"subscribers":["PHID-USER-a"]}`,
		`{"type":"message","threadPHID":"PHID-CONP-","messageID":42,"subscribers":["PHID-CONP-"]}`,
		`{"type":"message","threadPHID":"PHID-CONP-a","messageID":0,"subscribers":["PHID-CONP-a"]}`,
		`{"type":"message","threadPHID":"PHID-CONP-a","messageID":-1,"subscribers":["PHID-CONP-a"]}`,
		`{"type":"message","threadPHID":"PHID-CONP-a","messageID":1.5,"subscribers":["PHID-CONP-a"]}`,
		`{"type":"message","threadPHID":"PHID-CONP-a","messageID":"42","subscribers":["PHID-CONP-a"]}`,
		`{"type":"message","threadPHID":"PHID-CONP-a","subscribers":["PHID-CONP-a"]}`,
		`{"type":"other","threadPHID":"PHID-CONP-a","messageID":42,"subscribers":["PHID-CONP-a"]}`,
	} {
		t.Run(message, func(t *testing.T) {
			var permanent *worker.PermanentError
			if err := h(t.Context(), &contracts.Task{ID: 42}, json.RawMessage(`{"message":`+message+`}`)); !errors.As(err, &permanent) {
				t.Fatalf("unsafe room message accepted: %v", err)
			}
		})
	}
	if calls != 0 {
		t.Fatal("invalid room message reached the network")
	}
}
