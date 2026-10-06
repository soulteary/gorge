package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMailProjectionKeepsMissingAcknowledgmentPending(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	php := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"result":{"applied":false}}`)); err != nil {
			t.Error(err)
		}
	}))
	defer php.Close()
	mock.ExpectQuery("SELECT deliveryID,payload,result,revision").WillReturnRows(sqlmock.NewRows([]string{"deliveryID", "payload", "result", "revision", "projectionAttempts"}).AddRow("mail/1", `{"mailID":7}`, `{"deliveryID":"mail/1","state":"accepted","revision":2}`, 2, 0))
	mock.ExpectExec("UPDATE gorge_mail_delivery SET projectionAttempts").WithArgs(sqlmock.AnyArg(), "mail/1", 2).WillReturnResult(sqlmock.NewResult(0, 1))
	if err = ProjectMailResultsOnce(context.Background(), db, NewConduitClient(php.URL, "token")); err == nil {
		t.Fatal("missing projection acknowledgment cleared pending receipt")
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestMailProjectionAcknowledgesExactRevision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	php := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"result":{"applied":true}}`)); err != nil {
			t.Error(err)
		}
	}))
	defer php.Close()
	mock.ExpectQuery("SELECT deliveryID,payload,result,revision").WillReturnRows(sqlmock.NewRows([]string{"deliveryID", "payload", "result", "revision", "projectionAttempts"}).AddRow("mail/1", `{"mailID":7}`, `{"deliveryID":"mail/1","state":"accepted","revision":2}`, 2, 0))
	mock.ExpectExec("UPDATE gorge_mail_delivery SET projectionPending=0 WHERE deliveryID=\\? AND revision=\\?").WithArgs("mail/1", 2).WillReturnResult(sqlmock.NewResult(0, 1))
	if err = ProjectMailResultsOnce(context.Background(), db, NewConduitClient(php.URL, "token")); err != nil {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMailProjectionPoisonRecordDoesNotBlockFollowingReceipt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	php := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"result":{"applied":true}}`)); err != nil {
			t.Error(err)
		}
	}))
	defer php.Close()
	mock.ExpectQuery("SELECT deliveryID,payload,result,revision").WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"id", "payload", "result", "revision", "attempts"}).AddRow("bad", `not-json`, `{}`, 2, 0).AddRow("good", `{"mailID":9}`, `{"state":"accepted"}`, 2, 0))
	mock.ExpectExec("UPDATE gorge_mail_delivery SET projectionAttempts").WithArgs(sqlmock.AnyArg(), "bad", 2).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE gorge_mail_delivery SET projectionPending=0").WithArgs("good", 2).WillReturnResult(sqlmock.NewResult(0, 1))
	if err = ProjectMailResultsOnce(context.Background(), db, NewConduitClient(php.URL, "token")); err == nil {
		t.Fatal("poison result was ignored")
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
