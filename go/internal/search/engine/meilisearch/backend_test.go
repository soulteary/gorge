package meilisearch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/search/engine"
	"github.com/soulteary/gorge/go/internal/search/esquery"
)

func newBackend(def engine.BackendDef) *Backend { return New(def) }

// hostFor turns an httptest server URL into a def whose single host carries the
// scheme, which New keeps verbatim (the "://" branch), so the backend talks to
// the stub without any production change.
func hostFor(url string) engine.BackendDef {
	return engine.BackendDef{Hosts: []string{url}}
}

// --- defaults --------------------------------------------------------------

func TestDefaultsAreApplied(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})

	if b.index != engine.DefaultIndexName {
		t.Errorf("expected the default index name, got %q", b.index)
	}
	if b.timeout != defaultTimeoutSec {
		t.Errorf("expected the default timeout %d, got %d", defaultTimeoutSec, b.timeout)
	}
	if !b.HasRole("read") || !b.HasRole("write") {
		t.Error("an empty role list must mean both roles")
	}
	if b.Type() != "meilisearch" {
		t.Errorf("Type() = %q", b.Type())
	}
}

// A host without a scheme is prefixed with http:// (or the given protocol), and
// a trailing slash is trimmed so the URLs this backend builds stay well-formed.
func TestHostGetsASchemeAndLosesItsTrailingSlash(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700/"}})
	if b.host != "http://meili:7700" {
		t.Errorf("expected http:// prefix and no trailing slash, got %q", b.host)
	}

	withProto := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}, Protocol: "https"})
	if withProto.host != "https://meili:7700" {
		t.Errorf("expected the configured protocol, got %q", withProto.host)
	}
}

// A host that already carries a scheme is taken as written, which is what lets
// a test point it straight at an httptest.Server.
func TestHostWithASchemeIsTakenAsWritten(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"https://search.example.com/prefix"}})
	if b.host != "https://search.example.com/prefix" {
		t.Errorf("unexpected host: %q", b.host)
	}
}

func TestExplicitTimeoutAndRolesAndIndexAreKept(t *testing.T) {
	b := newBackend(engine.BackendDef{
		Hosts:   []string{"meili:7700"},
		Index:   "custom",
		Timeout: 42,
		Roles:   []string{"read"},
	})
	if b.index != "custom" {
		t.Errorf("index = %q", b.index)
	}
	if b.timeout != 42 {
		t.Errorf("timeout = %d", b.timeout)
	}
	if !b.HasRole("read") || b.HasRole("write") {
		t.Error("an explicit role list must be honoured verbatim")
	}
}

// --- Info ------------------------------------------------------------------

// Info reports type/host/index/roles and, most importantly, never the API key:
// this listing is served to clients and a leaked credential here is a leaked
// credential everywhere.
func TestInfoReportsTheExpectedKeys(t *testing.T) {
	info := newBackend(engine.BackendDef{
		Hosts:  []string{"meili:7700"},
		Index:  "idx",
		Roles:  []string{"read", "write"},
		APIKey: "super-secret",
	}).Info()

	for _, key := range []string{"type", "host", "index", "roles"} {
		if _, ok := info[key]; !ok {
			t.Errorf("Info() is missing %q", key)
		}
	}
	if info["type"] != "meilisearch" {
		t.Errorf("type = %v", info["type"])
	}
	if info["index"] != "idx" {
		t.Errorf("index = %v", info["index"])
	}
	if roles, ok := info["roles"].([]string); !ok || !reflect.DeepEqual(roles, []string{"read", "write"}) {
		t.Errorf("roles = %v", info["roles"])
	}
}

func TestInfoNeverCarriesCredentials(t *testing.T) {
	info := newBackend(engine.BackendDef{
		Hosts:  []string{"meili:7700"},
		APIKey: "super-secret",
	}).Info()

	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret") {
		t.Errorf("the backend listing leaked a credential: %s", raw)
	}
	if _, ok := info["apiKey"]; ok {
		t.Error("Info() must not carry an apiKey key at all")
	}
}

// --- buildDocument ---------------------------------------------------------

