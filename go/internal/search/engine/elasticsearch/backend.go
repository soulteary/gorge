// Package elasticsearch implements the search domain's Elasticsearch backend:
// document indexing, query translation and index management, for the same
// index layout Phorge's own PhabricatorElasticFulltextStorageEngine uses.
package elasticsearch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/search/engine"
	"github.com/soulteary/gorge/go/internal/search/esquery"
)

// Backend talks to one Elasticsearch cluster.
//
// It keeps a per-host health table rather than a single "up" flag, which is
// what lets a multi-host cluster keep serving through the loss of one node.
// The table is in-process state and is the reason this domain gets a binary of
// its own rather than sharing gorge-render's.
type Backend struct {
	hosts    []string
	index    string
	version  int
	timeout  int
	protocol string
	roles    map[string]bool

	mu     sync.RWMutex
	health map[string]bool
	client *http.Client
}

// Defaults applied to a definition that leaves a field empty. The version
// default of 5 matches Phorge's, and it is load-bearing rather than cosmetic:
// it selects the field type ("text" versus "string") and the timestamp field
// name that the whole mapping is built from.
const (
	defaultVersion    = 5
	defaultTimeoutSec = 15
	defaultProtocol   = "http"
)

// New builds the backend from its definition.
func New(def engine.BackendDef) *Backend {
	b := &Backend{
		// A slash in the index name would escape the path this backend builds
		// its URLs from, so it is stripped rather than escaped.
		hosts:    def.Hosts,
		index:    strings.ReplaceAll(def.Index, "/", ""),
		version:  def.Version,
		timeout:  def.Timeout,
		protocol: def.Protocol,
		roles:    make(map[string]bool),
		health:   make(map[string]bool),
	}
	if b.index == "" {
		b.index = engine.DefaultIndexName
	}
	if b.version == 0 {
		b.version = defaultVersion
	}
	if b.timeout == 0 {
		b.timeout = defaultTimeoutSec
	}
	if b.protocol == "" {
		b.protocol = defaultProtocol
	}
	for _, r := range def.Roles {
		b.roles[r] = true
	}
	if len(b.roles) == 0 {
		b.roles["read"] = true
		b.roles["write"] = true
	}
	// Hosts start healthy. Marking them unknown instead would make the first
	// request of a fresh process fail with "no healthy hosts", which is a
	// worse answer than trying and finding out.
	for _, h := range b.hosts {
		b.health[h] = true
	}
	b.client = &http.Client{Timeout: time.Duration(b.timeout) * time.Second}
	return b
}

func (b *Backend) Type() string { return "elasticsearch" }

func (b *Backend) HasRole(role string) bool { return b.roles[role] }

func (b *Backend) Info() contracts.BackendInfo {
	return contracts.BackendInfo{
		"type":    b.Type(),
		"hosts":   b.hosts,
		"index":   b.index,
		"version": b.version,
		"roles":   sortedRoles(b.roles),
	}
}

