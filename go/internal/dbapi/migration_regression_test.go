package dbapi

import (
	"context"
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
