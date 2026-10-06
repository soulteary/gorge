package taskqueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/contracts"
)

// Uses an isolated key prefix and never clears unrelated Redis data.
func TestExecutionRedisIntegration(t *testing.T) {
	addr := os.Getenv("GORGE_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set GORGE_TEST_REDIS_ADDR to run against Redis")
	}
	prefix := fmt.Sprintf("gorge:test:execution:%d:", time.Now().UnixNano())
	s, err := NewRedisStore(&Config{RedisAddr: addr, RedisKeyPrefix: prefix, LeaseDuration: 3600, RetryWait: 300})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	defer func() {
		keys, _ := s.rdb.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			s.rdb.Del(ctx, keys...)
		}
	}()
	if err := s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	exerciseExecutionStore(t, ctx, s, s)
}

func exerciseExecutionStore(t *testing.T, ctx context.Context, s Store, execution ExecutionStore) {
	t.Helper()
	inbox := s.(InboxStore)
	event := &contracts.EnqueueEventRequest{EventID: "feed/17", Task: contracts.EnqueueRequest{TaskClass: "InboxEvent", Data: "{}"}}
	first, err := inbox.EnqueueEvent(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := inbox.EnqueueEvent(ctx, event)
	if err != nil || first.ID != replay.ID {
		t.Fatalf("inbox replay: %v %v", replay, err)
	}
	event.Task.Data = "different"
	if _, err := inbox.EnqueueEvent(ctx, event); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("event collision accepted: %v", err)
	}
	if _, err := s.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	event.Task.Data = "{}"
	replay, err = inbox.EnqueueEvent(ctx, event)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("archived replay: %v %v", replay, err)
	}
	priority := 42
	parent, err := s.Enqueue(ctx, &contracts.EnqueueRequest{TaskClass: "ExecutionParent", Data: `{"parent":true}`, Priority: &priority})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Lease(ctx, 1, "integration-owner", nil)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("lease: %v, %v", tasks, err)
	}
	task := tasks[0]
	lease := contracts.ExecutionLease{TaskID: parent.ID, LeaseOwner: task.LeaseOwner, LeaseExpires: *task.LeaseExpires}
	wrong := lease
	wrong.LeaseOwner = "stale-owner"
	if err := execution.Finalize(ctx, &contracts.FinalizeRequest{ExecutionLease: wrong}); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("stale owner accepted: %v", err)
	}
	renewed, err := execution.Renew(ctx, &contracts.RenewRequest{ExecutionLease: lease, Duration: 7200})
	if err != nil {
		t.Fatal(err)
	}
	if *renewed.LeaseExpires <= lease.LeaseExpires {
		t.Fatal("lease was not extended")
	}
	if err := execution.Finalize(ctx, &contracts.FinalizeRequest{ExecutionLease: lease}); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("old lease accepted: %v", err)
	}
	lease.LeaseExpires = *renewed.LeaseExpires
	delay := time.Now().Unix() + 3600
	req := &contracts.FinalizeRequest{ExecutionLease: lease, Duration: 123, Followups: []contracts.EnqueueRequest{
		{TaskClass: "ExecutionChild", Data: `{"child":1}`},
		{TaskClass: "ExecutionChild", Data: `{"child":2}`, DelayUntil: &delay, ObjectPHID: "PHID-TEST-child"},
	}}
	for i := 0; i < 2; i++ {
		if err := execution.Finalize(ctx, req); err != nil {
			t.Fatalf("finalize attempt %d: %v", i, err)
		}
	}
	active, err := s.ListActive(ctx, 10, 0)
	if err != nil || len(active) != 2 {
		t.Fatalf("expected exactly two children after replay: %v, %v", active, err)
	}
	for _, child := range active {
		if child.Priority != priority {
			t.Fatalf("lost parent priority: %+v", child)
		}
	}
	if active[1].LeaseExpires == nil || *active[1].LeaseExpires != delay || active[1].ObjectPHID != "PHID-TEST-child" {
		t.Fatalf("lost child options: %+v", active[1])
	}
	if p, err := s.GetTask(ctx, parent.ID); err != nil || p != nil {
		t.Fatalf("parent remains active: %v %v", p, err)
	}
	for _, outcome := range []string{"retry", "yield", "failure"} {
		child, err := s.Enqueue(ctx, &contracts.EnqueueRequest{TaskClass: "Resolution" + outcome, Data: "{}"})
		if err != nil {
			t.Fatal(err)
		}
		leased, err := s.Lease(ctx, 1, "resolver", []string{child.TaskClass})
		if err != nil || len(leased) != 1 {
			t.Fatalf("resolution lease: %v %v", leased, err)
		}
		identity := contracts.ExecutionLease{TaskID: child.ID, LeaseOwner: leased[0].LeaseOwner, LeaseExpires: *leased[0].LeaseExpires}
		invalid := identity
		invalid.LeaseOwner = "obsolete"
		wait := 3600
		if err := execution.Resolve(ctx, &contracts.ResolveRequest{ExecutionLease: invalid, Outcome: outcome, RetryWait: &wait, Duration: 3600}); !errors.Is(err, ErrLeaseConflict) {
			t.Fatalf("%s accepted stale owner: %v", outcome, err)
		}
		request := &contracts.ResolveRequest{ExecutionLease: identity, Outcome: outcome, RetryWait: &wait, Duration: 3600}
		if err := execution.Resolve(ctx, request); err != nil {
			t.Fatal(err)
		}
		if err := execution.Resolve(ctx, request); !errors.Is(err, ErrLeaseConflict) {
			t.Fatalf("%s replay should conflict: %v", outcome, err)
		}
		changed, err := s.GetTask(ctx, child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if outcome == "failure" {
			if changed != nil {
				t.Fatal("permanent failure remained active")
			}
		} else if changed == nil || changed.LeaseExpires == nil {
			t.Fatal("missing resolution state")
		} else if outcome == "retry" && (changed.FailureCount != 1 || changed.LeaseOwner != "") {
			t.Fatalf("invalid retry state: %+v", changed)
		} else if outcome == "yield" && (changed.FailureCount != 0 || changed.LeaseOwner != contracts.YieldOwner) {
			t.Fatalf("invalid yield state: %+v", changed)
		}
	}

	runnable, err := s.Lease(ctx, 10, "child-owner", nil)
	if err != nil || len(runnable) != 1 {
		t.Fatalf("delay not preserved: %v %v", runnable, err)
	}
}

