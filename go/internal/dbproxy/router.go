package dbproxy

import (
	"context"
	"fmt"
	"sync"

	"github.com/soulteary/gorge/go/internal/dbapi"
)

// Router selects a master or replica connection per Phorge application,
// mirroring PhabricatorLiskDAO's cluster connection selection. It caches one
// connection per (node, application, read-only) triple, and it degrades to
// read-only when the master cannot be reached.
type Router struct {
	config   *dbapi.ClusterConfig
	password string
	connect  func(dbapi.DSN, bool, RetryPolicy) (*dbapi.Conn, error)

	mu       sync.Mutex
	readOnly bool
	conns    map[string]*dbapi.Conn
}

// NewRouter builds a router over a cluster. The password is the one resolved
// for the cluster; per-node passwords from a config file take precedence in the
// DSN the router builds.
func NewRouter(cfg *dbapi.ClusterConfig, password string) *Router {
	return &Router{
		config:   cfg,
		password: password,
		connect:  ConnectWithRetry,
		conns:    make(map[string]*dbapi.Conn),
	}
}

// SetReadOnly forces or clears the degraded state, for tests.
func (r *Router) SetReadOnly(readOnly bool) {
	r.mu.Lock()
	r.readOnly = readOnly
	r.mu.Unlock()
}

// IsReadOnly reports the degraded state.
func (r *Router) IsReadOnly() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readOnly
}

// GetWriter returns the master connection for an application, or ERR_READONLY
// when the service has degraded, or ERR_DB_UNREACHABLE when the master cannot
// be reached. It never falls back to a replica: a write must reach a master or
// fail.
func (r *Router) GetWriter(ctx context.Context, application string) (*dbapi.Conn, error) {
	r.mu.Lock()
	isRO := r.readOnly
	r.mu.Unlock()

	if isRO {
		return nil, dbapi.NewReadonlyError(
			"server is read-only, cannot write to %q", application)
	}

	master := r.config.GetMasterForApplication(application)
	if master == nil {
		return nil, dbapi.NewUnreachableError(
			"no master configured for application %q", application)
	}

	conn, err := r.getOrCreateConn(master, application, false)
	if err != nil {
		return nil, dbapi.NewUnreachableError(
			"cannot connect to master for %q", application)
	}
	return conn, nil
}

// GetReader returns a connection for a read: the master if reachable, otherwise
// a replica. A master that cannot be reached flips the router to read-only —
// the degradation Phorge itself performs — so a subsequent write is refused
// rather than sent to a stale replica.
func (r *Router) GetReader(ctx context.Context, application string) (*dbapi.Conn, error) {
	master := r.config.GetMasterForApplication(application)
	if master != nil {
		conn, err := r.getOrCreateConn(master, application, false)
		if err == nil {
			return conn, nil
		}
		r.mu.Lock()
		r.readOnly = true
		r.mu.Unlock()
	}

	replica := r.config.GetReplicaForApplication(application)
	if replica != nil {
		conn, err := r.getOrCreateConn(replica, application, true)
		if err == nil {
			return conn, nil
		}
	}

	if master == nil && replica == nil {
		return nil, dbapi.NewUnreachableError("no master or replica for %q", application)
	}
	return nil, dbapi.NewUnreachableError("all hosts unreachable for %q", application)
}

// Close releases every cached connection pool. It is called on shutdown.
func (r *Router) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		_ = c.Close()
	}
	r.conns = make(map[string]*dbapi.Conn)
	return nil
}

func (r *Router) getOrCreateConn(ref *dbapi.DatabaseRef, app string, readOnly bool) (*dbapi.Conn, error) {
	key := fmt.Sprintf("%s/%s/%v", ref.RefKey(), app, readOnly)

	r.mu.Lock()
	if c, ok := r.conns[key]; ok {
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()

	dsn := r.buildDSN(ref, app)

	conn, err := r.connect(dsn, readOnly, DefaultRetryPolicy())
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if cached, ok := r.conns[key]; ok {
		r.mu.Unlock()
		_ = conn.Close()
		return cached, nil
	}
	r.conns[key] = conn
	r.mu.Unlock()

	return conn, nil
}

func (r *Router) buildDSN(ref *dbapi.DatabaseRef, app string) dbapi.DSN {
	return dbapi.DSN{
		Host:            ref.Host,
		Port:            ref.Port,
		User:            ref.User,
		Password:        ref.PasswordOr(r.password),
		Database:        r.config.DatabaseName(app),
		MaxRetries:      3,
		ConnTimeoutSec:  10,
		QueryTimeoutSec: 30,
	}
}
