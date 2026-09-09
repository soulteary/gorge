// Package dbproxy holds the master/replica write-path abstractions the db-api
// domain grew for parity with Phorge's AphrontDatabaseConnection but does not
// yet drive from any HTTP handler or background task. It is a deliberately
// separated home — not dead code deleted, not a live route added — so the
// day a write endpoint is introduced it has a reviewed, tested foundation.
//
// See gorge/docs/adr/0001-isolate-db-proxy.md for the owner, the intended
// future entry point and the record that these abstractions are, as of this
// change, not on any runtime path. The read-only db-api endpoints do not use
// this package: each read service opens its own short-lived connection through
// dbapi.ConnFactory and never routes through here.
package dbproxy

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/soulteary/gorge/go/internal/dbapi"
)

// RetryPolicy bounds how many times a connect or a read query is retried. The
// default of three attempts matches the standalone service and Phorge.
type RetryPolicy struct {
	MaxAttempts int
}

// DefaultRetryPolicy returns the three-attempt policy.
func DefaultRetryPolicy() RetryPolicy { return RetryPolicy{MaxAttempts: 3} }

// ConnectWithRetry opens and pings a connection, retrying on the connect errors
// dbapi.IsRetryableConnectErr recognises (timeout, refused). A non-retryable
// error, or the last attempt, returns immediately.
func ConnectWithRetry(dsn dbapi.DSN, readOnly bool, policy RetryPolicy) (*dbapi.Conn, error) {
	maxAttempts := policy.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		conn, err := dbapi.NewConn(dsn, readOnly)
		if err != nil {
			lastErr = err
			if dbapi.IsRetryableConnectErr(err) && attempt < maxAttempts {
				continue
			}
			return nil, err
		}
		if err := conn.Ping(context.Background()); err != nil {
			_ = conn.Close()
			lastErr = err
			if dbapi.IsRetryableConnectErr(err) && attempt < maxAttempts {
				continue
			}
			return nil, err
		}
		return conn, nil
	}
	return nil, fmt.Errorf("connect failed after %d attempts: %w", maxAttempts, lastErr)
}

// QueryWithRetry runs a read query with retry, but only when it is safe: a
// write, or any query inside a transaction, is run once and not retried.
// Retrying a write would risk a double apply, and retrying inside a
// transaction would break its atomicity — the same constraint Phorge's own
// connection enforces.
func QueryWithRetry(ctx context.Context, conn *dbapi.Conn, txm *TxManager, policy RetryPolicy, query string, args ...any) (*sql.Rows, error) {
	if txm != nil {
		if rows, active, err := txm.queryContext(ctx, query, args...); active {
			return rows, err
		}
	}
	if !dbapi.IsReadQuery(query) {
		return conn.QueryContext(ctx, query, args...)
	}

	maxAttempts := policy.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		rows, err := conn.QueryContext(ctx, query, args...)
		if err == nil {
			return rows, nil
		}
		lastErr = err
		if dbapi.IsRetryableQueryErr(err) && attempt < maxAttempts {
			continue
		}
		return nil, dbapi.ClassifyMySQLError(err)
	}
	return nil, lastErr
}
