package dbapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// MigrationService reports how far Phorge's `bin/storage upgrade` has run by
// reading `{namespace}_meta_data.patch_status` on the masters that own the
// metadata partition. A replica receives the same rows through replication.
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
		Password:        ref.PasswordOr(m.password),
		Database:        m.config.DatabaseName("meta_data"),
		ConnTimeoutSec:  2,
		QueryTimeoutSec: 10,
	}
}

// Status returns the migration status of every enabled master that serves the
// meta_data application. Reporting all such masters (not only the first one
// the router would pick) lets Phorge compare their committed cluster state and
// catch a master that started with an out-of-date configuration; masters for
// other application partitions do not carry this database and are not reported.
func (m *MigrationService) Status(ctx context.Context) ([]contracts.MigrationStatus, error) {
	statuses := make([]contracts.MigrationStatus, 0)
	seen := make(map[string]bool)
	for _, ref := range m.config.Masters() {
		if ref.Disabled {
			continue
		}
		if !m.config.ServesApplication(ref, "meta_data") {
			continue
		}
		key := ref.RefKey()
		if seen[key] {
			continue
		}
		seen[key] = true

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

	for rows.Next() {
		var patch string
		if err := rows.Scan(&patch); err != nil {
			return st, err
		}
		st.AppliedPatches = append(st.AppliedPatches, patch)
	}
	if err := rows.Err(); err != nil {
		return st, classifyMySQLError(err)
	}

	// hoststate carries the raw cluster.databases state Phorge commits between
	// masters. The raw value names hosts, so it is never returned; its SHA-256
	// digest is, which lets Phorge detect that two masters disagree on the
	// committed topology (db.state.desync) without this service holding it. A
	// missing row leaves the digest empty.
	var stateValue *string
	_ = conn.QueryRowContext(ctx,
		"SELECT stateValue FROM hoststate WHERE stateKey = 'cluster.databases'").Scan(&stateValue)
	if stateValue != nil {
		sum := sha256.Sum256([]byte(*stateValue))
		st.ClusterStateDigest = hex.EncodeToString(sum[:])
	}

	return st, nil
}
