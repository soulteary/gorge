package elasticsearch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/search/engine"
	"github.com/soulteary/gorge/go/internal/search/esquery"
)

func newBackend(def engine.BackendDef) *Backend { return New(def) }

// dig walks a nested map, failing the test rather than panicking on a wrong
// turn. Most of the assertions below are about deep positions in the index
// configuration, and a nil map assertion three levels down is unreadable.
func dig(t *testing.T, m map[string]any, keys ...string) any {
	t.Helper()
	var cur any = m
	for i, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s is not a map", strings.Join(keys[:i], "."))
		}
		cur, ok = mm[k]
		if !ok {
			t.Fatalf("%s is missing", strings.Join(keys[:i+1], "."))
		}
	}
	return cur
}

func TestDefaultsAreApplied(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}})

	if b.index != engine.DefaultIndexName {
		t.Errorf("expected the default index name, got %q", b.index)
	}
	if b.version != defaultVersion {
		t.Errorf("expected version %d, got %d", defaultVersion, b.version)
	}
	if !b.HasRole("read") || !b.HasRole("write") {
		t.Error("an empty role list must mean both roles")
	}
}

// A slash would escape the path this backend builds its URLs from, turning an
// index name into an extra path segment.
func TestIndexNameLosesItsSlashes(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}, Index: "some/index"})

	if b.index != "someindex" {
		t.Errorf("expected the slashes stripped, got %q", b.index)
	}
	if got := b.baseURL("es:9200"); got != "http://es:9200/someindex" {
		t.Errorf("unexpected base URL: %s", got)
	}
}

func TestHostWithASchemeIsTakenAsWritten(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"https://search.example.com/prefix/"}})

	if got := b.baseURL("https://search.example.com/prefix/"); got != "https://search.example.com/prefix/phabricator" {
		t.Errorf("unexpected base URL: %s", got)
	}
}

// The version selects the field type and the timestamp field, so the whole
// mapping hangs off it.
func TestVersionSelectsTheFieldVocabulary(t *testing.T) {
	cases := []struct {
		version   int
		textType  string
		timestamp string
	}{
		{1, "string", "_timestamp"},
		{2, "string", "lastModified"},
		{5, "text", "lastModified"},
		{7, "text", "lastModified"},
	}

	for _, tc := range cases {
		b := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}, Version: tc.version})
		if got := b.textFieldType(); got != tc.textType {
			t.Errorf("version %d: expected text type %q, got %q", tc.version, tc.textType, got)
		}
		if got := b.timestampField(); got != tc.timestamp {
			t.Errorf("version %d: expected timestamp field %q, got %q", tc.version, tc.timestamp, got)
		}
	}
}

// --- CJK -------------------------------------------------------------------
//
// The CJK chain is the one part of this mapping that is not Phorge's. Its
// failure mode is silence: strip it and every endpoint keeps answering 200
// while Chinese queries quietly stop finding anything. These four tests are
// the only thing standing in the way. See compat/phorge/README.md section 七.

func TestCJKAnalyzerUsesOnlyBuiltInFilters(t *testing.T) {
	cfg := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).buildIndexConfig([]string{"TASK"})

	analyzer := dig(t, cfg, "settings", "index", "analysis", "analyzer", analyzerCJKText).(map[string]any)
	filters, ok := analyzer["filter"].([]string)
	if !ok {
		t.Fatalf("the cjk analyser's filter list is %T, not a string list", analyzer["filter"])
	}

	// cjk_width and cjk_bigram both ship with Elasticsearch. Naming anything
	// else here reintroduces a plugin dependency (analysis-icu, smartcn) that
	// this chain was chosen to avoid, and the index then fails to create at
	// all on a stock cluster.
	want := []string{"cjk_width", "lowercase", filterCJKBigram}
	if !reflect.DeepEqual(filters, want) {
		t.Errorf("expected the filter chain %v, got %v", want, filters)
	}
	if analyzer["tokenizer"] != "standard" {
		t.Errorf("expected the standard tokenizer, got %v", analyzer["tokenizer"])
	}
}

