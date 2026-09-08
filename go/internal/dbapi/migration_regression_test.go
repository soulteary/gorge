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
