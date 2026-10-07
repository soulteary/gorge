package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestDrainArchivesHandlerIgnoringCancellationAndRejectsLateCompletion(t *testing.T) {
	q := &fakeQueue{pending: []*contracts.Task{{ID: 1, TaskClass: "A"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/queue/resolve" {
			time.Sleep(1200 * time.Millisecond)
		}
		q.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "")
	started, release, handlerReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	registry := NewRegistry()
	registry.Register("A", func(context.Context, *contracts.Task, json.RawMessage) error {
		close(started)
		<-release
		defer close(handlerReturned)
		return &Completion{Duration: 1}
	})
	consumer := NewConsumer(client, registry, testConfig())
	consumer.drainTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); consumer.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	stopping := time.Now()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ignored cancellation prevented bounded archive and shutdown")
	}
	if time.Since(stopping) < time.Second {
		t.Fatal("shutdown did not wait for independent archive reporting")
	}
	q.mu.Lock()
	if len(q.failed) != 1 || len(q.activeTasks) != 0 || q.completeAttempts != 0 {
		t.Errorf("unknown task was not archived exclusively: %+v", q)
	}
	q.mu.Unlock()
	once.Do(func() { close(release) })
	<-handlerReturned
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.completeAttempts != 0 || consumer.Stats().Processed != 0 {
		t.Fatal("late Completion finalized an archived unknown execution")
	}
}

func TestUnknownArchiveRefreshesFencingAndNeverTakesAnotherOwner(t *testing.T) {
	for _, mode := range []string{"renewed-with-lost-reply", "renew-races-resolve", "other-owner", "already-finalized"} {
		t.Run(mode, func(t *testing.T) {
			var queueMu sync.Mutex
			expires := time.Now().Unix() + 120
			resolves := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queueMu.Lock()
				defer queueMu.Unlock()
				if r.Method == http.MethodGet {
					if mode == "already-finalized" {
						w.WriteHeader(http.StatusNotFound)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ERR_NOT_FOUND"}})
						return
					}
					owner := "ours"
					if mode == "other-owner" {
						owner = "someone-else"
					}
					writeData(w, &contracts.Task{ID: 1, LeaseOwner: owner, LeaseExpires: &expires})
					return
				}
				var req contracts.ResolveRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				resolves++
				if req.LeaseOwner != "ours" || req.LeaseExpires != expires || req.Outcome != "failure" {
					t.Errorf("shutdown used a stale or unfenced resolution: %+v", req)
				}
				if mode == "renew-races-resolve" && resolves == 1 {
					expires++
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ERR_LEASE_CONFLICT"}})
					return
				}
				writeData(w, map[string]string{"status": "resolved"})
			}))
			defer srv.Close()
			consumer := NewConsumer(NewClient(srv.URL, ""), NewRegistry(), testConfig())
			oldExpiry := time.Now().Unix() + 60
			consumer.archiveInterruptedTask(context.Background(), &contracts.Task{ID: 1, LeaseOwner: "ours", LeaseExpires: &oldExpiry})
			queueMu.Lock()
			defer queueMu.Unlock()
			want := 1
			switch mode {
			case "other-owner", "already-finalized":
				want = 0
			case "renew-races-resolve":
				want = 2
			}
			if resolves != want {
				t.Fatalf("resolve count = %d, want %d", resolves, want)
			}
		})
	}
}

func TestDrainReconcilesFinalizationWithLostReply(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(strconv.FormatBool(committed), func(t *testing.T) {
			var mu sync.Mutex
			var active *contracts.Task
			issued, resolutions := false, 0
			finalizing := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/api/queue/meta":
					writeData(w, contracts.ExecutionCapabilities{ExecutionVersion: 1, LeaseOutcomes: true})
				case "/api/queue/lease":
					if !issued {
						issued = true
						expiry := time.Now().Unix() + 3600
						active = &contracts.Task{ID: 1, TaskClass: "A", LeaseOwner: r.Header.Get("X-Lease-Owner"), LeaseExpires: &expiry}
						writeData(w, []*contracts.Task{active})
					} else {
						writeData(w, []*contracts.Task{})
					}
				case "/api/queue/renew":
					writeData(w, active)
				case "/api/queue/finalize":
					_ = json.NewDecoder(r.Body).Decode(new(contracts.FinalizeRequest))
					if committed {
						active = nil
					}
					close(finalizing)
					// Simulate a queue response disappearing after a possible commit.
					<-r.Context().Done()
				case "/api/queue/tasks/1":
					if active == nil {
						w.WriteHeader(http.StatusNotFound)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ERR_NOT_FOUND"}})
					} else {
						writeData(w, active)
					}
				case "/api/queue/resolve":
					resolutions++
					active = nil
					writeData(w, map[string]string{"status": "resolved"})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			registry := NewRegistry()
			registry.Register("A", NewNoop())
			consumer := NewConsumer(NewClient(srv.URL, ""), registry, testConfig())
			consumer.drainTimeout = 30 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); consumer.Run(ctx) }()
			select {
			case <-finalizing:
			case <-time.After(time.Second):
				t.Fatal("finalization did not start")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown failed to reconcile an unconfirmed finalization")
			}
			mu.Lock()
			defer mu.Unlock()
			if committed && resolutions != 0 || !committed && resolutions != 1 || active != nil {
				t.Fatalf("finalize/archive reconciliation failed: committed=%t resolutions=%d active=%v", committed, resolutions, active)
			}
		})
	}
}