// output_unigrams keeps the single characters beside the bigrams. Without it a
// one-character query finds nothing in a document that plainly contains it,
// and nothing reports an error.
func TestCJKBigramFilterKeepsUnigrams(t *testing.T) {
	cfg := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).buildIndexConfig([]string{"TASK"})

	filter := dig(t, cfg, "settings", "index", "analysis", "filter", filterCJKBigram).(map[string]any)
	if filter["type"] != "cjk_bigram" {
		t.Errorf("expected the cjk_bigram filter, got %v", filter["type"])
	}
	if filter["output_unigrams"] != true {
		t.Error("output_unigrams must stay on, or single-character queries match nothing")
	}
}

func TestEveryCorpusFieldCarriesTheCJKSubfield(t *testing.T) {
	cfg := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).buildIndexConfig([]string{"TASK", "DREV"})

	for _, docType := range []string{"TASK", "DREV"} {
		for _, field := range esquery.AllFields() {
			sub := dig(t, cfg, "mappings", docType, "properties", field,
				"fields", esquery.SubfieldCJK).(map[string]any)
			if sub["analyzer"] != analyzerCJKText {
				t.Errorf("%s.%s.%s: expected the cjk analyser, got %v",
					docType, field, esquery.SubfieldCJK, sub["analyzer"])
			}
		}
	}
}

// The three English subfields have to survive alongside it: raw, keywords and
// stems are Phorge's and its own engine's sanity check compares against them.
func TestTheEnglishSubfieldsSurvive(t *testing.T) {
	cfg := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).buildIndexConfig([]string{"TASK"})

	fields := dig(t, cfg, "mappings", "TASK", "properties", esquery.FieldTitle, "fields").(map[string]any)
	for _, name := range []string{"raw", "keywords", "stems", esquery.SubfieldCJK} {
		if _, ok := fields[name]; !ok {
			t.Errorf("the %q subfield is missing", name)
		}
	}
	if len(fields) != 4 {
		t.Errorf("expected exactly four subfields, got %d: %v", len(fields), fields)
	}
}

// The CJK query clause is separate from the English one because the analyser
// is a property of the clause. Folding the cjk subfields into the english_exact
// clause scores a bigram-indexed field almost at random.
func TestTheQueryCarriesACJKClauseOfItsOwn(t *testing.T) {
	spec := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).
		buildSearchSpec(&contracts.SearchQuery{Query: "搜索"})

	bq := spec["query"].(map[string]any)["bool"].(*esquery.BoolQuery)
	if len(bq.Should) != 2 {
		t.Fatalf("expected an English and a CJK should clause, got %d", len(bq.Should))
	}

	cjk := bq.Should[1].(map[string]any)["simple_query_string"].(map[string]any)
	if cjk["analyzer"] != analyzerCJKText {
		t.Errorf("expected the cjk analyser on the second clause, got %v", cjk["analyzer"])
	}
	for _, want := range []string{
		esquery.FieldTitle + ".cjk^4",
		esquery.FieldBody + ".cjk^3",
		esquery.FieldComment + ".cjk^1.2",
	} {
		found := false
		for _, f := range cjk["fields"].([]string) {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the cjk clause does not search %q", want)
		}
	}

	// The must clause reaches the subfield through its wildcard, which is what
	// makes a CJK query match at all rather than merely rank well.
	must := bq.Must[0].(map[string]any)["simple_query_string"].(map[string]any)
	if !reflect.DeepEqual(must["fields"], []string{"titl.*", "body.*", "cmnt.*"}) {
		t.Errorf("the must clause must keep its wildcards, got %v", must["fields"])
	}
}

// --- index sanity ----------------------------------------------------------

