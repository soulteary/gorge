package cleanup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func fixture(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("GORGE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GORGE_TEST_MYSQL_DSN to a disposable server")
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
	name := fmt.Sprintf("gorge_cleanup_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE DATABASE `" + name + "`"); err != nil {
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
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Error(err)
		}
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	})
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(Schema)
	exec("CREATE TABLE cache_general (id BIGINT UNSIGNED PRIMARY KEY,cacheCreated BIGINT UNSIGNED NOT NULL,cacheExpires BIGINT UNSIGNED NULL,KEY created(cacheCreated),KEY expires(cacheExpires)) ENGINE=InnoDB")
	for _, spec := range Specs() {
		if spec.Table == "cache_general" {
			continue
		}
		exec("CREATE TABLE `" + spec.Table + "` (id BIGINT UNSIGNED PRIMARY KEY,`" + spec.Column + "` BIGINT UNSIGNED NOT NULL, KEY t(`" + spec.Column + "`)) ENGINE=InnoDB")
	}
	store := &Store{DBs: map[string]*sql.DB{"cache": db, "conduit": db, "daemon": db}}
	for _, spec := range Specs() {
		if err := store.Import(context.Background(), policy(spec.ID)); err != nil {
			t.Fatal(err)
		}
	}
	return store, db
}
func enable(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.SetOwner(ctx, id, "paused"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(ctx, id, "gorge"); err != nil {
		t.Fatal(err)
	}
}
func TestMySQLCleanupBoundariesAndCacheRefresh(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	if err := s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	id := "cache.general.ttl"
	enable(t, s, id)
	l, err := s.Claim(ctx, id, "first", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO cache_general VALUES (1,1,NULL),(2,1,?),(3,1,?),(4,1,?)", l.Cutoff-1, l.Cutoff, l.Cutoff+3600); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Status(ctx, id)
	ids, err := s.DryRun(ctx, id)
	if err != nil || len(ids) == 0 {
		t.Fatalf("dryrun %v %v", ids, err)
	}
	after, _ := s.Status(ctx, id)
	if before != after {
		t.Fatal("dry-run changed control state")
	}
	// Refresh after selection; the deletion must recheck expiration.
	if _, err := db.Exec("UPDATE cache_general SET cacheExpires=? WHERE id=2", l.Cutoff+3600); err != nil {
		t.Fatal(err)
	}
	n, err := s.Batch(ctx, l, 100)
	if err != nil || n != 0 {
		t.Fatalf("refreshed value removed: %d %v", n, err)
	}
	if _, err := db.Exec("UPDATE cache_general SET cacheExpires=? WHERE id=2", l.Cutoff-1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(210 * time.Millisecond)
	n, err = s.Batch(ctx, l, 100)
	if err != nil || n != 1 {
		t.Fatalf("expired value not removed: %d %v", n, err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM cache_general").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatal("NULL/boundary/future expired")
	}
	// Backfilled low IDs remain discoverable; no permanent ID high-water mark.
	if _, err := db.Exec("INSERT INTO cache_general VALUES (0,1,?)", l.Cutoff-1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(210 * time.Millisecond)
	n, err = s.Batch(ctx, l, 100)
	if err != nil || n != 1 {
		t.Fatalf("backfill missed %d %v", n, err)
	}
	if err = s.Finish(ctx, l, "exhausted", nil); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Status(ctx, id)
	if st.Deleted != 2 || st.Status != "exhausted" {
		t.Fatalf("bad state %+v", st)
	}
	if _, err = s.Claim(ctx, id, "due", false); !errors.Is(err, ErrBusy) {
		t.Fatalf("ignored nextRun: %v", err)
	}
}
func TestMySQLCleanupFencesAndConcurrency(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	id := "cache.general.ttl"
	enable(t, s, id)
	var winners atomic.Int32
	var lease Lease
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := s.Claim(ctx, id, fmt.Sprint(i), true)
			if err == nil {
				winners.Add(1)
				mu.Lock()
				lease = l
				mu.Unlock()
			} else if !errors.Is(err, ErrBusy) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("multiple lease winners")
	}
	wrong := lease
	wrong.Fence++
	if _, err := s.Batch(ctx, wrong, 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale fence accepted %v", err)
	}
	if err := s.Import(ctx, policy(id)); err == nil {
		t.Fatal("active policy replaced")
	}
	if err := s.SetOwner(ctx, id, "paused"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Batch(ctx, lease, 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("paused lease accepted %v", err)
	}
	if err := s.Import(ctx, policy(id)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(ctx, id, "gorge"); err != nil {
		t.Fatal(err)
	}
	l, err := s.Claim(ctx, id, "new", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE gorge_gc_control SET leaseExpires=UNIX_TIMESTAMP()-1 WHERE collectorID=?", id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Batch(ctx, l, 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired lease accepted %v", err)
	}
	enable(t, s, "daemon.lock-log")
	if _, err = s.Claim(ctx, "daemon.lock-log", "disabled", true); !errors.Is(err, ErrBusy) {
		t.Fatal("indefinite collector ran")
	}
}
func TestMySQLCleanupRollbackAndMissingIndex(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	id := "cache.general.ttl"
	enable(t, s, id)
	l, err := s.Claim(ctx, id, "rollback", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO cache_general VALUES (1,1,1)"); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TRIGGER reject_cleanup BEFORE DELETE ON cache_general FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture rejection'")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Batch(ctx, l, 100); err == nil {
		t.Fatal("injected failure did not fail")
	}
	st, _ := s.Status(ctx, id)
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM cache_general").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if st.Deleted != 0 || count != 1 {
		t.Fatal("failed deletion escaped rollback")
	}
	if _, err := db.Exec("DROP TRIGGER reject_cleanup"); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, l, "retryable_failure", errors.New("fixture")); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status(ctx, id)
	if st.Failures != 1 || st.NextRun == 0 {
		t.Fatal("failure did not back off")
	}
	if _, err := db.Exec("ALTER TABLE cache_general DROP INDEX expires"); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateSchema(ctx, id); err == nil {
		t.Fatal("missing index accepted")
	}
	if _, err = s.RunOnce(ctx, id); err == nil {
		t.Fatal("run-once ignored missing index")
	}
}
func TestMySQLPauseSerializesWithBatchTransaction(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	id := "cache.general.ttl"
	enable(t, s, id)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = lock(ctx, tx, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		done <- s.SetOwner(bounded, id, "paused")
	}()
	select {
	case err := <-done:
		t.Fatalf("pause crossed in-flight transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	st, _ := s.Status(ctx, id)
	if st.Owner != "paused" {
		t.Fatal("pause lost")
	}
}

func TestMySQLCleanupTimeoutRollsBackAndRejectsForgedCutoff(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	id := "cache.general.ttl"
	p := policy(id)
	p.BatchTimeoutMS = 100
	if err := s.Import(ctx, p); err != nil {
		t.Fatal(err)
	}
	enable(t, s, id)
	l, err := s.Claim(ctx, id, "timeout", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO cache_general VALUES (1,1,1)"); err != nil {
		t.Fatal(err)
	}
	forged := l
	forged.Cutoff += 3600
	if _, err = s.Batch(ctx, forged, 100); !errors.Is(err, ErrConflict) {
		t.Fatal("forged cutoff accepted")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var value uint64
	if err = tx.QueryRow("SELECT id FROM cache_general WHERE id=1 FOR UPDATE").Scan(&value); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err = s.Batch(ctx, l, 100); err == nil {
		t.Fatal("blocked batch did not time out")
	}
	if time.Since(started) > time.Second {
		t.Fatal("batch timeout not bounded")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Status(ctx, id)
	if st.Deleted != 0 {
		t.Fatal("timed out batch updated stats")
	}
	n, err := s.Batch(ctx, l, 100)
	if err != nil || n != 1 {
		t.Fatalf("timeout recovery %d %v", n, err)
	}
}

func TestMySQLCleanupRunOnceRespectsBudgetAndAgePolicy(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	id := "cache.general"
	p := policy(id)
	p.BatchRows = 1
	p.RunRows = 1
	if err := s.Import(ctx, p); err != nil {
		t.Fatal(err)
	}
	enable(t, s, id)
	if _, err := db.Exec("INSERT INTO cache_general VALUES (1,1,NULL),(2,1,NULL),(3,UNIX_TIMESTAMP(),NULL)"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RunOnce(ctx, id)
	if err != nil || n != 1 {
		t.Fatalf("bounded age cleanup %d %v", n, err)
	}
	st, _ := s.Status(ctx, id)
	if st.Status != "budget_exhausted" || st.Deleted != 1 {
		t.Fatalf("budget state %+v", st)
	}
	n, err = s.RunOnce(ctx, id)
	if err != nil || n != 1 {
		t.Fatalf("manual nextRun override %d %v", n, err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM cache_general").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("fresh cache removed")
	}
	n, err = s.RunOnce(ctx, id)
	if err != nil || n != 0 {
		t.Fatalf("exhausted cleanup %d %v", n, err)
	}
	st, _ = s.Status(ctx, id)
	if st.Status != "exhausted" {
		t.Fatal("empty collector did not finish")
	}
}

func TestDatabaseBudgetAcrossCollectors(t *testing.T) {
	s, _ := fixture(t)
	ctx := context.Background()
	enable(t, s, "cache.general.ttl")
	enable(t, s, "cache.general")
	a, err := s.Claim(ctx, "cache.general.ttl", "instance-a", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Claim(ctx, "cache.general", "instance-b", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Batch(ctx, a, 100); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Batch(ctx, b, 100); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("shared budget bypassed: %v", err)
	}
	time.Sleep(210 * time.Millisecond)
	if _, err = s.Batch(ctx, b, 100); err != nil {
		t.Fatal(err)
	}
}
func TestInvisibleIndexAndIndexRemoval(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	var index string
	if err := db.QueryRow("SELECT INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='cache_general' AND COLUMN_NAME='cacheExpires' AND SEQ_IN_INDEX=1").Scan(&index); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ALTER TABLE cache_general ALTER INDEX `" + index + "` INVISIBLE"); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateSchema(ctx, "cache.general.ttl"); err == nil {
		t.Fatal("invisible index accepted")
	}
	if _, err := db.Exec("ALTER TABLE cache_general ALTER INDEX `" + index + "` VISIBLE"); err != nil {
		t.Fatal(err)
	}
	enable(t, s, "cache.general.ttl")
	l, err := s.Claim(ctx, "cache.general.ttl", "instance", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ALTER TABLE cache_general DROP INDEX `" + index + "`"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Batch(ctx, l, 100); err == nil {
		t.Fatal("index removal during lease accepted")
	}
}
func TestPendingPolicyRequiresImport(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	id := "cache.general"
	if err := s.SetOwner(ctx, id, "paused"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE gorge_gc_control SET policyHash='',lastState='policy_changing' WHERE collectorID=?", id); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(ctx, policy(id)); err == nil {
		t.Fatal("import crossed PHP config write")
	}
	if _, err := db.Exec("UPDATE gorge_gc_control SET lastState='policy_pending' WHERE collectorID=?", id); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(ctx, id, "gorge"); err == nil {
		t.Fatal("old policy resumed")
	}
	if err := s.Import(ctx, policy(id)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(ctx, id, "gorge"); err != nil {
		t.Fatal(err)
	}
}
