package httpx

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// runAllInBackground starts RunAll and hands back the channel its result will
// arrive on.
func runAllInBackground(servers ...*Server) <-chan error {
	done := make(chan error, 1)
	go func() { done <- RunAll(servers...) }()
	return done
}

// waitForListener blocks until the server is bound and answering, and returns
// the address it settled on. Tests must not signal the process before this
// returns: RunAll registers the signal handler that keeps SIGTERM from killing
// the test binary, and binding happens after that on another goroutine.
func waitForListener(t *testing.T, s *Server) string {
	t.Helper()

	client := noKeepAliveClient()
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.ListenerAddr(); addr != nil {
			resp, err := client.Get("http://" + addr.String() + "/healthz")
			if err == nil {
				_ = resp.Body.Close()
				return addr.String()
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("server never started listening")
	return ""
}

func noKeepAliveClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

// terminate delivers a real SIGTERM to the test process, the only way to reach
// the signal path RunAll is built around. It is safe only while a RunAll is in
// flight, since that call owns the handler which keeps the signal from ending
// the test binary.
func terminate(t *testing.T) {
	t.Helper()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("failed to signal the test process: %v", err)
	}
}

// awaitResult fails the test rather than hanging when a run never finishes.
func awaitResult(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
		return nil
	}
}

// refusesConnections reports whether the address has stopped accepting, which
// is how a test tells a drained listener from a live one.
func refusesConnections(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

func TestRunAllServesEveryListener(t *testing.T) {
	first := New(Config{ListenAddr: "127.0.0.1:0"})
	second := New(Config{ListenAddr: "127.0.0.1:0"})

	done := runAllInBackground(first, second)

	firstAddr := waitForListener(t, first)
	secondAddr := waitForListener(t, second)
	if firstAddr == secondAddr {
		t.Fatalf("both servers reported %s; they must be separate listeners", firstAddr)
	}

	terminate(t)

	if err := awaitResult(t, done); err != nil {
		t.Errorf("expected a clean shutdown, got %v", err)
	}
	for _, addr := range []string{firstAddr, secondAddr} {
		if !refusesConnections(addr) {
			t.Errorf("%s is still accepting connections after shutdown", addr)
		}
	}
}

// TestRunAllDrainsInFlightRequests covers what ShutdownTimeout buys: the signal
// stops the listener but the request already inside a handler still answers.
func TestRunAllDrainsInFlightRequests(t *testing.T) {
	const body = "drained"

	entered := make(chan struct{})
	release := make(chan struct{})

	srv := New(Config{ListenAddr: "127.0.0.1:0", ShutdownTimeout: 5 * time.Second})
	srv.App().Get("/slow", func(c fiber.Ctx) error {
		close(entered)
		<-release
		return c.SendString(body)
	})

	done := runAllInBackground(srv)
	addr := waitForListener(t, srv)

	type result struct {
		body string
		err  error
	}
	answered := make(chan result, 1)
	go func() {
		client := noKeepAliveClient()
		defer client.CloseIdleConnections()
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/slow", nil)
		if err != nil {
			answered <- result{err: err}
			return
		}
		// Fiber deliberately leaves keep-alive connections open during graceful
		// shutdown. Close this one after its response so the test measures the
		// in-flight handler drain rather than the client's idle connection pool.
		req.Close = true
		resp, err := client.Do(req)
		if err != nil {
			answered <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		read, err := io.ReadAll(resp.Body)
		answered <- result{body: string(read), err: err}
	}()

	<-entered
	terminate(t)
	close(release)

	select {
	case got := <-answered:
		if got.err != nil {
			t.Fatalf("in-flight request was cut off: %v", got.err)
		}
		if got.body != body {
			t.Errorf("expected %q, got %q", body, got.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight request never answered")
	}

	if err := awaitResult(t, done); err != nil {
		t.Errorf("expected a clean shutdown, got %v", err)
	}
}

// TestRunAllStopsWhenOneListenerFails pins the reason RunAll shares one call
// across ports: a service exposed on several ports is not usable when one of
// them is missing, so the surviving listener must not be left in rotation.
func TestRunAllStopsWhenOneListenerFails(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()

	healthy := New(Config{ListenAddr: "127.0.0.1:0"})
	doomed := New(Config{ListenAddr: occupied.Addr().String()})

	done := runAllInBackground(healthy, doomed)

	if err := awaitResult(t, done); err == nil {
		t.Fatal("expected the failed bind to surface as RunAll's error")
	}

	// The healthy server may or may not have finished binding before the
	// failure arrived; either way it must not still be accepting.
	if addr := healthy.ListenerAddr(); addr != nil && !refusesConnections(addr.String()) {
		t.Error("the surviving listener was left accepting after its sibling failed")
	}
}

func TestRunAllWithoutServers(t *testing.T) {
	if err := RunAll(); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// TestRunDelegatesToRunAll keeps the single-server path — the one gorge-render
// uses — behaving as it did before RunAll existed.
func TestRunDelegatesToRunAll(t *testing.T) {
	srv := New(Config{ListenAddr: "127.0.0.1:0"})

	done := make(chan error, 1)
	go func() { done <- srv.Run() }()

	addr := waitForListener(t, srv)
	terminate(t)

	if err := awaitResult(t, done); err != nil {
		t.Errorf("expected a clean shutdown, got %v", err)
	}
	if !refusesConnections(addr) {
		t.Errorf("%s is still accepting connections after shutdown", addr)
	}
}

// TestSkipRootProbeReachesHealth checks that the Config flag really lands on
// health.Register, since nothing else in the process would notice if it did not
// and the notification client port depends on GET / staying unregistered.
func TestSkipRootProbeReachesHealth(t *testing.T) {
	for _, tc := range []struct {
		name       string
		skip       bool
		wantStatus int
	}{
		{"probe registered by default", false, http.StatusOK},
		{"root left to the domain", true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(Config{SkipRootProbe: tc.skip})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			resp, err := srv.App().Test(req, fiber.TestConfig{Timeout: 0})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("GET /: expected %d, got %d", tc.wantStatus, resp.StatusCode)
			}
		})
	}
}
