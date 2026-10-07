package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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

	pending          []*contracts.Task
	activeTasks      map[int64]*contracts.Task
	completed        []int64
	failed           []int64
	yielded          []int64
	lastLease        contracts.LeaseRequest
	leaseCount       int
	completeFailures int
	completeAttempts int
}

func (q *fakeQueue) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q.mu.Lock()
		defer q.mu.Unlock()

		switch r.URL.Path {
		case "/api/queue/lease":
			q.leaseCount++
			_ = json.NewDecoder(r.Body).Decode(&q.lastLease)
			count := min(len(q.pending), q.lastLease.Limit)
			tasks := q.pending[:count]
			for _, task := range tasks {
				expiry := time.Now().Add(time.Hour).Unix()
				task.LeaseOwner = r.Header.Get("X-Lease-Owner")
				task.LeaseExpires = &expiry
				if q.activeTasks == nil {
					q.activeTasks = make(map[int64]*contracts.Task)
				}
				q.activeTasks[task.ID] = task
			}
			q.pending = q.pending[count:]
			writeData(w, tasks)
		case "/api/queue/finalize":
			q.completeAttempts++
			if q.completeFailures > 0 {
				q.completeFailures--
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"code": "ERR_INTERNAL", "message": "queue unavailable"},
				})
				return
			}
			var req contracts.FinalizeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			q.completed = append(q.completed, req.TaskID)
			delete(q.activeTasks, req.TaskID)
			writeData(w, map[string]string{"status": "ok"})
		case "/api/queue/renew":
			var req contracts.RenewRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			expiry := max(req.LeaseExpires, time.Now().Unix()+int64(req.Duration))
			writeData(w, &contracts.Task{ID: req.TaskID, LeaseOwner: req.LeaseOwner, LeaseExpires: &expiry})
		case "/api/queue/meta":
			writeData(w, contracts.ExecutionCapabilities{ExecutionVersion: 1, LeaseOutcomes: true})
		case "/api/queue/resolve":
			var req contracts.ResolveRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.LeaseOwner == "" || req.LeaseExpires == 0 {
				http.Error(w, "missing lease", 400)
				return
			}
			if req.Outcome == "yield" {
				q.yielded = append(q.yielded, req.TaskID)
			} else {
				q.failed = append(q.failed, req.TaskID)
				if req.Outcome == "failure" {
					delete(q.activeTasks, req.TaskID)
				}
			}
			writeData(w, map[string]string{"status": "ok"})
		default:
			if strings.HasPrefix(r.URL.Path, "/api/queue/tasks/") {
				id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/queue/tasks/"), 10, 64)
				if task := q.activeTasks[id]; task != nil {
					writeData(w, task)
				} else {
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ERR_NOT_FOUND"}})
				}
				return
			}
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

func TestConsumerFiltersClassesInLeaseRequest(t *testing.T) {
	q, client := newFakeQueue(t, nil)
	registry := NewRegistry()
	registry.Register("A", NewNoop())
	registry.Register("B", NewNoop())
	cfg := testConfig()
	cfg.TaskClassFilter = []string{"B"}

	consumer := NewConsumer(client, registry, cfg)
	drainConsumer(t, consumer)

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.lastLease.TaskClasses) != 1 || q.lastLease.TaskClasses[0] != "B" {
		t.Fatalf("lease taskClasses = %v, want [B]", q.lastLease.TaskClasses)
	}
}

