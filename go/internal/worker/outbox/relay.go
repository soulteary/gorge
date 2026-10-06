// Package outbox relays committed Phorge feed events into the queue inbox.
// It does not execute PHP business logic or remove source event receipts.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

type Queue interface {
	EnqueueEvent(context.Context, *contracts.EnqueueEventRequest) (*contracts.Task, error)
}
type Relay struct {
	DB    *sql.DB
	Queue Queue
	Table string
}

func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := r.Once(ctx); err != nil && ctx.Err() == nil {
			slog.Error("outbox relay failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (r *Relay) Once(ctx context.Context) error {
	table := r.Table
	if table == "" {
		table = "feed_gorgeoutbox"
	}
	if table != "feed_gorgeoutbox" && table != "metamta_gorgeoutbox" {
		return fmt.Errorf("unsupported outbox repository")
	}

	rows, err := r.DB.QueryContext(ctx, `SELECT eventID, payload, attempts FROM `+table+` WHERE deliveredEpoch IS NULL AND nextAttempt <= ? ORDER BY id LIMIT 32`, time.Now().Unix())
	if err != nil {
		return err
	}
	type event struct {
		id, payload string
		attempts    int
	}
	var events []event
	for rows.Next() {
		var e event
		if err = rows.Scan(&e.id, &e.payload, &e.attempts); err != nil {
			_ = rows.Close() // Preserve the scan error.
			return err
		}
		events = append(events, e)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, e := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var request contracts.EnqueueEventRequest
		err = json.Unmarshal([]byte(e.payload), &request)
		var task *contracts.Task
		if err == nil && request.EventID != e.id {
			err = fmt.Errorf("outbox event identity mismatch")
		}
		if err == nil {
			task, err = r.Queue.EnqueueEvent(ctx, &request)
		}
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		var updateErr error
		if err == nil {
			_, updateErr = r.DB.ExecContext(reportCtx, `UPDATE `+table+` SET deliveredEpoch = ?, queueTaskID = ?, lastError = '' WHERE eventID = ? AND deliveredEpoch IS NULL`, time.Now().Unix(), task.ID, e.id)
		} else {
			// Keep every failed event visible and retryable, without allowing one bad
			// event to block the rest of the batch. Never log payloads or credentials.
			message := err.Error()
			if len(message) > 1024 {
				message = message[:1024]
			}
			wait := min(3600, 1<<min(e.attempts+1, 12))
			_, updateErr = r.DB.ExecContext(reportCtx, `UPDATE `+table+` SET attempts = attempts + 1, nextAttempt = ?, lastError = ? WHERE eventID = ? AND deliveredEpoch IS NULL`, time.Now().Unix()+int64(wait), message, e.id)
			slog.Warn("outbox event pending", "eventID", e.id, "attempts", e.attempts+1)
		}
		cancel()
		if updateErr != nil {
			return updateErr
		}
	}
	return nil
}