func (b *Backend) hostForRole(role string) (string, error) {
	if !b.roles[role] {
		return "", fmt.Errorf("backend does not have role %q", role)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	healthy := make([]string, 0, len(b.hosts))
	for _, h := range b.hosts {
		if b.health[h] {
			healthy = append(healthy, h)
		}
	}
	if len(healthy) == 0 {
		return "", fmt.Errorf("no healthy hosts for role %q", role)
	}
	return healthy[rand.IntN(len(healthy))], nil
}

func (b *Backend) allHostsForRole(role string) []string {
	if !b.roles[role] {
		return nil
	}
	return b.hosts
}

func (b *Backend) markHealth(host string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.health[host] = ok
}

func (b *Backend) baseURL(host string) string {
	proto := b.protocol
	if proto == "" {
		proto = defaultProtocol
	}
	// A host that already carries a scheme is taken as written, so a
	// deployment behind a proxy can point at an https URL with a path.
	if strings.Contains(host, "://") {
		return strings.TrimRight(host, "/") + "/" + b.index
	}
	return fmt.Sprintf("%s://%s/%s", proto, host, b.index)
}

// timestampField is _timestamp on Elasticsearch 1.x, where the modification
// time was a mapping-level meta field, and an ordinary lastModified field
// afterwards.
func (b *Backend) timestampField() string {
	if b.version < 2 {
		return "_timestamp"
	}
	return "lastModified"
}

func (b *Backend) textFieldType() string {
	if b.version >= 5 {
		return "text"
	}
	return "string"
}

func (b *Backend) IndexDocument(doc *contracts.Document) error {
	host, err := b.hostForRole("write")
	if err != nil {
		return err
	}
	spec := b.buildDocSpec(doc)
	url := fmt.Sprintf("%s/%s/%s", b.baseURL(host), doc.Type, doc.PHID)
	return b.doRequest(host, url, http.MethodPut, spec)
}

func (b *Backend) Search(q *contracts.SearchQuery) ([]string, error) {
	hosts := b.allHostsForRole("read")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("backend has no read role")
	}

	spec := b.buildSearchSpec(q)

	var lastErr error
	for _, host := range hosts {
		// Restricting the URL to the requested types rather than searching the
		// whole index matters when "phabricator" is an alias over something
		// larger; Phorge's own engine does the same for that reason.
		var uri string
		if len(q.Types) > 0 {
			uri = fmt.Sprintf("%s/%s/_search", b.baseURL(host), strings.Join(q.Types, ","))
		} else {
			uri = fmt.Sprintf("%s/_search", b.baseURL(host))
		}

		body, err := b.doRequestRead(host, uri, http.MethodPost, spec)
		if err != nil {
			lastErr = err
			continue
		}

		var resp esSearchResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			b.markHealth(host, false)
			lastErr = fmt.Errorf("invalid JSON from elasticsearch: %w", err)
			continue
		}

		phids := make([]string, 0, len(resp.Hits.Hits))
		for _, h := range resp.Hits.Hits {
			phids = append(phids, h.ID)
		}
		return phids, nil
	}
	return nil, fmt.Errorf("all elasticsearch hosts failed: %w", lastErr)
}

func (b *Backend) IndexExists() (bool, error) {
	host, err := b.hostForRole("read")
	if err != nil {
		return false, err
	}

	if b.version >= 5 {
		url := fmt.Sprintf("%s/_stats/", b.baseURL(host))
		body, err := b.doRequestRead(host, url, http.MethodGet, nil)
		if err != nil {
			return false, err
		}
		var stats map[string]any
		if err := json.Unmarshal(body, &stats); err != nil {
			return false, err
		}
		indices, _ := stats["indices"].(map[string]any)
		_, exists := indices[b.index]
		return exists, nil
	}

	url := b.baseURL(host) + "/_status/"
	_, err = b.doRequestRead(host, url, http.MethodGet, nil)
	return err == nil, nil
}

// InitIndex drops the index and recreates it from the current configuration.
// Everything in it is gone afterwards and a full reindex has to follow.
func (b *Backend) InitIndex(docTypes []string) error {
	host, err := b.hostForRole("write")
	if err != nil {
		return err
	}

	url := b.baseURL(host)
	// A missing index makes the delete a 404, which is the normal case on a
	// first init and not worth reporting.
	_ = b.doRequest(host, url, http.MethodDelete, nil)

	data := b.buildIndexConfig(docTypes)
	return b.doRequest(host, url, http.MethodPut, data)
}

