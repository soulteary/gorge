package mailer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/soulteary/gorge/go/internal/contracttest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/contracts"
)

type countingAdapter struct {
	calls atomic.Int32
	err   error
}

func (a *countingAdapter) Type() string { return "counting" }
func (a *countingAdapter) Send(ctx context.Context, msg *contracts.EmailMessage) (string, error) {
	a.calls.Add(1)
	return "receipt", a.err
}
func deliveryTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("GORGE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GORGE_TEST_MYSQL_DSN")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("gorge_mail_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Error(err)
		}
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, ddl := range []string{
		`CREATE TABLE gorge_mail_delivery (deliveryID VARBINARY(128) PRIMARY KEY,payloadHash VARCHAR(64) NOT NULL,payload LONGTEXT NOT NULL,state VARCHAR(32) NOT NULL,revision INT NOT NULL,attempt INT NOT NULL,nextAttempt BIGINT NOT NULL,startedEpoch BIGINT NOT NULL,result LONGTEXT NOT NULL,projectionPending TINYINT NOT NULL DEFAULT 0,deadline BIGINT NOT NULL DEFAULT 0,projectionNextAttempt BIGINT NOT NULL DEFAULT 0,projectionAttempts INT NOT NULL DEFAULT 0,projectionLastError VARCHAR(255) NOT NULL DEFAULT '') ENGINE=InnoDB`,
		`CREATE TABLE gorge_mail_attempt (id BIGINT AUTO_INCREMENT PRIMARY KEY,deliveryID VARBINARY(128) NOT NULL,attempt INT NOT NULL,startedEpoch BIGINT NOT NULL,finishedEpoch BIGINT NULL,outcome VARCHAR(32) NOT NULL,UNIQUE KEY deliveryAttempt(deliveryID,attempt)) ENGINE=InnoDB`,
	} {
		if _, err = db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func TestMailDeliveryMySQLIntegration(t *testing.T) {
	db := deliveryTestDB(t)
	ctx := context.Background()
	a := &countingAdapter{}
	s := &DeliveryService{DB: db, Dispatcher: &Dispatcher{adapters: []namedAdapter{{key: "primary", adapter: a}}}}
	makeReq := func(id string) contracts.MailDeliveryRequest {
		return contracts.MailDeliveryRequest{SchemaVersion: 1, DeliveryID: id, MailID: 7, Deadline: time.Now().Unix() + 3600, Message: *testMessage()}
	}
	req := makeReq("mail/one")
	pause := false
	req.AllowSend = &pause
	if out, err := s.Deliver(ctx, req); err != nil || out.State != "prepared" || a.calls.Load() != 0 {
		t.Fatalf("prepare: %+v %v", out, err)
	}
	req.AllowSend = nil
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Deliver(ctx, req); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	out, err := s.Deliver(ctx, req)
	if err != nil || out.State != "accepted" || a.calls.Load() != 1 {
		t.Fatalf("concurrent replay: %+v %v calls=%d", out, err, a.calls.Load())
	}
	contracttest.RestoreFixture(t, db, "gorge_mail_delivery", "gorge_mail_attempt")
	out, err = s.Deliver(ctx, req)
	if err != nil || out.State != "accepted" || a.calls.Load() != 1 {
		t.Fatal("restored mail delivery repeated", out, err)
	}
	req.Message.Subject = "changed"
	if _, err = s.Deliver(ctx, req); !errors.Is(err, ErrDeliveryConflict) {
		t.Fatalf("collision: %v", err)
	}
	a.err = errors.New("response lost after provider acceptance")
	unknown := makeReq("mail/unknown")
	out, err = s.Deliver(ctx, unknown)
	if err != nil || out.State != "unknown" {
		t.Fatalf("unknown: %+v %v", out, err)
	}
	calls := a.calls.Load()
	_, err = s.Deliver(ctx, unknown)
	if err != nil || a.calls.Load() != calls {
		t.Fatalf("unknown resent: %v", err)
	}
	a.err = &SafeRetryError{Err: errors.New("confirmed not accepted")}
	retry := makeReq("mail/retry")
	out, err = s.Deliver(ctx, retry)
	if err != nil || out.State != "retry_wait" || out.NextAttempt <= time.Now().Unix() {
		t.Fatalf("retry: %+v %v", out, err)
	}
	calls = a.calls.Load()
	if _, err := s.Deliver(ctx, retry); err != nil {
		t.Fatal(err)
	}
	if a.calls.Load() != calls {
		t.Fatal("retry ignored nextAttempt")
	}
	dead := makeReq("mail/dead")
	dead.AllowSend = &pause
	if _, err := s.Deliver(ctx, dead); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE gorge_mail_delivery SET state='submitting',startedEpoch=0 WHERE deliveryID=?", dead.DeliveryID); err != nil {
		t.Fatal(err)
	}
	dead.AllowSend = nil
	out, err = s.Deliver(ctx, dead)
	if err != nil || out.State != "unknown" || a.calls.Load() != calls {
		t.Fatalf("dead submitter: %+v %v", out, err)
	}
	expired := makeReq("mail/expired")
	expired.Deadline = 1
	out, err = s.Deliver(ctx, expired)
	if err != nil || out.State != "expired" || a.calls.Load() != calls {
		t.Fatalf("expiry: %+v %v", out, err)
	}
	cancelled := makeReq("mail/cancel")
	cancelled.AllowSend = &pause
	if _, err := s.Deliver(ctx, cancelled); err != nil {
		t.Fatal(err)
	}
	out, err = s.Cancel(ctx, cancelled.DeliveryID)
	if err != nil || out.State != "cancelled" {
		t.Fatalf("cancel: %+v %v", out, err)
	}
	cancelled.AllowSend = nil
	if _, err := s.Deliver(ctx, cancelled); err != nil {
		t.Fatal(err)
	}
	if a.calls.Load() != calls {
		t.Fatal("cancelled delivery sent")
	}
	if _, err = s.Cancel(ctx, "mail/one"); !errors.Is(err, ErrDeliveryCancelConflict) {
		t.Fatalf("accepted delivery cancelled: %v", err)
	}
	var pending bool
	if err = db.QueryRow("SELECT projectionPending FROM gorge_mail_delivery WHERE deliveryID='mail/one'").Scan(&pending); err != nil || !pending {
		t.Fatal("receipt missing from result outbox")
	}

	replay, err := s.Cancel(ctx, cancelled.DeliveryID)
	if err != nil || replay.State != "cancelled" {
		t.Fatalf("cancel replay: %+v %v", replay, err)
	}
	orphan := makeReq("mail/orphan")
	orphan.AllowSend = &pause
	if _, err := s.Deliver(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE gorge_mail_delivery SET state='submitting',attempt=1,startedEpoch=0 WHERE deliveryID=?", orphan.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO gorge_mail_attempt(deliveryID,attempt,startedEpoch,outcome) VALUES (?,1,0,'submitting')", orphan.DeliveryID); err != nil {
		t.Fatal(err)
	}
	stale := makeReq("mail/expired-orphan")
	stale.AllowSend = &pause
	if _, err := s.Deliver(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE gorge_mail_delivery SET deadline=1 WHERE deliveryID=?", stale.DeliveryID); err != nil {
		t.Fatal(err)
	}
	calls = a.calls.Load()
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	var revision int
	if err = db.QueryRow("SELECT state,revision FROM gorge_mail_delivery WHERE deliveryID=?", orphan.DeliveryID).Scan(&state, &revision); err != nil || state != "unknown" {
		t.Fatalf("orphan submission: %s %v", state, err)
	}
	if err = db.QueryRow("SELECT outcome FROM gorge_mail_attempt WHERE deliveryID=?", orphan.DeliveryID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("stale attempt audit: %s %v", state, err)
	}
	if err = db.QueryRow("SELECT state FROM gorge_mail_delivery WHERE deliveryID=?", stale.DeliveryID).Scan(&state); err != nil || state != "expired" {
		t.Fatalf("orphan expiry: %s %v", state, err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var repeated int
	if err := db.QueryRow("SELECT revision FROM gorge_mail_delivery WHERE deliveryID=?", orphan.DeliveryID).Scan(&repeated); err != nil {
		t.Error(err)
		return
	}
	if repeated != revision || a.calls.Load() != calls {
		t.Fatal("recovery duplicated outcome or sent mail")
	}
	noBackend := &DeliveryService{DB: db, Dispatcher: &Dispatcher{}}
	blocked := makeReq("mail/no-provider")
	if _, err = noBackend.Deliver(ctx, blocked); err == nil {
		t.Fatal("missing backend accepted submission")
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM gorge_mail_attempt WHERE deliveryID=?", blocked.DeliveryID).Scan(&n); err != nil {
		t.Error(err)
		return
	}
	if n != 0 {
		t.Fatal("configuration failure consumed provider attempt")
	}

}
func TestDispatcherDoesNotRetryOrFailoverUnknown(t *testing.T) {
	a := &countingAdapter{err: errors.New("acceptance unknown")}
	b := &countingAdapter{}
	d := &Dispatcher{adapters: []namedAdapter{{key: "a", adapter: a}, {key: "b", adapter: b}}, retry: RetryPolicy{MaxRetries: 3}}
	if _, err := d.Send(context.Background(), testMessage()); err == nil || a.calls.Load() != 1 || b.calls.Load() != 0 {
		t.Fatalf("unknown submission retried: %v", err)
	}
}

func TestDeliveryInspectionMySQLIntegration(t *testing.T) {
	db := deliveryTestDB(t)
	ctx := context.Background()
	adapter := &countingAdapter{}
	s := &DeliveryService{DB: db, Dispatcher: &Dispatcher{adapters: []namedAdapter{{key: "primary", adapter: adapter}}}}
	pause := false
	req := contracts.MailDeliveryRequest{SchemaVersion: 1, DeliveryID: "mail/inspect", MailID: 7, Deadline: time.Now().Unix() + 3600, Message: *testMessage(), AllowSend: &pause}
	if _, err := s.Deliver(ctx, req); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Inspect(ctx, req.DeliveryID)
	if err != nil || prepared.Result.State != "prepared" || len(prepared.Attempts) != 0 || adapter.calls.Load() != 0 {
		t.Fatalf("prepared inspection: %+v %v", prepared, err)
	}
	req.AllowSend = nil
	if _, err = s.Deliver(ctx, req); err != nil {
		t.Fatal(err)
	}
	accepted, err := s.Inspect(ctx, req.DeliveryID)
	if err != nil || accepted.Result.State != "accepted" || !accepted.ProjectionPending || len(accepted.Attempts) != 1 || accepted.Attempts[0].FinishedEpoch == nil || accepted.Result.MessageID != "receipt" || adapter.calls.Load() != 1 {
		t.Fatalf("accepted inspection: %+v %v", accepted, err)
	}
	if _, err = s.Inspect(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing inspection: %v", err)
	}
	if adapter.calls.Load() != 1 {
		t.Fatal("inspection submitted mail")
	}
}
