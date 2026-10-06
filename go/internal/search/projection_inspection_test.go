package search

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type fakeInspector struct {
	err       error
	calls     int
	namespace string
}

func (f *fakeInspector) EventStatus(_ context.Context, namespace, id string) (*projection.EventStatus, error) {
	f.calls++
	f.namespace = namespace
	return &projection.EventStatus{Receipt: projection.Receipt{EventID: id, Status: "accepted"}, Deliveries: []projection.DeliveryState{}}, f.err
}
func (f *fakeInspector) DeliveryStats(_ context.Context, namespace string, _ []projection.Target) (*projection.DeliveryStats, error) {
	f.calls++
	f.namespace = namespace
	return &projection.DeliveryStats{Targets: []projection.TargetStats{}}, f.err
}
func TestProjectionInspectionHTTP(t *testing.T) {
	for _, tc := range []struct {
		path, token   string
		err           error
		status, calls int
	}{
		{"/api/search/projections/status?eventID=search%2Ftest%2F1", testToken, nil, 200, 1},
		{"/api/search/projections/stats", testToken, nil, 200, 1},
		{"/api/search/projections/status", testToken, nil, 400, 0},
		{"/api/search/projections/status?eventID=%00", testToken, nil, 400, 0},
		{"/api/search/projections/status?eventID=absent", testToken, sql.ErrNoRows, 404, 1},
		{"/api/search/projections/status?eventID=present", testToken, errors.New("secret DSN"), 503, 1},
		{"/api/search/projections/stats", testToken, errors.New("secret DSN"), 503, 1},
		{"/api/search/projections/stats", "", nil, 401, 0},
		{"/api/search/projections/stats?token=" + testToken, "", nil, 401, 0},
	} {
		t.Run(tc.path+tc.token, func(t *testing.T) {
			inspector := &fakeInspector{err: tc.err}
			srv := httpx.New(httpx.Config{})
			RegisterRoutes(srv.App(), &Deps{Token: testToken, Projection: &ProjectionIngress{Store: &fakeAcceptor{}, Inspector: inspector, Namespace: "default", Targets: []projection.Target{{BackendID: "es", GenerationID: "g1"}}}})
			req := httptest.NewRequest("GET", tc.path, nil)
			req.Header.Set("X-Service-Token", tc.token)
			resp, body := do(t, srv.App(), req)
			if resp.StatusCode != tc.status || inspector.calls != tc.calls {
				t.Fatalf("%d %s calls=%d", resp.StatusCode, body, inspector.calls)
			}
			if tc.calls > 0 && inspector.namespace != "default" {
				t.Fatal("namespace came from request")
			}
			if strings.Contains(body, "secret DSN") {
				t.Fatal("inspection leaked driver diagnostics")
			}
		})
	}
}

func TestProjectionSourceInspectionFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT UNIX_TIMESTAMP").WillReturnError(errors.New("secret source DSN"))
	srv := httpx.New(httpx.Config{})
	RegisterRoutes(srv.App(), &Deps{Token: testToken, Projection: &ProjectionIngress{Store: &fakeAcceptor{}, Inspector: &fakeInspector{}, SourceOutbox: db, Namespace: "default", Targets: []projection.Target{{BackendID: "es", GenerationID: "g1"}}}})
	resp, body := request(t, srv.App(), "GET", "/api/search/projections/stats", "")
	if resp.StatusCode != 503 || strings.Contains(body, "secret source DSN") {
		t.Fatalf("source failure hidden/leaked: %d %s", resp.StatusCode, body)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
