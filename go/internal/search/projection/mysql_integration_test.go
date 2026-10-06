package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/contracts"
)

// Use an isolated throwaway database; no production schemas are accessed.
func TestMySQLDurableProjectionIntegration(t *testing.T) {
	dsn := os.Getenv("GORGE_TEST_SEARCH_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GORGE_TEST_SEARCH_MYSQL_DSN for real MySQL")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	cfg.MultiStatements = false
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	}()
	name := fmt.Sprintf("gorge_search_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Error(err)
		}
	}()
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, ddl := range strings.Split(Schema, ";") {
		if strings.TrimSpace(ddl) != "" {
			if _, err = db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
		}
	}
	store := &MySQLStore{DB: db}
	ctx := context.Background()
	targets := []Target{{"es", "g1"}, {"shadow", "g2"}}
	event := goldenEvent(t)
	receipt, err := store.Accept(ctx, event, targets)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "accepted" || len(receipt.Targets) != 2 {
		t.Fatalf("%+v", receipt)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := store.Accept(ctx, event, targets); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM search_projection_delivery").Scan(&count); err != nil || count != 2 {
		t.Fatalf("count=%d %v", count, err)
	}
	conflict := *event
	conflict.SourceVersion = "changed"
	if _, err = store.Accept(ctx, &conflict, targets); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("event conflict: %v", err)
	}
	conflict.EventID = "different-event"
	if _, err = store.Accept(ctx, &conflict, targets); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("revision conflict: %v", err)
	}
	for _, revision := range []int64{43, 41} {
		next := *event
		next.EventID = fmt.Sprintf("revision/%d", revision)
		next.Revision = strconv.FormatInt(revision, 10)
		receipt, err = store.Accept(ctx, &next, targets)
		if err != nil {
			t.Fatal(err)
		}
		if (receipt.Status == "superseded") != (revision == 41) {
			t.Fatalf("%+v", receipt)
		}
	}
	next := *event
	next.Revision = "44"
	next.EventID = "delete/44"
	next.Operation = "delete"
	next.Document = nil
	next.PayloadHash, err = PayloadHash(&next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Accept(ctx, &next, targets); err != nil {
		t.Fatal(err)
	}
	var head int64
	if err = db.QueryRow("SELECT revision FROM search_projection_head").Scan(&head); err != nil || head != 44 {
		t.Fatalf("head=%d %v", head, err)
	}
	var revisionRows int
	if err = db.QueryRow("SELECT COUNT(*) FROM search_projection_revision").Scan(&revisionRows); err != nil || revisionRows != 4 {
		t.Fatalf("revisionRows=%d %v", revisionRows, err)
	}
	original, err := store.LoadEvent(ctx, event.Namespace, event.EventID)
	if err != nil || original.Revision != "42" || original.Document == nil {
		t.Fatalf("immutable old event lost: %+v %v", original, err)
	}
	// Same revision can populate a newly created target without global dedup.
	next.EventID = "delete/44/new-generation"
	if _, err = store.Accept(ctx, &next, []Target{{"es", "g3"}}); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM search_projection_delivery WHERE generationID='g3'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("new generation: %d %v", count, err)
	}
	// A delivery insert failure must roll back head, revision and receipt.
	if _, err = db.Exec(`CREATE TRIGGER reject_projection BEFORE INSERT ON search_projection_delivery FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test delivery failure'`); err != nil {
		t.Fatal(err)
	}
	rejected := *event
	rejected.EventID = "rejected/45"
	rejected.Revision = "45"
	if _, err = store.Accept(ctx, &rejected, targets); err == nil {
		t.Fatal("failed delivery insertion acknowledged")
	}
	if err = db.QueryRow("SELECT revision FROM search_projection_head").Scan(&head); err != nil || head != 44 {
		t.Fatalf("rollback head %d %v", head, err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM search_projection_inbox WHERE eventID='rejected/45'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback receipt %d %v", count, err)
	}

	// Exercise relay acceptance, failed source acknowledgment, and restart replay.
	if _, err = db.Exec("DROP TRIGGER reject_projection"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE search_gorgeoutbox (
 id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,eventID VARBINARY(128) UNIQUE NOT NULL,
 payload LONGTEXT NOT NULL,attempts INT UNSIGNED NOT NULL DEFAULT 0,
 nextAttempt BIGINT UNSIGNED NOT NULL DEFAULT 0,deliveredEpoch BIGINT UNSIGNED NULL,lastError LONGTEXT NULL
 ) ENGINE=InnoDB`); err != nil {
		t.Fatal(err)
	}
	relayEvent := *event
	relayEvent.Revision = "46"
	relayEvent.EventID = "relay/46"
	raw, _ := json.Marshal(&relayEvent)
	if _, err = db.Exec("INSERT INTO search_gorgeoutbox(eventID,payload) VALUES(?,?)", relayEvent.EventID, string(raw)); err != nil {
		t.Fatal(err)
	}
	relay := &Relay{Source: db, Store: store, Namespace: event.Namespace, Targets: targets}
	if _, err = db.Exec(`CREATE TRIGGER reject_ack BEFORE UPDATE ON search_gorgeoutbox FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test source ack failure'`); err != nil {
		t.Fatal(err)
	}
	if err = relay.Once(ctx); err == nil {
		t.Fatal("source ack failure swallowed")
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM search_projection_inbox WHERE eventID=?", relayEvent.EventID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("acceptance did not survive source failure: %d %v", count, err)
	}
	if _, err = db.Exec("DROP TRIGGER reject_ack"); err != nil {
		t.Fatal(err)
	}
	if err = relay.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM search_gorgeoutbox WHERE deliveredEpoch IS NOT NULL").Scan(&count); err != nil || count != 1 {
		t.Fatalf("replay not acknowledged: %d %v", count, err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM search_projection_delivery WHERE revision=46").Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate delivery: %d %v", count, err)
	}
	// A poison row remains retryable; a later valid row still advances.
	if _, err = db.Exec("INSERT INTO search_gorgeoutbox(eventID,payload) VALUES('invalid','{}')"); err != nil {
		t.Fatal(err)
	}
	relayEvent.Revision = "47"
	relayEvent.EventID = "relay/47"
	raw, _ = json.Marshal(&relayEvent)
	if _, err = db.Exec("INSERT INTO search_gorgeoutbox(eventID,payload) VALUES(?,?)", relayEvent.EventID, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err = relay.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM search_gorgeoutbox WHERE deliveredEpoch IS NOT NULL").Scan(&count); err != nil || count != 2 {
		t.Fatalf("poison blocked valid row: %d %v", count, err)
	}
	if err = db.QueryRow("SELECT attempts FROM search_gorgeoutbox WHERE eventID='invalid' AND deliveredEpoch IS NULL AND nextAttempt>0").Scan(&count); err != nil || count != 1 {
		t.Fatalf("poison lost: %d %v", count, err)
	}

	// Lease expiry fences all result paths, even when an old owner reports failure.
	target := Target{"lease-test", "g1"}
	leaseEvent := *event
	leaseEvent.Revision = "48"
	leaseEvent.EventID = "lease/48"
	if _, err = store.Accept(ctx, &leaseEvent, []Target{target}); err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(ctx, event.Namespace, target, "owner-a", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Claim(ctx, event.Namespace, target, "owner-b", 30*time.Second); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("double claim: %v", err)
	}
	// Renew twice in the same database second: changed-rows semantics must not
	// incorrectly classify an unchanged expiration timestamp as a lost lease.
	for i := 0; i < 2; i++ {
		if err = store.Renew(ctx, first, 30*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("UPDATE search_projection_delivery SET leaseExpires=UNIX_TIMESTAMP()-1 WHERE backendID='lease-test'"); err != nil {
		t.Fatal(err)
	}
	if err = store.Renew(ctx, first, 30*time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired renewal: %v", err)
	}
	second, err := store.Claim(ctx, event.Namespace, target, "owner-b", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch != first.Epoch+1 || second.Attempts != 2 {
		t.Fatalf("fence not advanced: %+v", second)
	}
	for _, status := range []string{"applied", "superseded", "retry"} {
		if err = store.Finish(ctx, first, status, time.Second); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("stale %s changed state: %v", status, err)
		}
	}
	if err = store.Finish(ctx, second, "retry", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Claim(ctx, event.Namespace, target, "owner-c", 30*time.Second); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retry backoff bypass: %v", err)
	}
	if _, err = db.Exec("UPDATE search_projection_delivery SET nextAttempt=0 WHERE backendID='lease-test'"); err != nil {
		t.Fatal(err)
	}
	third, err := store.Claim(ctx, event.Namespace, target, "owner-c", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(ctx, third, "applied", 0); err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(ctx, third, "retry", time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("completed task reopened: %v", err)
	}
	if _, err = store.Claim(ctx, event.Namespace, target, "owner-d", 30*time.Second); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("applied task claimed: %v", err)
	}

	// Verify the checked-in upgrade artifact against an acceptance-only schema.
	if _, err = db.Exec(`ALTER TABLE search_projection_delivery
 DROP COLUMN leaseOwner,DROP COLUMN leaseEpoch,DROP COLUMN leaseExpires,
 DROP COLUMN leaseRenewals,DROP COLUMN attempts,DROP COLUMN nextAttempt,
 DROP COLUMN lastError,DROP COLUMN appliedEpoch`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../resources/sql/search/20261006.delivery-leases.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(migration)); err != nil {
		t.Fatalf("lease migration: %v", err)
	}
	if _, err = store.Claim(ctx, event.Namespace, targets[0], "migration-owner", 30*time.Second); err != nil {
		t.Fatalf("upgraded pending delivery unavailable: %v", err)
	}

	if _, err = db.Exec("DROP TABLE search_projection_target"); err != nil {
		t.Fatal(err)
	}
	targetMigration, err := os.ReadFile("../../../../resources/sql/search/20261006.delivery-targets.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(targetMigration)); err != nil {
		t.Fatalf("target migration: %v", err)
	}
	hash := strings.Repeat("a", 64)
	if err = store.BindTarget(ctx, event.Namespace, Target{"binding", "g1"}, hash, "uuid-1"); err != nil {
		t.Fatal(err)
	}
	if err = store.BindTarget(ctx, event.Namespace, Target{"binding", "g1"}, hash, "uuid-1"); err != nil {
		t.Fatal(err)
	}
	if err = store.BindTarget(ctx, event.Namespace, Target{"binding", "g1"}, strings.Repeat("b", 64), "uuid-1"); err == nil {
		t.Fatal("config mutation accepted")
	}
	if err = store.BindTarget(ctx, event.Namespace, Target{"binding", "g1"}, hash, "uuid-2"); err == nil {
		t.Fatal("index recreation accepted")
	}
	if err = store.BindTarget(ctx, event.Namespace, Target{"other-binding", "g2"}, hash, "uuid-1"); err == nil {
		t.Fatal("physical index shared by generations")
	}
	workerTarget := Target{"worker-test", "g1"}
	workerEvent := *event
	workerEvent.EventID = "worker/49"
	workerEvent.Revision = "49"
	if _, err = store.Accept(ctx, &workerEvent, []Target{workerTarget}); err != nil {
		t.Fatal(err)
	}
	writerFailed := true
	worker := &Worker{Store: store, Namespace: event.Namespace, Target: workerTarget, Owner: "worker-test", Writer: writerFunc(func(_ context.Context, e *contracts.SearchProjection, target Target) (string, error) {
		if e.Revision != "49" || target != workerTarget {
			return "", fmt.Errorf("wrong envelope")
		}
		if writerFailed {
			return "", fmt.Errorf("uncertain backend response")
		}
		return "applied", nil
	})}
	if err = worker.Once(ctx); err == nil {
		t.Fatal("backend error swallowed")
	}
	var status string
	if err = db.QueryRow("SELECT status FROM search_projection_delivery WHERE backendID='worker-test'").Scan(&status); err != nil || status != "pending" {
		t.Fatalf("worker retry %s %v", status, err)
	}
	writerFailed = false
	if _, err = db.Exec("UPDATE search_projection_delivery SET nextAttempt=0 WHERE backendID='worker-test'"); err != nil {
		t.Fatal(err)
	}
	if err = worker.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT status FROM search_projection_delivery WHERE backendID='worker-test' AND appliedEpoch IS NOT NULL").Scan(&status); err != nil || status != "applied" {
		t.Fatalf("worker completion %s %v", status, err)
	}

	// A status read follows the original target/revision, even after a newer head
	// and even when a different event ID shared the original delivery row.
	observed, err := store.EventStatus(ctx, event.Namespace, workerEvent.EventID)
	if err != nil || observed.Receipt.Status != "accepted" || len(observed.Deliveries) != 1 || observed.Deliveries[0].Status != "applied" || observed.Deliveries[0].AppliedEpoch == nil || observed.Deliveries[0].CreatedEpoch == 0 {
		t.Fatalf("event inspection %+v %v", observed, err)
	}
	if _, err = store.EventStatus(ctx, "other", workerEvent.EventID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("namespace leak: %v", err)
	}
	if _, err = store.EventStatus(ctx, event.Namespace, "absent"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown receipt: %v", err)
	}
	metrics, err := store.DeliveryStats(ctx, event.Namespace, []Target{workerTarget, {"empty", "g1"}})
	if err != nil || len(metrics.Targets) != 2 || metrics.Targets[0].Applied != 1 || metrics.Targets[0].OldestPendingEpoch != nil || metrics.Targets[1].Pending != 0 {
		t.Fatalf("delivery stats %+v %v", metrics, err)
	}
	// Upgrade leaves old row ages unknown and uses the checked-in SQL artifact.
	if _, err = db.Exec("ALTER TABLE search_projection_delivery DROP COLUMN createdEpoch"); err != nil {
		t.Fatal(err)
	}
	ageMigration, err := os.ReadFile("../../../../resources/sql/search/20261006.delivery-inspection.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(ageMigration)); err != nil {
		t.Fatal(err)
	}
	inspectTarget := Target{"inspect", "g1"}
	inspectEvent := *event
	inspectEvent.Revision = "50"
	inspectEvent.EventID = "inspect/50"
	if _, err = store.Accept(ctx, &inspectEvent, []Target{inspectTarget}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, event.Namespace, inspectTarget, "inspect-owner", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE search_projection_delivery SET leaseExpires=UNIX_TIMESTAMP()-1,lastError='secret DSN' WHERE backendID='inspect'"); err != nil {
		t.Fatal(err)
	}
	observed, err = store.EventStatus(ctx, event.Namespace, inspectEvent.EventID)
	if err != nil || !observed.Deliveries[0].LeaseExpired || observed.Deliveries[0].LastError != "backend_delivery_failed" {
		t.Fatalf("expired/sanitized status %+v %v", observed, err)
	}
	if err = store.Finish(ctx, lease, "applied", 0); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("inspection altered expired lease")
	}
	metrics, err = store.DeliveryStats(ctx, event.Namespace, []Target{inspectTarget, targets[0]})
	if err != nil || metrics.Targets[0].ExpiredRunning != 1 || metrics.Targets[0].OldestPendingEpoch == nil || metrics.Targets[1].UnknownPendingAge == 0 {
		t.Fatalf("age/expiry stats %+v %v", metrics, err)
	}
	alias := inspectEvent
	alias.EventID = "inspect/50/alias"
	if _, err = store.Accept(ctx, &alias, []Target{inspectTarget}); err != nil {
		t.Fatal(err)
	}
	aliasStatus, err := store.EventStatus(ctx, event.Namespace, alias.EventID)
	if err != nil || len(aliasStatus.Deliveries) != 1 || aliasStatus.Deliveries[0].Status != "running" {
		t.Fatalf("shared revision inspection %+v %v", aliasStatus, err)
	}
	// Inspect an older receipt without substituting the latest head payload.
	observed, err = store.EventStatus(ctx, event.Namespace, workerEvent.EventID)
	if err != nil || observed.LatestRevision != "50" || observed.Receipt.Revision != "49" || observed.Deliveries[0].Status != "applied" {
		t.Fatalf("historical receipt %+v %v", observed, err)
	}

	// Exercise the actual heartbeat loop: takeover must cancel an in-flight
	// backend request and must not reschedule the new owner's delivery.
	heartbeatTarget := Target{"heartbeat", "g1"}
	heartbeatEvent := *event
	heartbeatEvent.EventID = "heartbeat/51"
	heartbeatEvent.Revision = "51"
	if _, err = store.Accept(ctx, &heartbeatEvent, []Target{heartbeatTarget}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	finished := make(chan error, 1)
	heartbeatWorker := &Worker{Store: store, Namespace: event.Namespace, Target: heartbeatTarget, Owner: "heart-old", Writer: writerFunc(func(jobCtx context.Context, _ *contracts.SearchProjection, _ Target) (string, error) {
		close(started)
		<-jobCtx.Done()
		close(cancelled)
		return "", jobCtx.Err()
	})}
	workerCtx, cancelWorker := context.WithTimeout(ctx, 15*time.Second)
	defer cancelWorker()
	go func() { finished <- heartbeatWorker.Once(workerCtx) }()
	select {
	case <-started:
	case <-workerCtx.Done():
		t.Fatal("heartbeat worker did not start")
	}
	if _, err = db.Exec("UPDATE search_projection_delivery SET leaseExpires=UNIX_TIMESTAMP()-1 WHERE backendID='heartbeat'"); err != nil {
		t.Fatal(err)
	}
	taken, err := store.Claim(ctx, event.Namespace, heartbeatTarget, "heart-new", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-finished:
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("heartbeat did not fence old result: %v", err)
		}
	case <-workerCtx.Done():
		t.Fatal("heartbeat did not cancel backend request")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("backend request continued after lost lease")
	}
	var owner string
	var epoch uint64
	if err = db.QueryRow("SELECT status,leaseOwner,leaseEpoch FROM search_projection_delivery WHERE backendID='heartbeat'").Scan(&status, &owner, &epoch); err != nil || status != "running" || owner != "heart-new" || epoch != taken.Epoch {
		t.Fatalf("old worker changed new lease: %s %s %d %v", status, owner, epoch, err)
	}

	sourceStats, err := InspectSourceOutbox(ctx, db)
	if err != nil || sourceStats.Pending != 1 || sourceStats.Retrying != 1 || sourceStats.AgeAvailable {
		t.Fatalf("source poison hidden: %+v %v", sourceStats, err)
	}
	if _, err = db.Exec("UPDATE search_gorgeoutbox SET nextAttempt=0 WHERE eventID='invalid'"); err != nil {
		t.Fatal(err)
	}
	sourceStats, err = InspectSourceOutbox(ctx, db)
	if err != nil || sourceStats.Due != 1 {
		t.Fatalf("source clock eligibility %+v %v", sourceStats, err)
	}

}

type writerFunc func(context.Context, *contracts.SearchProjection, Target) (string, error)

func (f writerFunc) ApplyProjection(ctx context.Context, e *contracts.SearchProjection, target Target) (string, error) {
	return f(ctx, e, target)
}
