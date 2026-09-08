package dbapi

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// SetupService runs the environment checks Phorge's PhabricatorDatabaseSetupCheck
// and PhabricatorMySQLSetupCheck run: version, InnoDB, the meta_data database,
// and a set of server variables. Each finding is a contracts.SetupIssue whose
// IsFatal flag matches Phorge's own — a fatal issue is one Phorge blocks
// startup on.
type SetupService struct {
	config      *ClusterConfig
	password    string
	connFactory ConnFactory
}

func NewSetupService(cfg *ClusterConfig, password string) *SetupService {
	return &SetupService{config: cfg, password: password, connFactory: NewConn}
}

func (s *SetupService) SetConnFactory(f ConnFactory) { s.connFactory = f }

func (s *SetupService) buildDSN(ref *DatabaseRef) DSN {
	return DSN{
		Host:            ref.Host,
		Port:            ref.Port,
		User:            ref.User,
		Password:        ref.passwordOr(s.password),
		ConnTimeoutSec:  2,
		QueryTimeoutSec: 10,
	}
}

// CollectIssues runs the checks against every enabled node.
func (s *SetupService) CollectIssues(ctx context.Context) ([]contracts.SetupIssue, error) {
	issues := make([]contracts.SetupIssue, 0)
	for _, ref := range s.config.GetAllRefs() {
		if ref.Disabled {
			continue
		}
		issues = append(issues, s.checkRef(ctx, ref)...)
	}
	return issues, nil
}

