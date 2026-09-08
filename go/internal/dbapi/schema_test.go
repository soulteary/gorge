package dbapi

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// The schema/setup tests are ported from the standalone service's
// internal/schema/setup_test.go. The version comparison and the setup checks
// are the domain algorithm preserved from Phorge's PhabricatorDatabaseSetupCheck
// / PhabricatorMySQLSetupCheck; the SQLite arm the standalone service also
// tested is gone, and checkMySQLConfig is now checkServerVariables. Findings
// are asserted against the camelCase contracts.SetupIssue this package emits.

// mockConnFactory returns a ConnFactory that hands back a sqlmock-backed Conn,
// so a setup check can be driven without a real MySQL.
func mockConnFactory(db *sql.DB) ConnFactory {
	return func(dsn DSN, readOnly bool) (*Conn, error) {
		return NewConnFromDB(db, dsn, readOnly), nil
	}
}

// failConnFactory returns a ConnFactory that always fails to connect.
func failConnFactory() ConnFactory {
	return func(dsn DSN, readOnly bool) (*Conn, error) {
		return nil, sql.ErrConnDone
	}
}

func singleNode(t *testing.T) *ClusterConfig {
	t.Helper()
	return &ClusterConfig{
		Refs:      []*DatabaseRef{{Host: "h1", Port: 3306, IsMaster: true}},
		Namespace: "phorge",
		masters:   []*DatabaseRef{{Host: "h1", Port: 3306, IsMaster: true}},
	}
}

func TestCompareVersions(t *testing.T) {
	equal := []struct{ a, b string }{
		{"8.0.0", "8.0.0"}, {"10.5.1", "10.5.1"}, {"8", "8.0.0"}, {"8.0", "8.0.0"},
	}
	for _, tc := range equal {
		if compareVersions(tc.a, tc.b) != 0 {
			t.Errorf("compareVersions(%q,%q) != 0", tc.a, tc.b)
		}
	}
	less := []struct{ a, b string }{
		{"7.0.0", "8.0.0"}, {"8.0.0", "8.0.1"}, {"8.0.0", "8.1.0"},
		{"10.4.9", "10.5.1"}, {"5.7.42", "8.0.0"},
	}
	for _, tc := range less {
		if compareVersions(tc.a, tc.b) >= 0 {
			t.Errorf("expected %s < %s", tc.a, tc.b)
		}
	}
	greater := []struct{ a, b string }{
		{"8.0.1", "8.0.0"}, {"8.1.0", "8.0.0"}, {"9.0.0", "8.0.0"}, {"10.6.0", "10.5.1"},
	}
	for _, tc := range greater {
		if compareVersions(tc.a, tc.b) <= 0 {
			t.Errorf("expected %s > %s", tc.a, tc.b)
		}
	}
}

// TestCheckVersion pins the minimum-version gate for both MySQL and MariaDB,
// including the exact minimum passing.
func TestCheckVersion(t *testing.T) {
	svc := &SetupService{}
	cases := []struct {
		version  string
		wantFail bool
	}{
		{"8.0.33", false},
		{"5.7.42", true},
		{"8.0.0", false}, // exact minimum
		{"10.6.12-MariaDB", false},
		{"10.4.9-MariaDB-1:10.4.9+maria~focal", true},
		{"10.5.1-MariaDB", false}, // exact minimum
	}
	for _, tc := range cases {
		issues := svc.checkVersion("ref", tc.version)
		if tc.wantFail && (len(issues) != 1 || !issues[0].IsFatal) {
			t.Errorf("%q should fail fatally, got %v", tc.version, issues)
		}
		if !tc.wantFail && len(issues) != 0 {
			t.Errorf("%q should pass, got %v", tc.version, issues)
		}
	}
}

func TestNewSetupServiceHasConnFactory(t *testing.T) {
	svc := NewSetupService(singleNode(t), "pass")
	if svc.connFactory == nil {
		t.Error("connFactory should default to NewConn")
	}
}

