package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type sourceHTTPFixture struct {
	job          *projection.SourceJob
	calls        int
	catalogCalls int
}

func (f *sourceHTTPFixture) Catalog(context.Context) (*projection.SourceCatalog, error) {
	f.catalogCalls++
	return &projection.SourceCatalog{Version: 1, Namespace: "tenant", SerializerVersion: "test", Scope: "registered-lisk-fulltext", Sources: []projection.SourceDescriptor{{ClassName: "TestSource", UpperID: "0"}}, UnsupportedClasses: []string{}}, nil
}
func (f *sourceHTTPFixture) Scan(context.Context, string, string, string) (*projection.SourcePage, error) {
	panic("HTTP handler must not scan inline")
}
func (f *sourceHTTPFixture) SourceJob(context.Context, string, string) (*projection.SourceJob, error) {
	f.calls++
	if f.job == nil {
		return nil, sql.ErrNoRows
	}
	return f.job, nil
}
func (f *sourceHTTPFixture) CreateSourceJob(_ context.Context, ns, id string, target projection.Target, c *projection.SourceCatalog) (*projection.SourceJob, error) {
	f.calls++
	f.job = &projection.SourceJob{Namespace: ns, JobID: id, Target: target, Catalog: c}
	return f.job, nil
}
func TestSourceScanHTTP(t *testing.T) {
	f := &sourceHTTPFixture{}
	srv := httpx.New(httpx.Config{})
	RegisterRoutes(srv.App(), &Deps{Token: testToken, Projection: &ProjectionIngress{Namespace: "tenant", Targets: []projection.Target{{BackendID: "es", GenerationID: "g1"}}, SourceScanner: &SourceScanRuntime{Store: f, Provider: f}}})
	for _, tc := range []struct {
		token, body string
		status      int
	}{
		{"", `{}`, 401}, {testToken, `{"jobID":"job","backendID":"es","generationID":"other"}`, 400}, {testToken, `{"jobID":"job","backendID":"es","generationID":"g1","activate":true}`, 400}, {testToken, `{"jobID":"job","backendID":"es","generationID":"g1"}`, 200}, {testToken, `{"jobID":"job","backendID":"es","generationID":"g1"}`, 200},
	} {
		req := httptest.NewRequest("POST", "/api/search/projections/source-scans", strings.NewReader(tc.body))
		req.Header.Set("X-Service-Token", tc.token)
		resp, body := do(t, srv.App(), req)
		if resp.StatusCode != tc.status {
			t.Fatal(resp.StatusCode, body)
		}
		if tc.status == 200 && (!strings.Contains(body, `"activationAllowed":false`) || !strings.Contains(body, `"sourceCoverageVerified":false`)) {
			t.Fatal("activation claim", body)
		}
	}

	reqCheck := httptest.NewRequest("POST", "/api/search/projections/source-scans/job/check", nil)
	reqCheck.Header.Set("X-Service-Token", testToken)
	checked, bodyCheck := do(t, srv.App(), reqCheck)
	if checked.StatusCode != 200 || !strings.Contains(bodyCheck, `"activationAllowed":false`) {
		t.Fatal(checked.StatusCode, bodyCheck)
	}
	if f.catalogCalls != 1 || f.calls != 3 {
		t.Fatal("retry refetched catalog", f.catalogCalls, f.calls)
	}
	req := httptest.NewRequest("GET", "/api/search/projections/source-scans/job", nil)
	req.Header.Set("X-Service-Token", testToken)
	resp, _ := do(t, srv.App(), req)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}
func TestConduitSourceProviderWireAndStrictResponse(t *testing.T) {
	mode := "valid"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search.source" || r.Header.Get("X-Service-Token") != "token" {
			t.Error("wire")
		}
		_ = r.ParseForm()
		var p map[string]any
		_ = json.Unmarshal([]byte(r.Form.Get("params")), &p)
		if p["phase"] != "scan" || p["afterID"] != "9007199254740993" || p["upperID"] != "9223372036854775807" {
			t.Error("cursor precision", p)
		}
		if mode == "error" {
			_, _ = w.Write([]byte(`{"error_code":"SECRET","error_info":"password=secret"}`))
			return
		}
		result := `{"sourceScanVersion":1,"namespace":"tenant","serializerVersion":"test","className":"TestSource","afterID":"9007199254740993","upperID":"9223372036854775807","nextID":"9223372036854775807","complete":true,"materialized":true,"sourceCoverageVerified":false,"items":[]}`
		if mode == "unknown" {
			result = strings.TrimSuffix(result, "}") + `,"activate":true}`
		}
		_, _ = w.Write([]byte(`{"result":` + result + `,"error_code":null,"error_info":null}`))
	}))
	defer srv.Close()
	provider, err := NewSourceProvider(&SourceScanConfig{ConduitURL: srv.URL}, "token")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"valid", "unknown", "error"} {
		mode = m
		p, err := provider.Scan(context.Background(), "TestSource", "9007199254740993", "9223372036854775807")
		if m == "valid" {
			if err != nil || p.NextID != "9223372036854775807" {
				t.Fatal(p, err)
			}
		} else if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe response", err)
		}
	}
	for _, u := range []string{"file:///etc/passwd", "http://user:pass@localhost", "http://localhost?token=secret"} {
		if _, err := NewSourceProvider(&SourceScanConfig{ConduitURL: u}, "token"); err == nil {
			t.Fatal("invalid endpoint", u)
		}
	}
}

func (f *sourceHTTPFixture) CheckSourceJob(context.Context, string, string) (*projection.SourceScanCheck, error) {
	return &projection.SourceScanCheck{Job: f.job}, nil
}
