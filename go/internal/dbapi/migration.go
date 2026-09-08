package dbapi

import (
	"context"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// MigrationService reports how far Phorge's `bin/storage upgrade` has run on
// each master, by reading `{namespace}_meta_data.patch_status`. Only masters
// are reported: a replica receives the same rows through replication.
type MigrationService struct {
	config      *ClusterConfig
	password    string
	connFactory ConnFactory
}

func NewMigrationService(cfg *ClusterConfig, password string) *MigrationService {
	return &MigrationService{config: cfg, password: password, connFactory: NewConn}
}

func (m *MigrationService) SetConnFactory(f ConnFactory) { m.connFactory = f }

func (m *MigrationService) buildDSN(ref *DatabaseRef) DSN {
	return DSN{
		Host:            ref.Host,
		Port:            ref.Port,
		User:            ref.User,
		Password:        ref.passwordOr(m.password),
		Database:        m.config.DatabaseName("meta_data"),
		ConnTimeoutSec:  2,
		QueryTimeoutSec: 10,
	}
}

// Status returns the migration status of each enabled master.
func (m *MigrationService) Status(ctx context.Context) ([]contracts.MigrationStatus, error) {
	statuses := make([]contracts.MigrationStatus, 0)
	for _, ref := range m.config.GetAllRefs() {
		if ref.Disabled || !ref.IsMaster {
			continue
		}
		status, err := m.checkRef(ctx, ref)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// checkRef reads one master. A connection or ping failure leaves Initialized
// false, which is the honest pre-upgrade report when the meta_data database does
// not exist yet. Once Ping succeeds, patch_status failures are returned so an
// unreadable ledger is not reported as an initialized database with no patches.
func (m *MigrationService) checkRef(ctx context.Context, ref *DatabaseRef) (contracts.MigrationStatus, error) {
	st := contracts.MigrationStatus{RefKey: ref.RefKey(), AppliedPatches: []string{}}

	dsn := m.buildDSN(ref)
	conn, err := m.connFactory(dsn, true)
	if err != nil {
		return st, nil
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Ping(ctx); err != nil {
		return st, nil
	}
	st.Initialized = true

	rows, err := conn.QueryContext(ctx, "SELECT patch FROM patch_status")
	if err != nil {
		return st, classifyMySQLError(err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]bool)
	for rows.Next() {
		var patch string
		if err := rows.Scan(&patch); err != nil {
			return st, err
		}
		applied[patch] = true
		st.AppliedPatches = append(st.AppliedPatches, patch)
	}
	if err := rows.Err(); err != nil {
		return st, classifyMySQLError(err)
	}
	st.TotalExpected = len(applied)

	// hoststate carries the cluster.databases state Phorge syncs between
	// masters. It is read and discarded here, kept as the interface point for
	// the multi-master sync the standalone service reserved it for.
	var stateValue *string
	_ = conn.QueryRowContext(ctx,
		"SELECT stateValue FROM hoststate WHERE stateKey = 'cluster.databases'").Scan(&stateValue)

	return st, nil
}
