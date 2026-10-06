package projection

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// Relay moves committed PHP snapshots into the local durable inbox. Concurrent
// relays may replay an event; Accept deduplicates it and acknowledgment is CAS.
// deliveredEpoch means accepted by Gorge, never applied to a search backend.
type Relay struct {
	Source    *sql.DB
	Store     *MySQLStore
	Namespace string
	Targets   []Target
}

func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := r.Once(ctx); err != nil && ctx.Err() == nil {
			slog.Error("search outbox relay unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (r *Relay) Once(ctx context.Context) error {
	if err := ValidateNamespace(r.Namespace); err != nil {
		return err
	}
	if err := ValidateTargets(r.Targets); err != nil {
		return err
	}
	readCtx, cancelRead := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRead()
	rows, err := r.Source.QueryContext(readCtx, `SELECT eventID,payload,attempts FROM search_gorgeoutbox WHERE deliveredEpoch IS NULL AND nextAttempt <= UNIX_TIMESTAMP() ORDER BY id LIMIT 32`)
	if err != nil {
		return err
	}
	type pending struct {
		id, raw  string
		attempts uint64
	}
	var batch []pending
	for rows.Next() {
		var e pending
		if err = rows.Scan(&e.id, &e.raw, &e.attempts); err != nil {
			rows.Close()
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
	for _, e := range batch {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		event, acceptErr := Decode([]byte(e.raw))
		if acceptErr == nil && (event.EventID != e.id || event.Namespace != r.Namespace) {
			acceptErr = fmt.Errorf("outbox identity mismatch")
		}
		if acceptErr == nil {
			acceptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, acceptErr = r.Store.Accept(acceptCtx, event, r.Targets)
			cancel()
		}
		// After cancellation, commit a known successful receipt or keep the row
		// pending. An uncertain acceptance is safely retried with the same event ID.
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if acceptErr == nil {
			_, err = r.Source.ExecContext(reportCtx, `UPDATE search_gorgeoutbox SET deliveredEpoch=UNIX_TIMESTAMP(),lastError='' WHERE eventID=? AND deliveredEpoch IS NULL`, e.id)
		} else {
			// Do not store raw driver errors or event bodies in diagnostics.
			delay := int64(1) << min(e.attempts+1, 12)
			_, err = r.Source.ExecContext(reportCtx, `UPDATE search_gorgeoutbox SET attempts=attempts+1,nextAttempt=UNIX_TIMESTAMP()+?,lastError=? WHERE eventID=? AND deliveredEpoch IS NULL AND attempts=?`, min(delay, 3600), "projection acceptance failed", e.id, e.attempts)
			slog.Warn("search outbox event pending", "eventID", e.id)
		}
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
