package projection

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func goldenEvent(t *testing.T) *contracts.SearchProjection {
	t.Helper()
	doc := new(contracts.Document)
	if err := json.Unmarshal([]byte(`{"phid": "PHID-TASK-golden", "type": "TASK", "title": "中文 <>&/", "dateCreated": 11, "dateModified": 12, "fields": [{"name": "titl", "corpus": "中文 <>&/"}, {"name": "cmnt", "corpus": "first"}, {"name": "cmnt", "corpus": "second", "aux": "PHID-XACT-golden"}], "relationships": [{"name": "auth", "relatedPHID": "PHID-USER-golden", "rtype": "USER"}]}`), doc); err != nil {
		t.Fatal(err)
	}
	hash, err := DocumentHash(doc)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "34b08b99566fb782a8542e593cf02b28d748b640ca7254988cde7b7aaf6ac552" {
		t.Fatalf("PHP/Go golden hash: %s", hash)
	}
	return &contracts.SearchProjection{ProjectionVersion: 1, EventID: "search/golden/42", Namespace: "default", PHID: doc.PHID, Type: doc.Type, Revision: "42", Operation: "upsert", SerializerVersion: "test-v1", SourceVersion: "opaque", PayloadHash: hash, Document: doc}
}

func TestGoldenProjectionAndTombstone(t *testing.T) {
	event := goldenEvent(t)
	if rev, err := Validate(event); err != nil || rev != 42 {
		t.Fatalf("%d %v", rev, err)
	}
	event.Operation = "delete"
	event.Document = nil
	hash, err := PayloadHash(event)
	if err != nil {
		t.Fatal(err)
	}
	event.PayloadHash = hash
	if _, err := Validate(event); err != nil {
		t.Fatal(err)
	}
	event.Document = &contracts.Document{}
	if _, err := Validate(event); err == nil {
		t.Fatal("delete with body accepted")
	}
}

func TestRejectUnsafeProjections(t *testing.T) {
	mutations := []func(*contracts.SearchProjection){
		func(e *contracts.SearchProjection) { e.Revision = "0" },
		func(e *contracts.SearchProjection) { e.Revision = "01" },
		func(e *contracts.SearchProjection) { e.Revision = "+1" },
		func(e *contracts.SearchProjection) { e.Revision = "9223372036854775808" },
		func(e *contracts.SearchProjection) { e.Namespace = "../unsafe" },
		func(e *contracts.SearchProjection) { e.Type = "USER" },
		func(e *contracts.SearchProjection) { e.ProjectionVersion = 2 },
		func(e *contracts.SearchProjection) { e.PayloadHash = strings.Repeat("0", 64) },
		func(e *contracts.SearchProjection) { e.Document.PHID = "PHID-TASK-other" },
		func(e *contracts.SearchProjection) { e.Operation = "missing" },
	}
	for i, mutate := range mutations {
		e := goldenEvent(t)
		mutate(e)
		if _, err := Validate(e); err == nil {
			t.Fatalf("mutation %d accepted", i)
		}
	}
	e := goldenEvent(t)
	raw, _ := json.Marshal(e)
	if _, err := Decode(raw); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(string(raw), `"operation":`, `"operation":"upsert","operation":`, 1),
		strings.Replace(string(raw), `"operation":`, `"Operation":"upsert","operation":`, 1), string(raw) + ` {}`, strings.Replace(string(raw), `"operation":`, `"unsupported":1,"operation":`, 1)} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("accepted: %s", bad)
		}
	}
	badDoc := goldenEvent(t).Document
	badDoc.Title = string([]byte{0xff})
	if _, err := DocumentHash(badDoc); err == nil {
		t.Fatal("invalid UTF-8 replaced silently")
	}
	if _, err := Decode([]byte{0xff}); err == nil {
		t.Fatal("invalid UTF-8 JSON accepted")
	}

}
