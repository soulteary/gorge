package cleanup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrConflict = errors.New("cleanup execution ownership changed")
var ErrRateLimited = errors.New("cleanup database batch budget busy")
var ErrBusy = errors.New("cleanup lease busy or not due")

const table = "gorge_gc_control"

// Schema is also pinned to the PHP migrations by the contract test.
const Schema = `CREATE TABLE gorge_gc_control (
 collectorID VARBINARY(64) NOT NULL PRIMARY KEY,
 owner VARBINARY(8) NOT NULL DEFAULT 'php',
 ownerEpoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
 fence BIGINT UNSIGNED NOT NULL DEFAULT 0,
 policyJSON LONGTEXT NOT NULL,
 policyHash VARBINARY(64) NOT NULL,
 leaseOwner VARBINARY(64) NOT NULL DEFAULT '',
 leaseExpires BIGINT UNSIGNED NOT NULL DEFAULT 0,
 nextRun BIGINT UNSIGNED NOT NULL DEFAULT 0,
 cycleCutoff BIGINT UNSIGNED NOT NULL DEFAULT 0,
 deletedRows BIGINT UNSIGNED NOT NULL DEFAULT 0,
 lastSuccess BIGINT UNSIGNED NOT NULL DEFAULT 0,
 lastState VARBINARY(32) NOT NULL DEFAULT 'idle',
 failures INT UNSIGNED NOT NULL DEFAULT 0,
 lastError LONGTEXT NOT NULL
) ENGINE=InnoDB`

type Store struct{ DBs map[string]*sql.DB }
type State struct {
	ID           string `json:"id"`
	Owner        string `json:"owner"`
	Epoch        uint64 `json:"ownerEpoch"`
	Fence        uint64 `json:"fence"`
	Policy       string `json:"-"`
	Hash         string `json:"policyHash"`
	LeaseOwner   string `json:"leaseOwner"`
	LeaseExpires int64  `json:"leaseExpires"`
	NextRun      int64  `json:"nextRun"`
	Cutoff       int64  `json:"cycleCutoff"`
	Deleted      int64  `json:"deletedRows"`
	LastSuccess  int64  `json:"lastSuccess"`
	Status       string `json:"status"`
	Failures     int    `json:"failures"`
	LastError    string `json:"lastError"`
}

const columns = "collectorID, owner, ownerEpoch, fence, policyJSON, policyHash, leaseOwner, leaseExpires, nextRun, cycleCutoff, deletedRows, lastSuccess, lastState, failures, lastError"

