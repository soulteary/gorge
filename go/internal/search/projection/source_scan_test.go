package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

type scanFixture struct {
	catalog *SourceCatalog
	page    *SourcePage
	err     error
	calls   int
	onScan  func()
}

func (f *scanFixture) Catalog(context.Context) (*SourceCatalog, error) { return f.catalog, f.err }
func (f *scanFixture) Scan(context.Context, string, string, string) (*SourcePage, error) {
	f.calls++
	if f.onScan != nil {
		f.onScan()
	}
	return f.page, f.err
}
func scanCatalog() *SourceCatalog {
	return &SourceCatalog{Version: 1, Namespace: "default", SerializerVersion: "test-v1", Scope: "registered-lisk-fulltext", Sources: []SourceDescriptor{{"TestSource", "2"}}, UnsupportedClasses: []string{}}
}
func scanPage(t *testing.T) *SourcePage {
	p := &SourcePage{Version: 1, Namespace: "default", SerializerVersion: "test-v1", ClassName: "TestSource", AfterID: "0", UpperID: "2", NextID: "2", Complete: true, Materialized: true}
	for i := 1; i <= 2; i++ {
		e := goldenEvent(t)
		e.PHID = fmt.Sprintf("PHID-TASK-scan%d", i)
		e.Document.PHID = e.PHID
		e.EventID = fmt.Sprintf("source/%d", i)
		e.PayloadHash, _ = PayloadHash(e)
		raw, _ := json.Marshal(e)
		p.Items = append(p.Items, SourceItem{SourceID: fmt.Sprint(i), PHID: e.PHID, Status: "materialized", Event: raw})
	}
	return p
}
func TestSourceScanProtocol(t *testing.T) {
	b, err := os.ReadFile("../../../../resources/sql/search/source-scan.sql")
	if err != nil || strings.TrimSpace(string(b)) != strings.TrimSpace(SourceScanSchema) {
		t.Fatal("operator schema differs", err)
	}
	c := scanCatalog()
	p := scanPage(t)
	if err := ValidateCatalog(c, "default"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePage(p, c, "TestSource", "0", "2"); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*SourcePage){
		func(p *SourcePage) { p.NextID = "1" }, func(p *SourcePage) { p.NextID = "03" }, func(p *SourcePage) { p.SourceCoverageVerified = true }, func(p *SourcePage) { p.Materialized = false }, func(p *SourcePage) { p.Items[0].Status = "missing" }, func(p *SourcePage) { p.Items[1].SourceID = "1" }, func(p *SourcePage) { p.Items[0].PHID = "PHID-TASK-other" }, func(p *SourcePage) { p.Items[0].Status = "retry" }, func(p *SourcePage) { p.Namespace = "other" },
	}
	for i, m := range mutations {
		p = scanPage(t)
		m(p)
		if ValidatePage(p, c, "TestSource", "0", "2") == nil {
			t.Fatalf("unsafe page %d", i)
		}
	}
	for _, id := range []string{"", "01", "-1", "9223372036854775808"} {
		c = scanCatalog()
		c.Sources[0].UpperID = id
		if ValidateCatalog(c, "default") == nil {
			t.Fatal("bad upper", id)
		}
	}
	if path := os.Getenv("GORGE_TEST_SEARCH_SCAN_PAGE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var sample struct {
			Catalog SourceCatalog `json:"catalog"`
			Page    SourcePage    `json:"page"`
		}
		if err = json.Unmarshal(b, &sample); err != nil {
			t.Fatal(err)
		}
		if err = ValidateCatalog(&sample.Catalog, sample.Catalog.Namespace); err != nil {
			t.Fatal(err)
		}
		if err = ValidatePage(&sample.Page, &sample.Catalog, sample.Page.ClassName, sample.Page.AfterID, sample.Page.UpperID); err != nil {
			t.Fatal("real PHP page", err)
		}
	}
}
func TestMySQLSourceScanRecovery(t *testing.T) {
	dsn := os.Getenv("GORGE_TEST_SEARCH_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GORGE_TEST_SEARCH_MYSQL_DSN")
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
	defer func() {
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	}()
	name := fmt.Sprintf("gorge_source_%d", time.Now().UnixNano())
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
	for _, ddl := range strings.Split(Schema+SourceScanSchema+RebuildSchema, ";") {
		if strings.TrimSpace(ddl) != "" {
			if _, err = db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := &MySQLStore{DB: db}
	ctx := context.Background()
	target := Target{"es", "new"}
	old := Target{"es", "old"}
	if err = s.BindTarget(ctx, "default", target, strings.Repeat("a", 64), "scan-index"); err != nil {
		t.Fatal(err)
	}
	provider := &scanFixture{catalog: scanCatalog(), page: scanPage(t)}
	if _, err = s.CreateSourceJob(ctx, "default", "job", target, provider.catalog); err != nil {
		t.Fatal(err)
	}
	changed := scanCatalog()
	changed.Sources[0].UpperID = "100"
	job, err := s.CreateSourceJob(ctx, "default", "job", target, changed)
	if err != nil || job.Shards[0].UpperID != "2" {
		t.Fatal("catalog not frozen", job, err)
	}
	for _, item := range provider.page.Items {
		e, _ := Decode(item.Event)
		if _, err = s.Accept(ctx, e, []Target{old}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`CREATE TRIGGER reject_scan BEFORE INSERT ON search_projection_inbox FOR EACH ROW BEGIN IF NEW.eventID LIKE 'source-scan/%' AND JSON_UNQUOTE(JSON_EXTRACT(NEW.envelope,'$.phid'))='PHID-TASK-scan2' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test partial fault'; END IF; END`); err != nil {
		t.Fatal(err)
	}
	if err = s.StepSourceShard(ctx, "default", "job", "TestSource", "owner1", provider); err == nil {
		t.Fatal("partial failure absent")
	}
	job, err = s.SourceJob(ctx, "default", "job")
	if err != nil || job.Shards[0].CursorID != "0" || job.Shards[0].Pages != 0 {
		t.Fatal("partial progress committed", job, err)
	}
	if _, err = db.Exec("DROP TRIGGER reject_scan"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE search_projection_source_shard SET nextAttempt=0"); err != nil {
		t.Fatal(err)
	}
	if err = s.StepSourceShard(ctx, "default", "job", "TestSource", "owner2", provider); err != nil {
		t.Fatal(err)
	}
	job, err = s.SourceJob(ctx, "default", "job")
	if err != nil || !job.SourceScanComplete || job.ActivationAllowed || job.SourceCoverageVerified || job.Shards[0].Materialized != 2 {
		t.Fatal(job, err)
	}

	repair, err := s.RebuildJob(ctx, "default", job.ControlRebuildJobID)
	if err != nil || repair.Target != target {
		t.Fatal("source completion omitted repair job", repair, err)
	}
	if err = s.StepRebuild(ctx, "default", job.ControlRebuildJobID, "repair-owner"); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := s.CheckRebuild(ctx, "default", job.ControlRebuildJobID)
	if err != nil || checkpoint.TransportCaughtUp || checkpoint.ActivationAllowed {
		t.Fatal("pending deliveries passed checkpoint", checkpoint, err)
	}
	var n int
	if err = db.QueryRow("SELECT COUNT(*) FROM search_projection_delivery WHERE generationID='new'").Scan(&n); err != nil || n != 2 {
		t.Fatal("retarget/replay", n, err)
	}

	// An exact current-revision receipt is required, even after scan completion.
	if _, err = db.Exec("UPDATE search_projection_delivery SET status='applied',appliedEpoch=UNIX_TIMESTAMP() WHERE generationID='new'"); err != nil {
		t.Fatal(err)
	}
	sourceCheck, err := s.CheckSourceJob(ctx, "default", "job")
	if err != nil || !sourceCheck.TransportCaughtUp || sourceCheck.ActivationAllowed || sourceCheck.SourceCoverageVerified {
		t.Fatal("exact receipt checkpoint", sourceCheck, err)
	}
	late, _ := Decode(provider.page.Items[0].Event)
	late.Revision = "43"
	late.EventID = "source/late43"
	if _, err = s.Accept(ctx, late, []Target{old}); err != nil {
		t.Fatal(err)
	}
	sourceCheck, err = s.CheckSourceJob(ctx, "default", "job")
	if err != nil || sourceCheck.TransportCaughtUp {
		t.Fatal("new revision did not invalidate checkpoint", sourceCheck, err)
	}
	if _, err = db.Exec("UPDATE search_projection_rebuild SET nextAttempt=0 WHERE jobID=?", job.ControlRebuildJobID); err != nil {
		t.Fatal(err)
	}
	if err = s.StepRebuild(ctx, "default", job.ControlRebuildJobID, "late-owner"); err != nil {
		t.Fatal(err)
	}
	late.Revision = "44"
	late.EventID = "source/delete44"
	late.Operation = "delete"
	late.Document = nil
	late.PayloadHash, _ = PayloadHash(late)
	if _, err = s.Accept(ctx, late, []Target{old}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE search_projection_rebuild SET nextAttempt=0 WHERE jobID=?", job.ControlRebuildJobID); err != nil {
		t.Fatal(err)
	}
	if err = s.StepRebuild(ctx, "default", job.ControlRebuildJobID, "delete-owner"); err != nil {
		t.Fatal(err)
	}
	var operation string
	if err = db.QueryRow("SELECT JSON_UNQUOTE(JSON_EXTRACT(i.envelope,'$.operation')) FROM search_projection_delivery d JOIN search_projection_inbox i ON i.namespace=d.namespace AND i.eventID=d.eventID WHERE d.generationID='new' AND d.phid=? AND d.revision=44", late.PHID).Scan(&operation); err != nil || operation != "delete" {
		t.Fatal("late tombstone omitted", operation, err)
	}
	calls := provider.calls
	if err = s.StepSourceShard(ctx, "default", "job", "TestSource", "owner3", provider); !errors.Is(err, sql.ErrNoRows) || calls != provider.calls {
		t.Fatal("completed shard reentered", err)
	}
	if err = s.BindTarget(ctx, "default", old, strings.Repeat("b", 64), "old-scan-index"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateSourceJob(ctx, "default", "job", old, scanCatalog()); !errors.Is(err, ErrRebuildConflict) {
		t.Fatal("target mutation", err)
	}
	if _, err = s.CreateSourceJob(ctx, "default", "takeover", target, scanCatalog()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE search_projection_source_shard SET leaseOwner='dead',leaseEpoch=10,leaseExpires=UNIX_TIMESTAMP()-1 WHERE jobID='takeover'"); err != nil {
		t.Fatal(err)
	}
	provider.err = errors.New("unavailable")
	if err = s.StepSourceShard(ctx, "default", "takeover", "TestSource", "replacement", provider); err == nil {
		t.Fatal("provider error ignored")
	}
	job, err = s.SourceJob(ctx, "default", "takeover")
	if err != nil || job.Shards[0].CursorID != "0" {
		t.Fatal("failed provider advances", err)
	}
	provider.err = nil
	if _, err = db.Exec("UPDATE search_projection_source_shard SET nextAttempt=0 WHERE jobID='takeover'"); err != nil {
		t.Fatal(err)
	}
	if err = s.StepSourceShard(ctx, "default", "takeover", "TestSource", "replacement", provider); err != nil {
		t.Fatal(err)
	}
	// A response received after another owner increments the epoch cannot commit progress.
	if _, err = s.CreateSourceJob(ctx, "default", "fenced", target, scanCatalog()); err != nil {
		t.Fatal(err)
	}
	provider.onScan = func() {
		if _, err = db.Exec("UPDATE search_projection_source_shard SET leaseOwner='successor',leaseEpoch=leaseEpoch+1,leaseExpires=UNIX_TIMESTAMP()+30 WHERE jobID='fenced'"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.StepSourceShard(ctx, "default", "fenced", "TestSource", "stale", provider); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("stale cursor commit", err)
	}
	job, err = s.SourceJob(ctx, "default", "fenced")
	if err != nil || job.Shards[0].CursorID != "0" || job.Shards[0].Pages != 0 {
		t.Fatal("stale progress", job, err)
	}
	var owner string
	if err = db.QueryRow("SELECT leaseOwner FROM search_projection_source_shard WHERE jobID='fenced'").Scan(&owner); err != nil || owner != "successor" {
		t.Fatal("old failure cleanup cleared new owner", owner, err)
	}

	// A repair insertion fault must roll back the terminal cursor too.
	if _, err = s.CreateSourceJob(ctx, "default", "atomic", target, scanCatalog()); err != nil {
		t.Fatal(err)
	}
	provider.onScan = nil
	if _, err = db.Exec(`CREATE TRIGGER reject_source_repair BEFORE INSERT ON search_projection_rebuild FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='repair unavailable'`); err != nil {
		t.Fatal(err)
	}
	if err = s.StepSourceShard(ctx, "default", "atomic", "TestSource", "atomic-owner", provider); err == nil {
		t.Fatal("repair creation fault ignored")
	}
	job, err = s.SourceJob(ctx, "default", "atomic")
	if err != nil || job.SourceScanComplete || job.Shards[0].CursorID != "0" {
		t.Fatal("completion without repair", job, err)
	}
	if _, err = db.Exec("DROP TRIGGER reject_source_repair"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE search_projection_source_shard SET nextAttempt=0 WHERE jobID='atomic'"); err != nil {
		t.Fatal(err)
	}
	if err = s.StepSourceShard(ctx, "default", "atomic", "TestSource", "atomic-owner", provider); err != nil {
		t.Fatal(err)
	}
	check, err := s.CheckSourceJob(ctx, "default", "atomic")
	if err != nil || check.Control == nil || check.TransportCaughtUp || check.ActivationAllowed || check.SourceCoverageVerified {
		t.Fatal(check, err)
	}

	// Completed jobs from the earlier scanner can recover their missing link.
	if _, err = db.Exec("DELETE FROM search_projection_rebuild WHERE jobID=?", check.Job.ControlRebuildJobID); err != nil {
		t.Fatal(err)
	}
	check, err = s.CheckSourceJob(ctx, "default", "atomic")
	if err != nil || check.Control == nil || check.TransportCaughtUp {
		t.Fatal("legacy completed job could not recover", check, err)
	}
	// Concurrent terminal shards serialize the final completion decision.
	catalog := scanCatalog()
	catalog.Sources = append(catalog.Sources, SourceDescriptor{"TestSourceB", "2"})
	if _, err = s.CreateSourceJob(ctx, "default", "concurrent", target, catalog); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, class := range []string{"TestSource", "TestSourceB"} {
		wg.Add(1)
		go func(class string) {
			defer wg.Done()
			page := scanPage(t)
			page.ClassName = class
			failures <- s.StepSourceShard(ctx, "default", "concurrent", class, "concurrent-owner", &scanFixture{page: page})
		}(class)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		if failure != nil {
			t.Fatal(failure)
		}
	}
	job, err = s.SourceJob(ctx, "default", "concurrent")
	if err != nil || !job.SourceScanComplete {
		t.Fatal(job, err)
	}
	if _, err = s.RebuildJob(ctx, "default", job.ControlRebuildJobID); err != nil {
		t.Fatal("concurrent completion lost repair", err)
	}

}
