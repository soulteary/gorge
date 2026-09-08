package taskqueue

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// memStore is an in-memory Store. It exists for the same reason the webhook
// domain's does: every endpoint here touches the store and there is no live
// MySQL in CI, so without it neither the handlers nor the contract fixtures
// could run. It is hand-written rather than a driver mock because what these
// tests assert is what the operations *mean* — that a leased task is not leased
// twice, that completing a task moves it to the archive — not which SQL was
// sent. The MySQL statements themselves are the untested seam, stated plainly:
// a change to mysql_store.go not mirrored here leaves these tests green against
// behaviour the database does not have.
//
// Both the MySQL and Redis backends reproduce this behaviour; this is the
// specification they are written against.
type memStore struct {
	mu sync.Mutex

	// now is the store's clock in Unix seconds. Tests move it by hand so the
	// lease-expiry and yield windows are observable without real time passing.
	now int64

	nextTaskID int64
	nextDataID int64

	active   map[int64]*contracts.Task
	archived []*contracts.ArchivedTask
	data     map[int64]string

	leaseDuration int
	retryWait     int

	// Injected failures, each standing in for the backend refusing one
	// operation — the case /readyz and the 5xx path have to survive.
	enqueueErr error
	leaseErr   error
	statsErr   error
	getErr     error
	listErr    error
	readyErr   error

	closed bool
}

func newMemStore(now int64) *memStore {
	return &memStore{
		now:           now,
		active:        map[int64]*contracts.Task{},
		data:          map[int64]string{},
		leaseDuration: 7200,
		retryWait:     300,
	}
}

func (m *memStore) advance(seconds int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now += seconds
}

// activeTask returns the store's own copy of a task, for assertions.
func (m *memStore) activeTask(id int64) *contracts.Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.active[id]
	if !ok {
		return nil
	}
	copied := *t
	return &copied
}

func (m *memStore) archivedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.archived)
}