// Adding the CJK subfield means every index built before it reports itself
// insane. That is the designed migration signal, and this test is what says so
// out loud: it will fail if someone weakens configDeepMatch to make the
// warning go away.
func TestAnIndexWithoutTheCJKSubfieldIsNotSane(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}})
	expected := b.buildIndexConfig([]string{"TASK"})

	// Round-trip through JSON, because that is how a live configuration
	// arrives: typed Go values become strings and float64s on the way back.
	actual := roundTrip(t, expected)

	if !configDeepMatch(actual, expected) {
		t.Fatal("an unmodified configuration must match itself")
	}

	fields := dig(t, actual, "mappings", "TASK", "properties", esquery.FieldTitle, "fields").(map[string]any)
	delete(fields, esquery.SubfieldCJK)

	if configDeepMatch(actual, expected) {
		t.Error("an index without the cjk subfield must report itself insane")
	}
}

func TestAnIndexWithoutTheCJKAnalyzerIsNotSane(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}})
	expected := b.buildIndexConfig([]string{"TASK"})
	actual := roundTrip(t, expected)

	analyzers := dig(t, actual, "settings", "index", "analysis", "analyzer").(map[string]any)
	delete(analyzers, analyzerCJKText)

	if configDeepMatch(actual, expected) {
		t.Error("an index without the cjk analyser must report itself insane")
	}
}

// The check is containment, not equality: Elasticsearch echoes back a great
// many defaults nobody asked for, and demanding equality would report every
// healthy index as insane.
func TestExtraSettingsDoNotBreakSanity(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}})
	expected := b.buildIndexConfig([]string{"TASK"})
	actual := roundTrip(t, expected)

	index := dig(t, actual, "settings", "index").(map[string]any)
	index["number_of_shards"] = "1"
	index["creation_date"] = "1700000000000"

	if !configDeepMatch(actual, expected) {
		t.Error("settings the cluster added on its own must not make an index insane")
	}
}

// _all is exempt because clusters that no longer support it drop it from what
// they return.
func TestMissingAllFieldIsExempt(t *testing.T) {
	if !configDeepMatch(map[string]any{}, map[string]any{"_all": map[string]any{"enabled": false}}) {
		t.Error("_all must stay exempt")
	}
}

func TestConfigValuesCompareAsText(t *testing.T) {
	// The cluster returns booleans and numbers as strings; comparing them any
	// other way reports a mismatch that is not one.
	actual := map[string]any{"a": "true", "b": "2", "c": "0-2"}
	expected := map[string]any{"a": true, "b": float64(2), "c": "0-2"}

	if !configDeepMatch(actual, expected) {
		t.Error("stringified settings must compare equal to their typed originals")
	}
	if configDeepMatch(map[string]any{"a": "false"}, map[string]any{"a": true}) {
		t.Error("a genuinely different value must not compare equal")
	}
	if configDeepMatch(map[string]any{"a": "x"}, map[string]any{"a": map[string]any{"b": 1}}) {
		t.Error("a scalar cannot satisfy a nested expectation")
	}
}