// Creates and removes only a random test database. The DSN user needs DDL
// privileges on the disposable server; never supply a production DSN.
func TestExecutionMySQLIntegration(t *testing.T) {
	dsn := os.Getenv("GORGE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GORGE_TEST_MYSQL_DSN to run against a disposable MySQL server")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	admin, err := OpenDB(cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("gorge_execution_test_%d", time.Now().UnixNano())
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(ctx, "DROP DATABASE "+name)
	cfg.DBName = name
	db, err := OpenDB(cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, ddl := range []string{
		`CREATE TABLE worker_gorgeinbox (eventID VARBINARY(128) NOT NULL PRIMARY KEY, payloadHash VARCHAR(64) NOT NULL, response LONGTEXT NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE worker_taskdata (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, data LONGTEXT NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE lisk_counter (counterName VARCHAR(64) NOT NULL PRIMARY KEY, counterValue BIGINT UNSIGNED NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE worker_activetask (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, taskClass VARCHAR(255) NOT NULL, leaseOwner VARCHAR(255) NULL, leaseExpires BIGINT NULL, failureCount INT NOT NULL, dataID BIGINT NOT NULL, failureTime BIGINT NULL, priority INT NOT NULL, objectPHID VARCHAR(64) NULL, containerPHID VARCHAR(64) NULL, dateCreated BIGINT NOT NULL, dateModified BIGINT NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE worker_archivetask (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, taskClass VARCHAR(255) NOT NULL, leaseOwner VARCHAR(255) NULL, leaseExpires BIGINT NULL, failureCount INT NOT NULL, dataID BIGINT NOT NULL, priority INT NOT NULL, objectPHID VARCHAR(64) NULL, containerPHID VARCHAR(64) NULL, result INT NOT NULL, duration BIGINT NOT NULL, dateCreated BIGINT NOT NULL, dateModified BIGINT NOT NULL, archivedEpoch BIGINT NULL) ENGINE=InnoDB`,
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	s := &MySQLStore{db: db, leaseDuration: 3600, retryWait: 300}
	exerciseExecutionStore(t, ctx, s, s)
	exerciseOutboxRelay(t, s)
}
