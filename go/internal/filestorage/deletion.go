package filestorage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/go-sql-driver/mysql"
	"strings"
	"time"
)

// MySQL blob handles are namespace-local IDs. Consuming another namespace's
// intents against this router could delete an unrelated blob with the same ID.
func ValidateDeletionDatabase(namespace, dsn string) error {
	cfg, e := mysql.ParseDSN(dsn)
	if e != nil {
		return fmt.Errorf("invalid file deletion DSN")
	}
	if namespace == "" || cfg.DBName != namespace+"_file" {
		return fmt.Errorf("file deletion database must match the configured file namespace")
	}
	return nil
}

// The intent is committed with the PHP file-row deletion. Consuming it never
// deletes a business row. Each attempt rechecks references, and physical
// deletion is idempotent so a lost commit may safely retry. Row locks serialize
// consumers; the external operation is bounded by the caller's context.
func ProcessDeletion(ctx context.Context, db *sql.DB, router *Router, uploads *Uploads) error {
	for attempt := 0; attempt < 3; attempt++ {
		e := processDeletionOnce(ctx, db, router, uploads)
		var dbErr *mysql.MySQLError
		if !errors.As(e, &dbErr) || (dbErr.Number != 1213 && dbErr.Number != 1205) || attempt == 2 {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 25 * time.Millisecond):
		}
	}
	return nil
}
func processDeletionOnce(ctx context.Context, db *sql.DB, router *Router, uploads *Uploads) error {
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	// Rollback is best-effort cleanup; after Commit it returns sql.ErrTxDone.
	defer func() { _ = tx.Rollback() }()
	var id int64
	var engine, handle string
	var attempts int
	e = tx.QueryRowContext(ctx, `SELECT id,storageEngine,storageHandle,attempts FROM file_gorgedeletion WHERE state='pending' AND nextAttempt<=? ORDER BY nextAttempt,id LIMIT 1 FOR UPDATE`, time.Now().Unix()).Scan(&id, &engine, &handle, &attempts)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	var used int
	e = tx.QueryRowContext(ctx, `SELECT id FROM file WHERE storageEngine=? AND storageHandle=? LIMIT 1 FOR UPDATE`, engine, handle).Scan(&used)
	state := "done"
	next := int64(0)
	message := ""
	if e == nil {
		state = "cancelled"
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	} else {
		operationCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		e = deleteTarget(operationCtx, router, uploads, engine, handle)
		cancel()
		if e != nil {
			state = "pending"
			next = time.Now().Add(time.Duration(min(3600, 5*(1<<min(attempts, 9)))) * time.Second).Unix()
			message = e.Error()
			if len(message) > 4096 {
				message = message[:4096]
			}
		}
	}
	if _, e = tx.ExecContext(ctx, `UPDATE file_gorgedeletion SET state=?,attempts=attempts+1,nextAttempt=?,lastError=? WHERE id=?`, state, next, message, id); e != nil {
		return e
	}
	return tx.Commit()
}
func deleteTarget(ctx context.Context, router *Router, uploads *Uploads, engine, handle string) error {
	if engine == "chunks" && strings.HasPrefix(handle, "gorge-upload/") {
		if uploads == nil {
			return fmt.Errorf("upload volume is not configured")
		}
		return uploads.Cancel(ctx, strings.TrimPrefix(handle, "gorge-upload/"))
	}
	if engine != "gorge" {
		return fmt.Errorf("unsupported deletion engine %q", engine)
	}
	backend, key, ok := strings.Cut(handle, "/")
	if !ok || key == "" {
		return ErrBadHandle
	}
	target, e := router.GetEngine(backend)
	if e != nil {
		return e
	}
	return target.DeleteFile(ctx, key)
}
