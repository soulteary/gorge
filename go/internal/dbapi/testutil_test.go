package dbapi

import (
	"bytes"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

const testToken = "test-token"

// testCluster is a single-node cluster the HTTP tests point the services at.
func testCluster() *ClusterConfig {
	ref := &DatabaseRef{Host: "db1", Port: 3306, User: "root", IsMaster: true, IsIndividual: true, IsDefaultPartition: true}
	return &ClusterConfig{
		Refs:      []*DatabaseRef{ref},
		Namespace: "phorge",
		masters:   []*DatabaseRef{ref},
	}
}

// setConnFactory points every read service at the same connection opener, so a
// test can inject a mock or a failing dial across the whole Deps at once.
func setConnFactory(deps *Deps, f ConnFactory) {
	deps.Health.SetConnFactory(f)
	deps.Schema.SetConnFactory(f)
	deps.Setup.SetConnFactory(f)
	deps.Migration.SetConnFactory(f)
}

// newTestServer builds the routes the way cmd/gorge-db-api does, so the
// platform error handler and the health probes are in play. The returned Deps
// lets a test swap in a connection factory before issuing a request.
func newTestServer(t *testing.T, configure func(*Deps)) *fiber.App {
	t.Helper()
	deps := NewDeps(testCluster(), "secret", testToken)
	if configure != nil {
		configure(deps)
	}
	srv := httpx.New(httpx.Config{Ready: deps.Ready})
	RegisterRoutes(srv.App(), deps)
	return srv.App()
}

// unreachableFactory dials nothing: every connect fails, as it would against a
// database that is down. It is what the "unavailable" assertions run against.
func unreachableFactory() ConnFactory {
	return func(dsn DSN, readOnly bool) (*Conn, error) {
		return nil, sql.ErrConnDone
	}
}

func do(t *testing.T, app *fiber.App, method, path string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Service-Token", testToken)
	return dispatch(t, app, req)
}

func dispatch(t *testing.T, app *fiber.App, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, string(body)
}

// mockFactory hands every service a Conn backed by a sqlmock whose expectations
// are matched by regexp and out of order, so one factory can answer the
// different query sets the four read services issue without the test caring
// which endpoint asked. A fresh *sql.DB is opened per connect so per-service
// concurrency does not collide on one mock's expectation set.
func mockFactory(t *testing.T, seed func(sqlmock.Sqlmock)) ConnFactory {
	t.Helper()
	return func(dsn DSN, readOnly bool) (*Conn, error) {
		db, mock, err := sqlmock.New(
			sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
			sqlmock.MonitorPingsOption(true),
		)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = db.Close() })
		mock.MatchExpectationsInOrder(false)
		mock.ExpectPing()
		if seed != nil {
			seed(mock)
		}
		return NewConnFromDB(db, dsn, readOnly), nil
	}
}

// quietLogs redirects slog to a buffer for the duration of a test and, on
// cleanup, fails the test if any request was answered by way of a recovered
// panic — httpx logs those under PANIC_RECOVERED, and a contract fixture that
// asserts only a status code cannot tell a clean answer from a panicked one on
// its own. It mirrors the webhook and file-storage helpers of the same name.
func quietLogs(t *testing.T) {
	t.Helper()

	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))

	t.Cleanup(func() {
		slog.SetDefault(previous)
		if written := logs.String(); strings.Contains(written, "PANIC_RECOVERED") {
			t.Errorf("a request was answered by way of a recovered panic:\n%s", written)
		}
	})
}

// syncBuffer collects log output behind a lock, since the readiness probe and
// the handlers may log from more than one goroutine while the test body reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
