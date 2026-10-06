package search

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type fakeRebuilder struct {
	calls     int
	err       error
	namespace string
}

func (f *fakeRebuilder) CreateRebuild(_ context.Context, ns, id string, target projection.Target) (*projection.RebuildJob, error) {
	f.calls++
	f.namespace = ns
	return &projection.RebuildJob{Namespace: ns, JobID: id, Target: target, Status: "scanning"}, f.err
}
func (f *fakeRebuilder) RebuildJob(_ context.Context, ns, id string) (*projection.RebuildJob, error) {
	f.calls++
	f.namespace = ns
	return &projection.RebuildJob{Namespace: ns, JobID: id}, f.err
}
func (f *fakeRebuilder) CheckRebuild(_ context.Context, ns, id string) (*projection.RebuildCheck, error) {
	f.calls++
	f.namespace = ns
	return &projection.RebuildCheck{Job: &projection.RebuildJob{Namespace: ns, JobID: id}, TransportCaughtUp: true}, f.err
}
func TestProjectionRebuildHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, token, body string
		err                             error
		status, calls                   int
	}{
		{"create", "POST", "/api/search/projections/rebuilds", testToken, `{"jobID":"job","backendID":"es","generationID":"g1"}`, nil, 200, 1},
		{"unauthenticated", "POST", "/api/search/projections/rebuilds", "", `{}`, nil, 401, 0},
		{"query token", "GET", "/api/search/projections/rebuilds/job?token=" + testToken, "", "", nil, 401, 0},
		{"unknown target", "POST", "/api/search/projections/rebuilds", testToken, `{"jobID":"job","backendID":"es","generationID":"unknown"}`, nil, 400, 0},
		{"unknown field", "POST", "/api/search/projections/rebuilds", testToken, `{"jobID":"job","backendID":"es","generationID":"g1","activate":true}`, nil, 400, 0},
		{"trailing JSON", "POST", "/api/search/projections/rebuilds", testToken, `{"jobID":"job","backendID":"es","generationID":"g1"}{}`, nil, 400, 0},
		{"bad ID", "GET", "/api/search/projections/rebuilds/bad%20id", testToken, "", nil, 400, 0},
		{"missing", "GET", "/api/search/projections/rebuilds/job", testToken, "", sql.ErrNoRows, 404, 1},
		{"conflict", "POST", "/api/search/projections/rebuilds", testToken, `{"jobID":"job","backendID":"es","generationID":"g1"}`, projection.ErrRebuildConflict, 409, 1},
		{"check", "POST", "/api/search/projections/rebuilds/job/check", testToken, "", nil, 200, 1},
		{"sanitized failure", "POST", "/api/search/projections/rebuilds/job/check", testToken, "", errors.New("password=secret"), 503, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRebuilder{err: tc.err}
			srv := httpx.New(httpx.Config{})
			RegisterRoutes(srv.App(), &Deps{Token: testToken, Projection: &ProjectionIngress{Rebuilder: f, Namespace: "tenant", Targets: []projection.Target{{BackendID: "es", GenerationID: "g1"}}}})
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("X-Service-Token", tc.token)
			resp, body := do(t, srv.App(), req)
			if resp.StatusCode != tc.status || f.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, f.calls, body)
			}
			if strings.Contains(body, "password=secret") {
				t.Fatal("raw error leaked")
			}
			if f.calls > 0 && f.namespace != "tenant" {
				t.Fatal("namespace escaped ingress")
			}
			if tc.name == "check" && (!strings.Contains(body, `"activationAllowed":false`) || !strings.Contains(body, `"sourceCoverageVerified":false`)) {
				t.Fatalf("claimed activation: %s", body)
			}
		})
	}
	srv := httpx.New(httpx.Config{})
	RegisterRoutes(srv.App(), &Deps{Token: testToken, Projection: &ProjectionIngress{}})
	req := httptest.NewRequest("POST", "/api/search/projections/rebuilds", strings.NewReader("{}"))
	req.Header.Set("X-Service-Token", testToken)
	resp, _ := do(t, srv.App(), req)
	if resp.StatusCode != 404 {
		t.Fatal("disabled rebuild route exposed")
	}
}
