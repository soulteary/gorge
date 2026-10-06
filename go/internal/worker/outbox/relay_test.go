package outbox

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/soulteary/gorge/go/internal/contracts"
)

type testQueue struct {
	err   error
	calls int
}

func (q *testQueue) EnqueueEvent(ctx context.Context, e *contracts.EnqueueEventRequest) (*contracts.Task, error) {
	q.calls++
	return &contracts.Task{ID: 42}, q.err
}
func TestRelayKeepsFailedEventAndContinuesBatch(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	q := &testQueue{err: errors.New("queue unavailable")}
	mock.ExpectQuery("SELECT eventID, payload, attempts").WillReturnRows(sqlmock.NewRows([]string{"eventID", "payload", "attempts"}).AddRow("one", `{"eventID":"one","task":{"taskClass":"Feed","data":"{}"}}`, 0).AddRow("two", `{"eventID":"wrong"}`, 0))
	mock.ExpectExec("UPDATE feed_gorgeoutbox SET attempts").WithArgs(sqlmock.AnyArg(), "queue unavailable", "one").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE feed_gorgeoutbox SET attempts").WithArgs(sqlmock.AnyArg(), "outbox event identity mismatch", "two").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := (&Relay{DB: db, Queue: q}).Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if q.calls != 1 {
		t.Fatal("malformed event was sent")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestRelayAckFailureLeavesEventForIdempotentReplay(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	q := &testQueue{}
	mock.ExpectQuery("SELECT eventID, payload, attempts").WillReturnRows(sqlmock.NewRows([]string{"eventID", "payload", "attempts"}).AddRow("one", `{"eventID":"one","task":{"taskClass":"Feed","data":"{}"}}`, 0))
	mock.ExpectExec("UPDATE feed_gorgeoutbox SET deliveredEpoch").WithArgs(sqlmock.AnyArg(), int64(42), "one").WillReturnError(errors.New("source unavailable"))
	if err := (&Relay{DB: db, Queue: q}).Once(t.Context()); err == nil {
		t.Fatal("lost acknowledgement was hidden")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
