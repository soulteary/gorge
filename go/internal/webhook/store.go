package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// Store is everything this domain does with Phorge's herald database.
//
// It is an interface for one concrete reason: both HTTP endpoints read the
// database, so unlike the file storage domain — which has a local-disk backend
// its contract fixtures can run against — there is no configuration of this
// service that answers a request without one. Without the interface the
// contract fixtures and the handler tests would need a live MySQL, which the
// repository's test suite does not have and should not need. The in-memory
// implementation the tests inject is hand-written rather than a mocked driver;
// see memstore_test.go.
//
// It is also what keeps the SQL in one file. In the standalone service the
// hooks handler reached through Store.DB() and ran its own COUNT(*), which put
// a query in the HTTP layer and made the store's boundary decorative.
type Store interface {
	// Stats counts the requests by status and the hooks that are not
	// disabled.
	Stats(ctx context.Context) (*contracts.DeliveryStats, error)

	// CountHooks counts every configured hook, disabled ones included.
	CountHooks(ctx context.Context) (int64, error)

	// FetchClaimable lists requests that are eligible for a delivery attempt
	// now. The rows are candidates and nothing more: Claim is what decides
	// which of them this process may actually deliver.
	FetchClaimable(ctx context.Context, q ClaimQuery) ([]*Request, error)

	// Claim takes exclusive ownership of one candidate for the lease window,
	// reporting whether it was won.
	Claim(ctx context.Context, id int64, version int64) (bool, error)

	// GetWebhook loads a hook by PHID. A hook that does not exist is
	// (nil, nil): Phorge's garbage collection can retire a hook while
	// requests naming it are still queued, so this is an expected answer and
	// not an error.
	GetWebhook(ctx context.Context, phid string) (*Hook, error)

	// CountRecentFailures counts failed attempts against one hook since the
	// given epoch. It backs the per-hook circuit breaker; the threshold
	// comparison is the dispatcher's, so this stays a plain count.
	CountRecentFailures(ctx context.Context, hookPHID string, since int64) (int64, error)

	// UpdateResult writes the outcome of one attempt back to the request row.
	UpdateResult(ctx context.Context, id int64, out Outcome) error

	// Ready reports whether the database can be reached. It backs /readyz.
	Ready(ctx context.Context) error

	// Close releases the connection pool.
	Close() error
}

// ClaimQuery is the candidate query's two independent cutoffs.
//
// They are two conditions rather than one because they answer different
// questions with different time scales, and collapsing them would make each
// wrong. LeaseCutoff asks "is another attempt already under way", which is a
// question about the delivery timeout. FailCutoff asks "has this request
// waited long enough after failing", which is a question about how fast a
// broken receiver should be retried — 60 seconds, matching Phorge.
type ClaimQuery struct {
	Limit int
	// LeaseCutoff excludes rows whose dateModified is newer than it, unless
	// the row has never been modified at all. See fetchClaimableStmt.
	LeaseCutoff int64
	// FailCutoff excludes rows whose last attempt failed more recently than
	// it.
	FailCutoff int64
}

// newClaimQuery derives the cutoffs from one reading of the clock, so the two
// conditions describe the same instant.
func newClaimQuery(now int64, limit, leaseSec, retryBackoffSec int) ClaimQuery {
	return ClaimQuery{
		Limit:       limit,
		LeaseCutoff: now - int64(leaseSec),
		FailCutoff:  now - int64(retryBackoffSec),
	}
}

// requestColumns is the row shape scanRequest expects.
const requestColumns = `id, phid, webhookPHID, objectPHID, status, properties,
		        lastRequestResult, lastRequestEpoch, dateCreated, dateModified`