// buildDocument flattens fields and relationships into top-level attributes.
// Header fields are always present; a repeated field name accumulates into a
// list; Aux joins its field either as a trailing space-joined string (first
// occurrence) or as an extra list element (later occurrences); and a
// relationship always lands as a list.
func TestBuildDocumentFlattensFieldsAndRelationships(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})

	doc := b.buildDocument(&contracts.Document{
		PHID:         "PHID-TASK-1",
		Type:         "TASK",
		Title:        "Fix it",
		DateCreated:  100,
		DateModified: 200,
		Fields: []contracts.DocumentField{
			{Name: esquery.FieldBody, Corpus: "first", Aux: "aux1"},
			{Name: esquery.FieldBody, Corpus: "second"},
			{Name: esquery.FieldComment, Corpus: "solo"},
		},
		Relationships: []contracts.DocumentRelation{
			{Name: esquery.RelAuthor, RelatedPHID: "PHID-USER-1"},
			{Name: esquery.RelProject, RelatedPHID: "PHID-PROJ-1"},
			{Name: esquery.RelProject, RelatedPHID: "PHID-PROJ-2"},
		},
	})

	if doc["id"] != "PHID-TASK-1" || doc["docType"] != "TASK" || doc["title"] != "Fix it" {
		t.Errorf("unexpected header fields: %v", doc)
	}
	if doc["dateCreated"] != int64(100) || doc["lastModified"] != int64(200) {
		t.Errorf("unexpected timestamps: created=%v modified=%v", doc["dateCreated"], doc["lastModified"])
	}

	// The first occurrence carries Aux joined by a space; the second appends a
	// bare corpus, turning the value into a list.
	if got := doc[esquery.FieldBody]; !reflect.DeepEqual(got, []any{"first aux1", "second"}) {
		t.Errorf("expected the body corpora accumulated, got %v", got)
	}
	// A single field stays scalar.
	if got := doc[esquery.FieldComment]; got != "solo" {
		t.Errorf("a single field must stay scalar, got %v", got)
	}
	// A single relationship is still a list.
	if got := doc[esquery.RelAuthor]; !reflect.DeepEqual(got, []any{"PHID-USER-1"}) {
		t.Errorf("unexpected author relationship: %v", got)
	}
	// Repeated relationships accumulate into that list.
	if got := doc[esquery.RelProject]; !reflect.DeepEqual(got, []any{"PHID-PROJ-1", "PHID-PROJ-2"}) {
		t.Errorf("unexpected project relationship: %v", got)
	}
}

// The second Aux path: once a field name already holds a list, a later Aux is
// appended as its own element rather than space-joined.
func TestBuildDocumentAppendsAuxToAnExistingList(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})

	doc := b.buildDocument(&contracts.Document{
		PHID: "PHID-TASK-1",
		Type: "TASK",
		Fields: []contracts.DocumentField{
			{Name: esquery.FieldBody, Corpus: "first"},
			{Name: esquery.FieldBody, Corpus: "second", Aux: "aux2"},
		},
	})

	if got := doc[esquery.FieldBody]; !reflect.DeepEqual(got, []any{"first", "second", "aux2"}) {
		t.Errorf("expected aux appended as its own element, got %v", got)
	}
}

// --- buildSearchRequest ----------------------------------------------------

// An empty query gets the default limit and a date sort; the retrieved
// attributes are pinned to id, since only ids leave this service.
func TestBuildSearchRequestDefaults(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})
	req := b.buildSearchRequest(&contracts.SearchQuery{})

	if req["q"] != "" {
		t.Errorf("q = %v", req["q"])
	}
	if req["limit"] != defaultResultLimit {
		t.Errorf("expected the default limit %d, got %v", defaultResultLimit, req["limit"])
	}
	if req["offset"] != 0 {
		t.Errorf("offset = %v", req["offset"])
	}
	if got, ok := req["attributesToRetrieve"].([]string); !ok || !reflect.DeepEqual(got, []string{"id"}) {
		t.Errorf("attributesToRetrieve = %v", req["attributesToRetrieve"])
	}
	// An empty query has no relevance to rank by, so it sorts by date.
	if got, ok := req["sort"].([]string); !ok || !reflect.DeepEqual(got, []string{"dateCreated:desc"}) {
		t.Errorf("expected a date sort for an empty query, got %v", req["sort"])
	}
	// No constraints means no filter key at all.
	if _, ok := req["filter"]; ok {
		t.Error("an unconstrained query must carry no filter")
	}
}

// A text query keeps the caller's limit/offset and drops the date sort so the
// engine ranks by relevance.
func TestBuildSearchRequestWithTextKeepsPagingAndDropsSort(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})
	req := b.buildSearchRequest(&contracts.SearchQuery{Query: "parser", Limit: 25, Offset: 50})

	if req["q"] != "parser" {
		t.Errorf("q = %v", req["q"])
	}
	if req["limit"] != 25 || req["offset"] != 50 {
		t.Errorf("expected limit=25 offset=50, got limit=%v offset=%v", req["limit"], req["offset"])
	}
	if _, ok := req["sort"]; ok {
		t.Error("a text query must rank by relevance, not by date")
	}
}