func roundTrip(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// --- mapping types across versions -----------------------------------------
//
// Elasticsearch 6 narrowed an index to one mapping type and 7 removed types
// altogether, which moves a document's type out of the index's structure and
// into a field on the document. The tests below pin all three shapes, because
// the failure mode of the 7.x one is total: a multi-type mapping is rejected
// with mapper_parsing_exception, so the index is never created and every later
// request fails for want of it.

func esBackend(version int) *Backend {
	return newBackend(engine.BackendDef{Hosts: []string{"es:9200"}, Version: version})
}

// Version 5 keeps Phorge's layout verbatim: one identical property set per
// document type. This is the regression guard for the branch — the shape below
// is what an index shared with Phorge's own engine has to have.
func TestVersion5KeepsAMappingPerDocumentType(t *testing.T) {
	cfg := esBackend(5).buildIndexConfig([]string{"TASK", "DREV"})

	mappings := dig(t, cfg, "mappings").(map[string]any)
	if len(mappings) != 2 {
		t.Fatalf("expected one mapping per document type, got %d: %v", len(mappings), mappings)
	}
	for _, docType := range []string{"TASK", "DREV"} {
		dig(t, cfg, "mappings", docType, "properties", esquery.FieldTitle)
	}
	// The type is the mapping, so it must not also be a field.
	if props, ok := dig(t, cfg, "mappings", "TASK", "properties").(map[string]any); ok {
		if _, exists := props[fieldDocType]; exists {
			t.Error("a typed mapping must not also carry a docType field")
		}
	}
}

// Version 7 carries the properties at the mapping's root, with no type level of
// any kind.
func TestVersion7HasNoMappingTypes(t *testing.T) {
	cfg := esBackend(7).buildIndexConfig([]string{"TASK", "DREV", "CMIT"})

	mappings := dig(t, cfg, "mappings").(map[string]any)
	if _, ok := mappings["properties"]; !ok {
		t.Fatalf("expected properties at the mapping root, got keys %v", sortedMapKeys(mappings))
	}
	if len(mappings) != 1 {
		t.Errorf("the mapping root must hold nothing but properties, got %v", sortedMapKeys(mappings))
	}
	// The document types must not have leaked back in under any spelling.
	for _, docType := range []string{"TASK", "DREV", "CMIT", docEndpointType} {
		if _, exists := mappings[docType]; exists {
			t.Errorf("%q must not be a mapping type on version 7", docType)
		}
	}
}

// The document types stop shaping the mapping entirely, so the same index
// serves any list of them. This is what makes it safe for InitIndex to keep
// taking the argument.
func TestVersion7MappingIgnoresTheDocumentTypeList(t *testing.T) {
	b := esBackend(7)
	one := roundTrip(t, b.buildIndexConfig([]string{"TASK"}))
	many := roundTrip(t, b.buildIndexConfig([]string{"TASK", "DREV", "CMIT", "USER"}))

	if !reflect.DeepEqual(one, many) {
		t.Error("the version 7 mapping must not depend on which document types are passed")
	}
}

// With the type gone from the mapping and the URL, this field is the only
// record of it, and a type-scoped query filters on it. Analysed it would match
// the four-character constants approximately, which for a filter is wrong.
func TestVersion7IndexesTheDocumentTypeAsAKeyword(t *testing.T) {
	cfg := esBackend(7).buildIndexConfig([]string{"TASK"})

	field := dig(t, cfg, "mappings", "properties", fieldDocType).(map[string]any)
	if field["type"] != "keyword" {
		t.Errorf("expected docType to be a keyword, got %v", field["type"])
	}
}

// Version 6 wants exactly one mapping type and no more, conventionally _doc.
func TestVersion6NestsASingleMappingType(t *testing.T) {
	cfg := esBackend(6).buildIndexConfig([]string{"TASK", "DREV"})

	mappings := dig(t, cfg, "mappings").(map[string]any)
	if len(mappings) != 1 {
		t.Fatalf("expected exactly one mapping type, got %v", sortedMapKeys(mappings))
	}
	dig(t, cfg, "mappings", docEndpointType, "properties", fieldDocType)
}

// _all was removed in 6.0, and a mapping that still mentions include_in_all is
// rejected rather than ignored — so this is not tidiness, it is the difference
// between an index that gets created and one that does not.
func TestIncludeInAllIsOmittedFromVersion6Onwards(t *testing.T) {
	for _, version := range []int{6, 7, 8} {
		cfg := roundTrip(t, esBackend(version).buildIndexConfig([]string{"TASK"}))
		if where := findKey(cfg, "include_in_all"); where != "" {
			t.Errorf("version %d: include_in_all must not appear, found at %s", version, where)
		}
	}
	// It has to stay on 5, where _all still exists and Phorge writes it: an
	// index missing it would report itself insane against Phorge's engine.
	cfg := roundTrip(t, esBackend(5).buildIndexConfig([]string{"TASK"}))
	if findKey(cfg, "include_in_all") == "" {
		t.Error("version 5 must keep include_in_all, which Phorge's own mapping carries")
	}
}

// A typeless cluster reads a type in the URL as an index name, so leaving it
// there would write into an index nobody created instead of the one configured.
func TestVersion7IndexesThroughTheDocEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	b := newBackend(engine.BackendDef{
		Hosts:   []string{strings.TrimPrefix(srv.URL, "http://")},
		Version: 7,
	})
	if err := b.IndexDocument(&contracts.Document{PHID: "PHID-TASK-1", Type: "TASK"}); err != nil {
		t.Fatal(err)
	}
	if want := "/phabricator/_doc/PHID-TASK-1"; gotPath != want {
		t.Errorf("expected %s, got %s", want, gotPath)
	}
}

