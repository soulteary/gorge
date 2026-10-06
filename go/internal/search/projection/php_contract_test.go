package projection

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPHPOutboxEnvelope(t *testing.T) {
	path := os.Getenv("GORGE_TEST_SEARCH_EVENT_FILE")
	if path == "" {
		t.Skip("set GORGE_TEST_SEARCH_EVENT_FILE to PHP search outbox fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []json.RawMessage
	if err = json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("expected all five PHP publication fixtures, got %d", len(events))
	}
	for i, raw := range events {
		event, err := Decode(raw)
		if err != nil {
			t.Fatalf("PHP event %d: %v", i, err)
		}
		revision, err := Validate(event)
		if err != nil || revision != int64(i+1) {
			t.Fatalf("revision %d: %v", revision, err)
		}
		if (event.Operation == "delete") != (i == 2) {
			t.Fatalf("wrong operation %s", event.Operation)
		}
	}
}
