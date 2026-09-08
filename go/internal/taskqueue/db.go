package taskqueue

import (
	"database/sql"
	"fmt"
	"time"

	// The MySQL driver registers itself. It is imported from the domain
	// package rather than cmd/ so the package is self-contained, exactly as
	// the file-storage and webhook domains do it. platform/ still grows no
	// database facility: three domains now share a driver, not a concern.
	_ "github.com/go-sql-driver/mysql"
)

// Connection pool settings. The pool shares the server with Phorge itself,
// whose own pool needs the headroom, so it stays modest. A lease touches the
// database a handful of times inside one transaction; the pool covers a few
// concurrent enqueue/lease/complete callers plus the two read endpoints.
const (
	maxOpenConns    = 25
	maxIdleConns    = 5
	connMaxLifetime = 5 * time.Minute
)

// WorkerDSN is the DSN for Phorge's worker database. The database name is
// derived from the storage namespace exactly as Phorge derives it, because
// this service reads and writes Phorge's own worker_activetask and
// worker_archivetask tables. See compat/phorge/README.md.
func (c *Config) WorkerDSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s_worker?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		c.MySQLUser, c.MySQLPass, c.MySQLHost, c.MySQLPort, c.Namespace,
	)
}

// OpenDB prepares the connection pool.
//
// It deliberately does not ping, which is a change from the standalone
// gorge-task-queue: there a database that was not up yet made the process
// exit, so a container ordered before its database restarted in a loop.
// sql.Open is lazy, so the service now starts, answers /healthz, reports the
// database unreachable through /readyz — the distinction an orchestrator needs
// to tell "starting up" from "broken" — and each request retries the
// connection on its own. This is what lets the service be ordered before the
// Phorge container that runs `bin/storage upgrade` to create the tables.
func OpenDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	return db, nil
}