// --- buildFilters ----------------------------------------------------------

// A single document type renders as one string entry (an AND term); several
// render as one nested list (an OR).
func TestBuildFiltersDocTypeSingleVersusMany(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})

	single := b.buildFilters(&contracts.SearchQuery{Types: []string{"TASK"}})
	if len(single) != 1 || single[0] != "docType = TASK" {
		t.Errorf("a single type must be one AND term, got %v", single)
	}

	many := b.buildFilters(&contracts.SearchQuery{Types: []string{"TASK", "DREV"}})
	if len(many) != 1 {
		t.Fatalf("several types must collapse into one nested OR, got %v", many)
	}
	nested, ok := many[0].([]string)
	if !ok || !reflect.DeepEqual(nested, []string{"docType = TASK", "docType = DREV"}) {
		t.Errorf("unexpected nested type filter: %v", many[0])
	}
}

func TestBuildFiltersExclude(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})
	filters := b.buildFilters(&contracts.SearchQuery{Exclude: "PHID-TASK-9"})
	if len(filters) != 1 || filters[0] != "id != PHID-TASK-9" {
		t.Errorf("unexpected exclude filter: %v", filters)
	}
}

// The relationship map is rendered in sorted key order so one query always
// renders to the same filter list.
func TestBuildFiltersRelationships(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})
	filters := b.buildFilters(&contracts.SearchQuery{
		AuthorPHIDs:     []string{"PHID-USER-1"},
		SubscriberPHIDs: []string{"PHID-USER-2"},
		ProjectPHIDs:    []string{"PHID-PROJ-1"},
		RepositoryPHIDs: []string{"PHID-REPO-1"},
	})

	want := map[string]string{
		esquery.RelAuthor:     "PHID-USER-1",
		esquery.RelSubscriber: "PHID-USER-2",
		esquery.RelProject:    "PHID-PROJ-1",
		esquery.RelRepository: "PHID-REPO-1",
	}
	got := make(map[string]bool)
	for _, f := range filters {
		got[f.(string)] = true
	}
	for field, phid := range want {
		if !got[field+" = "+phid] {
			t.Errorf("expected a %q filter, got %v", field+" = "+phid, filters)
		}
	}
}

// open-only and closed-only render as an EXISTS marker; asking for both, or
// neither, renders nothing — the two markers are on the document rather than a
// status field.
func TestBuildFiltersStatus(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})

	cases := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"open only", []string{esquery.RelOpen}, esquery.RelOpen + " EXISTS"},
		{"closed only", []string{esquery.RelClosed}, esquery.RelClosed + " EXISTS"},
		{"both", []string{esquery.RelOpen, esquery.RelClosed}, ""},
		{"neither", nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filters := b.buildFilters(&contracts.SearchQuery{Statuses: tc.statuses})
			var found []string
			for _, f := range filters {
				found = append(found, f.(string))
			}
			if tc.want == "" {
				if len(found) != 0 {
					t.Errorf("expected no status filter, got %v", found)
				}
				return
			}
			if len(found) != 1 || found[0] != tc.want {
				t.Errorf("expected %q, got %v", tc.want, found)
			}
		})
	}
}

func TestBuildFiltersWithUnowned(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})
	filters := b.buildFilters(&contracts.SearchQuery{WithUnowned: true})
	if len(filters) != 1 || filters[0] != esquery.RelUnowned+" EXISTS" {
		t.Errorf("unexpected unowned filter: %v", filters)
	}
}

// WithAnyOwner is an EXISTS marker and overrides an owner list rather than
// intersecting with it, matching the PHP query builder.
func TestBuildFiltersAnyOwnerOverridesOwnerList(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"meili:7700"}})

	anyOwner := b.buildFilters(&contracts.SearchQuery{
		WithAnyOwner: true,
		OwnerPHIDs:   []string{"PHID-USER-1"},
	})
	if len(anyOwner) != 1 || anyOwner[0] != esquery.RelOwner+" EXISTS" {
		t.Errorf("withAnyOwner must render one EXISTS and drop the owner list, got %v", anyOwner)
	}

	owned := b.buildFilters(&contracts.SearchQuery{OwnerPHIDs: []string{"PHID-USER-1", "PHID-USER-2"}})
	want := []any{
		esquery.RelOwner + " = PHID-USER-1",
		esquery.RelOwner + " = PHID-USER-2",
	}
	if !reflect.DeepEqual(owned, want) {
		t.Errorf("unexpected owner filters: %v", owned)
	}
}

