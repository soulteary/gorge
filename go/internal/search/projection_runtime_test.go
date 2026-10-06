package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/engine"
	"github.com/soulteary/gorge/go/internal/search/engine/elasticsearch"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

func TestProjectionDeliveryConfigurationGuard(t *testing.T) {
	target := projection.Target{BackendID: "es", GenerationID: "g1"}
	def := engine.BackendDef{Type: "elasticsearch", Hosts: []string{"http://localhost:9200"}, Index: "shadow", Version: 8, Options: map[string]string{"projection": "true"}}
	for _, mutate := range []func(*ProjectionConfig){
		func(c *ProjectionConfig) { c.Deliveries[0].Backend.Type = "meilisearch" },
		func(c *ProjectionConfig) { c.Deliveries[0].Backend.Version = 5 },
		func(c *ProjectionConfig) { c.Deliveries[0].Backend.Index = "phabricator" },
		func(c *ProjectionConfig) { c.Deliveries[0].Backend.Index = "../escape" },
		func(c *ProjectionConfig) {
			c.Deliveries[0].Backend.Hosts = []string{"http://user:secret@localhost:9200"}
		},
		func(c *ProjectionConfig) { c.Deliveries[0].GenerationID = "unknown" },
		func(c *ProjectionConfig) { c.Deliveries = append(c.Deliveries, c.Deliveries[0]) },
	} {
		cfg := &ProjectionConfig{Namespace: "default", Targets: []projection.Target{target}, Deliveries: []ProjectionBackend{{Target: target, Backend: def}}}
		mutate(cfg)
		if _, err := PrepareProjectionDelivery(context.Background(), cfg, []engine.BackendDef{{Type: "elasticsearch"}}, nil); err == nil {
			t.Fatal("unsafe delivery configuration accepted")
		}
	}
}
func TestProjectionIngressToElasticsearchIntegration(t *testing.T) {
	dsn, endpoint := os.Getenv("GORGE_TEST_SEARCH_MYSQL_DSN"), os.Getenv("GORGE_TEST_ES_URL")
	if dsn == "" || endpoint == "" {
		t.Skip("set real MySQL and Elasticsearch test endpoints")
	}
	dbcfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbcfg.DBName = ""
	admin, err := sql.Open("mysql", dbcfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	}()
	name := fmt.Sprintf("gorge_delivery_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Error(err)
		}
	}()
	dbcfg.DBName = name
	db, err := sql.Open("mysql", dbcfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, ddl := range strings.Split(projection.Schema, ";") {
		if strings.TrimSpace(ddl) != "" {
			if _, err = db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
		}
	}
	def := engine.BackendDef{Type: "elasticsearch", Hosts: []string{endpoint}, Index: name, Version: 8, Options: map[string]string{"projection": "true"}}
	backend := elasticsearch.New(def)
	if err = backend.InitIndex([]string{"TASK"}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		req, _ := http.NewRequest("DELETE", endpoint+"/"+name, nil)
		resp, e := http.DefaultClient.Do(req)
		if e == nil {
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}()
	target := projection.Target{BackendID: "es", GenerationID: "g1"}
	cfg := &ProjectionConfig{ControlDSN: dbcfg.FormatDSN(), Namespace: "default", Targets: []projection.Target{target}, Deliveries: []ProjectionBackend{{Target: target, Backend: def}}}
	ingress, control, err := OpenProjection(context.Background(), cfg, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := control.Close(); err != nil {
			t.Error(err)
		}
	}()
	workers, err := PrepareProjectionDelivery(context.Background(), cfg, nil, control)
	if err != nil {
		t.Fatal(err)
	}
	ingress.BackendDelivery = true
	srv := httpx.New(httpx.Config{})
	RegisterRoutes(srv.App(), &Deps{Token: testToken, Projection: ingress})
	event := &contracts.SearchProjection{ProjectionVersion: 1, EventID: "integration/1", Namespace: "default", PHID: "PHID-TASK-integration", Type: "TASK", Revision: "1", Operation: "upsert", SerializerVersion: "test", SourceVersion: "source-1", Document: &contracts.Document{PHID: "PHID-TASK-integration", Type: "TASK", Title: "end to end"}}
	event.PayloadHash, _ = projection.PayloadHash(event)
	raw, _ := json.Marshal(event)
	resp, body := request(t, srv.App(), "POST", "/api/search/projections", string(raw))
	if resp.StatusCode != 200 {
		t.Fatalf("ingress %d %s", resp.StatusCode, body)
	}
	if err = workers[0].Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = db.QueryRow("SELECT status FROM search_projection_delivery").Scan(&status); err != nil || status != "applied" {
		t.Fatalf("durable result %s %v", status, err)
	}
	req, _ := http.NewRequest("POST", endpoint+"/"+name+"/_refresh", nil)
	refresh, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := refresh.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if ids, err := backend.Search(&contracts.SearchQuery{}); err != nil || len(ids) != 1 || ids[0] != event.PHID {
		t.Fatalf("backend not applied %v %v", ids, err)
	}
	resp, body = request(t, srv.App(), "GET", "/api/search/projections/status?eventID=integration%2F1", "")
	if resp.StatusCode != 200 {
		t.Fatalf("live status %d %s", resp.StatusCode, body)
	}
	var observed projection.EventStatus
	if err = json.Unmarshal(envelope(t, body).Data, &observed); err != nil || observed.LatestRevision != "1" || len(observed.Deliveries) != 1 || observed.Deliveries[0].Status != "applied" {
		t.Fatalf("live inspection %+v %v", observed, err)
	}
	resp, body = request(t, srv.App(), "GET", "/api/search/projections/stats", "")
	if resp.StatusCode != 200 {
		t.Fatalf("live stats %d %s", resp.StatusCode, body)
	}
	var metrics projection.DeliveryStats
	if err = json.Unmarshal(envelope(t, body).Data, &metrics); err != nil || len(metrics.Targets) != 1 || metrics.Targets[0].Applied != 1 {
		t.Fatalf("live metrics %+v %v", metrics, err)
	}
	// Restart can bind exactly the same generation, but not a changed destination.
	if _, err = PrepareProjectionDelivery(context.Background(), cfg, nil, control); err != nil {
		t.Fatal(err)
	}
	cfg.Deliveries[0].Backend.Timeout = 16
	if _, err = PrepareProjectionDelivery(context.Background(), cfg, nil, control); err == nil {
		t.Fatal("changed generation binding accepted")
	}
}
