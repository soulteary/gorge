package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestHeartbeatExtendsLeaseAndStopsBeforeReporting(t *testing.T) {
	var count atomic.Int32
	renewed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req contracts.RenewRequest
		json.NewDecoder(r.Body).Decode(&req)
		count.Add(1)
		expiry := req.LeaseExpires + 60
		writeData(w, &contracts.Task{ID: req.TaskID, LeaseOwner: req.LeaseOwner, LeaseExpires: &expiry})
		select {
		case renewed <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()
	expiry := time.Now().Unix() + 3
	task := &contracts.Task{ID: 1, LeaseOwner: "owner", LeaseExpires: &expiry}
	client := NewClient(srv.URL, "")
	ctx, stop := client.startHeartbeat(t.Context(), task)
	select {
	case <-renewed:
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat did not renew")
	}
	current, err := stop()
	if err != nil || *current.LeaseExpires <= expiry || *task.LeaseExpires != expiry {
		t.Fatalf("invalid snapshot: %+v %v", current, err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("execution context not canceled on stop")
	}
	before := count.Load()
	time.Sleep(30 * time.Millisecond)
	if count.Load() != before {
		t.Fatal("renewal continued after stop")
	}
}
func TestHeartbeatCancelsOnOwnershipLoss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ERR_LEASE_CONFLICT", "message": "reassigned"}})
	}))
	defer srv.Close()
	expiry := time.Now().Unix() + 3
	ctx, stop := NewClient(srv.URL, "").startHeartbeat(t.Context(), &contracts.Task{ID: 1, LeaseOwner: "owner", LeaseExpires: &expiry})
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not canceled")
	}
	_, err := stop()
	if err != ErrLeaseConflict {
		t.Fatalf("ownership loss: %v", err)
	}
}
