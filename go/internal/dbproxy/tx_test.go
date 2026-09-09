package dbproxy

import (
	"context"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/soulteary/gorge/go/internal/dbapi"
)

// The transaction tests are ported from the standalone service's
// internal/dbcore/tx_test.go. The savepoint naming — Aphront_Savepoint_N — is
// a compatibility contract with Phorge's AphrontDatabaseConnection, not an
// implementation detail, so it is pinned at every depth.

// TestSavepointName pins the exact string emitted at a given depth. This is the
// same name Phorge emits, so a nested transaction opened by this service and
// one opened by Phorge unwind the same way.
func TestSavepointName(t *testing.T) {
	txm := &TxManager{}
	for depth := 0; depth <= 10; depth++ {
		txm.depth = depth
		want := fmt.Sprintf("Aphront_Savepoint_%d", depth)
		if got := txm.savepointName(); got != want {
			t.Errorf("depth=%d: savepointName() = %q, want %q", depth, got, want)
		}
	}
}

func TestNewTxManagerInitialState(t *testing.T) {
	txm := NewTxManager(nil)
	if txm.Depth() != 0 {
		t.Errorf("initial depth = %d, want 0", txm.Depth())
	}
	if txm.IsInsideTransaction() {
		t.Error("should not be inside a transaction initially")
	}
}

func TestTxManagerBeginCommit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectCommit()

	txm := NewTxManager(dbapi.NewConnFromDB(db, dbapi.DSN{}, false))
	ctx := context.Background()

	if err := txm.Begin(ctx); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if txm.Depth() != 1 || !txm.IsInsideTransaction() {
		t.Errorf("after Begin: depth=%d inside=%v", txm.Depth(), txm.IsInsideTransaction())
	}
	if err := txm.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if txm.Depth() != 0 || txm.IsInsideTransaction() {
		t.Errorf("after Commit: depth=%d inside=%v", txm.Depth(), txm.IsInsideTransaction())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestTxManagerBeginRollback(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectRollback()

	txm := NewTxManager(dbapi.NewConnFromDB(db, dbapi.DSN{}, false))
	ctx := context.Background()
	if err := txm.Begin(ctx); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := txm.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if txm.Depth() != 0 {
		t.Errorf("depth after Rollback = %d, want 0", txm.Depth())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestTxManagerNestedSavepoints: the outermost Begin opens a real transaction,
// and each nested Begin creates a savepoint named for its depth. A nested
// Commit unwinds a level; the savepoint is released implicitly on the outer
// commit.
func TestTxManagerNestedSavepoints(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec("SAVEPOINT Aphront_Savepoint_1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SAVEPOINT Aphront_Savepoint_2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	txm := NewTxManager(dbapi.NewConnFromDB(db, dbapi.DSN{}, false))
	ctx := context.Background()

	for wantDepth := 1; wantDepth <= 3; wantDepth++ {
		if err := txm.Begin(ctx); err != nil {
			t.Fatalf("Begin to depth %d: %v", wantDepth, err)
		}
		if txm.Depth() != wantDepth {
			t.Errorf("depth = %d, want %d", txm.Depth(), wantDepth)
		}
	}
	for wantDepth := 2; wantDepth >= 0; wantDepth-- {
		if err := txm.Commit(ctx); err != nil {
			t.Fatalf("Commit to depth %d: %v", wantDepth, err)
		}
		if txm.Depth() != wantDepth {
			t.Errorf("depth = %d, want %d", txm.Depth(), wantDepth)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestTxManagerNestedRollback: a nested Rollback rolls back to the savepoint,
// leaving the outer transaction open to commit.
func TestTxManagerNestedRollback(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec("SAVEPOINT Aphront_Savepoint_1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("ROLLBACK TO SAVEPOINT Aphront_Savepoint_1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	txm := NewTxManager(dbapi.NewConnFromDB(db, dbapi.DSN{}, false))
	ctx := context.Background()

	if err := txm.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txm.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if txm.Depth() != 2 {
		t.Errorf("depth = %d, want 2", txm.Depth())
	}
	if err := txm.Rollback(ctx); err != nil {
		t.Fatalf("nested Rollback: %v", err)
	}
	if txm.Depth() != 1 {
		t.Errorf("depth after nested rollback = %d, want 1", txm.Depth())
	}
	if err := txm.Commit(ctx); err != nil {
		t.Fatalf("outer Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestTxManagerCommitOrRollbackWithoutBegin(t *testing.T) {
	txm := NewTxManager(nil)
	if err := txm.Commit(context.Background()); err == nil {
		t.Error("expected an error committing without begin")
	}
	if err := txm.Rollback(context.Background()); err == nil {
		t.Error("expected an error rolling back without begin")
	}
}