func (s *SetupService) checkRef(ctx context.Context, ref *DatabaseRef) []contracts.SetupIssue {
	var issues []contracts.SetupIssue
	refKey := ref.RefKey()

	dsn := s.buildDSN(ref)
	conn, err := s.connFactory(dsn, true)
	if err != nil {
		return []contracts.SetupIssue{{
			Key: "db.connection", Name: "Database Connection Failed",
			Message: fmt.Sprintf("Cannot connect to %s", refKey), IsFatal: true, RefKey: refKey,
		}}
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Ping(ctx); err != nil {
		return []contracts.SetupIssue{{
			Key: "db.connection", Name: "Database Connection Failed",
			Message: fmt.Sprintf("Ping failed on %s", refKey), IsFatal: true, RefKey: refKey,
		}}
	}

	versionIssues, err := s.checkVersionAndEngine(ctx, conn, refKey)
	if err != nil {
		return setupQueryFailure(refKey)
	}
	issues = append(issues, versionIssues...)

	if s.config.ServesApplication(ref, "meta_data") {
		metadataIssues, err := s.checkMetaDataDB(ctx, conn, refKey)
		if err != nil {
			return setupQueryFailure(refKey)
		}
		issues = append(issues, metadataIssues...)
	}

	variableIssues, err := s.checkServerVariables(ctx, conn, refKey)
	if err != nil {
		return setupQueryFailure(refKey)
	}
	issues = append(issues, variableIssues...)
	return issues
}

func setupQueryFailure(refKey string) []contracts.SetupIssue {
	return []contracts.SetupIssue{{
		Key: "db.connection", Name: "Database Query Failed",
		Message: fmt.Sprintf("A setup query failed on %s", refKey), IsFatal: true, RefKey: refKey,
	}}
}

func (s *SetupService) checkVersionAndEngine(ctx context.Context, conn *Conn, refKey string) ([]contracts.SetupIssue, error) {
	var issues []contracts.SetupIssue

	var version string
	if err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return nil, err
	}
	issues = append(issues, s.checkVersion(refKey, version)...)

	rows, err := conn.QueryContext(ctx, "SHOW ENGINES")
	if err != nil {
		return nil, err
	}
	hasInnoDB := false
	for rows.Next() {
		var engine, support string
		var extra1, extra2, extra3, extra4 sql.NullString
		if err := rows.Scan(&engine, &support, &extra1, &extra2, &extra3, &extra4); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if engine == "InnoDB" && (support == "YES" || support == "DEFAULT") {
			hasInnoDB = true
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if !hasInnoDB {
		issues = append(issues, contracts.SetupIssue{
			Key: "mysql.innodb", Name: "InnoDB Not Available",
			Message: fmt.Sprintf("InnoDB engine not available on %s", refKey), IsFatal: true, RefKey: refKey,
		})
	}
	return issues, nil
}

func (s *SetupService) checkMetaDataDB(ctx context.Context, conn *Conn, refKey string) ([]contracts.SetupIssue, error) {
	metaDB := s.config.DatabaseName("meta_data")
	rows, err := conn.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	found := false
	for rows.Next() {
		var db string
		if err := rows.Scan(&db); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if db == metaDB {
			found = true
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if !found {
		return []contracts.SetupIssue{{
			Key: "storage.upgrade", Name: "Setup MySQL Schema",
			Message: fmt.Sprintf("Database %s not found on %s. Run bin/storage upgrade.", metaDB, refKey),
			IsFatal: true, RefKey: refKey,
		}}, nil
	}
	return nil, nil
}

func (s *SetupService) checkVersion(refKey, version string) []contracts.SetupIssue {
	isMariaDB := strings.Contains(strings.ToLower(version), "mariadb")
	ver := strings.SplitN(version, "-", 2)[0]

	var minVer, name string
	if isMariaDB {
		minVer, name = "10.5.1", "MariaDB"
	} else {
		minVer, name = "8.0.0", "MySQL"
	}

	if compareVersions(ver, minVer) < 0 {
		return []contracts.SetupIssue{{
			Key: "mysql.version", Name: fmt.Sprintf("Update %s", name),
			Message: fmt.Sprintf("Running %s %s, minimum required is %s", name, ver, minVer),
			IsFatal: true, RefKey: refKey,
		}}
	}
	return nil
}

func (s *SetupService) checkServerVariables(ctx context.Context, conn *Conn, refKey string) ([]contracts.SetupIssue, error) {
	var issues []contracts.SetupIssue

	var maxPacket int64
	if err := conn.QueryRowContext(ctx, "SELECT @@max_allowed_packet").Scan(&maxPacket); err != nil {
		return nil, err
	}
	if maxPacket < 32*1024*1024 {
		issues = append(issues, contracts.SetupIssue{
			Key: "mysql.max_allowed_packet", Name: "Small max_allowed_packet",
			Message: fmt.Sprintf("max_allowed_packet=%d on %s, recommended >= 33554432", maxPacket, refKey),
			RefKey:  refKey,
		})
	}

	var sqlMode string
	if err := conn.QueryRowContext(ctx, "SELECT @@sql_mode").Scan(&sqlMode); err != nil {
		return nil, err
	}
	if !strings.Contains(sqlMode, "STRICT_ALL_TABLES") {
		issues = append(issues, contracts.SetupIssue{
			Key: "sql_mode.strict", Name: "STRICT_ALL_TABLES Not Set",
			Summary: fmt.Sprintf("MySQL on %s not in strict mode", refKey),
			Message: "Enable STRICT_ALL_TABLES in sql_mode for safer behavior",
			RefKey:  refKey,
		})
	}

	var poolSize int64
	if err := conn.QueryRowContext(ctx, "SELECT @@innodb_buffer_pool_size").Scan(&poolSize); err != nil {
		return nil, err
	}
	if poolSize < 225*1024*1024 {
		issues = append(issues, contracts.SetupIssue{
			Key: "mysql.innodb_buffer_pool_size", Name: "Small Buffer Pool",
			Message: fmt.Sprintf("innodb_buffer_pool_size=%d on %s, recommended >= 235929600", poolSize, refKey),
			RefKey:  refKey,
		})
	}

	var localInfile int
	if err := conn.QueryRowContext(ctx, "SELECT @@local_infile").Scan(&localInfile); err != nil {
		return nil, err
	}
	if localInfile != 0 {
		issues = append(issues, contracts.SetupIssue{
			Key: "mysql.local_infile", Name: "Unsafe local_infile Enabled",
			Message: fmt.Sprintf("local_infile is enabled on %s, disable it for security", refKey),
			RefKey:  refKey,
		})
	}

	var epoch int64
	if err := conn.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()").Scan(&epoch); err != nil {
		return nil, err
	}
	delta := time.Now().Unix() - epoch
	if delta < 0 {
		delta = -delta
	}
	if delta > 60 {
		issues = append(issues, contracts.SetupIssue{
			Key: "mysql.clock", Name: "Major Clock Skew",
			Message: fmt.Sprintf("Clock skew of %d seconds between app server and %s", delta, refKey),
			RefKey:  refKey,
		})
	}

	return issues, nil
}

// compareVersions compares two dotted version strings numerically over their
// first three segments, so 8.0.32 and 8.0.32-ubuntu compare as equal once the
// suffix has been split off by the caller.
func compareVersions(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		va, vb := 0, 0
		if i < len(pa) {
			_, _ = fmt.Sscanf(pa[i], "%d", &va)
		}
		if i < len(pb) {
			_, _ = fmt.Sscanf(pb[i], "%d", &vb)
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}
