package dbproxy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"

	"github.com/soulteary/gorge/go/internal/dbapi"
)

// The retry tests are ported from the standalone service's
// internal/dbcore/retry_test.go. The retry policy is unchanged; the driver
// abstraction the standalone tests reached through (GetDriver) is gone, so the
// retryable-classification is asserted directly on the shared dbapi functions.

func TestDefaultRetryPolicy(t *testing.T) {
	if p := DefaultRetryPolicy(); p.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", p.MaxAttempts)
	}
}

// TestQueryWithRetryWriteQueryNoRetry: a write is run once and never retried —
// retrying it could double-apply.
func TestQueryWithRetryWriteQueryNoRetry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	conn := dbapi.NewConnFromDB(db, dbapi.DSN{}, false)
	mock.ExpectQuery("INSERT INTO foo").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))

	rows, err := QueryWithRetry(context.Background(), conn, nil, DefaultRetryPolicy(), "INSERT INTO foo VALUES (1)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = rows.Close()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestQueryWithRetryInsideTransactionNoRetry: a query inside a transaction is
// run once, because retrying it would break the transaction's atomicity.
func TestQueryWithRetryInsideTransactionNoRetry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectCommit()

	conn := dbapi.NewConnFromDB(db, dbapi.DSN{}, false)
	txm := NewTxManager(conn)
	ctx := context.Background()
	if err := txm.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := QueryWithRetry(ctx, conn, txm, DefaultRetryPolicy(), "SELECT 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = rows.Close()
	if err := txm.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestQueryWithRetryRejectsTransactionWriteOnReadOnlyConnection(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectRollback()
	conn := dbapi.NewConnFromDB(db, dbapi.DSN{Database: "phorge_config"}, true)
	txm := NewTxManager(conn)
	ctx := context.Background()
	if err := txm.Begin(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := QueryWithRetry(ctx, conn, txm, DefaultRetryPolicy(), "UPDATE config SET value = 1")
	if rows != nil {
		_ = rows.Close()
		t.Fatal("write on a read-only transaction unexpectedly returned rows")
	}
	if !dbapi.IsReadonlyError(err) {
		t.Fatalf("error = %v, want a read-only DBError", err)
	}
	if err := txm.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryWithRetryUsesActiveTransactionInsteadOfPassedPool(t *testing.T) {
	txDB, txMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txDB.Close() }()
	poolDB, poolMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = poolDB.Close() }()

	txMock.ExpectBegin()
	txMock.ExpectQuery("SELECT transaction_value").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("inside"))
	txMock.ExpectRollback()

	txm := NewTxManager(dbapi.NewConnFromDB(txDB, dbapi.DSN{}, false))
	ctx := context.Background()
	if err := txm.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	outside := dbapi.NewConnFromDB(poolDB, dbapi.DSN{}, false)
	rows, err := QueryWithRetry(ctx, outside, txm, DefaultRetryPolicy(), "SELECT transaction_value")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if err := txm.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := poolMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("query unexpectedly used the passed pool: %v", err)
	}
}

func TestQueryWithRetryReadSuccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("ok"))
	conn := dbapi.NewConnFromDB(db, dbapi.DSN{}, false)
	rows, err := QueryWithRetry(context.Background(), conn, nil, DefaultRetryPolicy(), "SELECT 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = rows.Close()
}

// TestQueryWithRetryClassifiesMySQLError: a non-retryable MySQL error on a read
// is classified into a *DBError, which is how an access-denied on a read
// surfaces as ERR_DB_ACCESS_DENIED rather than a bare driver error.
func TestQueryWithRetryClassifiesMySQLError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT").WillReturnError(&mysql.MySQLError{Number: 1044, Message: "Access denied"})
	conn := dbapi.NewConnFromDB(db, dbapi.DSN{}, false)
	_, qErr := QueryWithRetry(context.Background(), conn, nil, RetryPolicy{MaxAttempts: 1}, "SELECT 1")
	if qErr == nil {
		t.Fatal("expected an error")
	}
	var dbErr *dbapi.DBError
	if !errors.As(qErr, &dbErr) {
		t.Fatalf("expected a *dbapi.DBError, got %T", qErr)
	}
	if !dbapi.IsAccessDeniedError(qErr) {
		t.Errorf("error = %v, want an access-denied DBError", qErr)
	}
}

func TestQueryWithRetryZeroAttemptsRunsOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(1))
	conn := dbapi.NewConnFromDB(db, dbapi.DSN{}, false)
	rows, err := QueryWithRetry(context.Background(), conn, nil, RetryPolicy{MaxAttempts: 0}, "SELECT 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = rows.Close()
}

func TestQueryWithRetryNonMySQLReadErrorPropagates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT").WillReturnError(errors.New("network timeout"))
	conn := dbapi.NewConnFromDB(db, dbapi.DSN{}, false)
	_, qErr := QueryWithRetry(context.Background(), conn, nil, RetryPolicy{MaxAttempts: 1}, "SELECT 1")
	if qErr == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(qErr.Error(), "network timeout") {
		t.Errorf("error = %q, want it to carry 'network timeout'", qErr.Error())
	}
}
