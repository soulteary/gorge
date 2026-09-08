package dbapi

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// TxManager implements nested-transaction semantics with MySQL savepoints,
// matching Phorge's AphrontDatabaseConnection: the outermost Begin opens a real
// transaction, and each nested Begin creates a savepoint named
// `Aphront_Savepoint_N`. The savepoint naming is a compatibility contract, not
// an implementation detail — it is the same string Phorge emits.
type TxManager struct {
	conn  *Conn
	tx    *sql.Tx
	depth int
	mu    sync.Mutex
}

func NewTxManager(conn *Conn) *TxManager { return &TxManager{conn: conn} }

func (m *TxManager) Depth() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.depth
}

// Begin opens a transaction at depth 0 or a savepoint above it.
func (m *TxManager) Begin(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.depth == 0 {
		tx, err := m.conn.DB().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		m.tx = tx
	} else {
		name := m.savepointName()
		if _, err := m.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
			return err
		}
	}
	m.depth++
	return nil
}

// Commit commits the real transaction at depth 1, or just unwinds a level
// above it (a savepoint is released implicitly on the outer commit).
func (m *TxManager) Commit(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.depth <= 0 {
		return fmt.Errorf("no open transaction to commit")
	}
	m.depth--
	if m.depth == 0 {
		err := m.tx.Commit()
		m.tx = nil
		return err
	}
	return nil
}

// Rollback rolls back the real transaction at depth 1, or to the savepoint
// above it.
func (m *TxManager) Rollback(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.depth <= 0 {
		return fmt.Errorf("no open transaction to rollback")
	}
	m.depth--
	if m.depth == 0 {
		err := m.tx.Rollback()
		m.tx = nil
		return err
	}
	name := m.savepointName()
	_, err := m.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name)
	return err
}

func (m *TxManager) IsInsideTransaction() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.depth > 0
}

func (m *TxManager) savepointName() string {
	return fmt.Sprintf("Aphront_Savepoint_%d", m.depth)
}
