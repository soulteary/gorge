package dbproxy

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/soulteary/gorge/go/internal/dbapi"
)

func TestConcurrentConnectionCreationKeepsAndClosesOnePool(t *testing.T) {
	master := &dbapi.DatabaseRef{Host: "master", Port: 3306, IsMaster: true}
	router := NewRouter(dbapi.NewClusterConfigForRefs(
		"phorge", "secret", []*dbapi.DatabaseRef{master}), "secret")

	conns := make([]*dbapi.Conn, 2)
	mocks := make([]sqlmock.Sqlmock, 2)
	for i := range conns {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		mock.ExpectClose()
		conns[i] = dbapi.NewConnFromDB(db, dbapi.DSN{}, false)
		mocks[i] = mock
	}

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	router.connect = func(dbapi.DSN, bool, RetryPolicy) (*dbapi.Conn, error) {
		index := int(calls.Add(1)) - 1
		ready <- struct{}{}
		<-release
		return conns[index], nil
	}

	type result struct {
		conn *dbapi.Conn
		err  error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			conn, err := router.GetWriter(context.Background(), "config")
			results <- result{conn: conn, err: err}
		}()
	}
	<-ready
	<-ready
	close(release)

	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("GetWriter() errors = %v, %v", first.err, second.err)
	}
	if first.conn != second.conn {
		t.Fatal("concurrent callers received different connection pools")
	}
	if got := len(router.conns); got != 1 {
		t.Fatalf("cached pools = %d, want 1", got)
	}
	if err := router.Close(); err != nil {
		t.Fatal(err)
	}
	for _, mock := range mocks {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	}
}
