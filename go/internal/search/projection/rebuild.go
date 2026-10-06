package projection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

var ErrRebuildConflict = errors.New("rebuild job identity conflict")

const RebuildSchema = `CREATE TABLE IF NOT EXISTS search_projection_rebuild (
 namespace VARBINARY(64) NOT NULL, jobID VARBINARY(64) NOT NULL,
 backendID VARBINARY(64) NOT NULL, generationID VARBINARY(64) NOT NULL,
 upperPHID VARBINARY(64) NOT NULL, cursorPHID VARBINARY(64) NOT NULL DEFAULT '',
 status VARBINARY(32) NOT NULL DEFAULT 'scanning', pages BIGINT UNSIGNED NOT NULL DEFAULT 0,
 leaseOwner VARBINARY(64) NOT NULL DEFAULT '', leaseEpoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
 leaseExpires BIGINT NOT NULL DEFAULT 0, nextAttempt BIGINT NOT NULL DEFAULT 0,
 createdEpoch BIGINT NOT NULL,
 PRIMARY KEY(namespace,jobID), KEY key_pending(namespace,status,nextAttempt,leaseExpires)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_rebuild_check (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 namespace VARBINARY(64) NOT NULL, jobID VARBINARY(64) NOT NULL,
 knownHeads BIGINT UNSIGNED NOT NULL, unappliedHeads BIGINT UNSIGNED NOT NULL,
 transportCaughtUp BOOLEAN NOT NULL, observedEpoch BIGINT NOT NULL,
 KEY key_job(namespace,jobID,id)
) ENGINE=InnoDB;`

type RebuildJob struct {
	Target
	Namespace              string `json:"namespace"`
	JobID                  string `json:"jobID"`
	UpperPHID              string `json:"upperPHID"`
	CursorPHID             string `json:"cursorPHID"`
	Status                 string `json:"status"`
	Pages                  uint64 `json:"pages"`
	SourceCoverageVerified bool   `json:"sourceCoverageVerified"`
}
type RebuildCheck struct {
	Job               *RebuildJob `json:"job"`
	KnownHeads        uint64      `json:"knownHeads"`
	UnappliedHeads    uint64      `json:"unappliedHeads"`
	TransportCaughtUp bool        `json:"transportCaughtUp"`
	ActivationAllowed bool        `json:"activationAllowed"`
	ObservedEpoch     int64       `json:"observedEpoch"`
	CheckID           int64       `json:"checkID"`
}

func validateJob(namespace, id string, target Target) error {
	if err := ValidateNamespace(namespace); err != nil {
		return err
	}
	if !namespacePattern.MatchString(id) {
		return fmt.Errorf("invalid rebuild job ID")
	}
	return ValidateTargets([]Target{target})
}

