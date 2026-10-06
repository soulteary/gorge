package meilisearch

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestRealMeilisearchTaskCompletionIntegration(t *testing.T) {
	url := os.Getenv("GORGE_TEST_MEILI_URL")
	if url == "" {
		t.Skip("set GORGE_TEST_MEILI_URL for real Meilisearch")
	}
	def := hostFor(url)
	def.Index = fmt.Sprintf("gorge_search_test_%d", time.Now().UnixNano())
	def.APIKey = os.Getenv("GORGE_TEST_MEILI_KEY")
	b := newBackend(def)
	defer func() { _, _ = b.doRequest(b.host+"/indexes/"+b.index, "DELETE", nil) }()
	if err := b.InitIndex([]string{"TASK"}); err != nil {
		t.Fatal(err)
	}
	doc := &contracts.Document{PHID: "PHID-TASK-integration", Type: "TASK", Title: "oldmarker", Fields: []contracts.DocumentField{{Name: "body", Corpus: "obsoletecorpus"}}}
	uid, err := b.SubmitDocument(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = b.WaitTask(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if state, err := b.TaskState(ctx, uid); err != nil || state != "succeeded" {
		t.Fatalf("state %s: %v", state, err)
	}
	found, err := b.Search(&contracts.SearchQuery{Query: "obsoletecorpus"})
	if err != nil || len(found) != 1 {
		t.Fatalf("before replacement: %v %v", found, err)
	}
	doc.Title = "newmarker"
	doc.Fields = nil
	if err = b.IndexDocument(doc); err != nil {
		t.Fatal(err)
	}
	found, err = b.Search(&contracts.SearchQuery{Query: "obsoletecorpus"})
	if err != nil || len(found) != 0 {
		t.Fatalf("removed attribute survived: %v %v", found, err)
	}
}