func TestVersion7DocumentCarriesItsType(t *testing.T) {
	spec := esBackend(7).buildDocSpec(&contracts.Document{PHID: "PHID-TASK-1", Type: "TASK"})
	if spec[fieldDocType] != "TASK" {
		t.Errorf("expected the document to carry its type, got %v", spec[fieldDocType])
	}

	// On 5 the type is the mapping, so repeating it in the body would add a
	// field the mapping does not declare.
	spec5 := esBackend(5).buildDocSpec(&contracts.Document{PHID: "PHID-TASK-1", Type: "TASK"})
	if _, exists := spec5[fieldDocType]; exists {
		t.Error("a version 5 document must not carry a docType field")
	}
}

// The type restriction does not disappear, it moves: out of the URL and into
// the query as a filter. Losing it would widen every scoped search to the whole
// index, which returns plausible results and so would not look like a failure.
func TestVersion7ScopesTypesThroughTheQuery(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"hits":{"hits":[]}}`))
	}))
	defer srv.Close()

	b := newBackend(engine.BackendDef{
		Hosts:   []string{strings.TrimPrefix(srv.URL, "http://")},
		Version: 7,
	})
	q := &contracts.SearchQuery{Query: "x", Types: []string{"TASK", "DREV"}}
	if _, err := b.Search(q); err != nil {
		t.Fatal(err)
	}
	if want := "/phabricator/_search"; gotPath != want {
		t.Errorf("expected the type to leave the URL: wanted %s, got %s", want, gotPath)
	}

	spec := roundTrip(t, b.buildSearchSpec(q))
	if where := findValue(spec, "TASK"); where == "" {
		t.Error("the requested types must appear in the query as a filter")
	}
}

// bool.must_not, not the "not" query: that one was removed in 5.0, so on every
// version this backend supports it comes back as a parsing error instead of
// excluding anything.
func TestExcludeUsesMustNot(t *testing.T) {
	for _, version := range []int{5, 7} {
		spec := roundTrip(t, esBackend(version).buildSearchSpec(
			&contracts.SearchQuery{Exclude: "PHID-TASK-9"}))

		if where := findKey(spec, "not"); where != "" {
			t.Errorf("version %d: the removed \"not\" query must not be used, found at %s", version, where)
		}
		mustNot, ok := dig(t, spec, "query", "bool", "must_not").([]any)
		if !ok || len(mustNot) != 1 {
			t.Fatalf("version %d: expected one must_not clause, got %v", version, spec)
		}
		if findValue(mustNot[0], "PHID-TASK-9") == "" {
			t.Errorf("version %d: the excluded phid is missing from the clause: %v", version, mustNot[0])
		}
	}
}

// findKey reports a dotted path to the first occurrence of key, or "" if the
// tree does not contain it. Used for asserting a key's *absence* everywhere,
// which a targeted dig cannot express.
func findKey(v any, key string) string {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			if k == key {
				return k
			}
			if where := findKey(child, key); where != "" {
				return k + "." + where
			}
		}
	case []any:
		for i, child := range node {
			if where := findKey(child, key); where != "" {
				return fmt.Sprintf("[%d].%s", i, where)
			}
		}
	}
	return ""
}

// findValue is findKey's counterpart for leaf values.
func findValue(v any, want string) string {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			if where := findValue(child, want); where != "" {
				return k + "." + where
			}
		}
	case []any:
		for i, child := range node {
			if where := findValue(child, want); where != "" {
				return fmt.Sprintf("[%d].%s", i, where)
			}
		}
	case string:
		if node == want {
			return node
		}
	}
	return ""
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- document and query building -------------------------------------------

func TestDocumentFlattensFieldsAndRelationships(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}})

	spec := b.buildDocSpec(&contracts.Document{
		PHID:         "PHID-TASK-1",
		Type:         "TASK",
		Title:        "Fix it",
		DateCreated:  100,
		DateModified: 200,
		Fields: []contracts.DocumentField{
			{Name: esquery.FieldBody, Corpus: "first"},
			{Name: esquery.FieldBody, Corpus: "second", Aux: "aux"},
		},
		Relationships: []contracts.DocumentRelation{
			{Name: esquery.RelAuthor, RelatedPHID: "PHID-USER-1"},
			{Name: esquery.RelOpen, RelatedPHID: "PHID-TASK-1", Timestamp: 300},
		},
	})

	if spec["title"] != "Fix it" || spec["dateCreated"] != int64(100) || spec["lastModified"] != int64(200) {
		t.Errorf("unexpected header fields: %v", spec)
	}
	// Repeated field names accumulate, and aux joins the same list. A document
	// with two comments has to keep both.
	if got := spec[esquery.FieldBody]; !reflect.DeepEqual(got, []any{"first", "second", "aux"}) {
		t.Errorf("expected the corpora accumulated, got %v", got)
	}
	if got := spec[esquery.RelAuthor]; !reflect.DeepEqual(got, []any{"PHID-USER-1"}) {
		t.Errorf("unexpected author relationship: %v", got)
	}
	// A timestamped relationship gets its companion field, which is what makes
	// "closed since" sortable.
	if spec[esquery.RelOpen+"_ts"] != int64(300) {
		t.Errorf("expected open_ts to be 300, got %v", spec[esquery.RelOpen+"_ts"])
	}
	if _, ok := spec[esquery.RelAuthor+"_ts"]; ok {
		t.Error("a relationship without a timestamp must not get a _ts field")
	}
}

func TestQueryWithoutTextSortsByDate(t *testing.T) {
	spec := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).
		buildSearchSpec(&contracts.SearchQuery{})

	if _, ok := spec["sort"]; !ok {
		t.Error("a query with no text has no relevance to rank by and must sort by date")
	}
	bq := spec["query"].(map[string]any)["bool"].(*esquery.BoolQuery)
	if bq.MustCount() != 1 {
		t.Fatalf("expected the match_all fallback, got %d must clauses", bq.MustCount())
	}
	if _, ok := bq.Must[0].(map[string]any)["match_all"]; !ok {
		t.Errorf("expected match_all, got %v", bq.Must[0])
	}
}

func TestQueryWithTextDoesNotSort(t *testing.T) {
	spec := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).
		buildSearchSpec(&contracts.SearchQuery{Query: "parser"})

	if _, ok := spec["sort"]; ok {
		t.Error("a text query must rank by relevance, not by date")
	}
}

// Open and closed are markers on the document rather than a status field, so
// asking for both is the same as asking for neither.
func TestStatusFilters(t *testing.T) {
	cases := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"open only", []string{esquery.RelOpen}, esquery.RelOpen},
		{"closed only", []string{esquery.RelClosed}, esquery.RelClosed},
		{"both", []string{esquery.RelOpen, esquery.RelClosed}, ""},
		{"neither", nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).
				buildSearchSpec(&contracts.SearchQuery{Statuses: tc.statuses})
			bq := spec["query"].(map[string]any)["bool"].(*esquery.BoolQuery)

			var fields []string
			for _, f := range bq.Filter {
				if ex, ok := f.(map[string]any)["exists"].(map[string]any); ok {
					fields = append(fields, ex["field"].(string))
				}
			}
			if tc.want == "" {
				if len(fields) != 0 {
					t.Errorf("expected no exists filter, got %v", fields)
				}
				return
			}
			if len(fields) != 1 || fields[0] != tc.want {
				t.Errorf("expected an exists filter on %q, got %v", tc.want, fields)
			}
		})
	}
}

// withAnyOwner overrides an owner list rather than intersecting with it,
// matching the PHP query builder.
func TestAnyOwnerOverridesTheOwnerList(t *testing.T) {
	spec := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).
		buildSearchSpec(&contracts.SearchQuery{
			WithAnyOwner: true,
			OwnerPHIDs:   []string{"PHID-USER-1"},
		})

	bq := spec["query"].(map[string]any)["bool"].(*esquery.BoolQuery)
	for _, f := range bq.Filter {
		if _, ok := f.(map[string]any)["terms"]; ok {
			t.Error("withAnyOwner must not leave a terms filter on the owner list")
		}
	}
}

// from+size past index.max_result_window is an error the user cannot act on,
// so a deep page is clamped into a short answer instead.
func TestDeepPagingIsClamped(t *testing.T) {
	cases := []struct {
		offset, limit         int
		wantOffset, wantLimit int
	}{
		{0, 0, 0, defaultResultLimit},
		{50, 25, 50, 25},
		{9999, 101, maxResultWindow - 101, 101},
		{0, maxResultWindow + 100, 0, maxResultWindow + 100},
	}

	for _, tc := range cases {
		spec := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}}).
			buildSearchSpec(&contracts.SearchQuery{Offset: tc.offset, Limit: tc.limit})
		if spec["from"] != tc.wantOffset || spec["size"] != tc.wantLimit {
			t.Errorf("offset=%d limit=%d: expected from=%d size=%d, got from=%v size=%v",
				tc.offset, tc.limit, tc.wantOffset, tc.wantLimit, spec["from"], spec["size"])
		}
	}
}

// --- HTTP ------------------------------------------------------------------

// A 5xx takes the host out of rotation; a 4xx does not. Taking a host out for
// a 4xx would let one malformed document empty the whole health table.
func TestOnlyServerErrorsMarkAHostUnhealthy(t *testing.T) {
	cases := []struct {
		status      int
		wantHealthy bool
	}{
		{http.StatusOK, true},
		{http.StatusBadRequest, true},
		{http.StatusNotFound, true},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	}

	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{}`))
		}))

		host := strings.TrimPrefix(srv.URL, "http://")
		b := newBackend(engine.BackendDef{Hosts: []string{host}})
		_, _ = b.doRequestRead(host, srv.URL, http.MethodGet, nil)

		b.mu.RLock()
		healthy := b.health[host]
		b.mu.RUnlock()
		if healthy != tc.wantHealthy {
			t.Errorf("status %d: expected healthy=%v, got %v", tc.status, tc.wantHealthy, healthy)
		}
		srv.Close()
	}
}