func scan(row interface{ Scan(...any) error }) (State, error) {
	var s State
	err := row.Scan(&s.ID, &s.Owner, &s.Epoch, &s.Fence, &s.Policy, &s.Hash, &s.LeaseOwner, &s.LeaseExpires, &s.NextRun, &s.Cutoff, &s.Deleted, &s.LastSuccess, &s.Status, &s.Failures, &s.LastError)
	return s, err
}
func (s *Store) db(id string) (*sql.DB, Spec, error) {
	spec, err := Lookup(id)
	if err != nil {
		return nil, spec, err
	}
	db := s.DBs[spec.Role]
	if db == nil {
		return nil, spec, fmt.Errorf("%s database not configured", spec.Role)
	}
	return db, spec, nil
}
func decodeState(st State) (Policy, error) {
	var p Policy
	if err := json.Unmarshal([]byte(st.Policy), &p); err != nil {
		return p, err
	}
	if p.ID != st.ID {
		return p, fmt.Errorf("policy identity mismatch")
	}
	h := sha256.Sum256([]byte(st.Policy))
	if hex.EncodeToString(h[:]) != st.Hash {
		return p, fmt.Errorf("policy hash mismatch")
	}
	return p, p.Validate()
}
func (s *Store) Status(ctx context.Context, id string) (State, error) {
	db, _, err := s.db(id)
	if err != nil {
		return State{}, err
	}
	return scan(db.QueryRowContext(ctx, "SELECT "+columns+" FROM "+table+" WHERE collectorID=?", id))
}
func now(ctx context.Context, tx *sql.Tx) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()").Scan(&n)
	return n, err
}
func lock(ctx context.Context, tx *sql.Tx, id string) (State, error) {
	return scan(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM "+table+" WHERE collectorID=? FOR UPDATE", id))
}

func (s *Store) Import(ctx context.Context, p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	db, _, err := s.db(p.ID)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	hash := hex.EncodeToString(h[:])
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "INSERT IGNORE INTO "+table+" (collectorID,policyJSON,policyHash,lastError) VALUES (?,?,?,'')", p.ID, string(b), hash)
	if err != nil {
		return err
	}
	st, err := lock(ctx, tx, p.ID)
	if err != nil {
		return err
	}
	if st.Status == "policy_changing" {
		return fmt.Errorf("PHP policy configuration change in progress; reconcile configuration before import")
	}
	if st.Owner != "php" && st.Owner != "paused" {
		return fmt.Errorf("pause before policy import")
	}
	_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET policyJSON=?,policyHash=?,ownerEpoch=ownerEpoch+1,fence=fence+1,leaseOwner='',leaseExpires=0,nextRun=0,lastState='idle',failures=0,lastError='' WHERE collectorID=?", string(b), hash, p.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) SetOwner(ctx context.Context, id, owner string) error {
	if owner != "paused" && owner != "php" && owner != "gorge" {
		return fmt.Errorf("invalid owner")
	}
	db, _, err := s.db(id)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := lock(ctx, tx, id)
	if err != nil {
		return err
	}
	if owner != "paused" && st.Owner != "paused" {
		return fmt.Errorf("pause and drain before ownership transfer")
	}
	if owner == "gorge" {
		if _, err := decodeState(st); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET owner=?,ownerEpoch=ownerEpoch+1,fence=fence+1,leaseOwner='',leaseExpires=0,nextRun=0,lastState=? WHERE collectorID=?", owner, owner, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type Lease struct {
	ID, Owner    string
	Epoch, Fence uint64
	Cutoff       int64
	Policy       Policy
}

func (s *Store) Claim(ctx context.Context, id, owner string, force bool) (Lease, error) {
	if owner == "" || len(owner) > 64 {
		return Lease{}, fmt.Errorf("invalid lease owner")
	}
	db, _, err := s.db(id)
	if err != nil {
		return Lease{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback()
	st, err := lock(ctx, tx, id)
	if err != nil {
		return Lease{}, err
	}
	p, err := decodeState(st)
	if err != nil {
		return Lease{}, err
	}
	n, err := now(ctx, tx)
	if err != nil {
		return Lease{}, err
	}
	if st.Owner != "gorge" {
		return Lease{}, ErrConflict
	}
	if p.Mode == "indefinite" || st.LeaseExpires > n || !force && st.NextRun > n {
		return Lease{}, ErrBusy
	}
	cutoff := n
	if p.Mode == "retention_seconds" {
		cutoff -= p.RetentionSeconds
	}
	if cutoff < 0 {
		cutoff = 0
	}
	_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET leaseOwner=?,leaseExpires=?,fence=fence+1,cycleCutoff=?,lastState='running' WHERE collectorID=?", owner, n+15, cutoff, id)
	if err != nil {
		return Lease{}, err
	}
	if err = tx.Commit(); err != nil {
		return Lease{}, err
	}
	return Lease{ID: id, Owner: owner, Epoch: st.Epoch, Fence: st.Fence + 1, Cutoff: cutoff, Policy: p}, nil
}
func checkLease(st State, l Lease, n int64) error {
	if st.Owner != "gorge" || st.Epoch != l.Epoch || st.Fence != l.Fence || st.LeaseOwner != l.Owner || st.LeaseExpires <= n || st.Cutoff != l.Cutoff {
		return ErrConflict
	}
	return nil
}
func (s *Store) Batch(ctx context.Context, l Lease, limit int) (int64, error) {
	if err := l.Policy.Validate(); err != nil {
		return 0, err
	}
	if limit < 1 || limit > l.Policy.BatchRows {
		return 0, fmt.Errorf("invalid batch limit")
	}
	db, spec, err := s.db(l.ID)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(l.Policy.BatchTimeoutMS)*time.Millisecond)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	st, err := lock(ctx, tx, l.ID)
	if err != nil {
		return 0, err
	}
	n, err := now(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err = checkLease(st, l, n); err != nil {
		return 0, err
	}
	p, err := decodeState(st)
	if err != nil {
		return 0, err
	}
	if p != l.Policy {
		return 0, ErrConflict
	}
	// One reserved control row coordinates every Gorge instance and collector
	// in this database. It is outside the fixed collector registry.
	if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO "+table+" (collectorID,owner,policyJSON,policyHash,lastError) VALUES ('__database_budget__','paused','{}','','')"); err != nil {
		return 0, err
	}
	var next, clock int64
	if err = tx.QueryRowContext(ctx, "SELECT cycleCutoff FROM "+table+" WHERE collectorID='__database_budget__' FOR UPDATE").Scan(&next); err != nil {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(NOW(6))*1000000 AS UNSIGNED)").Scan(&clock); err != nil {
		return 0, err
	}
	if next > clock {
		return 0, ErrRateLimited
	}
	if err = validatePlan(ctx, tx, spec, l.Cutoff, limit); err != nil {
		return 0, err
	}
	// The predicate is evaluated under the row locks taken by DELETE, so a cache
	// refreshed before the delete is not removed using stale selected IDs.
	result, err := tx.ExecContext(ctx, deleteSQL(spec), l.Cutoff, limit)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET deletedRows=deletedRows+?,leaseExpires=?,lastSuccess=?,failures=0,lastError='' WHERE collectorID=?", rows, n+15, n, l.ID)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE "+table+" SET cycleCutoff=CAST(UNIX_TIMESTAMP(NOW(6))*1000000 AS UNSIGNED)+200000 WHERE collectorID='__database_budget__'"); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return rows, nil
}
func (s *Store) Finish(ctx context.Context, l Lease, status string, cause error) error {
	if status != "exhausted" && status != "budget_exhausted" && status != "retryable_failure" {
		return fmt.Errorf("invalid finish status")
	}
	db, _, err := s.db(l.ID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := lock(ctx, tx, l.ID)
	if err != nil {
		return err
	}
	n, err := now(ctx, tx)
	if err != nil {
		return err
	}
	if err = checkLease(st, l, n); err != nil {
		return err
	}
	delay := l.Policy.IntervalSeconds
	failures := 0
	msg := ""
	if status == "budget_exhausted" {
		delay = 30
	}
	if cause != nil {
		failures = st.Failures + 1
		shift := failures
		if shift > 6 {
			shift = 6
		}
		delay = int64(1<<shift) * 5
		msg = "batch failed; inspect service logs"
	}
	_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET leaseOwner='',leaseExpires=0,nextRun=?,lastState=?,failures=?,lastError=? WHERE collectorID=?", n+delay, status, failures, msg, l.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) DryRun(ctx context.Context, id string) ([]uint64, error) {
	db, spec, err := s.db(id)
	if err != nil {
		return nil, err
	}
	st, err := s.Status(ctx, id)
	if err != nil {
		return nil, err
	}
	p, err := decodeState(st)
	if err != nil {
		return nil, err
	}
	if p.Mode == "indefinite" {
		return []uint64{}, nil
	}
	var n int64
	if err = db.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()").Scan(&n); err != nil {
		return nil, err
	}
	if p.Mode == "retention_seconds" {
		n -= p.RetentionSeconds
	}
	rows, err := db.QueryContext(ctx, "SELECT id FROM `"+spec.Table+"` WHERE "+predicate(spec)+" ORDER BY `"+spec.Column+"`,id LIMIT ?", n, p.BatchRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []uint64{}
	for rows.Next() {
		var id uint64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
