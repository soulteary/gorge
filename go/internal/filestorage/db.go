package filestorage

import (
	"database/sql"
	"fmt"
	"time"

	// The MySQL driver registers itself. This is the first database driver in
	// the monorepo, and it is imported here rather than from cmd/ so the
	// domain package is self-contained: platform/ has no database facilities
	// and must not grow any, since a single domain needing a connection pool
	// is not a shared concern. See docs/findings.md.
	_ "github.com/go-sql-driver/mysql"
)

// Connection pool settings for the blob backend. The pool is small on purpose:
// this service holds a connection only for the duration of one INSERT or one
// SELECT of a row capped at MySQLBlobMaxSize, and it shares the database with
// Phorge itself, whose own pool is the one that needs the headroom.
const (
	maxOpenConns    = 25
	maxIdleConns    = 5
	connMaxLifetime = 5 * time.Minute
)

// FileDSN is the DSN for Phorge's file database. The database name is derived
// from the storage namespace exactly as Phorge derives it, because the blob
// backend writes into Phorge's own `file_storageblob` table — the same table
// PhabricatorMySQLFileStorageEngine uses. See compat/phorge/README.md section 7.
func (c *Config) FileDSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s_file?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		c.MySQLUser, c.MySQLPass, c.MySQLHost, c.MySQLPort, c.Namespace,
	)
}

// OpenDB prepares the connection pool.
//
// It deliberately does not ping. sql.Open is lazy, so a service whose database
// is not up yet still starts and answers /healthz, while /readyz reports the
// database as unreachable — which is what an orchestrator needs in order to
// tell "starting up" from "broken". Pinging here instead would put the
// container in a restart loop for a dependency that is merely slow.
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
