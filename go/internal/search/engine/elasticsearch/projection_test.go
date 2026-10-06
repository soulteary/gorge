package elasticsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/search/engine"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

func projectionEvent(t *testing.T, revision string, deleted bool) *contracts.SearchProjection {
	t.Helper()
	e := &contracts.SearchProjection{ProjectionVersion: 1, Namespace: "test", EventID: "event/" + revision, PHID: "PHID-TASK-test", Type: "TASK", Revision: revision, Operation: "upsert", SerializerVersion: "test", SourceVersion: "test"}
	e.Document = &contracts.Document{PHID: e.PHID, Type: e.Type, Title: "projection test"}
	if deleted {
		e.Operation = "delete"
		e.Document = nil
	}
	var err error
	e.PayloadHash, err = projection.PayloadHash(e)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestProjectionConflictVerification(t *testing.T) {
	for _, tc := range []struct {
		name            string
		version         int
		hash, namespace string
		deleted         bool
		want            string
	}{
		{"replay", 2, "same", "test", false, "applied"},
		{"newer tombstone", 3, strings.Repeat("b", 64), "test", true, "superseded"},
		{"same version changed", 2, "other", "test", false, ""},
		{"same hash wrong operation", 2, "same", "test", true, ""},
		{"foreign namespace", 3, "same", "other", false, ""},
		{"missing marker", 2, "same", "test", false, ""},
		{"invalid newer hash", 3, strings.Repeat("z", 64), "test", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := projectionEvent(t, "2", false)
			hash := tc.hash
			if hash == "same" {
				hash = e.PayloadHash
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					if r.URL.Query().Get("version_type") != "external" || r.URL.Query().Get("version") != "2" {
						t.Error("missing strict external fence")
					}
					w.WriteHeader(409)
					return
				}
				meta := map[string]any{"namespace": tc.namespace, "generation": "g1", "hash": hash, "deleted": tc.deleted}
				if tc.name == "missing marker" {
					delete(meta, "deleted")
				}
				json.NewEncoder(w).Encode(map[string]any{"_id": e.PHID, "found": true, "_version": tc.version, "_source": map[string]any{"_gorge": meta}})
			}))
			defer srv.Close()
			b := New(engine.BackendDef{Hosts: []string{srv.URL}, Index: "shadow", Version: 8, Options: map[string]string{"projection": "true"}})
			got, err := b.ApplyProjection(context.Background(), e, projection.Target{BackendID: "es", GenerationID: "g1"})
			if tc.want == "" {
				if err == nil {
					t.Fatal("unverified conflict accepted")
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("%s %v", got, err)
			}
		})
	}
}
func TestProjectionTombstoneAndUnversionedGuard(t *testing.T) {
	e := projectionEvent(t, "3", true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		meta := body["_gorge"].(map[string]any)
		if r.Method != "PUT" || meta["deleted"] != true || body["title"] != nil {
			t.Error("delete was not full replacement tombstone")
		}
		json.NewEncoder(w).Encode(map[string]any{"_id": e.PHID, "_version": 3, "result": "updated", "_shards": map[string]any{"failed": 0}})
	}))
	defer srv.Close()
	b := New(engine.BackendDef{Hosts: []string{srv.URL}, Index: "shadow", Version: 8, Options: map[string]string{"projection": "true"}})
	if status, err := b.ApplyProjection(context.Background(), e, projection.Target{BackendID: "es", GenerationID: "g1"}); err != nil || status != "applied" {
		t.Fatalf("%s %v", status, err)
	}
	if err := b.IndexDocument(&contracts.Document{}); err == nil {
		t.Fatal("unversioned write allowed")
	}
	raw, _ := json.Marshal(b.buildSearchSpec(&contracts.SearchQuery{}))
	if !strings.Contains(string(raw), `"_gorge.deleted":true`) {
		t.Fatal("tombstone query filter missing")
	}
}

func TestElasticsearchProjectionRuntime(t *testing.T) {
	endpoint := os.Getenv("GORGE_TEST_ES_URL")
	if endpoint == "" {
		t.Skip("set GORGE_TEST_ES_URL for real Elasticsearch")
	}
	b := New(engine.BackendDef{Hosts: []string{endpoint}, Index: fmt.Sprintf("gorge_projection_%d", time.Now().UnixNano()), Version: 8, Options: map[string]string{"projection": "true"}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.InitIndex([]string{"TASK"}); err != nil {
		t.Fatal(err)
	}
	defer b.projectionRequest(context.Background(), "DELETE", b.baseURL(endpoint), nil)
	if uuid, err := b.ProjectionIndexUUID(ctx); err != nil || uuid == "" {
		t.Fatalf("preflight: %s %v", uuid, err)
	}
	target := projection.Target{BackendID: "es", GenerationID: "g1"}
	for _, step := range []struct {
		rev     string
		deleted bool
		want    string
	}{{"1", false, "applied"}, {"1", false, "applied"}, {"2", true, "applied"}, {"1", false, "superseded"}} {
		status, err := b.ApplyProjection(ctx, projectionEvent(t, step.rev, step.deleted), target)
		if err != nil || status != step.want {
			t.Fatalf("revision %s: %s %v", step.rev, status, err)
		}
	}
	if code, _, err := b.projectionRequest(ctx, "POST", b.baseURL(endpoint)+"/_refresh", nil); err != nil || code != 200 {
		t.Fatalf("refresh %d %v", code, err)
	}
	if phids, err := b.Search(&contracts.SearchQuery{}); err != nil || len(phids) != 0 {
		t.Fatalf("tombstone visible: %v %v", phids, err)
	}
	if _, err := b.ApplyProjection(ctx, projectionEvent(t, "3", false), target); err != nil {
		t.Fatal(err)
	}
	b.projectionRequest(ctx, "POST", b.baseURL(endpoint)+"/_refresh", nil)
	if phids, err := b.Search(&contracts.SearchQuery{}); err != nil || len(phids) != 1 {
		t.Fatalf("restore not visible: %v %v", phids, err)
	}
}

func TestProjectionUnknownSubmissionReplay(t *testing.T) {
	event := projectionEvent(t, "4", false)
	var committed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			if !committed.Swap(true) {
				io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
				return
			}
			w.WriteHeader(409)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"_id": event.PHID, "found": true, "_version": 4, "_source": map[string]any{"_gorge": map[string]any{"namespace": event.Namespace, "generation": "g1", "hash": event.PayloadHash, "deleted": false}}})
	}))
	defer srv.Close()
	backend := New(engine.BackendDef{Hosts: []string{srv.URL}, Index: "shadow", Version: 8, Options: map[string]string{"projection": "true"}})
	target := projection.Target{BackendID: "es", GenerationID: "g1"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := backend.ApplyProjection(ctx, event, target); err == nil {
		t.Fatal("unknown response treated as applied")
	}
	if status, err := backend.ApplyProjection(context.Background(), event, target); err != nil || status != "applied" {
		t.Fatalf("committed-but-unacknowledged replay: %s %v", status, err)
	}
}
