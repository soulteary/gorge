package integrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/go-sql-driver/mysql"
	_ "github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/platform/conduitclient"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMySQLIntegration(t *testing.T) {
	dsn := os.Getenv("GORGE_TEST_INTEGRATIONS_DSN")
	if dsn == "" {
		t.Skip("set GORGE_TEST_INTEGRATIONS_DSN to disposable gorge_integrations_test database")
	}
	cfg, e := mysql.ParseDSN(dsn)
	if e != nil || cfg.DBName != "gorge_integrations_test" {
		t.Fatal("refusing non-test schema")
	}
	db, e := sql.Open("mysql", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	raw, e := os.ReadFile("schema.sql")
	if e != nil {
		t.Fatal(e)
	}
	for _, q := range strings.Split(string(raw), ";") {
		if strings.TrimSpace(q) == "" {
			continue
		}
		if _, e = db.ExecContext(ctx, q); e != nil {
			t.Fatal(e)
		}
	}
	for _, q := range []string{"CREATE TABLE fact_cursor(id INT AUTO_INCREMENT PRIMARY KEY,name VARCHAR(64) UNIQUE,position VARCHAR(64)) ENGINE=InnoDB", "CREATE TABLE fact_keydimension(id INT AUTO_INCREMENT PRIMARY KEY,factKey VARCHAR(64) UNIQUE) ENGINE=InnoDB", "CREATE TABLE fact_objectdimension(id INT AUTO_INCREMENT PRIMARY KEY,objectPHID VARCHAR(64) UNIQUE) ENGINE=InnoDB", "CREATE TABLE fact_intdatapoint(id BIGINT AUTO_INCREMENT PRIMARY KEY,keyID INT,objectID INT,dimensionID INT NULL,value BIGINT,epoch INT UNSIGNED) ENGINE=InnoDB"} {
		if _, e = db.ExecContext(ctx, q); e != nil {
			t.Fatal(e)
		}
	}
	defer func() {
		for _, table := range []string{"gorge_integration_resolution", "gorge_integration_effect", "gorge_integration_inbox", "fact_cursor", "fact_keydimension", "fact_objectdimension", "fact_intdatapoint"} {
			_, _ = db.Exec("DROP TABLE " + table)
		}
	}()
	s := Store{db}
	if e = s.Ready(ctx); e != nil {
		t.Fatal(e)
	}
	if e = FactReady(ctx, db); e != nil {
		t.Fatal(e)
	}
	msg := Inbound{Provider: "mailgun", Headers: map[string]string{"message-id": "one", "to": "to", "from": "from"}, Text: "first"}
	id, e := s.Accept(ctx, msg)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Accept(ctx, msg)
	if e != nil || again != id {
		t.Fatal("inbox replay", e)
	}
	msg.Text = "collision"
	if _, e = s.Accept(ctx, msg); !errors.Is(e, ErrConflict) {
		t.Fatal("inbox conflict", e)
	}
	var sends atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Effect(ctx, "sms-one", "payload", "twilio", func() (json.RawMessage, int, error) { sends.Add(1); return json.RawMessage(`{"sid":"one"}`), 201, nil })
			if e != nil && !errors.Is(e, ErrUnknown) {
				errs <- e
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if sends.Load() != 1 {
		t.Fatalf("%d duplicate SMS submits", sends.Load())
	}
	_, e = s.Effect(ctx, "sms-one", "changed", "twilio", func() (json.RawMessage, int, error) { t.Fatal("collision sent"); return nil, 0, nil })
	if !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	_, e = s.Effect(ctx, "sms-unknown", "payload", "sns", func() (json.RawMessage, int, error) { return nil, 0, errors.New("lost reply") })
	if !errors.Is(e, ErrUnknown) {
		t.Fatal(e)
	}
	_, e = s.Effect(ctx, "sms-unknown", "payload", "sns", func() (json.RawMessage, int, error) { t.Fatal("repeated unknown outcome"); return nil, 0, nil })
	if !errors.Is(e, ErrUnknown) {
		t.Fatal(e)
	}
	tx, e := db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	page := FactPage{Objects: []FactObject{{PHID: "task", Facts: []Fact{{Key: "count", Object: "task", Value: "9223372036854775807", Epoch: 1}}}}}
	if e = applyFacts(ctx, tx, page); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	tx, e = db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	if e = applyFacts(ctx, tx, FactPage{Objects: []FactObject{{PHID: "task"}}}); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = db.QueryRow("SELECT COUNT(*) FROM fact_intdatapoint").Scan(&n); e != nil || n != 0 {
		t.Fatal("stale facts", n, e)
	}

	var relayCalls atomic.Int32
	var mode atomic.Value
	mode.Store("done")
	sourcePage := FactPage{Next: "1:1", Objects: []FactObject{{PHID: "task", Facts: []Fact{{Key: "count", Object: "task", Value: "7", Epoch: 1}}}}}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Service-Token") != "source-secret" {
			t.Error("source authentication missing")
		}
		if e := r.ParseForm(); e != nil {
			t.Error(e)
		}
		var params map[string]any
		if e := json.Unmarshal([]byte(r.Form.Get("params")), &params); e != nil {
			t.Error(e)
		}
		var result any
		if r.URL.Path == "/api/integration.inbound" {
			relayCalls.Add(1)
			result = map[string]string{"state": mode.Load().(string)}
		} else if params["phase"] == "catalog" {
			result = []string{"TaskSource"}
		} else {
			result = sourcePage
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error_code": nil, "error_info": nil})
	}))
	defer source.Close()
	client := conduitclient.NewBounded(source.URL, "source-secret", 1024*1024)
	relayErrors := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Relay(ctx, client); err != nil && !errors.Is(err, sql.ErrNoRows) {
				relayErrors <- err
			}
		}()
	}
	wg.Wait()
	close(relayErrors)
	for err := range relayErrors {
		t.Fatal(err)
	}
	if relayCalls.Load() != 1 {
		t.Fatal("concurrent relays duplicated a receipt", relayCalls.Load())
	}
	var state string
	if e = db.QueryRow("SELECT state FROM gorge_integration_inbox WHERE id=?", id).Scan(&state); e != nil || state != "done" {
		t.Fatal("inbound not acknowledged", state, e)
	}
	if e = s.Relay(ctx, client); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("completed inbound repeated", e)
	}
	msg.Headers["message-id"] = "second"
	id, e = s.Accept(ctx, msg)
	if e != nil {
		t.Fatal(e)
	}
	mode.Store("unknown")
	if e = s.Relay(ctx, client); e != nil {
		t.Fatal(e)
	}
	if e = db.QueryRow("SELECT state FROM gorge_integration_inbox WHERE id=?", id).Scan(&state); e != nil || state != "unknown" {
		t.Fatal("unknown inbound repeated", state, e)
	}
	if e = ProjectFacts(ctx, db, client); e != nil {
		t.Fatal(e)
	}
	var position string
	if e = db.QueryRow("SELECT position FROM fact_cursor WHERE name='TaskSource'").Scan(&position); e != nil || position != "1:1" {
		t.Fatal("cursor not committed", position, e)
	}
	if _, e = db.Exec("CREATE TRIGGER reject_fact BEFORE INSERT ON fact_intdatapoint FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture reject'"); e != nil {
		t.Fatal(e)
	}
	sourcePage.Next = "2:1"
	sourcePage.Objects[0].Facts[0].Value = "9"
	if e = ProjectFacts(ctx, db, client); e == nil {
		t.Fatal("failed page committed")
	}
	if _, e = db.Exec("DROP TRIGGER reject_fact"); e != nil {
		t.Fatal(e)
	}
	if e = db.QueryRow("SELECT position FROM fact_cursor WHERE name='TaskSource'").Scan(&position); e != nil || position != "1:1" {
		t.Fatal("failed cursor advanced", position, e)
	}
	var value int64
	if e = db.QueryRow("SELECT value FROM fact_intdatapoint").Scan(&value); e != nil || value != 7 {
		t.Fatal("partial fact projection", value, e)
	}

	// Operators cannot invent a new identity or change content to clear unknown.
	resolution := Resolution{Domain: "effect", ID: "sms-unknown", Digest: strings.Repeat("f", 64), State: "accepted", Status: 200, Result: json.RawMessage(`{"messageID":"verified"}`), Operator: "test-operator", Evidence: "Provider receipt manually verified"}
	if e = s.Resolve(ctx, resolution, nil); !errors.Is(e, ErrConflict) {
		t.Fatal("wrong digest reconciled", e)
	}
	// This test effect originally used a placeholder digest, replace it with a valid one.
	if _, e = db.Exec("UPDATE gorge_integration_effect SET digest=? WHERE id='sms-unknown'", resolution.Digest); e != nil {
		t.Fatal(e)
	}
	if e = s.Resolve(ctx, resolution, nil); e != nil {
		t.Fatal("verified outcome not reconciled", e)
	}
	if _, e = s.Effect(ctx, resolution.ID, resolution.Digest, "sns", func() (json.RawMessage, int, error) { t.Fatal("reconciliation resent SMS"); return nil, 0, nil }); e != nil {
		t.Fatal(e)
	}
	if e = s.Resolve(ctx, resolution, nil); !errors.Is(e, ErrConflict) {
		t.Fatal("terminal outcome rewritten", e)
	}
	if e = db.QueryRow("SELECT COUNT(*) FROM gorge_integration_resolution").Scan(&n); e != nil || n != 1 {
		t.Fatal("resolution evidence missing", n, e)
	}
	inboundResolution := Resolution{Domain: "inbound", ID: id, State: "done", Operator: "test-operator", Evidence: "Business transaction manually verified"}
	if e = db.QueryRow("SELECT digest FROM gorge_integration_inbox WHERE id=?", id).Scan(&inboundResolution.Digest); e != nil {
		t.Fatal(e)
	}
	if e = s.Resolve(ctx, inboundResolution, func(json.RawMessage) error { return errors.New("source refused") }); e == nil {
		t.Fatal("unverified source acknowledged")
	}
	if e = db.QueryRow("SELECT state FROM gorge_integration_inbox WHERE id=?", id).Scan(&state); e != nil || state != "unknown" {
		t.Fatal("failed reconciliation changed state", state, e)
	}
	if e = s.Resolve(ctx, inboundResolution, func(raw json.RawMessage) error {
		if len(raw) == 0 {
			t.Fatal("missing source payload")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("UPDATE gorge_integration_inbox SET due=1 WHERE state='done'"); e != nil {
		t.Fatal(e)
	}
	if n, e := s.Purge(ctx, 30); e != nil || n != 2 {
		t.Fatal("acknowledged payload cleanup", n, e)
	}
	// Identity tombstones survive purging, including content conflict protection.
	if again, e := s.Accept(ctx, msg); e != nil || again != id {
		t.Fatal("late provider retry lost tombstone", again, e)
	}
	if e = db.QueryRow("SELECT state FROM gorge_integration_inbox WHERE id=?", id).Scan(&state); e != nil || state != "done" {
		t.Fatal("purged email requeued", state, e)
	}
	if _, e = s.Health(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Usage(ctx); e != nil {
		t.Fatal(e)
	}
}