// --- HTTP layer ------------------------------------------------------------

// IndexDocument POSTs one document array to /indexes/{index}/documents. The
// Authorization header carries the API key when one is set.
func TestIndexDocumentRoundTrip(t *testing.T) {
	var (
		gotPath   string
		gotMethod string
		gotAuth   string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"taskUid":1}`))
	}))
	defer srv.Close()

	def := hostFor(srv.URL)
	def.APIKey = "k3y"
	b := newBackend(def)

	err := b.IndexDocument(&contracts.Document{PHID: "PHID-TASK-1", Type: "TASK", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s", gotMethod)
	}
	if gotPath != "/indexes/"+engine.DefaultIndexName+"/documents" {
		t.Errorf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer k3y" {
		t.Errorf("expected the API key on the wire, got %q", gotAuth)
	}
	// The body is a one-element array of documents.
	var docs []map[string]any
	if err := json.Unmarshal(gotBody, &docs); err != nil {
		t.Fatalf("body is not a document array: %v (%s)", err, gotBody)
	}
	if len(docs) != 1 || docs[0]["id"] != "PHID-TASK-1" {
		t.Errorf("unexpected document body: %s", gotBody)
	}
}

// Search POSTs to /indexes/{index}/search and reads the ids out of hits[].id.
func TestSearchRoundTrip(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_, _ = w.Write([]byte(`{"hits":[{"id":"PHID-TASK-1"},{"id":"PHID-TASK-2"},{"nope":true}]}`))
	}))
	defer srv.Close()

	b := newBackend(hostFor(srv.URL))
	phids, err := b.Search(&contracts.SearchQuery{Query: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s", gotMethod)
	}
	if gotPath != "/indexes/"+engine.DefaultIndexName+"/search" {
		t.Errorf("unexpected search path: %s", gotPath)
	}
	// A hit with no string id is skipped rather than turned into an empty PHID.
	if !reflect.DeepEqual(phids, []string{"PHID-TASK-1", "PHID-TASK-2"}) {
		t.Errorf("unexpected results: %v", phids)
	}
}

func TestSearchRejectsInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	b := newBackend(hostFor(srv.URL))
	if _, err := b.Search(&contracts.SearchQuery{}); err == nil {
		t.Error("expected an error on malformed JSON")
	}
}

// IndexExists maps a 200 to true and a 404 to (false, nil): a missing index is
// a normal answer, not a failure.
func TestIndexExists(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantExists bool
		wantErr    bool
	}{
		{"present", http.StatusOK, true, false},
		{"absent", http.StatusNotFound, false, false},
		{"server error", http.StatusInternalServerError, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			b := newBackend(hostFor(srv.URL))
			exists, err := b.IndexExists()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if exists != tc.wantExists {
				t.Errorf("exists = %v, want %v", exists, tc.wantExists)
			}
		})
	}
}

// IndexStats maps Meilisearch's two keys onto the contract's documents/indexing
// pair. It reports two where Elasticsearch reports four, and that is on the wire.
func TestIndexStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/indexes/"+engine.DefaultIndexName+"/stats" {
			t.Errorf("unexpected stats path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"numberOfDocuments":42,"isIndexing":true}`))
	}))
	defer srv.Close()

	b := newBackend(hostFor(srv.URL))
	stats, err := b.IndexStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats["documents"] != 42 {
		t.Errorf("documents = %v, want 42", stats["documents"])
	}
	if stats["indexing"] != true {
		t.Errorf("indexing = %v, want true", stats["indexing"])
	}
}

func TestIndexStatsRejectsInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{`))
	}))
	defer srv.Close()

	b := newBackend(hostFor(srv.URL))
	if _, err := b.IndexStats(); err == nil {
		t.Error("expected an error on malformed stats JSON")
	}
}

// IndexIsSane compares the live settings against what configureIndex writes:
// the searchable list has to be the same length and every expected filterable
// attribute has to be present. The check is containment, not equality, on the
// filterable side.
func TestIndexIsSane(t *testing.T) {
	b := newBackend(hostFor("http://placeholder")) // only used for the expected lists

	buildSettings := func(searchable, filterable []string) []byte {
		raw, err := json.Marshal(msSettings{
			SearchableAttributes: searchable,
			FilterableAttributes: filterable,
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	expectedSearchable := b.searchableAttributes()
	expectedFilterable := b.filterableAttributes()

	cases := []struct {
		name       string
		searchable []string
		filterable []string
		want       bool
	}{
		{
			name:       "matching settings are sane",
			searchable: expectedSearchable,
			filterable: expectedFilterable,
			want:       true,
		},
		{
			name:       "extra filterable attributes are tolerated",
			searchable: expectedSearchable,
			filterable: append(append([]string{}, expectedFilterable...), "extraAttr"),
			want:       true,
		},
		{
			name:       "a shorter searchable list is insane",
			searchable: expectedSearchable[:len(expectedSearchable)-1],
			filterable: expectedFilterable,
			want:       false,
		},
		{
			name:       "a missing filterable attribute is insane",
			searchable: expectedSearchable,
			filterable: expectedFilterable[:len(expectedFilterable)-1],
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := buildSettings(tc.searchable, tc.filterable)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// IndexIsSane first checks existence (GET /indexes/{index}),
				// then reads the settings (GET .../settings). Both are GETs on
				// this stub; the settings path returns the settings body and the
				// bare index path returns 200 so the index looks present.
				if strings.HasSuffix(r.URL.Path, "/settings") {
					_, _ = w.Write(settings)
					return
				}
				_, _ = w.Write([]byte(`{"uid":"gorge"}`))
			}))
			defer srv.Close()

			sb := newBackend(hostFor(srv.URL))
			sane, err := sb.IndexIsSane(nil)
			if err != nil {
				t.Fatal(err)
			}
			if sane != tc.want {
				t.Errorf("IndexIsSane = %v, want %v", sane, tc.want)
			}
		})
	}
}

// A missing index is not sane, and it is not an error either: IndexExists
// answers (false, nil) and IndexIsSane relays it.
func TestIndexIsSaneOnMissingIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	b := newBackend(hostFor(srv.URL))
	sane, err := b.IndexIsSane(nil)
	if err != nil || sane {
		t.Errorf("a missing index must be (false, nil), got sane=%v err=%v", sane, err)
	}
}

// doRequest turns a >= 400 status into an error whose text carries the status
// code, which is exactly what IndexExists keys its 404 branch off. A wrong
// method that yields a non-error status still returns the body.
func TestDoRequestErrorCarriesStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"bad_request"}`))
	}))
	defer srv.Close()

	b := newBackend(hostFor(srv.URL))
	_, err := b.doRequest(srv.URL, http.MethodGet, nil)
	if err == nil {
		t.Fatal("expected an error for a 400")
	}
	if !strings.Contains(err.Error(), "status 400") {
		t.Errorf("the error must name the status code, got %v", err)
	}
}

func TestDoRequestReturnsBodyOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	b := newBackend(hostFor(srv.URL))
	body, err := b.doRequest(srv.URL, http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("unexpected body: %s", body)
	}
}

// InitIndex drops the index, recreates it, waits for the task queue to drain
// and then writes the settings. The delete's 404 on a first run is normal, and
// waitForIdle returns as soon as the tasks endpoint reports an empty queue, so
// the whole sequence completes without a real Meilisearch.
func TestInitIndexDropsCreatesAndConfigures(t *testing.T) {
	var (
		sawDelete   bool
		sawCreate   bool
		sawSettings bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			// A missing index on a first init makes this a 404, which InitIndex
			// swallows.
			sawDelete = true
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/indexes":
			sawCreate = true
			_, _ = w.Write([]byte(`{"taskUid":1}`))
		case strings.HasPrefix(r.URL.Path, "/tasks"):
			// An empty queue lets waitForIdle return immediately.
			_, _ = w.Write([]byte(`{"total":0}`))
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/settings"):
			sawSettings = true
			_, _ = w.Write([]byte(`{"taskUid":2}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	b := newBackend(hostFor(srv.URL))
	if err := b.InitIndex([]string{"TASK"}); err != nil {
		t.Fatalf("InitIndex: %v", err)
	}
	if !sawDelete || !sawCreate || !sawSettings {
		t.Errorf("expected delete/create/settings, got delete=%v create=%v settings=%v",
			sawDelete, sawCreate, sawSettings)
	}
}

// The three attribute lists are configureIndex's inputs. sortableAttributes is
// the one with no other caller, so it is asserted directly.
func TestSortableAttributes(t *testing.T) {
	b := newBackend(hostFor("http://placeholder"))
	if got := b.sortableAttributes(); !reflect.DeepEqual(got, []string{"dateCreated", "lastModified"}) {
		t.Errorf("unexpected sortable attributes: %v", got)
	}
}
