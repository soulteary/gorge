package taskqueue

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
	"github.com/soulteary/gorge/go/internal/worker/outbox"
)

// Exercise the relay through the actual authenticated queue HTTP route. Lose
// the source acknowledgement after queue commit, then replay the source event.
func exerciseOutboxRelay(t *testing.T, s *MySQLStore) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE feed_gorgeoutbox(id BIGINT AUTO_INCREMENT PRIMARY KEY,eventID VARBINARY(128) UNIQUE,payload LONGTEXT,attempts INT DEFAULT 0,nextAttempt BIGINT DEFAULT 0,deliveredEpoch BIGINT NULL,queueTaskID BIGINT NULL,lastError TEXT) ENGINE=InnoDB`); err != nil {
		t.Fatal(err)
	}
	request := &contracts.EnqueueEventRequest{EventID: "relay/1", Task: contracts.EnqueueRequest{TaskClass: "RelayFeed", Data: "{}"}}
	raw, _ := json.Marshal(request)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO feed_gorgeoutbox (eventID,payload,lastError) VALUES (?,?,'')", request.EventID, string(raw)); err != nil {
		t.Fatal(err)
	}
	app := newTestServer(t, s)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := app.Test(r, fiber.TestConfig{Timeout: 0})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer server.Close()
	relay := &outbox.Relay{DB: s.db, Queue: worker.NewClient(server.URL, testToken)}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER lose_ack BEFORE UPDATE ON feed_gorgeoutbox FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'simulated lost acknowledgement'`); err != nil {
		t.Fatal(err)
	}
	if err := relay.Once(ctx); err == nil {
		t.Fatal("source acknowledgement failure was hidden")
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER lose_ack"); err != nil {
		t.Fatal(err)
	}
	if err := relay.Once(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM worker_activetask WHERE taskClass = 'RelayFeed'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("replayed event duplicated: %d %v", count, err)
	}
	var delivered, taskID int64
	if err := s.db.QueryRowContext(ctx, "SELECT deliveredEpoch,queueTaskID FROM feed_gorgeoutbox WHERE eventID = ?", request.EventID).Scan(&delivered, &taskID); err != nil || delivered <= 0 || taskID <= 0 {
		t.Fatalf("source event not acknowledged: %v", err)
	}
	// Concurrent inbox replay must resolve to the same original task.
	results := make(chan *contracts.Task, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { task, err := s.EnqueueEvent(ctx, request); results <- task; errs <- err }()
	}
	for i := 0; i < 8; i++ {
		select {
		case task := <-results:
			if task == nil || task.ID != taskID {
				t.Fatalf("concurrent replay mismatch: %+v", task)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("inbox concurrency deadlock")
		}
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