func (m *memStore) Enqueue(_ context.Context, req *contracts.EnqueueRequest) (*contracts.Task, error) {
	if m.enqueueErr != nil {
		return nil, m.enqueueErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	priority := contracts.PriorityDefault
	if req.Priority != nil {
		priority = *req.Priority
	}

	m.nextDataID++
	dataID := m.nextDataID
	m.data[dataID] = req.Data

	m.nextTaskID++
	taskID := m.nextTaskID

	var leaseExpires *int64
	if req.DelayUntil != nil && *req.DelayUntil > 0 {
		leaseExpires = req.DelayUntil
	}

	t := &contracts.Task{
		ID:            taskID,
		TaskClass:     req.TaskClass,
		DataID:        dataID,
		Priority:      priority,
		ObjectPHID:    req.ObjectPHID,
		ContainerPHID: req.ContainerPHID,
		DateCreated:   m.now,
		DateModified:  m.now,
		LeaseExpires:  leaseExpires,
	}
	m.active[taskID] = t

	copied := *t
	copied.Data = req.Data
	return &copied, nil
}

func (m *memStore) Lease(_ context.Context, limit int, leaseOwner string, taskClasses []string) ([]*contracts.Task, error) {
	if m.leaseErr != nil {
		return nil, m.leaseErr
	}
	if limit <= 0 {
		limit = 1
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	leaseExp := m.now + int64(m.leaseDuration)

	candidates := m.sortedActiveLocked()
	allowed := make(map[string]bool, len(taskClasses))
	for _, taskClass := range taskClasses {
		allowed[taskClass] = true
	}
	matches := func(t *contracts.Task) bool {
		return len(allowed) == 0 || allowed[t.TaskClass]
	}

	var leased []*contracts.Task
	// Phase 1: unleased tasks, ordered priority then id.
	for _, t := range candidates {
		if len(leased) >= limit {
			break
		}
		if matches(t) && t.LeaseOwner == "" && t.LeaseExpires == nil {
			t.LeaseOwner = leaseOwner
			t.LeaseExpires = &leaseExp
			t.DateModified = m.now
			leased = append(leased, m.copyWithData(t))
		}
	}
	// Phase 2: tasks whose lease expired.
	for _, t := range candidates {
		if len(leased) >= limit {
			break
		}
		if matches(t) && t.LeaseExpires != nil && *t.LeaseExpires < m.now {
			t.LeaseOwner = leaseOwner
			exp := leaseExp
			t.LeaseExpires = &exp
			t.DateModified = m.now
			leased = append(leased, m.copyWithData(t))
		}
	}
	return leased, nil
}

func (m *memStore) Complete(_ context.Context, taskID int64, duration int64) (*contracts.ArchivedTask, error) {
	return m.archive(taskID, contracts.ResultSuccess, duration)
}

func (m *memStore) Cancel(_ context.Context, taskID int64) (*contracts.ArchivedTask, error) {
	return m.archive(taskID, contracts.ResultCancelled, 0)
}

func (m *memStore) Fail(_ context.Context, req *contracts.FailRequest) error {
	if req.Permanent {
		_, err := m.archive(req.TaskID, contracts.ResultFailure, 0)
		return err
	}

	retryWait := m.retryWait
	if req.RetryWait != nil {
		retryWait = *req.RetryWait
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.active[req.TaskID]
	if !ok {
		return nil
	}
	t.FailureCount++
	ft := m.now
	t.FailureTime = &ft
	t.LeaseOwner = ""
	exp := m.now + int64(retryWait)
	t.LeaseExpires = &exp
	t.DateModified = m.now
	return nil
}

func (m *memStore) Yield(_ context.Context, taskID int64, duration int) error {
	if duration < 5 {
		duration = 5
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.active[taskID]
	if !ok {
		return nil
	}
	t.LeaseOwner = contracts.YieldOwner
	exp := m.now + int64(duration)
	t.LeaseExpires = &exp
	t.DateModified = m.now
	return nil
}

func (m *memStore) Awaken(_ context.Context, taskIDs []int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	epochAgo := m.now - 3600
	var count int64
	for _, id := range taskIDs {
		t, ok := m.active[id]
		if !ok {
			continue
		}
		if t.LeaseOwner == contracts.YieldOwner &&
			t.LeaseExpires != nil && *t.LeaseExpires > epochAgo &&
			t.FailureCount == 0 {
			exp := epochAgo
			t.LeaseExpires = &exp
			count++
		}
	}
	return count, nil
}

func (m *memStore) Stats(_ context.Context) (*contracts.QueueStats, error) {
	if m.statsErr != nil {
		return nil, m.statsErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	stats := &contracts.QueueStats{
		ActiveCount:   int64(len(m.active)),
		ArchivedCount: int64(len(m.archived)),
	}
	for _, t := range m.active {
		if t.LeaseOwner != "" && t.LeaseExpires != nil && *t.LeaseExpires >= m.now {
			stats.LeasedCount++
		}
		if t.FailureCount > 0 {
			stats.FailedCount++
		}
	}
	return stats, nil
}

func (m *memStore) GetTask(_ context.Context, taskID int64) (*contracts.Task, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.active[taskID]
	if !ok {
		return nil, nil
	}
	return m.copyWithData(t), nil
}

func (m *memStore) ListActive(_ context.Context, limit, offset int) ([]*contracts.Task, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	if limit <= 0 {
		limit = 100
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	ordered := m.sortedActiveLocked()
	var out []*contracts.Task
	for i := offset; i < len(ordered) && len(out) < limit; i++ {
		copied := *ordered[i]
		out = append(out, &copied)
	}
	return out, nil
}

func (m *memStore) Ready(_ context.Context) error { return m.readyErr }

func (m *memStore) Close() error {
	m.closed = true
	return nil
}

// archive moves an active task into the archive under the given result, the
// in-memory analogue of the MySQL backend's single transaction.
func (m *memStore) archive(taskID int64, result int, duration int64) (*contracts.ArchivedTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.active[taskID]
	if !ok {
		return nil, errors.New("select activetask: task not found")
	}
	delete(m.active, taskID)

	epoch := m.now
	at := &contracts.ArchivedTask{
		Task:          *t,
		Result:        result,
		Duration:      duration,
		ArchivedEpoch: &epoch,
	}
	m.archived = append(m.archived, at)

	copied := *at
	return &copied, nil
}

// sortedActiveLocked returns the live tasks ordered by priority then id, the
// order both backends lease and list in. The caller holds the lock.
func (m *memStore) sortedActiveLocked() []*contracts.Task {
	tasks := make([]*contracts.Task, 0, len(m.active))
	for _, t := range m.active {
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Priority != tasks[j].Priority {
			return tasks[i].Priority < tasks[j].Priority
		}
		return tasks[i].ID < tasks[j].ID
	})
	return tasks
}

func (m *memStore) copyWithData(t *contracts.Task) *contracts.Task {
	copied := *t
	copied.Data = m.data[t.DataID]
	return &copied
}

// errStoreDown is what an injected failure reports. The message is
// recognisable on purpose: a test asserts it does not reach a 5xx body.
var errStoreDown = errors.New("worker database is unreachable: dial tcp 10.0.0.9:3306: connect: connection refused")
