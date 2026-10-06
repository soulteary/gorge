package taskqueue

import (
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/soulteary/gorge/go/internal/contracts"
	"testing"
	"time"
)

func executionRows(expiry int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "taskClass", "leaseOwner", "leaseExpires", "failureCount", "dataID", "failureTime", "priority", "objectPHID", "containerPHID", "dateCreated", "dateModified"}).AddRow(7, "Parent", "owner", expiry, 0, 9, nil, 2500, nil, nil, 10, 10)
}
func executionRequest() *contracts.FinalizeRequest {
	return &contracts.FinalizeRequest{ExecutionLease: contracts.ExecutionLease{TaskID: 7, LeaseOwner: "owner", LeaseExpires: time.Now().Add(time.Hour).Unix()}, Duration: 12}
}
func TestFinalizeMySQLRejectsReassignedLease(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	req := executionRequest()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT.*FOR UPDATE").WithArgs(int64(7)).WillReturnRows(executionRows(req.LeaseExpires + 1))
	mock.ExpectRollback()
	err := (&MySQLStore{db: db}).Finalize(t.Context(), req)
	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestFinalizeMySQLLostResponseReplayDoesNotEnqueueAgain(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	req := executionRequest()
	req.Followups = []contracts.EnqueueRequest{{TaskClass: "Child", Data: "{}"}}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT.*FOR UPDATE").WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("SELECT leaseOwner, leaseExpires, result FROM worker_archivetask").WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"leaseOwner", "leaseExpires", "result"}).AddRow("owner", req.LeaseExpires, 0))
	mock.ExpectCommit()
	if err := (&MySQLStore{db: db}).Finalize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestFinalizeMySQLChildFailureRollsBackParent(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	req := executionRequest()
	req.Followups = []contracts.EnqueueRequest{{TaskClass: "Child", Data: "{}"}}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT.*FOR UPDATE").WithArgs(int64(7)).WillReturnRows(executionRows(req.LeaseExpires))
	mock.ExpectExec("INSERT INTO worker_taskdata").WithArgs("{}").WillReturnError(errors.New("storage unavailable"))
	mock.ExpectRollback()
	if err := (&MySQLStore{db: db}).Finalize(t.Context(), req); err == nil {
		t.Fatal("expected rollback")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestFinalizeMySQLArchivesOnlyInsideTransaction(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	req := executionRequest()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT.*FOR UPDATE").WithArgs(int64(7)).WillReturnRows(executionRows(req.LeaseExpires))
	mock.ExpectExec("INSERT INTO worker_archivetask").WillReturnResult(sqlmock.NewResult(7, 1))
	mock.ExpectExec("DELETE FROM worker_activetask").WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := (&MySQLStore{db: db}).Finalize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestRenewMySQLKeepsLongerExistingLease(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	req := executionRequest()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT.*FOR UPDATE").WithArgs(int64(7)).WillReturnRows(executionRows(req.LeaseExpires))
	mock.ExpectExec("UPDATE worker_activetask SET leaseExpires").WithArgs(req.LeaseExpires, int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	task, err := (&MySQLStore{db: db}).Renew(t.Context(), &contracts.RenewRequest{ExecutionLease: req.ExecutionLease, Duration: 30})
	if err != nil || *task.LeaseExpires != req.LeaseExpires {
		t.Fatalf("task=%v err=%v", task, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
