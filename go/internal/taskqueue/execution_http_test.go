package taskqueue

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

type executionHTTPStore struct {
	Store
	err       error
	finalized *contracts.FinalizeRequest
}

func (s *executionHTTPStore) Finalize(_ context.Context, req *contracts.FinalizeRequest) error {
	s.finalized = req
	return s.err
}
func (s *executionHTTPStore) Renew(_ context.Context, req *contracts.RenewRequest) (*contracts.Task, error) {
	return &contracts.Task{ID: req.TaskID, LeaseOwner: req.LeaseOwner, LeaseExpires: &req.LeaseExpires}, s.err
}

func TestExecutionHTTPProtocol(t *testing.T) {
	s := &executionHTTPStore{}
	app := newTestServer(t, s)
	resp, body := get(t, app, "/api/queue/meta")
	if resp.StatusCode != 200 {
		t.Fatal(body)
	}
	var meta contracts.ExecutionCapabilities
	if err := json.Unmarshal(envelope(t, body).Data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.ExecutionVersion != 1 {
		t.Fatal(meta)
	}
	for _, path := range []string{"/api/queue/meta", "/api/queue/finalize", "/api/queue/renew"} {
		method := http.MethodPost
		if path == "/api/queue/meta" {
			method = http.MethodGet
		}
		resp, _ := dispatch(t, app, httptest.NewRequest(method, path, nil))
		if resp.StatusCode != 401 {
			t.Fatalf("%s allowed without token: %d", path, resp.StatusCode)
		}
	}
	bodyReq := `{"taskID":1,"leaseOwner":"owner","leaseExpires":1234,"duration":5,"followups":[{"taskClass":"child","data":"{}"}]}`
	resp, body = post(t, app, "/api/queue/finalize", bodyReq)
	if resp.StatusCode != 200 || s.finalized == nil || len(s.finalized.Followups) != 1 {
		t.Fatal(body)
	}
	s.err = ErrLeaseConflict
	resp, body = post(t, app, "/api/queue/finalize", bodyReq)
	if resp.StatusCode != 409 || envelope(t, body).Error.Code != "ERR_LEASE_CONFLICT" {
		t.Fatal(body)
	}
	resp, body = post(t, app, "/api/queue/renew", bodyReq)
	if resp.StatusCode != 409 {
		t.Fatal(body)
	}
	resp, body = post(t, app, "/api/queue/renew", `{"taskID":1,"leaseOwner":"owner","leaseExpires":1234,"duration":604801}`)
	if resp.StatusCode != 400 {
		t.Fatal(body)
	}
}
