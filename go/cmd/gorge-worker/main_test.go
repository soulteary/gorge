package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Run the actual command in a child so an HTTP request can be held in flight
// while SIGTERM tests when the separate queue consumer stops taking work.
func TestWorkerSignalStopsIntakeBeforeHTTPDrain(t *testing.T) {
	if os.Getenv("GORGE_TEST_WORKER_SIGNAL_CHILD") == "1" {
		main()
		os.Exit(0)
	}
	var leased atomic.Int64
	var blockMeta atomic.Bool
	metaBlocked, releaseMeta := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseMeta) }) }
	defer release()
	queue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api/queue/meta":
			if blockMeta.Load() {
				select {
				case metaBlocked <- struct{}{}:
				default:
				}
				select {
				case <-releaseMeta:
				case <-r.Context().Done():
					return
				}
			}
			data = map[string]any{"executionVersion": 1, "leaseOutcomes": true}
		case "/api/queue/lease":
			leased.Add(1)
			data = []any{}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	}))
	defer queue.Close()
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := socket.Addr().String()
	_ = socket.Close()
	logFile, err := os.CreateTemp(t.TempDir(), "worker-log")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			t.Errorf("close worker log: %v", err)
		}
	}()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerSignalStopsIntakeBeforeHTTPDrain$")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GORGE_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GORGE_TEST_WORKER_SIGNAL_CHILD=1", "GORGE_LISTEN_ADDR="+address,
		"GORGE_WORKER_TASK_QUEUE_URL="+queue.URL, "GORGE_WORKER_POLL_INTERVAL_MS=10",
		"GORGE_WORKER_IDLE_TIMEOUT_SEC=0", "GORGE_WORKER_DRAIN_TIMEOUT_SEC=1")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(10 * time.Second)
	for leased.Load() == 0 && time.Now().Before(deadline) {
		select {
		case err := <-finished:
			log, _ := os.ReadFile(logFile.Name())
			t.Fatalf("worker exited before leasing: %v\n%s", err, log)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if leased.Load() == 0 {
		t.Fatal("worker did not start taking work")
	}
	blockMeta.Store(true)
	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://" + address + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-metaBlocked:
	case <-time.After(5 * time.Second):
		t.Fatal("readiness request was not held in flight")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// Allow an already-started lease request to finish, then check that intake
	// stays stopped while the HTTP listener is still draining its request.
	time.Sleep(50 * time.Millisecond)
	count := leased.Load()
	time.Sleep(200 * time.Millisecond)
	if leased.Load() != count {
		t.Fatal("worker kept taking work during HTTP shutdown")
	}
	select {
	case err := <-finished:
		t.Fatalf("worker exited before the HTTP drain completed: %v", err)
	default:
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			log, _ := os.ReadFile(logFile.Name())
			t.Fatalf("worker failed to exit cleanly: %v\n%s", err, log)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish concurrent shutdown")
	}
	<-requestDone
}
