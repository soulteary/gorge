package dbapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The conn tests are ported from the standalone service's
// internal/dbcore/conn_test.go. The read-query regex and the read-only guard
// are the domain algorithm preserved verbatim; what changed is the guard's
// error, which is now this package's *DBError with kindReadonly rather than the
// standalone compat.ErrReadonlyWrite, and the driver arm (PRAGMA/SQLite) is
// gone because this port is MySQL-only.

// TestIsReadQuery pins the regex that decides what a read-only connection will
// run. SELECT/SHOW/EXPLAIN (optionally parenthesised for a UNION) are reads;
// everything that writes is not.
func TestIsReadQuery(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"SELECT * FROM foo", true},
		{"  SELECT 1", true},
		{"(SELECT a) UNION (SELECT b)", true},
		{"SHOW TABLES", true},
		{"EXPLAIN SELECT 1", true},
		{"select * from foo", true},
		{"  show databases", true},
		{"  explain analyze SELECT 1", true},
		{"INSERT INTO foo VALUES (1)", false},
		{"UPDATE foo SET a=1", false},
		{"DELETE FROM foo", false},
		{"CREATE TABLE foo (id INT)", false},
		{"DROP TABLE foo", false},
		{"START TRANSACTION", false},
		{"COMMIT", false},
		{"ALTER TABLE foo ADD COLUMN bar INT", false},
		{"TRUNCATE TABLE foo", false},
		{"REPLACE INTO foo VALUES (1)", false},
		// PRAGMA was a read on the dropped SQLite arm; on MySQL it is neither
		// a read nor valid, and the regex no longer admits it.
		{"PRAGMA table_info('foo')", false},
	}
	for _, tc := range cases {
		if got := isReadQuery(tc.query); got != tc.want {
			t.Errorf("isReadQuery(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// TestDSNString pins the DSN layout: it is the same go-sql-driver format
// taskqueue uses, with parseTime and interpolateParams on.
func TestDSNString(t *testing.T) {
	s := DSN{
		Host: "db.example.com", Port: 3306, User: "testuser", Password: "testpass",
		Database: "testdb", ConnTimeoutSec: 10, QueryTimeoutSec: 30,
	}.String()

	for _, want := range []string{
		"testuser", "testpass", "db.example.com:3306", "testdb", "tcp",
		"parseTime=true", "interpolateParams=true",
		"timeout=10s", "readTimeout=30s", "writeTimeout=30s",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("DSN %q should contain %q", s, want)
		}
	}
}

func TestDSNStringDifferentPort(t *testing.T) {
	if s := (DSN{Host: "localhost", Port: 3307, User: "root"}).String(); !strings.Contains(s, "localhost:3307") {
		t.Errorf("expected localhost:3307, got %q", s)
	}
}

func TestDSNStringBracketsIPv6Literal(t *testing.T) {
	s := (DSN{Host: "2001:db8::1", Port: 3306, User: "root"}).String()
	if !strings.Contains(s, "tcp([2001:db8::1]:3306)") {
		t.Errorf("expected a bracketed IPv6 address, got %q", s)
	}
}

func TestNewConnFromDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	dsn := DSN{Host: "mock", Port: 3306, Database: "testdb"}
	conn := NewConnFromDB(db, dsn, false)
	if conn.DB() != db {
		t.Error("DB() should return the injected *sql.DB")
	}
	if conn.IsReadOnly() {
		t.Error("should not be read-only")
	}
	if conn.DSN().Database != "testdb" {
		t.Errorf("DSN() mismatch: %+v", conn.DSN())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestConnPingAndClose(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing()
	mock.ExpectClose()

	conn := NewConnFromDB(db, DSN{}, false)
	if err := conn.Ping(context.Background()); err != nil {
		t.Errorf("Ping failed: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestConnReadOnlyRefusesWrite: a write on a read-only connection is refused
// before it reaches the driver, with a kindReadonly *DBError, and the refusal
// message names no host or table.
func TestConnReadOnlyRefusesWrite(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	conn := NewConnFromDB(db, DSN{Database: "mydb"}, true)

	for _, run := range []struct {
		name string
		fn   func() error
	}{
		{"QueryContext", func() error {
			_, e := conn.QueryContext(context.Background(), "INSERT INTO foo VALUES (1)")
			return e
		}},
		{"ExecContext", func() error {
			_, e := conn.ExecContext(context.Background(), "INSERT INTO foo VALUES (1)")
			return e
		}},
	} {
		err := run.fn()
		if err == nil {
			t.Fatalf("%s: expected an error for a write on a read-only conn", run.name)
		}
		var dbErr *DBError
		if !errors.As(err, &dbErr) {
			t.Fatalf("%s: expected *DBError, got %T", run.name, err)
		}
		if dbErr.Kind != kindReadonly {
			t.Errorf("%s: kind = %d, want kindReadonly", run.name, dbErr.Kind)
		}
	}
}

// TestConnReadOnlyAllowsRead: a SELECT is allowed on a read-only connection.
func TestConnReadOnlyAllowsRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	conn := NewConnFromDB(db, DSN{}, true)
	rows, err := conn.QueryContext(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = rows.Close()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestConnReadWriteAllowsWrite: the same write goes through on a read-write
// connection.
func TestConnReadWriteAllowsWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec("INSERT INTO foo").WillReturnResult(sqlmock.NewResult(1, 1))
	conn := NewConnFromDB(db, DSN{}, false)
	res, err := conn.ExecContext(context.Background(), "INSERT INTO foo VALUES (1)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		t.Errorf("expected 1 row affected, got %d", affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestConnQueryRowContext(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT 42").WillReturnRows(sqlmock.NewRows([]string{"val"}).AddRow(42))
	conn := NewConnFromDB(db, DSN{}, false)
	var val int
	if err := conn.QueryRowContext(context.Background(), "SELECT 42").Scan(&val); err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if val != 42 {
		t.Errorf("expected 42, got %d", val)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestConnQueryRowContextRejectsWriteWhenReadOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	conn := NewConnFromDB(db, DSN{Database: "phorge_config"}, true)
	var value int
	err = conn.QueryRowContext(context.Background(),
		"UPDATE config SET value = 1 RETURNING value").Scan(&value)
	var dbErr *DBError
	if !errors.As(err, &dbErr) || dbErr.Kind != kindReadonly {
		t.Fatalf("error = %v, want kindReadonly", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