// TestCheckRefConnectionFailed: a connect failure is a single fatal
// db.connection issue, the generic report that names the ref but no internals.
func TestCheckRefConnectionFailed(t *testing.T) {
	cfg := singleNode(t)
	svc := &SetupService{config: cfg, connFactory: failConnFactory()}
	issues := svc.checkRef(context.Background(), cfg.Refs[0])
	if len(issues) != 1 {
		t.Fatalf("expected one connection issue, got %d", len(issues))
	}
	if issues[0].Key != "db.connection" || !issues[0].IsFatal {
		t.Errorf("unexpected issue: %+v", issues[0])
	}
}

func TestCheckRefPingFailed(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.MonitorPingsOption(true))
	defer func() { _ = db.Close() }()
	mock.ExpectPing().WillReturnError(sql.ErrConnDone)
	mock.ExpectClose()

	cfg := singleNode(t)
	svc := &SetupService{config: cfg, connFactory: mockConnFactory(db)}
	issues := svc.checkRef(context.Background(), cfg.Refs[0])
	if len(issues) == 0 || issues[0].Key != "db.connection" {
		t.Errorf("expected ping-failure issue, got %v", issues)
	}
}

// expectHealthyServerVariables queues the five server-variable rows at their
// healthy values, so a full-pass check produces no variable warnings.
func expectHealthyServerVariables(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("SELECT @@max_allowed_packet").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow(int64(64 * 1024 * 1024)))
	mock.ExpectQuery("SELECT @@sql_mode").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow("STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION"))
	mock.ExpectQuery("SELECT @@innodb_buffer_pool_size").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow(int64(512 * 1024 * 1024)))
	mock.ExpectQuery("SELECT @@local_infile").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow(0))
	mock.ExpectQuery("SELECT UNIX_TIMESTAMP").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow(time.Now().Unix()))
}

func TestCheckRefFullPass(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.MonitorPingsOption(true))
	defer func() { _ = db.Close() }()

	mock.ExpectPing()
	mock.ExpectQuery("SELECT VERSION").WillReturnRows(
		sqlmock.NewRows([]string{"VERSION()"}).AddRow("8.0.33"))
	mock.ExpectQuery("SHOW ENGINES").WillReturnRows(
		sqlmock.NewRows([]string{"Engine", "Support", "c1", "c2", "c3", "c4"}).
			AddRow("InnoDB", "DEFAULT", nil, nil, nil, nil))
	mock.ExpectQuery("SHOW DATABASES").WillReturnRows(
		sqlmock.NewRows([]string{"Database"}).AddRow("phorge_meta_data"))
	expectHealthyServerVariables(mock)
	mock.ExpectClose()

	cfg := singleNode(t)
	svc := &SetupService{config: cfg, connFactory: mockConnFactory(db)}
	if issues := svc.checkRef(context.Background(), cfg.Refs[0]); len(issues) != 0 {
		t.Errorf("expected 0 issues for a healthy server, got %v", issues)
	}
}

func TestCheckRefNoInnoDBIsFatal(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.MonitorPingsOption(true))
	defer func() { _ = db.Close() }()

	mock.ExpectPing()
	mock.ExpectQuery("SELECT VERSION").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow("8.0.33"))
	mock.ExpectQuery("SHOW ENGINES").WillReturnRows(
		sqlmock.NewRows([]string{"Engine", "Support", "c1", "c2", "c3", "c4"}).
			AddRow("MyISAM", "YES", nil, nil, nil, nil))
	mock.ExpectQuery("SHOW DATABASES").WillReturnRows(
		sqlmock.NewRows([]string{"Database"}).AddRow("phorge_meta_data"))
	expectHealthyServerVariables(mock)
	mock.ExpectClose()

	cfg := singleNode(t)
	svc := &SetupService{config: cfg, connFactory: mockConnFactory(db)}
	if !hasIssue(svc.checkRef(context.Background(), cfg.Refs[0]), "mysql.innodb") {
		t.Error("expected a mysql.innodb issue when InnoDB is unavailable")
	}
}

