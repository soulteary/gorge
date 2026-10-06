package handlers

import (
	"context"
	"encoding/json"
	"github.com/soulteary/gorge/go/internal/contracts"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFeedSnapshotPreservesCompletePHPEncodedPayload(t *testing.T) {
	body := "storyID=17&storyType=commit&storyData%5Bnested%5D=%E4%B8%AD%E6%96%87&storyAuthorPHID=PHID-USER-a&storyText=hello%26world&epoch=123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Errorf("payload changed: %s", got)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("wrong content type")
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	raw, _ := json.Marshal(FeedHTTPData{URI: srv.URL, DeliveryVersion: 1, Body: body})
	if err := NewFeedHTTPHandler()(context.Background(), &contracts.Task{}, raw); err != nil {
		t.Fatal(err)
	}
}
func TestNativeFeedRejectsLegacyKeyInsteadOfSendingZeroStory(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	raw := json.RawMessage(`{"key":"123","uri":"` + srv.URL + `"}`)
	if err := NewFeedHTTPHandler()(context.Background(), &contracts.Task{}, raw); err == nil {
		t.Fatal("legacy input accepted")
	}
	if called {
		t.Fatal("invalid snapshot sent")
	}
}
func TestFeedDoesNotFollowRedirectToAnotherDestination(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	raw, _ := json.Marshal(FeedHTTPData{URI: source.URL, DeliveryVersion: 1, Body: "storyID=17"})
	err := NewFeedHTTPHandler()(context.Background(), &contracts.Task{}, raw)
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("unexpected %v", err)
	}
	if called {
		t.Fatal("followed redirect")
	}
}
