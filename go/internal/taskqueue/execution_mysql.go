package taskqueue

import (
	"context"
	"database/sql"
	"errors"
	"github.com/soulteary/gorge/go/internal/contracts"
	"time"
)

func lockExecution(ctx context.Context, tx *sql.Tx, l contracts.ExecutionLease) (*contracts.Task, error) {
	t := new(contracts.Task)
	err := scanTaskNoData(tx.QueryRowContext(ctx, `SELECT id, taskClass, leaseOwner, leaseExpires,
 failureCount, dataID, failureTime, priority, objectPHID, containerPHID, dateCreated, dateModified
 FROM worker_activetask WHERE id = ? FOR UPDATE`, l.TaskID), t)
	if err != nil {
		return nil, err
	}
	if t.LeaseOwner != l.LeaseOwner || t.LeaseExpires == nil || *t.LeaseExpires != l.LeaseExpires || l.LeaseExpires <= time.Now().Unix() {
		return nil, ErrLeaseConflict
	}
	return t, nil
}

func (s *MySQLStore) Finalize(ctx context.Context, req *contracts.FinalizeRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := lockExecution(ctx, tx, req.ExecutionLease)
	if errors.Is(err, sql.ErrNoRows) {
		// The archived ownership window is a durable receipt for retries.
		var owner string
		var expiry int64
		var result int
		err = tx.QueryRowContext(ctx, "SELECT leaseOwner, leaseExpires, result FROM worker_archivetask WHERE id = ?", req.TaskID).Scan(&owner, &expiry, &result)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseConflict
		}
		if err != nil {
			return err
		}
		if owner != req.LeaseOwner || expiry != req.LeaseExpires || result != contracts.ResultSuccess {
			return ErrLeaseConflict
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	for _, child := range req.Followups {
		if child.Priority == nil {
			priority := t.Priority
			child.Priority = &priority
		}
		if _, err := enqueueInTx(ctx, tx, &child); err != nil {
			return err
		}
	}
	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `INSERT INTO worker_archivetask
 (id, taskClass, leaseOwner, leaseExpires, failureCount, dataID, priority, objectPHID, containerPHID,
 result, duration, dateCreated, dateModified, archivedEpoch) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.TaskClass, t.LeaseOwner, req.LeaseExpires, t.FailureCount, t.DataID, t.Priority,
		nullStr(t.ObjectPHID), nullStr(t.ContainerPHID), contracts.ResultSuccess, req.Duration, t.DateCreated, now, now)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM worker_activetask WHERE id = ?", t.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MySQLStore) Renew(ctx context.Context, req *contracts.RenewRequest) (*contracts.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := lockExecution(ctx, tx, req.ExecutionLease)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLeaseConflict
		}
		return nil, err
	}
	expiry := time.Now().Unix() + int64(req.Duration)
	if expiry < req.LeaseExpires {
		expiry = req.LeaseExpires
	}
	if _, err = tx.ExecContext(ctx, "UPDATE worker_activetask SET leaseExpires = ? WHERE id = ?", expiry, t.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	t.LeaseExpires = &expiry
	return t, nil
}
