package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type fakeAcceptor struct {
	calls int
	err   error
}

func (f *fakeAcceptor) Accept(_ context.Context, e *contracts.SearchProjection, targets []projection.Target) (*projection.Receipt, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &projection.Receipt{EventID: e.EventID, PHID: e.PHID, Revision: e.Revision, Status: "accepted", Targets: targets}, nil
}
func TestProjectionIngressBoundary(t *testing.T) {
	e := &contracts.SearchProjection{ProjectionVersion: 1, EventID: "search/test/1", Namespace: "default", PHID: "PHID-TASK-test", Type: "TASK", Revision: "1", Operation: "delete", SerializerVersion: "test", SourceVersion: "delete-source"}
	e.PayloadHash, _ = projection.PayloadHash(e)
	raw, _ := json.Marshal(e)
	cases := []struct {
		name, token, path, body string
		failure                 error
		status, calls           int
	}{
		{"accepted", testToken, "/api/search/projections", string(raw), nil, 200, 1},
		{"missing auth", "", "/api/search/projections", string(raw), nil, 401, 0},
		{"query auth", "", "/api/search/projections?token=" + testToken, string(raw), nil, 401, 0},
		{"duplicate keys", testToken, "/api/search/projections", strings.Replace(string(raw), `"revision":"1"`, `"revision":"1","Revision":"2"`, 1), nil, 400, 0},
		{"wrong namespace", testToken, "/api/search/projections", strings.Replace(string(raw), `"default"`, `"other"`, 1), nil, 400, 0},
		{"identity conflict", testToken, "/api/search/projections", string(raw), projection.ErrEventConflict, 409, 1},
		{"revision conflict", testToken, "/api/search/projections", string(raw), projection.ErrRevisionConflict, 409, 1},
		{"database failure", testToken, "/api/search/projections", string(raw), errors.New("secret DSN"), 503, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAcceptor{err: tc.failure}
			srv := httpx.New(httpx.Config{})
			RegisterRoutes(srv.App(), &Deps{Token: testToken, Projection: &ProjectionIngress{Store: store, Namespace: "default", Targets: []projection.Target{{BackendID: "es", GenerationID: "shadow"}}}})
			req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			req.Header.Set("X-Service-Token", tc.token)
			resp, body := do(t, srv.App(), req)
			if resp.StatusCode != tc.status || store.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, store.calls, body)
			}
			if strings.Contains(body, "secret DSN") {
				t.Fatal("database credentials leaked")
			}
			if tc.status == 200 {
				var receipt projection.Receipt
				json.Unmarshal(envelope(t, body).Data, &receipt)
				if receipt.Status != "accepted" {
					t.Fatal("receipt claimed wrong state")
				}
			}
		})
	}
}
func TestProjectionDisabledAndInvalidConfiguration(t *testing.T) {
	srv := httpx.New(httpx.Config{})
	RegisterRoutes(srv.App(), &Deps{Token: testToken})
	resp, _ := request(t, srv.App(), "POST", "/api/search/projections", "{}")
	if resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
	for _, cfg := range []*ProjectionConfig{{}, {ControlDSN: "unused", Namespace: "../bad", Targets: []projection.Target{{BackendID: "es", GenerationID: "shadow"}}}, {ControlDSN: "unused", Namespace: "default"}, {invalid: true}} {
		if _, db, err := OpenProjection(context.Background(), cfg, testToken); err == nil || db != nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	if _, _, err := OpenProjection(context.Background(), &ProjectionConfig{}, ""); err == nil {
		t.Fatal("empty token accepted")
	}
	t.Setenv("GORGE_SEARCH_PROJECTION", "{")
	if _, _, err := OpenProjection(context.Background(), LoadFromEnv().Projection, testToken); err == nil {
		t.Fatal("invalid environment JSON silently disabled ingress")
	}
}

func TestProjectionCapabilities(t *testing.T) {
	srv := httpx.New(httpx.Config{})
	RegisterRoutes(srv.App(), &Deps{Token: testToken, Projection: &ProjectionIngress{Store: &fakeAcceptor{}, Namespace: "default", Targets: []projection.Target{{BackendID: "es", GenerationID: "shadow"}}}})
	resp, body := request(t, srv.App(), "GET", "/api/search/projections/capabilities", "")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var caps struct {
		DurableAcceptance bool                `json:"durableAcceptance"`
		BackendDelivery   bool                `json:"backendDelivery"`
		Namespace         string              `json:"namespace"`
		Targets           []projection.Target `json:"targets"`
	}
	if err := json.Unmarshal(envelope(t, body).Data, &caps); err != nil {
		t.Fatal(err)
	}
	if !caps.DurableAcceptance || caps.BackendDelivery || caps.Namespace != "default" || len(caps.Targets) != 1 {
		t.Fatalf("incorrect capability claim: %+v", caps)
	}
}
