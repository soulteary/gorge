package taskqueue

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// Runs in the random disposable database created by TestExecutionMySQLIntegration.
func exerciseSchedulerMySQL(t *testing.T, s *MySQLStore) {
	ctx := t.Context()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, ddl := range []string{
		`CREATE TABLE worker_trigger (id INT UNSIGNED NOT NULL PRIMARY KEY,triggerVersion INT UNSIGNED NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE worker_triggerevent (id INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,triggerID INT UNSIGNED NOT NULL,lastEventEpoch INT UNSIGNED NULL,nextEventEpoch INT UNSIGNED NULL,UNIQUE KEY key_trigger(triggerID),KEY key_next(nextEventEpoch)) ENGINE=InnoDB`,
		`CREATE TABLE worker_gorgeschedulercontrol (id INT UNSIGNED NOT NULL PRIMARY KEY,owner VARBINARY(16) NOT NULL,epoch BIGINT UNSIGNED NOT NULL,databaseID VARBINARY(36) NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE worker_gorgeschedule (triggerID INT UNSIGNED NOT NULL PRIMARY KEY,scheduledVersion INT UNSIGNED NOT NULL,retryAfter BIGINT UNSIGNED NOT NULL DEFAULT 0,lastEvaluatedEpoch BIGINT UNSIGNED NOT NULL DEFAULT 0) ENGINE=InnoDB`,
	} {
		exec(ddl)
	}
	exec(`INSERT INTO worker_gorgeschedulercontrol VALUES (1,'gorge',1,?)`, testSchedulerDatabaseID)
	if err := s.ScheduleReady(ctx); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO worker_trigger VALUES (900,1)`)
	candidate := func() ScheduleSnapshot {
		t.Helper()
		list, err := s.ScheduleCandidates(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Fatalf("expected one candidate, got %v", list)
		}
		return list[0]
	}
	snap := candidate()
	id, err := s.ScheduleDatabaseID(ctx)
	if err != nil || id != testSchedulerDatabaseID {
		t.Fatal("database identity unavailable", id, err)
	}
	past := time.Now().Unix() - 60
	p := SchedulePlan{Protocol: 1, ID: 900, Version: 1, Next: &past}
	wrongDatabase := snap
	wrongDatabase.DatabaseID = "00000000-0000-0000-0000-000000000002"
	if err := s.ApplySchedule(ctx, wrongDatabase, p); !errors.Is(err, ErrScheduleConflict) {
		t.Fatal("wrong database plan accepted", err)
	}
	if err := s.ApplySchedule(ctx, snap, p); err != nil {
		t.Fatal(err)
	}
	snap = candidate()
	// Multiple replicas race on the same plan. Only the event/queue transaction
	// winner may enqueue; its lost HTTP/source response is harmless on retry.
	p.Fire = true
	p.Next = epoch(past + 1)
	p.Task = &contracts.EnqueueRequest{TaskClass: "SchedulerTask", Data: `{"trigger.this-epoch":1}`}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { results <- s.ApplySchedule(ctx, snap, p) })
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrScheduleConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_activetask WHERE taskClass='SchedulerTask'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("duplicate enqueue", n, err)
	}
	if err := s.ApplySchedule(ctx, snap, p); !errors.Is(err, ErrScheduleConflict) {
		t.Fatal("replay accepted", err)
	}
	// An edited clock cannot be overwritten or fired by the previous plan.
	old := candidate()
	exec(`UPDATE worker_trigger SET triggerVersion=2 WHERE id=900`)
	oldPlan := SchedulePlan{Protocol: 1, ID: 900, Version: 1, Fire: true, Task: p.Task}
	if err := s.ApplySchedule(ctx, old, oldPlan); !errors.Is(err, ErrScheduleConflict) {
		t.Fatal("stale edit accepted", err)
	}
	snap = candidate()
	future := time.Now().Unix() + 3600
	if err := s.ApplySchedule(ctx, snap, SchedulePlan{Protocol: 1, ID: 900, Version: 2, Next: &future}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ScheduleCandidates(ctx, 100)
	if err != nil || len(list) != 0 {
		t.Fatal("future event selected", list, err)
	}
	// Pause + switchback invalidates a plan even though the trigger is unchanged.
	exec(`UPDATE worker_trigger SET triggerVersion=3 WHERE id=900`)
	snap = candidate()
	exec(`UPDATE worker_gorgeschedulercontrol SET owner='paused',epoch=2 WHERE id=1`)
	list, err = s.ScheduleCandidates(ctx, 100)
	if err != nil || len(list) != 0 {
		t.Fatal("paused scheduler active", list, err)
	}
	exec(`UPDATE worker_gorgeschedulercontrol SET owner='gorge',epoch=3 WHERE id=1`)
	if err = s.ApplySchedule(ctx, snap, SchedulePlan{Protocol: 1, ID: 900, Version: 3, Next: &future}); !errors.Is(err, ErrScheduleConflict) {
		t.Fatal("old owner plan accepted", err)
	}
	snap = candidate()
	if err = s.ApplySchedule(ctx, snap, SchedulePlan{Protocol: 1, ID: 900, Version: 3, Next: &past}); err != nil {
		t.Fatal(err)
	}
	snap = candidate()
	exec(`CREATE TRIGGER scheduler_reject BEFORE INSERT ON worker_activetask FOR EACH ROW
 BEGIN IF NEW.taskClass='SchedulerReject' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected enqueue failure'; END IF; END`)
	var before int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_taskdata`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rejected := SchedulePlan{Protocol: 1, ID: 900, Version: 3, Fire: true, Task: &contracts.EnqueueRequest{TaskClass: "SchedulerReject", Data: `{}`}}
	if err = s.ApplySchedule(ctx, snap, rejected); err == nil {
		t.Fatal("expected rollback")
	}
	after := candidate()
	if !sameEpoch(after.Last, snap.Last) || !sameEpoch(after.Next, snap.Next) {
		t.Fatal("enqueue failure advanced event")
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_taskdata`).Scan(&n); err != nil || n != before {
		t.Fatal("orphan task payload", n, before, err)
	}
	if err = s.ScheduleFailed(ctx, snap); err != nil {
		t.Fatal(err)
	}
	list, err = s.ScheduleCandidates(ctx, 100)
	if err != nil || len(list) != 0 {
		t.Fatal("retry delay missing", list, err)
	}
	// Deletion wins against a plan prepared before the object disappeared.
	exec(`DELETE FROM worker_trigger WHERE id=900`)
	if err = s.ApplySchedule(ctx, snap, rejected); !errors.Is(err, ErrScheduleConflict) {
		t.Fatal("deleted trigger fired", err)
	}
}
