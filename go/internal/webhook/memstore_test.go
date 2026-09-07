package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// memStore is an in-memory Store. It exists because every endpoint in this
// domain reads the database and the poll loop *is* a database loop, so without
// it neither the handlers nor the claim could be exercised without a live
// MySQL.
//
// It is hand-written rather than a mocked driver, which is the repository's
// habit (see filestorage's memEngine) and here it earns its keep: a driver
// mock asserts which SQL was sent, and what these tests need to assert is what
// the claim *means* — that a second attempt on a row already claimed does not
// deliver it.
//
// The cost of that choice is stated plainly: FetchClaimable and Claim below
// reimplement the predicates in store.go's statements, so a change to one that
// is not made to the other leaves the tests passing against behaviour the
// database does not have. That gap is covered from the other side by
// store_test.go, which pins the statements themselves.
type memStore struct {
	mu sync.Mutex

	// now is the store's clock, in Unix seconds. Tests move it by hand; the
	// claim's GREATEST semantics are only observable with a clock that does
	// not advance on its own.
	now int64

	requests []*Request
	hooks    map[string]*Hook

	// updates records every result write, in order, so a test can assert what
	// a delivery concluded as well as what it did.
	updates []recordedUpdate

	// claimAttempts counts calls to Claim, won or lost.
	claimAttempts int

	// Injected failures. Each stands in for the database refusing one
	// operation, which is the case the poll loop has to survive rather than
	// act on.
	fetchErr    error
	claimErr    error
	hookErr     error
	failuresErr error
	updateErr   error
	statsErr    error
	countErr    error
	readyErr    error

	closed bool
}

type recordedUpdate struct {
	ID      int64
	Outcome Outcome
}

func newMemStore(now int64) *memStore {
	return &memStore{now: now, hooks: map[string]*Hook{}}
}

// addHook registers a hook the requests can point at.
func (m *memStore) addHook(h *Hook) *memStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks[h.PHID] = h
	return m
}

// addRequest seeds a queued request. dateCreated and dateModified are set
// equal, which is how Lisk writes a fresh row and what makes it claimable
// without waiting out a lease.
//
// It returns a *copy*, deliberately: what a caller holds is a candidate it
// fetched, and a claim by anyone else has to be able to make that copy stale.
// Handing back the live row would make every test that claims one silently
// hold the winner's version.
func (m *memStore) addRequest(id int64, hookPHID string, props RequestProperties) *Request {
	encoded, err := json.Marshal(props)
	if err != nil {
		panic(err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r := &Request{
		ID:                id,
		PHID:              fmt.Sprintf("PHID-HWRQ-request%04d", id),
		WebhookPHID:       hookPHID,
		ObjectPHID:        "PHID-TASK-abcdefghijklmnopqrst",
		Status:            StatusQueued,
		Properties:        string(encoded),
		LastRequestResult: ResultNone,
		DateCreated:       m.now,
		DateModified:      m.now,
	}
	m.requests = append(m.requests, r)

	copied := *r
	return &copied
}

// setRawProperties replaces the properties column with bytes of the caller's
// choosing, which is the only way to seed a document that does not parse.
func (m *memStore) setRawProperties(id int64, raw string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests {
		if r.ID == id {
			r.Properties = raw
		}
	}
}

// clock is the store's view of the time, for a dispatcher under test to share.
func (m *memStore) clock() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// row returns the store's own copy, for assertions about what a delivery left
// behind.
func (m *memStore) row(id int64) *Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests {
		if r.ID == id {
			copied := *r
			return &copied
		}
	}
	return nil
}

func (m *memStore) advance(seconds int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now += seconds
}

func (m *memStore) recorded() []recordedUpdate {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedUpdate(nil), m.updates...)
}

func (m *memStore) claimCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claimAttempts
}

