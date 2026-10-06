package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

func TestFeedPolicyRevocationAndSilentAreReadAtExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	handler := NewFeedPolicyHandler(path)
	data := json.RawMessage(`{"deliveryVersion":1,"uri":"https://example.test/removed","body":"x=1"}`)
	if err := handler(context.Background(), &contracts.Task{}, data); err == nil {
		t.Fatal("missing policy was accepted")
	}
	if err := os.WriteFile(path, []byte(`{"silent":true,"uris":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), &contracts.Task{}, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"silent":false,"uris":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var permanent *worker.PermanentError
	if err := handler(t.Context(), &contracts.Task{}, data); !errors.As(err, &permanent) {
		t.Fatalf("removed hook accepted: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"uris":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), &contracts.Task{}, data); err == nil {
		t.Fatal("incomplete policy accepted")
	}
}