// CreateRebuild captures a bounded head range. It is not a business-source scan.
// A target must already be immutably bound to an actual physical generation.
func (s *MySQLStore) CreateRebuild(ctx context.Context, namespace, id string, target Target) (*RebuildJob, error) {
	if err := validateJob(namespace, id, target); err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var bound string
	if err = tx.QueryRowContext(ctx, `SELECT configHash FROM search_projection_target WHERE namespace=? AND backendID=? AND generationID=?`, namespace, target.BackendID, target.GenerationID).Scan(&bound); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO search_projection_rebuild(namespace,jobID,backendID,generationID,upperPHID,createdEpoch) SELECT ?,?,?,?,COALESCE(MAX(phid),''),UNIX_TIMESTAMP() FROM search_projection_head WHERE namespace=? ON DUPLICATE KEY UPDATE jobID=jobID`, namespace, id, target.BackendID, target.GenerationID, namespace)
	if err != nil {
		return nil, err
	}
	job, err := loadRebuild(ctx, tx, namespace, id)
	if err != nil {
		return nil, err
	}
	if job.Target != target {
		return nil, ErrRebuildConflict
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadRebuild(ctx context.Context, q rowQuerier, namespace, id string) (*RebuildJob, error) {
	j := &RebuildJob{Namespace: namespace, JobID: id}
	err := q.QueryRowContext(ctx, `SELECT backendID,generationID,upperPHID,cursorPHID,status,pages FROM search_projection_rebuild WHERE namespace=? AND jobID=?`, namespace, id).Scan(&j.BackendID, &j.GenerationID, &j.UpperPHID, &j.CursorPHID, &j.Status, &j.Pages)
	return j, err
}
func (s *MySQLStore) RebuildJob(ctx context.Context, namespace, id string) (*RebuildJob, error) {
	if err := ValidateNamespace(namespace); err != nil {
		return nil, err
	}
	if !namespacePattern.MatchString(id) {
		return nil, fmt.Errorf("invalid job ID")
	}
	return loadRebuild(ctx, s.DB, namespace, id)
}

// StepRebuild persists the cursor only after a complete accepted page. Crashes
// replay stable event identities; lease expiry fences cursor/status updates.
func (s *MySQLStore) StepRebuild(ctx context.Context, namespace, id, owner string) error {
	if err := ValidateNamespace(namespace); err != nil {
		return err
	}
	if !namespacePattern.MatchString(id) || !namespacePattern.MatchString(owner) {
		return fmt.Errorf("invalid rebuild identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var lockedID string
	if err = tx.QueryRowContext(ctx, `SELECT jobID FROM search_projection_rebuild WHERE namespace=? AND jobID=? FOR UPDATE`, namespace, id).Scan(&lockedID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE search_projection_rebuild SET leaseOwner=?,leaseEpoch=leaseEpoch+1,leaseExpires=UNIX_TIMESTAMP()+30 WHERE namespace=? AND jobID=? AND leaseExpires<=UNIX_TIMESTAMP() AND nextAttempt<=UNIX_TIMESTAMP() AND status IN ('scanning','awaiting-delivery')`, owner, namespace, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	var epoch uint64
	if err = tx.QueryRowContext(ctx, `SELECT leaseEpoch FROM search_projection_rebuild WHERE namespace=? AND jobID=?`, namespace, id).Scan(&epoch); err != nil {
		return err
	}
	// Read progress after claiming the lease; the row remains locked by this transaction.
	j, err := loadRebuild(ctx, tx, namespace, id)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			report, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = s.DB.ExecContext(report, `UPDATE search_projection_rebuild SET leaseOwner='',leaseExpires=0,nextAttempt=UNIX_TIMESTAMP()+30 WHERE namespace=? AND jobID=? AND leaseOwner=? AND leaseEpoch=?`, namespace, id, owner, epoch)
		}
	}()
	var rows *sql.Rows
	if j.Status == "scanning" {
		rows, err = s.DB.QueryContext(ctx, `SELECT phid,envelope FROM search_projection_head WHERE namespace=? AND phid>? AND phid<=? ORDER BY phid LIMIT 32`, namespace, j.CursorPHID, j.UpperPHID)
	} else {
		rows, err = s.DB.QueryContext(ctx, `SELECT h.phid,h.envelope FROM search_projection_head h LEFT JOIN search_projection_delivery d ON d.namespace=h.namespace AND d.phid=h.phid AND d.revision=h.revision AND d.backendID=? AND d.generationID=? WHERE h.namespace=? AND d.phid IS NULL ORDER BY h.phid LIMIT 32`, j.BackendID, j.GenerationID, namespace)
	}
	if err != nil {
		return err
	}
	type entry struct{ phid, raw string }
	var batch []entry
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.phid, &e.raw); err != nil {
			_ = rows.Close()
			return err
		}
		batch = append(batch, e)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	cursor := j.CursorPHID
	for _, e := range batch {
		event, eErr := Decode([]byte(e.raw))
		if eErr != nil {
			return eErr
		}
		if event.Namespace != namespace || event.PHID != e.phid {
			return fmt.Errorf("rebuild head identity mismatch")
		}
		sum := sha256.Sum256([]byte(namespace + "\x00" + id + "\x00" + e.phid + "\x00" + event.Revision))
		event.EventID = "rebuild/" + hex.EncodeToString(sum[:])
		if _, err = s.Accept(ctx, event, []Target{j.Target}); err != nil {
			return err
		}
		cursor = e.phid
	}
	status := j.Status
	if j.Status == "scanning" && len(batch) < 32 {
		status = "awaiting-delivery"
	}
	if j.Status != "scanning" {
		cursor = j.CursorPHID
	}
	delay := 1
	if status == "awaiting-delivery" {
		delay = 60
	}
	result, err = s.DB.ExecContext(ctx, `UPDATE search_projection_rebuild SET cursorPHID=?,status=?,pages=pages+1,leaseOwner='',leaseExpires=0,nextAttempt=UNIX_TIMESTAMP()+? WHERE namespace=? AND jobID=? AND leaseOwner=? AND leaseEpoch=? AND leaseExpires>UNIX_TIMESTAMP()`, cursor, status, delay, namespace, id, owner, epoch)
	if err != nil {
		return err
	}
	n, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLeaseLost
	}
	finished = true
	return nil
}