func TestSearchRoundTrip(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"hits":{"hits":[{"_id":"PHID-TASK-1"},{"_id":"PHID-TASK-2"}]}}`))
	}))
	defer srv.Close()

	b := newBackend(engine.BackendDef{Hosts: []string{strings.TrimPrefix(srv.URL, "http://")}})
	phids, err := b.Search(&contracts.SearchQuery{Query: "x", Types: []string{"TASK", "DREV"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(phids, []string{"PHID-TASK-1", "PHID-TASK-2"}) {
		t.Errorf("unexpected results: %v", phids)
	}
	// Scoping the URL to the requested types rather than searching the whole
	// index is what keeps an alias over a larger index from leaking into the
	// answer.
	if gotPath != "/phabricator/TASK,DREV/_search" {
		t.Errorf("unexpected search path: %s", gotPath)
	}
}

func TestSearchFailsOverBetweenHosts(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hits":{"hits":[{"_id":"PHID-TASK-1"}]}}`))
	}))
	defer up.Close()

	b := newBackend(engine.BackendDef{Hosts: []string{
		strings.TrimPrefix(down.URL, "http://"),
		strings.TrimPrefix(up.URL, "http://"),
	}})

	phids, err := b.Search(&contracts.SearchQuery{Query: "x"})
	if err != nil {
		t.Fatalf("expected the second host to answer, got %v", err)
	}
	if len(phids) != 1 {
		t.Errorf("unexpected results: %v", phids)
	}
}

func TestStatsReportTheFourKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"indices":{"phabricator":{
			"primaries":{"search":{"query_total":7}},
			"total":{"docs":{"count":42,"deleted":3},"store":{"size_in_bytes":1024}}}}}`))
	}))
	defer srv.Close()

	b := newBackend(engine.BackendDef{Hosts: []string{strings.TrimPrefix(srv.URL, "http://")}})
	stats, err := b.IndexStats()
	if err != nil {
		t.Fatal(err)
	}

	// storage_bytes is the one snake_case name on this service's wire. It is
	// kept deliberately: the PHP side reads it by this spelling, and renaming
	// it leaves the cluster panel's storage column blank without any error.
	for key, want := range map[string]float64{
		"queries": 7, "documents": 42, "deleted": 3, "storage_bytes": 1024,
	} {
		if got, ok := stats[key].(float64); !ok || got != want {
			t.Errorf("expected %s to be %v, got %v", key, want, stats[key])
		}
	}
}

func TestStatsRejectAnIndexThatIsNotThere(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"indices":{}}`))
	}))
	defer srv.Close()

	b := newBackend(engine.BackendDef{Hosts: []string{strings.TrimPrefix(srv.URL, "http://")}})
	if _, err := b.IndexStats(); err == nil {
		t.Error("expected an error when the index is absent from the stats")
	}
}

