package webhook

import (
	"database/sql"
	"fmt"
	"time"

	// The MySQL driver registers itself. It is imported here rather than from
	// cmd/ so the domain package is self-contained, and it is imported by a
	// second domain now without platform/ growing a database package: the two
	// domains share a driver, not a concern. Their DSN shapes, pool sizes and
	// query semantics have nothing in common. See docs/findings.md.
	_ "github.com/go-sql-driver/mysql"
)

// Connection pool settings. The pool has to cover MaxConcurrent deliveries,
// each of which touches the database three or four times — the claim, the hook
// lookup, the circuit-breaker count and the result write — plus the two status
// endpoints. It shares the server with Phorge itself, whose own pool is the
// one that needs the headroom.
const (
	maxOpenConns    = 20
	maxIdleConns    = 5
	connMaxLifetime = 5 * time.Minute
)

// HeraldDSN is the DSN for Phorge's herald database. The database name is
// derived from the storage namespace exactly as Phorge derives it, because
// this service reads and writes Phorge's own herald_webhookrequest table — the
// same rows HeraldWebhookWorker would have delivered. See
// compat/phorge/README.md.
func (c *Config) HeraldDSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s_herald?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		c.MySQLUser, c.MySQLPass, c.MySQLHost, c.MySQLPort, c.Namespace,
	)
}

// OpenDB prepares the connection pool.
//
// It deliberately does not ping, which is a change from the standalone
// service: there a database that was not up yet made the process exit, so a
// container ordered before its database restarted in a loop. sql.Open is lazy,
// so the service now starts, answers /healthz, reports the database
// unreachable through /readyz — the distinction an orchestrator needs in order
// to tell "starting up" from "broken" — and the dispatcher's poll retries on
// its own every tick.
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
