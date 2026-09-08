package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestRegistrySupportedClasses(t *testing.T) {
	r := NewRegistry()
	r.Register("A", NewNoop())
	r.Register("B", NewNoop())

	classes := r.SupportedClasses()
	if len(classes) != 2 {
		t.Fatalf("expected 2 classes, got %v", classes)
	}
	if !r.Has("A") || !r.Has("B") {
		t.Error("registered classes must be reported as supported")
	}
	if r.Has("C") {
		t.Error("an unregistered class with no fallback is unsupported")
	}
}

func TestRegistryFallbackAppendsStar(t *testing.T) {
	r := NewRegistry()
	r.Register("A", NewNoop())
	r.SetFallback(NewNoop())

	if !r.Has("anything") {
		t.Error("a fallback must make every class supported")
	}
	var hasStar bool
	for _, c := range r.SupportedClasses() {
		if c == "*" {
			hasStar = true
		}
	}
	if !hasStar {
		t.Error("a fallback must be reported as * in supported classes")
	}
}

// NewNoop is a test handler that always succeeds. It is not the handlers
// package's NewNoopHandler because that would make this package import its own
// subpackage; the loop only needs a TaskHandler, which this is.
func NewNoop() TaskHandler {
	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		return nil
	}
}

// fakeQueue is a taskqueue service stand-in: it hands out a fixed set of tasks
// on the first lease, then empties, and records every result the worker
// reports back. It lets the consumer be driven end to end without a real
// queue.
type fakeQueue struct {
	mu sync.Mutex

	pending   []*contracts.Task
	completed []int64
	failed    []int64
	yielded   []int64
}

func (q *fakeQueue) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q.mu.Lock()
		defer q.mu.Unlock()

		switch r.URL.Path {
		case "/api/queue/lease":
			tasks := q.pending
			q.pending = nil
			writeData(w, tasks)
		case "/api/queue/complete":
			var req contracts.CompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			q.completed = append(q.completed, req.TaskID)
			writeData(w, map[string]string{"status": "ok"})
		case "/api/queue/fail":
			var req contracts.FailRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			q.failed = append(q.failed, req.TaskID)
			writeData(w, map[string]string{"status": "ok"})
		case "/api/queue/yield":
			var req contracts.YieldRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			q.yielded = append(q.yielded, req.TaskID)
			writeData(w, map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}
}

func writeData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func newFakeQueue(t *testing.T, tasks []*contracts.Task) (*fakeQueue, *Client) {
	t.Helper()
	q := &fakeQueue{pending: tasks}
	srv := httptest.NewServer(q.handler())
	t.Cleanup(srv.Close)
	return q, NewClient(srv.URL, "")
}

func drainConsumer(t *testing.T, consumer *Consumer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(ctx)
	}()
	// Give the loop a few poll ticks to lease and process, then stop it.
	time.Sleep(120 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer did not stop after cancellation")
	}
}

func testConfig() *Config {
	return &Config{
		LeaseLimit:     10,
		PollIntervalMs: 20,
		MaxWorkers:     4,
		IdleTimeoutSec: 0,
	}
}

func TestConsumerCompletesSuccessfulTasks(t *testing.T) {
	q, client := newFakeQueue(t, []*contracts.Task{
		{ID: 1, TaskClass: "A"},
		{ID: 2, TaskClass: "A"},
	})
	registry := NewRegistry()
	registry.Register("A", NewNoop())

	consumer := NewConsumer(client, registry, testConfig())
	drainConsumer(t, consumer)

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.completed) != 2 {
		t.Errorf("expected 2 completed, got %v", q.completed)
	}
	if consumer.Stats().Processed != 2 {
		t.Errorf("processed = %d, want 2", consumer.Stats().Processed)
	}
}

func TestConsumerPermanentFailureIsReportedPermanent(t *testing.T) {
	q, client := newFakeQueue(t, []*contracts.Task{{ID: 1, TaskClass: "A"}})
	registry := NewRegistry()
	registry.Register("A", func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		return &PermanentError{Msg: "cannot recover"}
	})

	consumer := NewConsumer(client, registry, testConfig())
	drainConsumer(t, consumer)

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.failed) != 1 {
		t.Errorf("a permanent failure must be reported to fail, got %v", q.failed)
	}
	if consumer.Stats().Failed != 1 {
		t.Errorf("failed = %d, want 1", consumer.Stats().Failed)
	}
}

func TestConsumerYieldIsReportedAsYield(t *testing.T) {
	q, client := newFakeQueue(t, []*contracts.Task{{ID: 1, TaskClass: "A"}})
	registry := NewRegistry()
	registry.Register("A", func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		return &YieldError{Msg: "later", Duration: 10}
	})

	consumer := NewConsumer(client, registry, testConfig())
	drainConsumer(t, consumer)

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.yielded) != 1 {
		t.Errorf("a yield must be reported to yield, got %v", q.yielded)
	}
	// A yield is neither a completion nor a failure counter.
	if consumer.Stats().Processed != 0 || consumer.Stats().Failed != 0 {
		t.Errorf("a yield must not count as processed or failed: %+v", consumer.Stats())
	}
}

func TestConsumerSkipsUnsupportedClass(t *testing.T) {
	q, client := newFakeQueue(t, []*contracts.Task{{ID: 1, TaskClass: "Unknown"}})
	registry := NewRegistry()
	registry.Register("A", NewNoop())

	consumer := NewConsumer(client, registry, testConfig())
	drainConsumer(t, consumer)

	q.mu.Lock()
	defer q.mu.Unlock()
	// An unsupported class is returned to the queue (temporary fail) for a
	// worker that can handle it, not completed here.
	if len(q.completed) != 0 {
		t.Errorf("an unsupported class must not be completed, got %v", q.completed)
	}
	if len(q.failed) != 1 {
		t.Errorf("an unsupported class must be returned to the queue, got %v", q.failed)
	}
}

func TestClientLeaseParsesContractTasks(t *testing.T) {
	q, client := newFakeQueue(t, []*contracts.Task{
		{ID: 42, TaskClass: "A", Data: `{"x":1}`},
	})
	_ = q

	tasks, err := client.Lease(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != 42 || tasks[0].Data != `{"x":1}` {
		t.Errorf("lease did not round-trip the task: %+v", tasks)
	}
}

func TestClientReportsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "ERR_INTERNAL", "message": "boom"},
		})
	}))
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL, "")
	_, err := client.Lease(context.Background(), 1)
	if err == nil {
		t.Fatal("an error envelope must surface as an error")
	}
	if !strings.Contains(err.Error(), "ERR_INTERNAL") {
		t.Errorf("error must carry the API code, got %v", err)
	}
}