func TestCheckRefMissingMetaDataDatabase(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.MonitorPingsOption(true))
	defer func() { _ = db.Close() }()

	mock.ExpectPing()
	mock.ExpectQuery("SELECT VERSION").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow("8.0.33"))
	mock.ExpectQuery("SHOW ENGINES").WillReturnRows(
		sqlmock.NewRows([]string{"Engine", "Support", "c1", "c2", "c3", "c4"}).
			AddRow("InnoDB", "YES", nil, nil, nil, nil))
	mock.ExpectQuery("SHOW DATABASES").WillReturnRows(
		sqlmock.NewRows([]string{"Database"}).AddRow("some_other_db"))
	expectHealthyServerVariables(mock)
	mock.ExpectClose()

	cfg := singleNode(t)
	svc := &SetupService{config: cfg, connFactory: mockConnFactory(db)}
	if !hasIssue(svc.checkRef(context.Background(), cfg.Refs[0]), "storage.upgrade") {
		t.Error("expected a storage.upgrade issue when the meta_data database is absent")
	}
}

// TestCheckServerVariablesAllWarn: every server variable at an unhealthy value
// produces its warning, none of them fatal.
func TestCheckServerVariablesAllWarn(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT @@max_allowed_packet").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow(int64(1024 * 1024)))
	mock.ExpectQuery("SELECT @@sql_mode").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow("NO_ENGINE_SUBSTITUTION"))
	mock.ExpectQuery("SELECT @@innodb_buffer_pool_size").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow(int64(8 * 1024 * 1024)))
	mock.ExpectQuery("SELECT @@local_infile").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow(1))
	mock.ExpectQuery("SELECT UNIX_TIMESTAMP").WillReturnRows(
		sqlmock.NewRows([]string{"v"}).AddRow(int64(1_000_000)))

	svc := &SetupService{}
	issues := svc.checkServerVariables(context.Background(), NewConnFromDB(db, DSN{}, true), "h1:3306")
	for _, key := range []string{
		"mysql.max_allowed_packet", "sql_mode.strict",
		"mysql.innodb_buffer_pool_size", "mysql.local_infile", "mysql.clock",
	} {
		if !hasIssue(issues, key) {
			t.Errorf("expected a %q warning", key)
		}
	}
	for _, i := range issues {
		if i.IsFatal {
			t.Errorf("server-variable issue %q must be a warning, not fatal", i.Key)
		}
	}
}

func TestCheckServerVariablesAllGood(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	expectHealthyServerVariables(mock)

	svc := &SetupService{}
	issues := svc.checkServerVariables(context.Background(), NewConnFromDB(db, DSN{}, true), "h1:3306")
	if len(issues) != 0 {
		t.Errorf("expected 0 issues for healthy variables, got %v", issues)
	}
}

func TestCollectIssuesSkipsDisabled(t *testing.T) {
	cfg := &ClusterConfig{
		Refs:      []*DatabaseRef{{Host: "h1", Disabled: true}},
		Namespace: "phorge",
	}
	svc := &SetupService{config: cfg, connFactory: failConnFactory()}
	issues, err := svc.CollectIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Errorf("a disabled ref should be skipped, got %d issues", len(issues))
	}
}

func TestCollectIssuesAggregatesAcrossRefs(t *testing.T) {
	cfg := &ClusterConfig{
		Refs:      []*DatabaseRef{{Host: "h1", Port: 3306}, {Host: "h2", Port: 3306}},
		Namespace: "phorge",
	}
	svc := &SetupService{config: cfg, connFactory: failConnFactory()}
	issues, _ := svc.CollectIssues(context.Background())
	if len(issues) != 2 {
		t.Errorf("expected 2 connection issues (one per ref), got %d", len(issues))
	}
}

func hasIssue(issues []contracts.SetupIssue, key string) bool {
	for _, i := range issues {
		if i.Key == key {
			return true
		}
	}
	return false
}