// fetchClaimableStmt selects the requests that may be attempted now.
//
// The shape of the WHERE clause is deliberate in three places:
//
// **`status = ?` first, then `ORDER BY id`.** Phorge ships no index on
// `status`, so this was a full table scan against a table whose `sent` rows
// are kept for seven days by garbage collection — it grows with traffic and
// never shrinks to nothing. Phase B of the migration adds `key_status
// (status, id)`; written this way the query uses it for both the equality and
// the ordering, and adding the index changes nothing about the results.
//
// **`dateModified = dateCreated OR dateModified <= ?`** is the lease. A
// claimed row is left alone until the lease expires, because the claim cannot
// change `status` — Phorge owns that column's values — so "being delivered"
// and "waiting" look alike from here. The first half of the condition is what
// keeps a *fresh* row from waiting out a lease it was never under: Lisk sets
// dateCreated and dateModified to the same second on insert
// (LiskDAO::willSaveObject), and every write this service makes afterwards
// leaves dateModified strictly greater, so equality means "untouched". If that
// ever stopped holding the condition would merely stop matching, which costs
// latency and not correctness.
//
// **`lastRequestResult <> ? OR lastRequestEpoch <= ?`** is the retry backoff,
// and it is separate from the lease on purpose: a request whose delivery
// failed waits the full backoff, while one whose claim was abandoned by a dead
// process waits only the lease.
const fetchClaimableStmt = `SELECT ` + requestColumns + `
		 FROM herald_webhookrequest
		 WHERE status = ?
		   AND (dateModified = dateCreated OR dateModified <= ?)
		   AND (lastRequestResult <> ? OR lastRequestEpoch <= ?)
		 ORDER BY id ASC
		 LIMIT ?`

// claimStmt takes ownership of one candidate row.
//
// `dateModified` is the optimistic version. It is the right column and the
// only available one: Lisk manages it, Phorge's UI never shows it, the herald
// garbage collector judges rows by `dateCreated` alone, and once phase B's
// configuration guard is in place this service is its only writer. Claiming
// therefore needs no new column, no long-running transaction, and no `SELECT
// … FOR UPDATE SKIP LOCKED` — which would have required MySQL 8.0 or MariaDB
// 10.6, versions a Phorge deployment cannot be assumed to be on.
//
// `GREATEST(dateModified + 1, UNIX_TIMESTAMP())` rather than a plain
// `UNIX_TIMESTAMP()` is what makes the version change every time. The column
// has one-second resolution, so a row inserted and claimed within the same
// second would be written back with the value it already held — leaving the
// WHERE clause still true for a second claimant, which is the entire failure
// this statement exists to prevent. The `+ 1` also means a burst of claims on
// one row cannot stall: the value advances even while the clock does not.
//
// `status = ?` is in the WHERE as well as the id and the version, so a row
// that reached a terminal state between the candidate query and here cannot be
// claimed at all.
const claimStmt = `UPDATE herald_webhookrequest
		 SET dateModified = GREATEST(dateModified + 1, UNIX_TIMESTAMP())
		 WHERE id = ? AND status = ? AND dateModified = ?`

// MySQLStore is the Store backed by Phorge's herald database.
type MySQLStore struct {
	db *sql.DB
}

// NewMySQLStore opens the pool. It does not dial; see OpenDB.
func NewMySQLStore(cfg *Config) (*MySQLStore, error) {
	db, err := OpenDB(cfg.HeraldDSN())
	if err != nil {
		return nil, err
	}
	return &MySQLStore{db: db}, nil
}

func (s *MySQLStore) Close() error { return s.db.Close() }