func TestStatsAreUnavailableOnVersionOne(t *testing.T) {
	b := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}, Version: 1})
	if _, err := b.IndexStats(); err == nil {
		t.Error("expected an error: the 1.x stats API is not the one this reads")
	}
}

func TestIndexExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"indices":{"phabricator":{}}}`))
	}))
	defer srv.Close()

	b := newBackend(engine.BackendDef{Hosts: []string{strings.TrimPrefix(srv.URL, "http://")}})
	exists, err := b.IndexExists()
	if err != nil || !exists {
		t.Fatalf("expected the index to exist: exists=%v err=%v", exists, err)
	}

	other := newBackend(engine.BackendDef{
		Hosts: []string{strings.TrimPrefix(srv.URL, "http://")},
		Index: "elsewhere",
	})
	exists, err = other.IndexExists()
	if err != nil || exists {
		t.Fatalf("expected a different index to be absent: exists=%v err=%v", exists, err)
	}
}

// InitIndex deletes before it creates, and the delete's 404 on a first run is
// normal rather than an error.
func TestInitIndexDeletesThenCreates(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"acknowledged":true}`))
	}))
	defer srv.Close()

	b := newBackend(engine.BackendDef{Hosts: []string{strings.TrimPrefix(srv.URL, "http://")}})
	if err := b.InitIndex([]string{"TASK"}); err != nil {
		t.Fatalf("a 404 from the delete must not fail the init: %v", err)
	}
	if !reflect.DeepEqual(methods, []string{http.MethodDelete, http.MethodPut}) {
		t.Errorf("expected a delete then a put, got %v", methods)
	}
}

func TestOperationsRefuseARoleTheBackendDoesNotHave(t *testing.T) {
	readOnly := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}, Roles: []string{"read"}})
	if err := readOnly.IndexDocument(&contracts.Document{PHID: "x", Type: "TASK"}); err == nil {
		t.Error("a read-only backend must refuse a write")
	}
	if err := readOnly.InitIndex([]string{"TASK"}); err == nil {
		t.Error("a read-only backend must refuse an init")
	}

	writeOnly := newBackend(engine.BackendDef{Hosts: []string{"es:9200"}, Roles: []string{"write"}})
	if _, err := writeOnly.Search(&contracts.SearchQuery{}); err == nil {
		t.Error("a write-only backend must refuse a search")
	}
	if _, err := writeOnly.IndexExists(); err == nil {
		t.Error("a write-only backend must refuse an existence check")
	}
}

func TestInfoNeverCarriesCredentials(t *testing.T) {
	info := newBackend(engine.BackendDef{
		Hosts:  []string{"es:9200"},
		APIKey: "super-secret",
	}).Info()

	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret") {
		t.Errorf("the backend listing leaked a credential: %s", raw)
	}
}
