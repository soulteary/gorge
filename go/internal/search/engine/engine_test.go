package engine

import (
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func newTestBackend(t *testing.T, def BackendDef) *TestBackend {
	t.Helper()
	b, err := NewTestBackend(def)
	if err != nil {
		t.Fatalf("building the test backend: %v", err)
	}
	return b
}

func TestReadyNeedsAReadableBackend(t *testing.T) {
	cases := []struct {
		name     string
		backends []SearchBackend
		wantErr  string
	}{
		{"nothing configured", nil, "no search backends configured"},
		{
			"write only",
			[]SearchBackend{newTestBackend(t, BackendDef{Roles: []string{"write"}})},
			"no search backend has the read role",
		},
		{
			"readable",
			[]SearchBackend{newTestBackend(t, BackendDef{Roles: []string{"read"}})},
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := New(tc.backends).Ready()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected ready, got %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("expected %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// A write-only deployment is a legitimate configuration and a read-only one is
// too, but neither may pretend to have done the other's job. Reporting
// "indexed" for a document nothing stored is how a full reindex runs to
// completion against an empty index.
func TestWritesWithoutAWritableBackendAreAnError(t *testing.T) {
	se := New([]SearchBackend{newTestBackend(t, BackendDef{Roles: []string{"read"}})})

	if err := se.IndexDocument(&contracts.Document{PHID: "PHID-TASK-1", Type: "TASK"}); err == nil {
		t.Error("indexing with no writable backend must fail, not silently succeed")
	}
	if err := se.InitIndex([]string{"TASK"}); err == nil {
		t.Error("initialising with no writable backend must fail, not silently succeed")
	}
}

func TestWritesReachEveryWritableBackend(t *testing.T) {
	a := newTestBackend(t, BackendDef{Roles: []string{"write"}})
	b := newTestBackend(t, BackendDef{Roles: []string{"read", "write"}})
	se := New([]SearchBackend{a, b})

	doc := &contracts.Document{PHID: "PHID-TASK-1", Type: "TASK", Title: "Hello"}
	if err := se.IndexDocument(doc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both, not just the first: two write backends are how a replacement index
	// is filled beside a live one.
	for name, backend := range map[string]*TestBackend{"first": a, "second": b} {
		if _, ok := backend.Documents()["PHID-TASK-1"]; !ok {
			t.Errorf("the %s backend did not receive the document", name)
		}
	}
}

func TestAWriteFailureOnOneBackendIsReported(t *testing.T) {
	ok := newTestBackend(t, BackendDef{Roles: []string{"write"}})
	broken := newTestBackend(t, BackendDef{
		Roles:   []string{"write"},
		Options: map[string]string{"fail": "index"},
	})

	err := New([]SearchBackend{ok, broken}).IndexDocument(
		&contracts.Document{PHID: "PHID-TASK-1", Type: "TASK"})
	if err == nil {
		t.Fatal("a partially failed write must be reported: the indexes are out of step")
	}
	if _, stored := ok.Documents()["PHID-TASK-1"]; !stored {
		t.Error("the healthy backend should still have taken the document")
	}
}

func TestReadsFailOverToTheNextBackend(t *testing.T) {
	broken := newTestBackend(t, BackendDef{
		Roles:   []string{"read"},
		Options: map[string]string{"fail": "search"},
	})
	working := newTestBackend(t, BackendDef{Roles: []string{"read", "write"}})
	if err := working.IndexDocument(&contracts.Document{PHID: "PHID-TASK-1", Type: "TASK"}); err != nil {
		t.Fatal(err)
	}

	phids, err := New([]SearchBackend{broken, working}).Search(&contracts.SearchQuery{})
	if err != nil {
		t.Fatalf("expected the second backend to answer, got %v", err)
	}
	if len(phids) != 1 || phids[0] != "PHID-TASK-1" {
		t.Fatalf("unexpected results: %v", phids)
	}
}

func TestSearchReportsTheLastFailureWhenEveryBackendIsDown(t *testing.T) {
	broken := newTestBackend(t, BackendDef{
		Roles:   []string{"read"},
		Options: map[string]string{"fail": "search"},
	})

	_, err := New([]SearchBackend{broken}).Search(&contracts.SearchQuery{Query: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	// The cause has to survive the wrapping: the handler puts this string in
	// the 502 body, and "all backends failed" alone tells an operator nothing.
	if !strings.Contains(err.Error(), "injected search failure") {
		t.Errorf("the underlying failure should be reported, got %q", err)
	}
}

// Stats are the one read where a failing backend is skipped rather than
// reported, because the endpoint feeds a status panel.
func TestStatsSkipABackendThatCannotReport(t *testing.T) {
	broken := newTestBackend(t, BackendDef{
		Roles:   []string{"read"},
		Options: map[string]string{"fail": "stats"},
	})
	working := newTestBackend(t, BackendDef{Roles: []string{"read"}})

	stats, err := New([]SearchBackend{broken, working}).IndexStats()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := stats["documents"]; !ok {
		t.Errorf("expected the second backend's stats, got %v", stats)
	}

	if _, err := New([]SearchBackend{broken}).IndexStats(); err == nil {
		t.Error("expected an error when no backend can report")
	}
}

func TestIndexLifecycleThroughTheEngine(t *testing.T) {
	b := newTestBackend(t, BackendDef{Roles: []string{"read", "write"}})
	se := New([]SearchBackend{b})

	exists, err := se.IndexExists()
	if err != nil || exists {
		t.Fatalf("a fresh index must not exist: exists=%v err=%v", exists, err)
	}

	if err := se.InitIndex([]string{"TASK", "DREV"}); err != nil {
		t.Fatal(err)
	}

	exists, err = se.IndexExists()
	if err != nil || !exists {
		t.Fatalf("after init the index must exist: exists=%v err=%v", exists, err)
	}

	sane, err := se.IndexIsSane([]string{"TASK", "DREV"})
	if err != nil || !sane {
		t.Fatalf("an index just built for these types must be sane: sane=%v err=%v", sane, err)
	}

	// The point of the sanity check: an index built for fewer types than the
	// caller now needs is not sane, and that is the signal to reindex.
	sane, err = se.IndexIsSane([]string{"TASK", "DREV", "CMIT"})
	if err != nil {
		t.Fatal(err)
	}
	if sane {
		t.Error("an index missing a document type must report itself insane")
	}
}

func TestReadOperationsWithoutAReadableBackend(t *testing.T) {
	se := New([]SearchBackend{newTestBackend(t, BackendDef{Roles: []string{"write"}})})

	if _, err := se.Search(&contracts.SearchQuery{}); err == nil {
		t.Error("expected Search to fail")
	}
	if _, err := se.IndexExists(); err == nil {
		t.Error("expected IndexExists to fail")
	}
	if _, err := se.IndexIsSane([]string{"TASK"}); err == nil {
		t.Error("expected IndexIsSane to fail")
	}
	if _, err := se.IndexStats(); err == nil {
		t.Error("expected IndexStats to fail")
	}
}

func TestBackendInfoDescribesEveryBackend(t *testing.T) {
	se := New([]SearchBackend{
		newTestBackend(t, BackendDef{Roles: []string{"read"}, Index: "one"}),
		newTestBackend(t, BackendDef{Roles: []string{"write"}, Index: "two"}),
	})

	info := se.BackendInfo()
	if len(info) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(info))
	}
	for i, entry := range info {
		if entry["type"] != "test" {
			t.Errorf("entry %d: expected type test, got %v", i, entry["type"])
		}
		if _, ok := entry["roles"]; !ok {
			t.Errorf("entry %d: an entry must report its roles", i)
		}
	}
}

func TestHasBackends(t *testing.T) {
	if New(nil).HasBackends() {
		t.Error("an empty engine reports backends")
	}
	if !New([]SearchBackend{newTestBackend(t, BackendDef{})}).HasBackends() {
		t.Error("a configured engine reports none")
	}
}

// A typo in the fail option would otherwise turn a fixture that means to
// assert a 502 into one that quietly asserts a 200.
func TestUnknownFailOperationIsRejected(t *testing.T) {
	_, err := NewTestBackend(BackendDef{Options: map[string]string{"fail": "sarch"}})
	if err == nil {
		t.Fatal("expected an error for an unknown fail operation")
	}
	if !strings.Contains(err.Error(), "unknown fail operation") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestTestBackendDefaults(t *testing.T) {
	b := newTestBackend(t, BackendDef{})

	if !b.HasRole("read") || !b.HasRole("write") {
		t.Error("an empty role list must mean both roles")
	}
	if b.Info()["index"] != DefaultIndexName {
		t.Errorf("expected the default index name, got %v", b.Info()["index"])
	}
}

func TestTestBackendSearchFiltersAndPages(t *testing.T) {
	b := newTestBackend(t, BackendDef{})
	docs := []*contracts.Document{
		{PHID: "PHID-TASK-1", Type: "TASK", Title: "Fix the parser"},
		{PHID: "PHID-TASK-2", Type: "TASK", Title: "Unrelated"},
		{PHID: "PHID-DREV-1", Type: "DREV", Title: "Fix the parser again",
			Fields: []contracts.DocumentField{{Name: "body", Corpus: "parser work"}}},
	}
	for _, d := range docs {
		if err := b.IndexDocument(d); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name  string
		query contracts.SearchQuery
		want  []string
	}{
		{"no text matches everything", contracts.SearchQuery{},
			[]string{"PHID-DREV-1", "PHID-TASK-1", "PHID-TASK-2"}},
		{"text matches the title", contracts.SearchQuery{Query: "parser"},
			[]string{"PHID-DREV-1", "PHID-TASK-1"}},
		{"text matches a field corpus", contracts.SearchQuery{Query: "parser work"},
			[]string{"PHID-DREV-1"}},
		{"types narrow the answer", contracts.SearchQuery{Types: []string{"DREV"}},
			[]string{"PHID-DREV-1"}},
		{"exclude drops one", contracts.SearchQuery{Exclude: "PHID-DREV-1"},
			[]string{"PHID-TASK-1", "PHID-TASK-2"}},
		{"limit pages", contracts.SearchQuery{Limit: 2},
			[]string{"PHID-DREV-1", "PHID-TASK-1"}},
		{"offset pages", contracts.SearchQuery{Offset: 2},
			[]string{"PHID-TASK-2"}},
		{"offset past the end is empty, not an error", contracts.SearchQuery{Offset: 99},
			[]string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := b.Search(&tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("expected %v, got %v", tc.want, got)
				}
			}
		})
	}
}

// Every one of the five domain error codes has to be reachable, or the
// fixtures that assert them cannot exist. This is the inventory.
func TestEveryOperationCanBeMadeToFail(t *testing.T) {
	cases := []struct {
		fail string
		call func(*TestBackend) error
	}{
		{"index", func(b *TestBackend) error {
			return b.IndexDocument(&contracts.Document{PHID: "PHID-TASK-1", Type: "TASK"})
		}},
		{"search", func(b *TestBackend) error {
			_, err := b.Search(&contracts.SearchQuery{})
			return err
		}},
		{"init", func(b *TestBackend) error { return b.InitIndex([]string{"TASK"}) }},
		{"exists", func(b *TestBackend) error {
			_, err := b.IndexExists()
			return err
		}},
		{"sane", func(b *TestBackend) error {
			_, err := b.IndexIsSane([]string{"TASK"})
			return err
		}},
		{"stats", func(b *TestBackend) error {
			_, err := b.IndexStats()
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.fail, func(t *testing.T) {
			b := newTestBackend(t, BackendDef{Options: map[string]string{"fail": tc.fail}})
			if err := tc.call(b); err == nil {
				t.Errorf("injecting %q did not make the operation fail", tc.fail)
			}

			// The other half of the assertion: without the injection the same
			// call succeeds, so the fixture is testing the injection rather
			// than a call that never worked.
			if err := tc.call(newTestBackend(t, BackendDef{})); err != nil {
				t.Errorf("the same call fails without injection: %v", err)
			}
		})
	}
}

// init drops the index; a fixture must not be able to assert that documents
// live through one.
func TestInitClearsTheIndex(t *testing.T) {
	b := newTestBackend(t, BackendDef{})
	if err := b.IndexDocument(&contracts.Document{PHID: "PHID-TASK-1", Type: "TASK"}); err != nil {
		t.Fatal(err)
	}
	if err := b.InitIndex([]string{"TASK"}); err != nil {
		t.Fatal(err)
	}
	if len(b.Documents()) != 0 {
		t.Error("init must leave the index empty")
	}
}
