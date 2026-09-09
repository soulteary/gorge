package dbapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

func TestMigrationStatusPropagatesLedgerQueryFailures(t *testing.T) {
	tests := []struct {
		name     string
		queryErr error
		wantKind errorKind
	}{
		{name: "access denied", queryErr: &mysql.MySQLError{Number: 1142, Message: "denied"}, wantKind: kindAccessDenied},
		{name: "connection lost", queryErr: errors.New("connection reset"), wantKind: kindUnreachable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			mock.ExpectPing()
			mock.ExpectQuery("SELECT patch FROM patch_status").WillReturnError(test.queryErr)
			mock.ExpectClose()

			ref := &DatabaseRef{Host: "db1", Port: 3306, IsMaster: true}
			svc := NewMigrationService(&ClusterConfig{
				Refs: []*DatabaseRef{ref}, Namespace: "phorge", masters: []*DatabaseRef{ref},
			}, "secret")
			svc.SetConnFactory(mockConnFactory(db))
			_, gotErr := svc.Status(context.Background())
			var dbErr *DBError
			if !errors.As(gotErr, &dbErr) || dbErr.Kind != test.wantKind {
				t.Fatalf("error = %v, want kind %d", gotErr, test.wantKind)
			}
		})
	}
}

func TestMigrationStatusUsesOnlyTheMetadataMaster(t *testing.T) {
	defaultMaster := &DatabaseRef{Host: "default", Port: 3306, IsMaster: true, IsDefaultPartition: true}
	metadataMaster := &DatabaseRef{
		Host: "metadata", Port: 3306, IsMaster: true,
		ApplicationMap: map[string]bool{"meta_data": true},
	}
	applicationMaster := &DatabaseRef{
		Host: "maniphest", Port: 3306, IsMaster: true,
		ApplicationMap: map[string]bool{"maniphest": true},
	}
	masters := []*DatabaseRef{defaultMaster, applicationMaster, metadataMaster}
	svc := NewMigrationService(&ClusterConfig{
		Refs: masters, Namespace: "phorge", masters: masters,
	}, "secret")
	var connected []string
	svc.SetConnFactory(func(dsn DSN, readOnly bool) (*Conn, error) {
		connected = append(connected, dsn.Host)
		return nil, errors.New("not initialized")
	})

	statuses, err := svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].RefKey != metadataMaster.RefKey() {
		t.Fatalf("migration statuses = %+v, want only metadata master", statuses)
	}
	if len(connected) != 1 || connected[0] != "metadata" {
		t.Fatalf("connected hosts = %v, want [metadata]", connected)
	}
}

// TestMigrationStatusReadsClusterStatePresence drives the hoststate query the
// checkRef runs after patch_status and asserts the presence/digest contract:
// a present row yields ClusterStatePresent=true and a digest that is the
// SHA-256 hex of the raw stateValue bytes, while a genuinely absent row
// (sql.ErrNoRows) yields present=false, an empty digest, and a successful
// status that still reports initialized=true.
func TestMigrationStatusReadsClusterStatePresence(t *testing.T) {
	const rawState = `{"databases":{"db1":{"master":true}}}`
	wantDigest := func() string {
		sum := sha256.Sum256([]byte(rawState))
		return hex.EncodeToString(sum[:])
	}()

	tests := []struct {
		name        string
		seedState   func(sqlmock.Sqlmock)
		wantPresent bool
		wantDigest  string
	}{
		{
			name: "row present",
			seedState: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery("SELECT stateValue FROM hoststate").
					WillReturnRows(sqlmock.NewRows([]string{"stateValue"}).AddRow(rawState))
			},
			wantPresent: true,
			wantDigest:  wantDigest,
		},
		{
			name: "row absent",
			seedState: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery("SELECT stateValue FROM hoststate").
					WillReturnError(sql.ErrNoRows)
			},
			wantPresent: false,
			wantDigest:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			mock.ExpectPing()
			mock.ExpectQuery("SELECT patch FROM patch_status").
				WillReturnRows(sqlmock.NewRows([]string{"patch"}).AddRow("phabricator:0001.legacy.sql"))
			test.seedState(mock)
			mock.ExpectClose()

			ref := &DatabaseRef{Host: "db1", Port: 3306, IsMaster: true}
			svc := NewMigrationService(&ClusterConfig{
				Refs: []*DatabaseRef{ref}, Namespace: "phorge", masters: []*DatabaseRef{ref},
			}, "secret")
			svc.SetConnFactory(mockConnFactory(db))

			statuses, gotErr := svc.Status(context.Background())
			if gotErr != nil {
				t.Fatalf("Status returned error: %v", gotErr)
			}
			if len(statuses) != 1 {
				t.Fatalf("statuses = %+v, want exactly one", statuses)
			}
			st := statuses[0]
			if !st.Initialized {
				t.Fatalf("Initialized = false, want true (the ledger query succeeded)")
			}
			if st.ClusterStatePresent != test.wantPresent {
				t.Fatalf("ClusterStatePresent = %v, want %v", st.ClusterStatePresent, test.wantPresent)
			}
			if st.ClusterStateDigest != test.wantDigest {
				t.Fatalf("ClusterStateDigest = %q, want %q", st.ClusterStateDigest, test.wantDigest)
			}
		})
	}
}

// TestMigrationStatusPropagatesClusterStateQueryFailures asserts a hoststate
// query failure is surfaced as a classified DBError rather than faked as an
// absent row: an access-denied errno (1142) becomes a kindAccessDenied error
// and a dropped connection becomes a kindUnreachable error. In both cases the
// overall Status must fail — no initialized=true, present=false success is
// allowed to leak from a query the service could not actually run.
func TestMigrationStatusPropagatesClusterStateQueryFailures(t *testing.T) {
	tests := []struct {
		name     string
		queryErr error
		wantKind errorKind
	}{
		{name: "access denied", queryErr: &mysql.MySQLError{Number: 1142, Message: "denied"}, wantKind: kindAccessDenied},
		{name: "connection reset", queryErr: errors.New("connection reset by peer"), wantKind: kindUnreachable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			mock.ExpectPing()
			mock.ExpectQuery("SELECT patch FROM patch_status").
				WillReturnRows(sqlmock.NewRows([]string{"patch"}).AddRow("phabricator:0001.legacy.sql"))
			mock.ExpectQuery("SELECT stateValue FROM hoststate").WillReturnError(test.queryErr)
			mock.ExpectClose()

			ref := &DatabaseRef{Host: "db1", Port: 3306, IsMaster: true}
			svc := NewMigrationService(&ClusterConfig{
				Refs: []*DatabaseRef{ref}, Namespace: "phorge", masters: []*DatabaseRef{ref},
			}, "secret")
			svc.SetConnFactory(mockConnFactory(db))

			statuses, gotErr := svc.Status(context.Background())
			if statuses != nil {
				t.Fatalf("statuses = %+v, want nil on a hoststate query failure", statuses)
			}
			var dbErr *DBError
			if !errors.As(gotErr, &dbErr) || dbErr.Kind != test.wantKind {
				t.Fatalf("error = %v, want kind %d", gotErr, test.wantKind)
			}
		})
	}
}
