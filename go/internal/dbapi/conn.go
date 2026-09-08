package dbapi

import (
	"context"
	"database/sql"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Connection pool settings for a per-node connection. They match the standalone
// service and Phorge's own modest footprint: this service shares each MySQL
// server with Phorge, whose pool needs the headroom.
const (
	maxOpenConns    = 25
	maxIdleConns    = 5
	connMaxLifetime = 5 * time.Minute
)

// DSN is the set of parameters a connection is opened from. It is MySQL-only:
// the standalone service carried a driver abstraction with a SQLite arm, which
// is dropped here because every gorge deployment targets MySQL and the second
// driver only ever served the standalone service's own tests.
type DSN struct {
	Host            string
	Port            int
	User            string
	Password        string
	Database        string
	MaxRetries      int
	ConnTimeoutSec  int
	QueryTimeoutSec int
}

// String renders the DSN in the go-sql-driver/mysql format, the same layout as
// taskqueue/db.go: ParseTime so DATETIME columns scan into time.Time, and
// InterpolateParams so a parameterised query costs one round trip instead of
// two.
func (d DSN) String() string {
	cfg := mysql.NewConfig()
	cfg.User = d.User
	cfg.Passwd = d.Password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
	cfg.DBName = d.Database
	cfg.Timeout = time.Duration(d.ConnTimeoutSec) * time.Second
	cfg.ReadTimeout = time.Duration(d.QueryTimeoutSec) * time.Second
	cfg.WriteTimeout = time.Duration(d.QueryTimeoutSec) * time.Second
	cfg.ParseTime = true
	cfg.InterpolateParams = true
	return cfg.FormatDSN()
}

// Conn wraps a *sql.DB pool with a read-only guard. The guard is the first of
// two layers of write protection; the router refusing to hand out a writer
// while degraded is the second. Together they keep a read-only degradation
// from silently letting a write through.
type Conn struct {
	db       *sql.DB
	dsn      DSN
	readOnly bool
}

// ConnFactory opens a Conn from a DSN. It is a field on the services so a test
// can inject a sqlmock-backed connection in place of a real dial.
type ConnFactory func(dsn DSN, readOnly bool) (*Conn, error)

// NewConn opens the pool. It does not ping: like taskqueue/db.go, the open is
// lazy so the service can start before the database is up and report the
// difference through /readyz rather than crash-looping.
func NewConn(dsn DSN, readOnly bool) (*Conn, error) {
	db, err := sql.Open("mysql", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	return &Conn{db: db, dsn: dsn, readOnly: readOnly}, nil
}

// NewConnFromDB wraps an existing *sql.DB, for tests using sqlmock.
func NewConnFromDB(db *sql.DB, dsn DSN, readOnly bool) *Conn {
	return &Conn{db: db, dsn: dsn, readOnly: readOnly}
}

func (c *Conn) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

func (c *Conn) Close() error { return c.db.Close() }

func (c *Conn) IsReadOnly() bool { return c.readOnly }

func (c *Conn) DB() *sql.DB { return c.db }

func (c *Conn) DSN() DSN { return c.dsn }

// QueryContext runs a query, refusing a write on a read-only connection. A
// read-only Conn allows only statements the read-query test recognises; a
// write becomes ERR_READONLY.
func (c *Conn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if c.readOnly && !isReadQuery(query) {
		return nil, newDBError(kindReadonly,
			"write query on read-only connection (database %q)", c.dsn.Database)
	}
	return c.db.QueryContext(ctx, query, args...)
}

// ExecContext runs a statement, refused outright on a read-only connection.
func (c *Conn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if c.readOnly {
		return nil, newDBError(kindReadonly,
			"write query on read-only connection (database %q)", c.dsn.Database)
	}
	return c.db.ExecContext(ctx, query, args...)
}

func (c *Conn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return c.db.QueryRowContext(ctx, query, args...)
}

// readQueryRe matches the statements safe to run on a read-only connection:
// SELECT, SHOW, EXPLAIN, allowing a leading paren for a parenthesised UNION.
// It is compiled once and matches with zero allocation thereafter.
var readQueryRe = regexp.MustCompile(`(?i)^\s*\(?\s*(SELECT|SHOW|EXPLAIN)\s`)

func isReadQuery(q string) bool { return readQueryRe.MatchString(q) }
