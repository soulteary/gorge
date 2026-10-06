package filestorage

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
)

func TestDeletionDatabaseNamespaceBoundary(t *testing.T) {
	if e := ValidateDeletionDatabase("tenant", "user:password@tcp(mysql:3306)/tenant_file"); e != nil {
		t.Fatal(e)
	}
	for _, dsn := range []string{"user:password@tcp(mysql:3306)/other_file", "user:password@tcp(mysql:3306)/", "malformed"} {
		if e := ValidateDeletionDatabase("tenant", dsn); e == nil {
			t.Fatal("accepted wrong deletion database")
		}
	}
}

func TestDeletionConsumerReferencesAndRetry(t *testing.T) {
	for _, mode := range []string{"referenced", "gone", "bad-backend"} {
		t.Run(mode, func(t *testing.T) {
			db, m, e := sqlmock.New()
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = db.Close() }()
			m.ExpectBegin()
			handle := "local-disk/ab/cd/0123456789abcdef0123456789ab"
			m.ExpectQuery("SELECT id,storageEngine").WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"id", "storageEngine", "storageHandle", "attempts"}).AddRow(1, "gorge", handle, 0))
			q := m.ExpectQuery("SELECT id FROM file WHERE").WithArgs("gorge", handle)
			state := "cancelled"
			if mode == "referenced" {
				q.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(2))
			} else {
				q.WillReturnError(sql.ErrNoRows)
				state = "pending"
			}
			router := NewRouter(nil)
			if mode == "gone" {
				e, _ := NewLocalDiskEngine(t.TempDir())
				router = NewRouter([]StorageEngine{e})
				state = "done"
			}
			m.ExpectExec("UPDATE file_gorgedeletion").WithArgs(state, sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()
			if e := ProcessDeletion(context.Background(), db, router, nil); e != nil {
				t.Fatal(e)
			}
			if e = m.ExpectationsWereMet(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestDeletionRollbackOnSourceFailure(t *testing.T) {
	db, m, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	m.ExpectBegin()
	m.ExpectQuery("SELECT id,storageEngine").WillReturnError(fmt.Errorf("unavailable"))
	m.ExpectRollback()
	if e := ProcessDeletion(context.Background(), db, NewRouter(nil), nil); e == nil {
		t.Fatal("lost source error")
	}
	if e := m.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
