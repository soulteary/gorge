package gitea

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

type recordingSink struct{ tasks []string }

func (s *recordingSink) Append(_ context.Context, task string, _ Event) (bool, error) {
	s.tasks = append(s.tasks, task)
	return true, nil
}

func TestSignedPullRequestLinksTasks(t *testing.T) {
	sink := &recordingSink{}
	srv := httpx.New(httpx.Config{})
	RegisterRoutes(srv.App(), &Deps{Sink: sink, WebhookSecret: "secret", BaseURL: "https://git.example.com"})
	body := []byte(`{"action":"opened","pull_request":{"title":"Implement T42","body":"Also T7","html_url":"https://git.example.com/o/r/pulls/1"}}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitea", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Gitea-Delivery", "delivery-1")
	req.Header.Set("X-Gitea-Event", "pull_request")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(sink.tasks) != 2 || sink.tasks[0] != "T42" || sink.tasks[1] != "T7" {
		t.Fatalf("tasks = %#v", sink.tasks)
	}
}

func TestWebhookRejectsInvalidSignature(t *testing.T) {
	srv := httpx.New(httpx.Config{})
	RegisterRoutes(srv.App(), &Deps{Sink: &recordingSink{}, WebhookSecret: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitea", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Gitea-Signature", "bad")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
