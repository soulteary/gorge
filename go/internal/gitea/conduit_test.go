package gitea

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestConduitSinkSearchesOlderTransactions(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
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

		switch requests {
		case 1:
			if _, ok := params["after"]; ok {
				t.Error("first page unexpectedly has an after cursor")
			}
			_, _ = w.Write([]byte(`{"result":{"data":[],"cursor":{"limit":100,"after":"older-page","before":null}},"error_code":null,"error_info":null}`))
		case 2:
			if params["after"] != "older-page" {
				t.Errorf("after = %#v", params["after"])
			}
			_, _ = w.Write([]byte(`{"result":{"data":[{"comments":[{"content":{"raw":"Gitea-Delivery: delivery-old"}}]}],"cursor":{"limit":100,"after":null,"before":"newer-page"}},"error_code":null,"error_info":null}`))
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	sink := NewConduitSink(server.URL, "api-token", "", server.Client())
	posted, err := sink.Append(context.Background(), "T1", Event{DeliveryID: "delivery-old"})
	if err != nil {
		t.Fatal(err)
	}
	if posted || requests != 2 {
		t.Fatalf("posted=%v requests=%d", posted, requests)
	}
}

func TestConduitSinkRejectsRepeatedCursor(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"result":{"data":[],"cursor":{"limit":100,"after":"same-page","before":null}},"error_code":null,"error_info":null}`))
	}))
	defer server.Close()

	sink := NewConduitSink(server.URL, "api-token", "", server.Client())
	posted, err := sink.Append(context.Background(), "T1", Event{DeliveryID: "delivery-loop"})
	if err == nil || !strings.Contains(err.Error(), "repeated cursor") {
		t.Fatalf("posted=%v err=%v", posted, err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestConduitSinkSerializesDuplicateDelivery(t *testing.T) {
	var mu sync.Mutex
	posted := false
	searches := 0
	edits := 0
	firstSearchStarted := make(chan struct{})
	releaseFirstSearch := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/transaction.search"):
			mu.Lock()
			searches++
			searchNumber := searches
			alreadyPosted := posted
			mu.Unlock()
			if searchNumber == 1 {
				close(firstSearchStarted)
				<-releaseFirstSearch
			}
			if alreadyPosted {
				_, _ = w.Write([]byte(`{"result":{"data":[{"comments":[{"content":{"raw":"Gitea-Delivery: delivery-concurrent"}}]}]},"error_code":null,"error_info":null}`))
			} else {
				_, _ = w.Write([]byte(`{"result":{"data":[]},"error_code":null,"error_info":null}`))
			}
		case strings.HasSuffix(r.URL.Path, "/api/maniphest.edit"):
			mu.Lock()
			posted = true
			edits++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"result":{"object":{"id":1}},"error_code":null,"error_info":null}`))
		default:
			t.Errorf("unexpected method path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	sink := NewConduitSink(server.URL, "api-token", "", server.Client())
	type result struct {
		posted bool
		err    error
	}
	results := make(chan result, 2)
	secondStarted := make(chan struct{})
	appendEvent := func() {
		ok, err := sink.Append(context.Background(), "T1", Event{DeliveryID: "delivery-concurrent"})
		results <- result{posted: ok, err: err}
	}

	go appendEvent()
	<-firstSearchStarted
	go func() {
		close(secondStarted)
		appendEvent()
	}()
	<-secondStarted
	<-time.After(50 * time.Millisecond)
	mu.Lock()
	concurrentSearches := searches
	mu.Unlock()
	close(releaseFirstSearch)
	if concurrentSearches != 1 {
		t.Fatalf("concurrent searches=%d", concurrentSearches)
	}

	linked := 0
	skipped := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.posted {
			linked++
		} else {
			skipped++
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if linked != 1 || skipped != 1 || searches != 2 || edits != 1 {
		t.Fatalf("linked=%d skipped=%d searches=%d edits=%d", linked, skipped, searches, edits)
	}
}

func readRequestBody(t *testing.T, r *http.Request) string {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return r.Form.Encode()
}
