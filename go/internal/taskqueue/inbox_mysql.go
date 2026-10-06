package taskqueue

import (
	"context"
	"encoding/json"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// The unique receipt is acquired before enqueue, so concurrent relays commit
// only one task. Receipts remain after task archival until explicitly retired.
func (s *MySQLStore) EnqueueEvent(ctx context.Context, req *contracts.EnqueueEventRequest) (*contracts.Task, error) {
	digest, _, err := eventDigest(req)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Rollback is cleanup and may return sql.ErrTxDone after commit.
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "INSERT INTO worker_gorgeinbox (eventID, payloadHash, response) VALUES (?, ?, '') ON DUPLICATE KEY UPDATE eventID = eventID", req.EventID, digest)
	if err != nil {
		return nil, err
	}
	var storedHash, response string
	err = tx.QueryRowContext(ctx, "SELECT payloadHash, response FROM worker_gorgeinbox WHERE eventID = ? FOR UPDATE", req.EventID).Scan(&storedHash, &response)
	if err != nil {
		return nil, err
	}
	if storedHash != digest {
		return nil, ErrEventConflict
	}
	task := new(contracts.Task)
	if response != "" {
		if err = json.Unmarshal([]byte(response), task); err != nil {
			return nil, err
		}
	} else {
		task, err = enqueueInTx(ctx, tx, &req.Task)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(task)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE worker_gorgeinbox SET response = ? WHERE eventID = ?", string(raw), req.EventID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}
