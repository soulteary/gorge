package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

func TestConsumerDrainArchivesBlackholeDelegateWithoutRedelivery(t *testing.T) {
	var mu sync.Mutex
	var active *contracts.Task
	issued, renewals, archives, finalizations := false, 0, 0, 0
	queue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		data := func(value any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": value}) }
		conflict := func() {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ERR_LEASE_CONFLICT"}})
		}
		switch r.URL.Path {
		case "/api/queue/meta":
			data(contracts.ExecutionCapabilities{ExecutionVersion: 1, LeaseOutcomes: true})
		case "/api/queue/lease":
			if !issued {
				issued = true
				expires := time.Now().Unix() + 2
				active = &contracts.Task{ID: 1, TaskClass: "Delegate", LeaseOwner: r.Header.Get("X-Lease-Owner"), LeaseExpires: &expires}
				data([]*contracts.Task{active})
			} else if active != nil && *active.LeaseExpires <= time.Now().Unix() {
				// An expired active task would be eligible for redelivery.
				data([]*contracts.Task{active})
			} else {
				data([]*contracts.Task{})
			}
		case "/api/queue/renew":
			var req contracts.RenewRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if active == nil || req.LeaseOwner != active.LeaseOwner || req.LeaseExpires != *active.LeaseExpires {
				conflict()
				return
			}
			renewals++
			expires := time.Now().Unix() + 2
			active.LeaseExpires = &expires
			data(active)
		case "/api/queue/tasks/1":
			if active == nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ERR_NOT_FOUND"}})
				return
			}
			data(active)
		case "/api/queue/resolve":
			var req contracts.ResolveRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if active == nil || req.LeaseOwner != active.LeaseOwner || req.LeaseExpires != *active.LeaseExpires || req.Outcome != "failure" {
				conflict()
				return
			}
			archives++
			active = nil
			data(map[string]string{"status": "resolved"})
		case "/api/queue/finalize":
			finalizations++
			data(map[string]string{"status": "finalized"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer queue.Close()
	started, release := make(chan struct{}), make(chan struct{})
	conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var params struct {
			Phase string `json:"phase"`
		}
		_ = json.Unmarshal([]byte(r.Form.Get("params")), &params)
		result := executeResult{ExecutionVersion: 1}
		switch params.Phase {
		case "capabilities":
			result.Result = "capabilities"
		case "prepare":
			result.Result = "prepared"
		case "execute":
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
	}))
	defer func() { close(release); conduit.Close() }()
	client := worker.NewClient(queue.URL, "")
	registry := worker.NewRegistry()
	registry.Register("Delegate", NewConduitDelegateHandler(NewConduitClient(conduit.URL, ""), client))
	consumer := worker.NewConsumer(client, registry, &worker.Config{LeaseLimit: 1, PollIntervalMs: 5, MaxWorkers: 1, DrainTimeoutSec: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); consumer.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("delegate did not start its blackhole HTTP request")
	}
	stop := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("healthy heartbeat kept blackhole execution draining forever")
	}
	if time.Since(stop) < 900*time.Millisecond {
		t.Fatal("shutdown abandoned the independent drain period")
	}
	mu.Lock()
	if renewals < 2 || archives != 1 || finalizations != 0 {
		t.Errorf("unexpected shutdown outcome: renewals=%d archives=%d finalizations=%d", renewals, archives, finalizations)
	}
	mu.Unlock()
	leased, err := client.Lease(context.Background(), 1, nil)
	if err != nil || len(leased) != 0 {
		t.Fatalf("unknown execution was eligible for redelivery: %v %v", leased, err)
	}
	if consumer.Stats().Processed != 0 || consumer.Stats().Failed != 0 || consumer.Stats().Active != 0 {
		t.Fatalf("administrative archive was reported as confirmed business success/failure: %+v", consumer.Stats())
	}
}
