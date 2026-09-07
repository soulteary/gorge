package filestorage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// identifierMySQLBlob is what Phorge records against every file this backend
// writes. It is `blob` rather than `mysql` because that is the identifier
// Phorge's own PhabricatorMySQLFileStorageEngine uses, and both engines write
// the same table.
const identifierMySQLBlob = "blob"

// MySQLBlobEngine stores each file as one row of Phorge's `file_storageblob`
// table, with the row's auto-increment id as the handle.
//
// It shares that table with Phorge's native MySQL engine — same table, same
// handle scheme — which is why enabling both at once is a documented hazard
// rather than a feature. See compat/phorge/README.md section 8.
type MySQLBlobEngine struct {
	db      *sql.DB
	maxSize int64
}

// NewMySQLBlobEngine takes ownership of the pool: Close closes it.
func NewMySQLBlobEngine(db *sql.DB, maxSize int64) *MySQLBlobEngine {
	return &MySQLBlobEngine{db: db, maxSize: maxSize}
}

func (e *MySQLBlobEngine) Identifier() string { return identifierMySQLBlob }

// Priority is the lowest of the three, so small files land in the database
// where Phorge's own backups already cover them.
func (e *MySQLBlobEngine) Priority() int      { return 1 }
func (e *MySQLBlobEngine) CanWrite() bool     { return e.maxSize > 0 }
func (e *MySQLBlobEngine) HasSizeLimit() bool { return true }
func (e *MySQLBlobEngine) MaxFileSize() int64 { return e.maxSize }

// Close releases the connection pool.
func (e *MySQLBlobEngine) Close() error { return e.db.Close() }

// Ready pings the database.
//
// It pings and nothing more — in particular it does not ask whether
// `file_storageblob` exists. That table is created by Phorge's `bin/storage
// upgrade`, and the Phorge container that runs it waits for this service to be
// healthy first, so checking for the table deadlocks the stack. See
// Router.Ready.
func (e *MySQLBlobEngine) Ready(ctx context.Context) error {
	if err := e.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// WriteFile buffers the file and inserts it as one row.
//
// This is the one backend that cannot stream: a row is written whole. The
// buffer is bounded by maxSize, which is also why the size limit is enforced
// twice — once against the caller's declared length, so an oversized file is
// refused before a byte is read, and once against what actually arrived, since
// a declared length is not a promise.
func (e *MySQLBlobEngine) WriteFile(ctx context.Context, src io.Reader, size int64, _ WriteParams) (string, error) {
	if size > e.maxSize {
		return "", fmt.Errorf("file size %d exceeds the blob limit of %d bytes", size, e.maxSize)
	}

	data, err := io.ReadAll(io.LimitReader(src, e.maxSize+1))
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if int64(len(data)) > e.maxSize {
		return "", fmt.Errorf("file exceeds the blob limit of %d bytes", e.maxSize)
	}

	now := time.Now().Unix()
	res, err := e.db.ExecContext(ctx,
		"INSERT INTO file_storageblob (data, dateCreated, dateModified) VALUES (?, ?, ?)",
		data, now, now)
	if err != nil {
		return "", fmt.Errorf("insert blob: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("get blob id: %w", err)
	}
	return strconv.FormatInt(id, 10), nil
}

// ReadFile selects the row. The size is exact, because the whole row is
// already in memory by the time it returns.
func (e *MySQLBlobEngine) ReadFile(ctx context.Context, handle string) (io.ReadCloser, int64, error) {
	id, err := parseBlobHandle(handle)
	if err != nil {
		return nil, 0, err
	}

	var data []byte
	err = e.db.QueryRowContext(ctx,
		"SELECT data FROM file_storageblob WHERE id = ?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("blob %s not found", handle)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read blob: %w", err)
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// DeleteFile removes the row. A row that is already gone is a success; see
// StorageEngine.DeleteFile.
func (e *MySQLBlobEngine) DeleteFile(ctx context.Context, handle string) error {
	id, err := parseBlobHandle(handle)
	if err != nil {
		return err
	}

	if _, err := e.db.ExecContext(ctx, "DELETE FROM file_storageblob WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete blob: %w", err)
	}
	return nil
}

// parseBlobHandle rejects a handle that is not a row id.
//
// MySQL would coerce a non-numeric string to 0 and answer "no rows", so
// without this a malformed handle would be reported as a missing file — the
// same answer as a real deletion, which is the more expensive mistake to
// debug.
func parseBlobHandle(handle string) (uint64, error) {
	id, err := strconv.ParseUint(handle, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed blob handle %q: %w", handle, ErrBadHandle)
	}
	return id, nil
}
