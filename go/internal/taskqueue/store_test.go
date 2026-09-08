package taskqueue

import (
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// These tests pin the queue's semantics against the in-memory store. They are
// the specification the MySQL and Redis backends are written to reproduce: a
// leased task is not leased again until its lease expires, an archived task
// leaves the active set, a temporary failure returns the task with an
// incremented failureCount, and awaken only pulls forward a yielded task.

func mustEnqueue(t *testing.T, m *memStore, class string, priority *int) *contracts.Task {
	t.Helper()
	task, err := m.Enqueue(t.Context(), &contracts.EnqueueRequest{
		TaskClass: class,
		Data:      `{"k":"v"}`,
		Priority:  priority,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return task
}

func TestEnqueueDefaultsAndData(t *testing.T) {
	m := newMemStore(1_000_000)
	task := mustEnqueue(t, m, "TestWorker", nil)

	if task.Priority != contracts.PriorityDefault {
		t.Errorf("priority = %d, want the default %d", task.Priority, contracts.PriorityDefault)
	}
	if task.DataID == 0 {
		t.Error("a task must carry a dataID")
	}
	if task.Data != `{"k":"v"}` {
		t.Errorf("enqueue must round-trip the payload, got %q", task.Data)
	}
}

func TestLeaseTakesEachTaskOnce(t *testing.T) {
	m := newMemStore(1_000_000)
	mustEnqueue(t, m, "A", nil)
	mustEnqueue(t, m, "B", nil)

	first, err := m.Lease(t.Context(), 10, "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("expected both tasks leased, got %d", len(first))
	}

	// A second lease before any expires must find nothing: the tasks are held.
	second, err := m.Lease(t.Context(), 10, "owner-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Errorf("a held task must not be leased twice, got %d", len(second))
	}
}

func TestLeaseOrderIsPriorityThenID(t *testing.T) {
	m := newMemStore(1_000_000)
	bulk := contracts.PriorityBulk
	alerts := contracts.PriorityAlerts
	low := mustEnqueue(t, m, "bulk", &bulk)
	high := mustEnqueue(t, m, "alerts", &alerts)

	leased, err := m.Lease(t.Context(), 1, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(leased) != 1 || leased[0].ID != high.ID {
		t.Fatalf("expected the higher-priority task %d first, got %+v", high.ID, leased)
	}
	_ = low
}

func TestExpiredLeaseIsLeasableAgain(t *testing.T) {
	m := newMemStore(1_000_000)
	task := mustEnqueue(t, m, "A", nil)

	if _, err := m.Lease(t.Context(), 1, "owner-1"); err != nil {
		t.Fatal(err)
	}
	// Move past the lease.
	m.advance(int64(m.leaseDuration) + 1)

	again, err := m.Lease(t.Context(), 1, "owner-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].ID != task.ID {
		t.Fatalf("an expired lease must be re-leasable, got %+v", again)
	}
	if again[0].LeaseOwner != "owner-2" {
		t.Errorf("re-lease must record the new owner, got %q", again[0].LeaseOwner)
	}
}

func TestCompleteArchivesTheTask(t *testing.T) {
	m := newMemStore(1_000_000)
	task := mustEnqueue(t, m, "A", nil)

	archived, err := m.Complete(t.Context(), task.ID, 1234)
	if err != nil {
		t.Fatal(err)
	}
	if archived.Result != contracts.ResultSuccess {
		t.Errorf("result = %d, want success", archived.Result)
	}
	if archived.Duration != 1234 {
		t.Errorf("duration = %d, want 1234", archived.Duration)
	}
	if m.activeTask(task.ID) != nil {
		t.Error("a completed task must leave the active set")
	}
	if m.archivedCount() != 1 {
		t.Errorf("archived count = %d, want 1", m.archivedCount())
	}
}

func TestTemporaryFailureRequeuesWithCount(t *testing.T) {
	m := newMemStore(1_000_000)
	task := mustEnqueue(t, m, "A", nil)

	if err := m.Fail(t.Context(), &contracts.FailRequest{TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	row := m.activeTask(task.ID)
	if row == nil {
		t.Fatal("a temporary failure must leave the task in the queue")
	}
	if row.FailureCount != 1 {
		t.Errorf("failureCount = %d, want 1", row.FailureCount)
	}
	if row.FailureTime == nil {
		t.Error("a failure must record failureTime")
	}
	if row.LeaseOwner != "" {
		t.Error("a failed task must be released to another worker")
	}
}

func TestPermanentFailureArchives(t *testing.T) {
	m := newMemStore(1_000_000)
	task := mustEnqueue(t, m, "A", nil)

	if err := m.Fail(t.Context(), &contracts.FailRequest{TaskID: task.ID, Permanent: true}); err != nil {
		t.Fatal(err)
	}
	if m.activeTask(task.ID) != nil {
		t.Error("a permanent failure must archive the task")
	}
	if m.archivedCount() != 1 {
		t.Errorf("archived count = %d, want 1", m.archivedCount())
	}
}

func TestYieldFloorsDurationAndSetsSentinel(t *testing.T) {
	m := newMemStore(1_000_000)
	task := mustEnqueue(t, m, "A", nil)

	if err := m.Yield(t.Context(), task.ID, 1); err != nil {
		t.Fatal(err)
	}
	row := m.activeTask(task.ID)
	if row.LeaseOwner != contracts.YieldOwner {
		t.Errorf("a yielded task must carry the yield sentinel, got %q", row.LeaseOwner)
	}
	if row.LeaseExpires == nil || *row.LeaseExpires < m.now+5 {
		t.Errorf("yield must floor the window at 5s, got %v", row.LeaseExpires)
	}
}

func TestAwakenOnlyPullsYieldedTasks(t *testing.T) {
	m := newMemStore(1_000_000)
	yielded := mustEnqueue(t, m, "A", nil)
	plain := mustEnqueue(t, m, "B", nil)

	if err := m.Yield(t.Context(), yielded.ID, 100); err != nil {
		t.Fatal(err)
	}

	n, err := m.Awaken(t.Context(), []int64{yielded.ID, plain.ID})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("awaken pulled %d tasks, want only the yielded one", n)
	}
}

func TestStatsCountsTheQueue(t *testing.T) {
	m := newMemStore(1_000_000)
	a := mustEnqueue(t, m, "A", nil)
	mustEnqueue(t, m, "B", nil)
	if _, err := m.Complete(t.Context(), a.ID, 0); err != nil {
		t.Fatal(err)
	}

	stats, err := m.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.ActiveCount != 1 {
		t.Errorf("activeCount = %d, want 1", stats.ActiveCount)
	}
	if stats.ArchivedCount != 1 {
		t.Errorf("archivedCount = %d, want 1", stats.ArchivedCount)
	}
}