func (b *Backend) IndexStats() (contracts.IndexStats, error) {
	if b.version < 2 {
		return nil, fmt.Errorf("stats not supported for elasticsearch version %d", b.version)
	}
	host, err := b.hostForRole("read")
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/_stats/", b.baseURL(host))
	body, err := b.doRequestRead(host, url, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	indices, _ := raw["indices"].(map[string]any)
	idx, _ := indices[b.index].(map[string]any)
	if idx == nil {
		return nil, fmt.Errorf("index %q not found in stats", b.index)
	}

	return contracts.IndexStats{
		"queries":   jsonPath(idx, "primaries", "search", "query_total"),
		"documents": jsonPath(idx, "total", "docs", "count"),
		"deleted":   jsonPath(idx, "total", "docs", "deleted"),
		// snake_case, unlike every other name on this service's wire. It
		// predates the monorepo and the PHP side reads it by this spelling.
		"storage_bytes": jsonPath(idx, "total", "store", "size_in_bytes"),
	}, nil
}

// IndexIsSane compares the live index configuration against the one this
// service would create today.
//
// It answers false — not an error — whenever the mapping this package builds
// changes, which is exactly what happened when the CJK subfield was added.
// That is the intended behaviour: a mapping change cannot be applied to an
// existing index in place, so the honest answer is "reindex", and Phorge's
// setup checks surface it. See docs/modules/search.md section 3.3.
func (b *Backend) IndexIsSane(docTypes []string) (bool, error) {
	exists, err := b.IndexExists()
	if err != nil || !exists {
		return false, err
	}

	host, err := b.hostForRole("read")
	if err != nil {
		return false, err
	}

	mappingURL := fmt.Sprintf("%s/_mapping/", b.baseURL(host))
	mappingBody, err := b.doRequestRead(host, mappingURL, http.MethodGet, nil)
	if err != nil {
		return false, err
	}

	settingsURL := fmt.Sprintf("%s/_settings/", b.baseURL(host))
	settingsBody, err := b.doRequestRead(host, settingsURL, http.MethodGet, nil)
	if err != nil {
		return false, err
	}

	var mappingResp, settingsResp map[string]any
	if json.Unmarshal(mappingBody, &mappingResp) != nil {
		return false, fmt.Errorf("invalid mapping response from elasticsearch")
	}
	if json.Unmarshal(settingsBody, &settingsResp) != nil {
		return false, fmt.Errorf("invalid settings response from elasticsearch")
	}

	actual := mergeAny(
		asMap(settingsResp[b.index]),
		asMap(mappingResp[b.index]),
	)

	return configDeepMatch(actual, b.buildIndexConfig(docTypes)), nil
}

// HTTP helpers

func (b *Backend) doRequest(host, url, method string, body any) error {
	_, err := b.doRequestRead(host, url, method, body)
	return err
}

// doRequestRead performs one request and maintains the host health table.
//
// Only transport failures and 5xx mark a host unhealthy. A 4xx is the
// cluster answering correctly about a bad request, and taking a host out of
// rotation for it would let one malformed document empty the whole table.
func (b *Backend) doRequestRead(host, url, method string, body any) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		b.markHealth(host, false)
		return nil, fmt.Errorf("elasticsearch request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		b.markHealth(host, false)
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 500 {
		b.markHealth(host, false)
		return nil, fmt.Errorf("elasticsearch returned status %d: %s", resp.StatusCode, string(respBody))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("elasticsearch returned status %d: %s", resp.StatusCode, string(respBody))
	}

	b.markHealth(host, true)
	return respBody, nil
}

type esSearchResponse struct {
	Hits struct {
		Hits []struct {
			ID string `json:"_id"`
		} `json:"hits"`
	} `json:"hits"`
}

// Query and index building

// buildDocSpec flattens a document into the shape the index expects: every
// field and relationship becomes a top-level key named after its
// four-character Phorge constant, and repeated names accumulate into a list.
func (b *Backend) buildDocSpec(doc *contracts.Document) map[string]any {
	ts := b.timestampField()
	spec := map[string]any{
		"title":       doc.Title,
		"dateCreated": doc.DateCreated,
		ts:            doc.DateModified,
	}

	for _, f := range doc.Fields {
		key := f.Name
		existing, ok := spec[key]
		if ok {
			if arr, isArr := existing.([]any); isArr {
				arr = append(arr, f.Corpus)
				if f.Aux != "" {
					arr = append(arr, f.Aux)
				}
				spec[key] = arr
			} else {
				vals := []any{existing, f.Corpus}
				if f.Aux != "" {
					vals = append(vals, f.Aux)
				}
				spec[key] = vals
			}
		} else {
			vals := []any{f.Corpus}
			if f.Aux != "" {
				vals = append(vals, f.Aux)
			}
			spec[key] = vals
		}
	}

	for _, r := range doc.Relationships {
		key := r.Name
		existing, ok := spec[key]
		if ok {
			if arr, isArr := existing.([]any); isArr {
				spec[key] = append(arr, r.RelatedPHID)
			} else {
				spec[key] = []any{existing, r.RelatedPHID}
			}
		} else {
			spec[key] = []any{r.RelatedPHID}
		}
		if r.Timestamp > 0 {
			spec[key+"_ts"] = r.Timestamp
		}
	}

	return spec
}

func (b *Backend) buildSearchSpec(q *contracts.SearchQuery) map[string]any {
	bq := &esquery.BoolQuery{}

	if q.Query != "" {
		// The must clause searches every subfield of the three corpus fields,
		// so the cjk subfield is reached through the wildcard without being
		// named. The should clauses below only add scoring.
		bq.AddMust(map[string]any{
			"simple_query_string": map[string]any{
				"query": q.Query,
				"fields": []string{
					esquery.FieldTitle + ".*",
					esquery.FieldBody + ".*",
					esquery.FieldComment + ".*",
				},
				"default_operator": "AND",
			},
		})

		bq.AddShould(map[string]any{
			"simple_query_string": map[string]any{
				"query": q.Query,
				"fields": []string{
					"*.raw",
					esquery.FieldTitle + "^4",
					esquery.FieldBody + "^3",
					esquery.FieldComment + "^1.2",
				},
				"analyzer":         analyzerEnglishExact,
				"default_operator": "and",
			},
		})

		// The CJK counterpart of the clause above. It is a separate clause
		// rather than more fields on that one because the analyser is a
		// property of the clause: english_exact tokenises Han text into single
		// characters, which scores a bigram-indexed field almost at random.
		bq.AddShould(map[string]any{
			"simple_query_string": map[string]any{
				"query": q.Query,
				"fields": []string{
					esquery.FieldTitle + "." + esquery.SubfieldCJK + "^4",
					esquery.FieldBody + "." + esquery.SubfieldCJK + "^3",
					esquery.FieldComment + "." + esquery.SubfieldCJK + "^1.2",
				},
				"analyzer":         analyzerCJKText,
				"default_operator": "and",
			},
		})
	}

	if q.Exclude != "" {
		bq.AddFilter(map[string]any{
			"not": map[string]any{
				"ids": map[string]any{
					"values": []string{q.Exclude},
				},
			},
		})
	}

	relMap := map[string][]string{
		esquery.RelAuthor:     q.AuthorPHIDs,
		esquery.RelSubscriber: q.SubscriberPHIDs,
		esquery.RelProject:    q.ProjectPHIDs,
		esquery.RelRepository: q.RepositoryPHIDs,
	}
	// Sorted, so the generated spec is stable enough to assert on. Ranging a
	// map here would reorder the filter list on every call.
	for _, field := range sortedKeys(relMap) {
		if phids := relMap[field]; len(phids) > 0 {
			bq.AddTerms(field, phids)
		}
	}

	// Open and closed are markers on the document, not a status field, so
	// asking for both is the same as asking for neither: no filter at all.
	statusSet := make(map[string]bool, len(q.Statuses))
	for _, s := range q.Statuses {
		statusSet[s] = true
	}
	includeOpen := statusSet[esquery.RelOpen]
	includeClosed := statusSet[esquery.RelClosed]
	if includeOpen && !includeClosed {
		bq.AddExists(esquery.RelOpen)
	} else if !includeOpen && includeClosed {
		bq.AddExists(esquery.RelClosed)
	}

	if q.WithUnowned {
		bq.AddExists(esquery.RelUnowned)
	}

	if q.WithAnyOwner {
		bq.AddExists(esquery.RelOwner)
	} else if len(q.OwnerPHIDs) > 0 {
		bq.AddTerms(esquery.RelOwner, q.OwnerPHIDs)
	}

	if bq.MustCount() == 0 {
		bq.AddMust(map[string]any{
			"match_all": map[string]any{"boost": 1},
		})
	}

	spec := map[string]any{
		// Only the ids are wanted: Phorge loads the objects itself so that its
		// policy checks stay in the loop.
		"_source": false,
		"query": map[string]any{
			"bool": bq,
		},
	}

	// With no query text there is no relevance to sort by, so the newest
	// documents come first instead.
	if q.Query == "" {
		spec["sort"] = []any{
			map[string]string{"dateCreated": "desc"},
		}
	}

	offset := q.Offset
	limit := q.Limit
	if limit == 0 {
		limit = defaultResultLimit
	}
	// Elasticsearch refuses from+size past index.max_result_window, which
	// defaults to 10000. Clamping keeps a deep page as a short answer rather
	// than as an error the user cannot act on.
	if offset+limit > maxResultWindow {
		offset = maxResultWindow - limit
		if offset < 0 {
			offset = 0
		}
	}
	spec["from"] = offset
	spec["size"] = limit

	return spec
}

// Analyser and paging constants.
//
// The three English analysers are Phorge's, copied name for name so an index
// built by either implementation satisfies the other's sanity check.
// cjk_text is this service's addition; see buildIndexConfig.
const (
	analyzerEnglishExact = "english_exact"
	analyzerLetterStop   = "letter_stop"
	analyzerEnglishStem  = "english_stem"
	analyzerCJKText      = "cjk_text"

	filterCJKBigram = "cjk_bigram_filter"

	// defaultResultLimit is Phorge's page size plus one: it asks for one more
	// row than it shows in order to know whether a "next page" link is due.
	defaultResultLimit = 101
	maxResultWindow    = 10000
)

// buildIndexConfig produces the settings and mappings for the index. It is
// used both to create the index and, by IndexIsSane, as the expectation an
// existing index is compared against — so any change here makes every existing
// index report itself insane until it is rebuilt.
func (b *Backend) buildIndexConfig(docTypes []string) map[string]any {
	textType := b.textFieldType()

	data := map[string]any{
		"settings": map[string]any{
			"index": map[string]any{
				"auto_expand_replicas": "0-2",
				"analysis": map[string]any{
					"filter": map[string]any{
						"english_stop": map[string]any{
							"type":      "stop",
							"stopwords": "_english_",
						},
						"english_stemmer": map[string]any{
							"type":     "stemmer",
							"language": "english",
						},
						"english_possessive_stemmer": map[string]any{
							"type":     "stemmer",
							"language": "possessive_english",
						},
						// output_unigrams keeps the single characters beside
						// the bigrams, which is what lets a one-character
						// query match at all. Without it "猫" finds nothing in
						// a document that plainly contains it.
						filterCJKBigram: map[string]any{
							"type":            "cjk_bigram",
							"output_unigrams": true,
						},
					},
					"analyzer": map[string]any{
						analyzerEnglishExact: map[string]any{
							"tokenizer": "standard",
							"filter":    []string{"lowercase"},
						},
						analyzerLetterStop: map[string]any{
							"tokenizer": "letter",
							"filter":    []string{"lowercase", "english_stop"},
						},
						analyzerEnglishStem: map[string]any{
							"tokenizer": "standard",
							"filter": []string{
								"english_possessive_stemmer",
								"lowercase",
								"english_stop",
								"english_stemmer",
							},
						},
						// Both filters here are built into Elasticsearch, so
						// this chain needs no analysis-icu and no smartcn.
						// cjk_width folds full-width Latin and half-width
						// katakana onto their normal forms before anything
						// else looks at the text; skipping it makes "ＡＢＣ"
						// and "ABC" two different terms.
						analyzerCJKText: map[string]any{
							"tokenizer": "standard",
							"filter":    []string{"cjk_width", "lowercase", filterCJKBigram},
						},
					},
				},
			},
		},
	}

	fields := esquery.AllFields()
	rels := esquery.AllRelationships()
	mappings := map[string]any{}

	for _, docType := range docTypes {
		props := map[string]any{}

		for _, f := range fields {
			props[f] = map[string]any{
				"type": textType,
				"fields": map[string]any{
					"raw": map[string]any{
						"type":                  textType,
						"analyzer":              analyzerEnglishExact,
						"search_analyzer":       "english",
						"search_quote_analyzer": analyzerEnglishExact,
					},
					"keywords": map[string]any{
						"type":     textType,
						"analyzer": analyzerLetterStop,
					},
					"stems": map[string]any{
						"type":     textType,
						"analyzer": analyzerEnglishStem,
					},
					// The three subfields above are English-only chains: the
					// letter tokenizer swallows a run of Han characters whole,
					// and the standard one shatters it into single characters.
					// This one is the only place CJK text is segmented in a
					// way a query can hit.
					esquery.SubfieldCJK: map[string]any{
						"type":     textType,
						"analyzer": analyzerCJKText,
					},
				},
			}
		}

		for _, rel := range rels {
			if b.version >= 5 {
				props[rel] = map[string]any{
					"type":           "keyword",
					"include_in_all": false,
					"doc_values":     false,
				}
			} else {
				props[rel] = map[string]any{
					"type":           "string",
					"index":          "not_analyzed",
					"include_in_all": false,
				}
			}
			props[rel+"_ts"] = map[string]any{
				"type":           "date",
				"include_in_all": false,
			}
		}

		props["dateCreated"] = map[string]any{"type": "date"}
		props["lastModified"] = map[string]any{"type": "date"}

		mappings[docType] = map[string]any{
			"properties": props,
		}
	}

	data["mappings"] = mappings
	return data
}

// Utility helpers

func jsonPath(m map[string]any, keys ...string) any {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

// configDeepMatch reports whether actual contains everything required, with
// the same values. It is a containment check rather than an equality one
// because Elasticsearch echoes back a great many defaults nobody asked for.
//
// _all is exempt: the cluster drops it from what it returns on versions that
// no longer support it, and requiring it would report every healthy index as
// insane.
func configDeepMatch(actual, required map[string]any) bool {
	for key, rval := range required {
		aval, exists := actual[key]
		if !exists {
			if key == "_all" {
				continue
			}
			return false
		}

		rmap, rIsMap := rval.(map[string]any)
		if rIsMap {
			amap, aIsMap := aval.(map[string]any)
			if !aIsMap {
				return false
			}
			if !configDeepMatch(amap, rmap) {
				return false
			}
			continue
		}

		if normalizeConfigValue(aval) != normalizeConfigValue(rval) {
			return false
		}
	}
	return true
}

// normalizeConfigValue renders a value as text. Elasticsearch returns settings
// as strings ("true", "0-2") where this package writes them as typed values,
// so comparing them any other way reports a mismatch that is not one.
func normalizeConfigValue(v any) string {
	switch val := v.(type) {
	case bool:
		if val {
			return "true"
		}
		return "false"
	case string:
		return val
	case float64:
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%v", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func mergeAny(a, b map[string]any) map[string]any {
	result := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		result[k] = v
	}
	for k, v := range b {
		result[k] = v
	}
	return result
}

func sortedRoles(roles map[string]bool) []string {
	out := make([]string, 0, len(roles))
	for r := range roles {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