// Ready pings the database and nothing more.
//
// In particular it does not check that herald_webhookrequest exists. That
// table is created by Phorge's own `bin/storage upgrade`, and the container
// that runs it may start after this one, so requiring the table would report a
// healthy deployment as broken for as long as its first migration takes.
func (s *MySQLStore) Ready(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

func (s *MySQLStore) FetchClaimable(ctx context.Context, q ClaimQuery) ([]*Request, error) {
	rows, err := s.db.QueryContext(ctx, fetchClaimableStmt,
		StatusQueued, q.LeaseCutoff, ResultFail, q.FailCutoff, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("query claimable requests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var requests []*Request
	for rows.Next() {
		r := &Request{}
		if err := rows.Scan(
			&r.ID, &r.PHID, &r.WebhookPHID, &r.ObjectPHID, &r.Status,
			&r.Properties, &r.LastRequestResult, &r.LastRequestEpoch,
			&r.DateCreated, &r.DateModified,
		); err != nil {
			return nil, fmt.Errorf("scan request: %w", err)
		}
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

// Claim reports whether this process won the row.
//
// Only RowsAffected() == 1 counts. Zero means someone else advanced the
// version, or the row is no longer queued, and either way this process must
// not deliver it — a second POST is a duplicate the receiver has no way to
// recognise as one.
func (s *MySQLStore) Claim(ctx context.Context, id int64, version int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, claimStmt, id, StatusQueued, version)
	if err != nil {
		return false, fmt.Errorf("claim request %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim request %d: %w", id, err)
	}
	return affected == 1, nil
}

func (s *MySQLStore) GetWebhook(ctx context.Context, phid string) (*Hook, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, phid, name, webhookURI, status, hmacKey
		 FROM herald_webhook
		 WHERE phid = ?`,
		phid)

	h := &Hook{}
	err := row.Scan(&h.ID, &h.PHID, &h.Name, &h.WebhookURI, &h.Status, &h.HmacKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan webhook %s: %w", phid, err)
	}
	return h, nil
}

// CountRecentFailures uses Phorge's own key_ratelimit index, whose columns are
// exactly the three this query filters on.
func (s *MySQLStore) CountRecentFailures(ctx context.Context, hookPHID string, since int64) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM herald_webhookrequest
		 WHERE webhookPHID = ?
		   AND lastRequestResult = ?
		   AND lastRequestEpoch >= ?`,
		hookPHID, ResultFail, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count recent failures for %s: %w", hookPHID, err)
	}
	return count, nil
}

// UpdateResult writes the attempt's outcome.
//
// The whole properties column is rewritten, which is why RequestProperties has
// to round-trip the keys this service does not read: Phorge's UI shows
// transactionPHIDs and triggerPHIDs from it, and dropping them would empty the
// request's detail page.
//
// dateModified is set to the current time, which also ends the claim — the row
// is in a terminal state, or back in the queue with a fresh lastRequestEpoch
// for the retry backoff to measure from.
func (s *MySQLStore) UpdateResult(ctx context.Context, id int64, out Outcome) error {
	propsJSON, err := json.Marshal(out.Properties)
	if err != nil {
		return fmt.Errorf("marshal properties: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE herald_webhookrequest
		 SET status = ?, lastRequestResult = ?, lastRequestEpoch = ?,
		     properties = ?, dateModified = ?
		 WHERE id = ?`,
		out.Status, out.RequestResult, out.Epoch, string(propsJSON),
		time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("update request %d: %w", id, err)
	}
	return nil
}

// Stats runs one COUNT per status rather than a single grouped query. Each is
// an index range scan once phase B's key_status exists, and a failure names
// which count could not be produced.
func (s *MySQLStore) Stats(ctx context.Context) (*contracts.DeliveryStats, error) {
	stats := &contracts.DeliveryStats{}

	for _, c := range []struct {
		status string
		into   *int64
	}{
		{StatusQueued, &stats.QueuedCount},
		{StatusSent, &stats.SentCount},
		{StatusFailed, &stats.FailedCount},
	} {
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM herald_webhookrequest WHERE status = ?`,
			c.status).Scan(c.into)
		if err != nil {
			return nil, fmt.Errorf("count %s requests: %w", c.status, err)
		}
	}

	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM herald_webhook WHERE status <> ?`,
		HookStatusDisabled).Scan(&stats.ActiveWebhooks)
	if err != nil {
		return nil, fmt.Errorf("count active webhooks: %w", err)
	}
	return stats, nil
}

func (s *MySQLStore) CountHooks(ctx context.Context) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM herald_webhook`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count webhooks: %w", err)
	}
	return count, nil
}
