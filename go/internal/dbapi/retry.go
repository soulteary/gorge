package dbapi

import (
	"context"
	"database/sql"
	"fmt"
)

// RetryPolicy bounds how many times a connect or a read query is retried. The
// default of three attempts matches the standalone service and Phorge.
type RetryPolicy struct {
	MaxAttempts int
}

func DefaultRetryPolicy() RetryPolicy { return RetryPolicy{MaxAttempts: 3} }

// ConnectWithRetry opens and pings a connection, retrying on the connect errors
// isRetryableConnectErr recognises (timeout, refused). A non-retryable error,
// or the last attempt, returns immediately.
func ConnectWithRetry(dsn DSN, readOnly bool, policy RetryPolicy) (*Conn, error) {
	maxAttempts := policy.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		conn, err := NewConn(dsn, readOnly)
		if err != nil {
			lastErr = err
			if isRetryableConnectErr(err) && attempt < maxAttempts {
				continue
			}
			return nil, err
		}
		if err := conn.Ping(context.Background()); err != nil {
			_ = conn.Close()
			lastErr = err
			if isRetryableConnectErr(err) && attempt < maxAttempts {
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
func QueryWithRetry(ctx context.Context, conn *Conn, txm *TxManager, policy RetryPolicy, query string, args ...any) (*sql.Rows, error) {
	if txm != nil {
		if rows, active, err := txm.queryContext(ctx, query, args...); active {
			return rows, err
		}
	}
	if !isReadQuery(query) {
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
		if isRetryableQueryErr(err) && attempt < maxAttempts {
			continue
		}
		return nil, classifyMySQLError(err)
	}
	return nil, lastErr
}
