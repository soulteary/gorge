package projection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestRebuildSchemaArtifact(t *testing.T) {
	b, err := os.ReadFile("../../../../resources/sql/search/rebuild.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != strings.TrimSpace(RebuildSchema) {
		t.Fatal("rebuild operator schema differs")
	}
	for _, id := range []string{"", "../x", "a b", strings.Repeat("a", 65)} {
		if validateJob("default", id, Target{"es", "g1"}) == nil {
			t.Fatal("invalid job accepted", id)
		}
	}
}
func TestMySQLRebuildRecovery(t *testing.T) {
	dsn := os.Getenv("GORGE_TEST_SEARCH_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GORGE_TEST_SEARCH_MYSQL_DSN")
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
	defer func() { _ = admin.Close() }()
	name := fmt.Sprintf("gorge_rebuild_%d", time.Now().UnixNano())
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
	defer func() { _ = db.Close() }()
	for _, ddl := range strings.Split(Schema+RebuildSchema, ";") {
		if strings.TrimSpace(ddl) != "" {
			if _, err = db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx := context.Background()
	s := &MySQLStore{DB: db}
	old := Target{"es", "old"}
	target := Target{"es", "new"}
	if err = s.BindTarget(ctx, "default", target, strings.Repeat("a", 64), "rebuild-index-uuid"); err != nil {
		t.Fatal(err)
	}
	makeEvent := func(id string, revision string) *contracts.SearchProjection {
		e := &contracts.SearchProjection{ProjectionVersion: 1, Namespace: "default", EventID: "source/" + id + "/" + revision, PHID: "PHID-TASK-" + id, Type: "TASK", Revision: revision, Operation: "delete", SourceVersion: "authoritative-source", SerializerVersion: "test"}
		e.PayloadHash, _ = PayloadHash(e)
		return e
	}
	for i := 0; i < 40; i++ {
		if _, err = s.Accept(ctx, makeEvent(fmt.Sprintf("r%04d", i), "1"), []Target{old}); err != nil {
			t.Fatal(err)
		}
	}
	job, err := s.CreateRebuild(ctx, "default", "job", target)
	if err != nil {
		t.Fatal(err)
	}
	if job.UpperPHID != "PHID-TASK-r0039" || job.SourceCoverageVerified {
		t.Fatalf("%+v", job)
	}
	if _, err = s.CreateRebuild(ctx, "default", "unbound", Target{"es", "absent"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unbound=%v", err)
	}
	if err = s.BindTarget(ctx, "default", old, strings.Repeat("b", 64), "old-index-uuid"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRebuild(ctx, "default", "job", old); !errors.Is(err, ErrRebuildConflict) {
		t.Fatalf("conflict=%v", err)
	}
	// A partial acceptance must not advance the durable page cursor.
	_, err = db.Exec(`CREATE TRIGGER reject_rebuild BEFORE INSERT ON search_projection_inbox FOR EACH ROW BEGIN IF NEW.eventID LIKE 'rebuild/%' AND JSON_UNQUOTE(JSON_EXTRACT(NEW.envelope,'$.phid'))='PHID-TASK-r0010' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test page failure'; END IF; END`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StepRebuild(ctx, "default", "job", "owner1"); err == nil {
		t.Fatal("expected partial page failure")
	}
	job, err = s.RebuildJob(ctx, "default", "job")
	if err != nil || job.CursorPHID != "" || job.Pages != 0 {
		t.Fatalf("%+v %v", job, err)
	}
	if _, err = db.Exec("DROP TRIGGER reject_rebuild"); err != nil {
		t.Fatal(err)
	}
	due := func() {
		t.Helper()
		if _, err = db.Exec("UPDATE search_projection_rebuild SET nextAttempt=0 WHERE jobID='job'"); err != nil {
			t.Fatal(err)
		}
	}
	due()
	if err = s.StepRebuild(ctx, "default", "job", "owner2"); err != nil {
		t.Fatal(err)
	}
	job, err = s.CreateRebuild(ctx, "default", "job", target)
	if err != nil || job.Pages != 1 || job.CursorPHID != "PHID-TASK-r0031" {
		t.Fatalf("restart=%+v %v", job, err)
	}
	due()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- s.StepRebuild(ctx, "default", "job", fmt.Sprintf("concurrent%d", i))
		}(i)
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent page winners=%d", success)
	}
	job, err = s.RebuildJob(ctx, "default", "job")
	if err != nil || job.Status != "awaiting-delivery" || job.Pages != 2 {
		t.Fatalf("%+v %v", job, err)
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM search_projection_delivery WHERE generationID='new'").Scan(&count); err != nil || count != 40 {
		t.Fatalf("deliveries=%d %v", count, err)
	}
	check, err := s.CheckRebuild(ctx, "default", "job")
	if err != nil || check.UnappliedHeads != 40 || check.TransportCaughtUp || check.ActivationAllowed || check.CheckID <= 0 {
		t.Fatalf("%+v %v", check, err)
	}
	// Actual fenced SQL claims/finishes, rather than forcing the barrier counters.
	for i := 0; i < 40; i++ {
		d, e := s.Claim(ctx, "default", target, "apply", time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Finish(ctx, d, "applied", 0); e != nil {
			t.Fatal(e)
		}
	}
	check, err = s.CheckRebuild(ctx, "default", "job")
	if err != nil || !check.TransportCaughtUp || check.ActivationAllowed || check.Job.SourceCoverageVerified {
		t.Fatalf("%+v %v", check, err)
	}
	// A late lower-PHID object and a new revision invalidate the observed barrier.
	if _, err = s.Accept(ctx, makeEvent("aaalate", "1"), []Target{old}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Accept(ctx, makeEvent("r0000", "2"), []Target{old}); err != nil {
		t.Fatal(err)
	}
	check, err = s.CheckRebuild(ctx, "default", "job")
	if err != nil || check.TransportCaughtUp || check.UnappliedHeads != 2 {
		t.Fatalf("drift=%+v %v", check, err)
	}
	due()
	if err = s.StepRebuild(ctx, "default", "job", "repair"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		d, e := s.Claim(ctx, "default", target, "repair-apply", time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Finish(ctx, d, "applied", 0); e != nil {
			t.Fatal(e)
		}
	}
	check, err = s.CheckRebuild(ctx, "default", "job")
	if err != nil || !check.TransportCaughtUp || check.KnownHeads != 41 || check.ActivationAllowed {
		t.Fatalf("repair=%+v %v", check, err)
	}
	// Expired-owner updates cannot move a live job's cursor.
	if _, err = db.Exec("UPDATE search_projection_rebuild SET leaseOwner='live',leaseExpires=UNIX_TIMESTAMP()+60,nextAttempt=0 WHERE jobID='job'"); err != nil {
		t.Fatal(err)
	}
	if err = s.StepRebuild(ctx, "default", "job", "stale"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("live lease=%v", err)
	}
	if _, err = db.Exec("UPDATE search_projection_rebuild SET leaseExpires=UNIX_TIMESTAMP()-1 WHERE jobID='job'"); err != nil {
		t.Fatal(err)
	}
	if err = s.StepRebuild(ctx, "default", "job", "takeover"); err != nil {
		t.Fatal(err)
	}
	// Config removal pauses an old-generation job without deleting its cursor.
	if _, err = s.CreateRebuild(ctx, "default", "aaa-disabled", old); err != nil {
		t.Fatal(err)
	}
	due()
	runCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	s.RunRebuilds(runCtx, "default", "runner", []Target{target})
	cancel()
	parked, err := s.RebuildJob(ctx, "default", "aaa-disabled")
	if err != nil || parked.Pages != 0 {
		t.Fatalf("unconfigured target ran: %+v %v", parked, err)
	}
}