// FetchClaimable mirrors fetchClaimableStmt, including both halves of each OR.
func (m *memStore) FetchClaimable(_ context.Context, q ClaimQuery) ([]*Request, error) {
	if m.fetchErr != nil {
		return nil, m.fetchErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	ordered := append([]*Request(nil), m.requests...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	var out []*Request
	for _, r := range ordered {
		if r.Status != StatusQueued {
			continue
		}
		underLease := r.DateModified != r.DateCreated && r.DateModified > q.LeaseCutoff
		if underLease {
			continue
		}
		inBackoff := r.LastRequestResult == ResultFail && r.LastRequestEpoch > q.FailCutoff
		if inBackoff {
			continue
		}
		// A copy, so a caller mutating what it fetched cannot rewrite the
		// store the way a real one could not.
		copied := *r
		out = append(out, &copied)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// Claim mirrors claimStmt: the compare-and-set on dateModified, and the
// GREATEST that guarantees the version changes even within one second.
func (m *memStore) Claim(_ context.Context, id int64, version int64) (bool, error) {
	m.mu.Lock()
	m.claimAttempts++
	m.mu.Unlock()

	if m.claimErr != nil {
		return false, m.claimErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.requests {
		if r.ID != id || r.Status != StatusQueued || r.DateModified != version {
			continue
		}
		bumped := r.DateModified + 1
		if m.now > bumped {
			bumped = m.now
		}
		r.DateModified = bumped
		return true, nil
	}
	return false, nil
}

func (m *memStore) GetWebhook(_ context.Context, phid string) (*Hook, error) {
	if m.hookErr != nil {
		return nil, m.hookErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	h, ok := m.hooks[phid]
	if !ok {
		return nil, nil
	}
	copied := *h
	return &copied, nil
}

func (m *memStore) CountRecentFailures(_ context.Context, hookPHID string, since int64) (int64, error) {
	if m.failuresErr != nil {
		return 0, m.failuresErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var count int64
	for _, r := range m.requests {
		if r.WebhookPHID == hookPHID && r.LastRequestResult == ResultFail && r.LastRequestEpoch >= since {
			count++
		}
	}
	return count, nil
}

func (m *memStore) UpdateResult(_ context.Context, id int64, out Outcome) error {
	if m.updateErr != nil {
		return m.updateErr
	}

	encoded, err := json.Marshal(out.Properties)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.updates = append(m.updates, recordedUpdate{ID: id, Outcome: out})
	for _, r := range m.requests {
		if r.ID != id {
			continue
		}
		r.Status = out.Status
		r.LastRequestResult = out.RequestResult
		r.LastRequestEpoch = out.Epoch
		r.Properties = string(encoded)
		r.DateModified = m.now
	}
	return nil
}

func (m *memStore) Stats(_ context.Context) (*contracts.DeliveryStats, error) {
	if m.statsErr != nil {
		return nil, m.statsErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	stats := &contracts.DeliveryStats{}
	for _, r := range m.requests {
		switch r.Status {
		case StatusQueued:
			stats.QueuedCount++
		case StatusSent:
			stats.SentCount++
		case StatusFailed:
			stats.FailedCount++
		}
	}
	for _, h := range m.hooks {
		if !h.Disabled() {
			stats.ActiveWebhooks++
		}
	}
	return stats, nil
}

func (m *memStore) CountHooks(_ context.Context) (int64, error) {
	if m.countErr != nil {
		return 0, m.countErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.hooks)), nil
}

func (m *memStore) Ready(_ context.Context) error { return m.readyErr }

func (m *memStore) Close() error {
	m.closed = true
	return nil
}

// errStoreDown is what an injected failure reports. The message is
// recognisable on purpose: a test asserts it does not reach a response body.
var errStoreDown = errors.New("herald database is unreachable: dial tcp 10.0.0.9:3306: connect: connection refused")

// quietLogs keeps the dispatcher's logging out of the test output, and fails
// the test if a request was answered by way of a recovered panic — a test
// asserting only a status code cannot tell the difference on its own.
func quietLogs(t *testing.T) *syncBuffer {
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
	return logs
}

// syncBuffer collects log output. The lock is for the delivery goroutines,
// which log while the test body reads.
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