func TestConsumerRetriesCompletionBeforeCountingProcessed(t *testing.T) {
	q, client := newFakeQueue(t, []*contracts.Task{{ID: 1, TaskClass: "A"}})
	q.completeFailures = 2
	registry := NewRegistry()
	registry.Register("A", NewNoop())

	consumer := NewConsumer(client, registry, testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		attempts := q.completeAttempts
		q.mu.Unlock()
		if attempts >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.completeAttempts != 3 {
		t.Fatalf("complete attempts = %d, want 3", q.completeAttempts)
	}
	if len(q.completed) != 1 || consumer.Stats().Processed != 1 {
		t.Fatalf("completion was not acknowledged exactly once: completed=%v stats=%+v", q.completed, consumer.Stats())
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

	tasks, err := client.Lease(context.Background(), 5, nil)
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
	_, err := client.Lease(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("an error envelope must surface as an error")
	}
	if !strings.Contains(err.Error(), "ERR_INTERNAL") {
		t.Errorf("error must carry the API code, got %v", err)
	}
}

// An idle queue must not delay newly enqueued work by a fixed sleep window.
func TestConsumerProcessesTaskAfterIdle(t *testing.T) {
	q, client := newFakeQueue(t, nil)
	registry := NewRegistry()
	registry.Register("A", NewNoop())
	consumer := NewConsumer(client, registry, testConfig())
	consumer.idleTimeout = time.Nanosecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); consumer.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("idle consumer did not stop")
		}
	}()
	// Three observed empty leases prove polling continued past the idle threshold.
	idleDeadline := time.After(time.Second)
	idleTick := time.NewTicker(5 * time.Millisecond)
	defer idleTick.Stop()
idle:
	for {
		select {
		case <-idleDeadline:
			t.Fatal("idle consumer stopped polling")
		case <-idleTick.C:
			q.mu.Lock()
			leases := q.leaseCount
			q.mu.Unlock()
			if leases >= 3 {
				break idle
			}
		}
	}
	q.mu.Lock()
	q.pending = append(q.pending, &contracts.Task{ID: 99, TaskClass: "A"})
	q.mu.Unlock()
	deadline := time.After(time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("task enqueued after idle was not completed within one second")
		case <-tick.C:
			q.mu.Lock()
			completed := len(q.completed) == 1 && q.completed[0] == 99
			q.mu.Unlock()
			if completed {
				return
			}
		}
	}
}

// A long-running task must occupy only its own slot, and graceful shutdown
// must drain accepted tasks without cancelling their ownership heartbeat.
func TestConsumerRefillsFreeSlotAndDrainsOnStop(t *testing.T) {
	q, client := newFakeQueue(t, []*contracts.Task{{ID: 1, TaskClass: "A"}, {ID: 2, TaskClass: "A"}, {ID: 3, TaskClass: "A"}})
	release := make(chan struct{})
	longStarted := make(chan struct{})
	thirdStarted := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	registry := NewRegistry()
	registry.Register("A", func(ctx context.Context, task *contracts.Task, _ json.RawMessage) error {
		switch task.ID {
		case 1:
			close(longStarted)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		case 3:
			close(thirdStarted)
		}
		return nil
	})
	cfg := testConfig()
	cfg.MaxWorkers = 2
	cfg.LeaseLimit = 10
	consumer := NewConsumer(client, registry, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); consumer.Run(ctx) }()
	select {
	case <-longStarted:
	case <-time.After(time.Second):
		t.Fatal("long task not started")
	}
	select {
	case <-thirdStarted:
	case <-time.After(time.Second):
		t.Fatal("free slot waited for the long task's batch")
	}
	q.mu.Lock()
	limit := q.lastLease.Limit
	q.mu.Unlock()
	if limit > cfg.MaxWorkers {
		t.Fatalf("leased beyond idle capacity: %d", limit)
	}
	cancel()
	select {
	case <-done:
		t.Fatal("shutdown abandoned its accepted long task")
	case <-time.After(50 * time.Millisecond):
	}
	q.mu.Lock()
	leases := q.leaseCount
	q.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	q.mu.Lock()
	after := q.leaseCount
	q.mu.Unlock()
	if after != leases {
		t.Fatal("shutdown continued leasing")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drained consumer did not stop")
	}
	if consumer.Stats().Processed != 3 {
		t.Fatalf("accepted tasks not finalized on drain: %+v", consumer.Stats())
	}
}

func TestClientRejectsLeaseBeyondCapacityAndNullTask(t *testing.T) {
	for _, data := range []string{`[{"id":1},{"id":2}]`, `[null]`} {
		t.Run(data, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":` + data + `}`))
			}))
			defer srv.Close()
			if _, err := NewClient(srv.URL, "").Lease(context.Background(), 1, nil); err == nil {
				t.Fatal("malformed lease accepted")
			}
		})
	}
}