// CheckRebuild uses one repeatable-read snapshot of heads and delivery receipts.
// Applied does not prove backend refresh, source completeness or query equality.
func (s *MySQLStore) CheckRebuild(ctx context.Context, namespace, id string) (*RebuildCheck, error) {
	if err := ValidateNamespace(namespace); err != nil {
		return nil, err
	}
	if !namespacePattern.MatchString(id) {
		return nil, fmt.Errorf("invalid job ID")
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	j, err := loadRebuild(ctx, tx, namespace, id)
	if err != nil {
		return nil, err
	}
	c := &RebuildCheck{Job: j}
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(d.status IS NULL OR d.status<>'applied'),0) FROM search_projection_head h LEFT JOIN search_projection_delivery d ON d.namespace=h.namespace AND d.phid=h.phid AND d.revision=h.revision AND d.backendID=? AND d.generationID=? WHERE h.namespace=?`, j.BackendID, j.GenerationID, namespace).Scan(&c.KnownHeads, &c.UnappliedHeads)
	if err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT UNIX_TIMESTAMP()`).Scan(&c.ObservedEpoch); err != nil {
		return nil, err
	}
	c.TransportCaughtUp = j.Status == "awaiting-delivery" && c.UnappliedHeads == 0
	result, err := tx.ExecContext(ctx, `INSERT INTO search_projection_rebuild_check(namespace,jobID,knownHeads,unappliedHeads,transportCaughtUp,observedEpoch) VALUES(?,?,?,?,?,?)`, namespace, id, c.KnownHeads, c.UnappliedHeads, c.TransportCaughtUp, c.ObservedEpoch)
	if err != nil {
		return nil, err
	}
	c.CheckID, err = result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return c, nil
}

// RunRebuilds bounds each pass and fairly rotates jobs through nextAttempt.
func (s *MySQLStore) RunRebuilds(ctx context.Context, namespace, owner string, targets []Target) {
	if ValidateNamespace(namespace) != nil || ValidateTargets(targets) != nil || !namespacePattern.MatchString(owner) {
		return
	}
	filters := make([]string, 0, len(targets))
	args := []any{namespace}
	for _, target := range targets {
		filters = append(filters, "(backendID=? AND generationID=?)")
		args = append(args, target.BackendID, target.GenerationID)
	}
	query := `SELECT jobID FROM search_projection_rebuild WHERE namespace=? AND status IN ('scanning','awaiting-delivery') AND leaseExpires<=UNIX_TIMESTAMP() AND nextAttempt<=UNIX_TIMESTAMP() AND (` + strings.Join(filters, " OR ") + `) ORDER BY nextAttempt,jobID LIMIT 1`
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		var id string
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.DB.QueryRowContext(pollCtx, query, args...).Scan(&id)
		cancel()
		if err == nil {
			err = s.StepRebuild(ctx, namespace, id, owner)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
			slog.Error("search rebuild pending", "jobID", id)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
