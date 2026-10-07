// Package meilisearch implements the search domain's Meilisearch backend.
//
// It is the alternative to Elasticsearch for installs that do not want a JVM.
// Its coverage is the weakest spot in this domain and is registered as such in
// docs/findings.md; the fixtures exercise the HTTP layer through the test
// backend instead, so nothing here is asserted by the contract suite.
package meilisearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/search/engine"
	"github.com/soulteary/gorge/go/internal/search/esquery"
)

// Backend talks to one Meilisearch instance.
//
// Meilisearch has no sharding, so "hosts" is a list of replicas behind a load
// balancer and only the first is used. There is no per-host health table here
// for the same reason.
//
// The concepts map across as follows:
//
//   - an Elasticsearch index becomes a Meilisearch index (uid);
//   - an Elasticsearch type becomes a filterable "docType" attribute;
//   - an Elasticsearch _id becomes the primary key "id", which is the PHID;
//   - document fields are flattened into top-level attributes, under the same
//     four-character Phorge names.
//
// CJK needs no special handling here: Meilisearch segments Chinese, Japanese
// and Korean text natively, which is why the cjk subfield the Elasticsearch
// backend adds has no counterpart in this file.
type Backend struct {
	host    string
	index   string
	apiKey  string
	timeout int
	roles   map[string]bool
	client  *http.Client
}

const defaultTimeoutSec = 15

// New builds the backend from its definition.
func New(def engine.BackendDef) *Backend {
	b := &Backend{
		index: def.Index,
		roles: make(map[string]bool),
	}
	if b.index == "" {
		b.index = engine.DefaultIndexName
	}
	if len(def.Hosts) > 0 {
		host := def.Hosts[0]
		if !strings.Contains(host, "://") {
			proto := def.Protocol
			if proto == "" {
				proto = "http"
			}
			host = proto + "://" + host
		}
		b.host = strings.TrimRight(host, "/")
	}
	b.apiKey = def.APIKey
	b.timeout = def.Timeout
	if b.timeout == 0 {
		b.timeout = defaultTimeoutSec
	}
	for _, r := range def.Roles {
		b.roles[r] = true
	}
	if len(b.roles) == 0 {
		b.roles["read"] = true
		b.roles["write"] = true
	}
	b.client = &http.Client{Timeout: time.Duration(b.timeout) * time.Second}
	return b
}

func (b *Backend) Type() string { return "meilisearch" }

func (b *Backend) HasRole(role string) bool { return b.roles[role] }

// Info reports a single "host" where the Elasticsearch backend reports a
// "hosts" list. The difference is real and is part of the wire; see
// contracts.BackendInfo.
func (b *Backend) Info() contracts.BackendInfo {
	return contracts.BackendInfo{
		"type":  b.Type(),
		"host":  b.host,
		"index": b.index,
		"roles": sortedRoles(b.roles),
	}
}

// IndexDocument preserves the synchronous API: accepted is not indexed.
func (b *Backend) IndexDocument(doc *contracts.Document) error {
	return b.IndexDocumentContext(context.Background(), doc)
}

// IndexDocumentContext shares the engine/caller deadline across submission and
// task confirmation, while retaining this backend's shorter configured timeout.
func (b *Backend) IndexDocumentContext(ctx context.Context, doc *contracts.Document) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.timeout)*time.Second)
	defer cancel()
	uid, err := b.SubmitDocument(ctx, doc)
	if err != nil {
		return err
	}
	return b.WaitTask(ctx, uid)
}

// SubmitDocument returns a receipt for durable delivery callers to persist.
// POST is the full-replacement API; PUT would retain removed attributes.
func (b *Backend) SubmitDocument(ctx context.Context, doc *contracts.Document) (int64, error) {
	body, err := json.Marshal([]any{b.buildDocument(doc)})
	if err != nil {
		return 0, fmt.Errorf("marshal document: %w", err)
	}
	url := fmt.Sprintf("%s/indexes/%s/documents", b.host, b.index)
	response, err := b.doRequestContext(ctx, url, http.MethodPost, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	return decodeTaskReceipt(response)
}

func decodeTaskReceipt(response []byte) (int64, error) {
	var receipt struct {
		UID *int64 `json:"taskUid"`
	}
	if json.Unmarshal(response, &receipt) != nil || receipt.UID == nil || *receipt.UID < 0 {
		return 0, fmt.Errorf("meilisearch accepted a write without a valid task receipt")
	}
	return *receipt.UID, nil
}

// TaskState checks one receipt, never the queue-wide idle state.
func (b *Backend) TaskState(ctx context.Context, uid int64) (string, error) {
	if uid < 0 {
		return "", fmt.Errorf("invalid task UID")
	}
	body, err := b.doRequestContext(ctx, fmt.Sprintf("%s/tasks/%d", b.host, uid), http.MethodGet, nil)
	if err != nil {
		return "", err
	}
	var task struct {
		UID    *int64 `json:"uid"`
		Status string `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &task); err != nil {
		return "", fmt.Errorf("invalid task response: %w", err)
	}
	if task.UID == nil || *task.UID != uid {
		return "", fmt.Errorf("meilisearch task receipt mismatch")
	}
	switch task.Status {
	case "enqueued", "processing", "succeeded":
		return task.Status, nil
	case "failed", "canceled":
		return task.Status, &TaskFailure{UID: uid, Status: task.Status, Code: task.Error.Code}
	default:
		return "", fmt.Errorf("unknown meilisearch task status %q", task.Status)
	}
}

func (b *Backend) WaitTask(ctx context.Context, uid int64) error {
	for {
		state, err := b.TaskState(ctx, uid)
		if err != nil {
			return err
		}
		if state == "succeeded" {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("meilisearch task %d completion unknown: %w", uid, ctx.Err())
		case <-timer.C:
		}
	}
}

func (b *Backend) Search(q *contracts.SearchQuery) ([]string, error) {
	req := b.buildSearchRequest(q)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal search request: %w", err)
	}

	url := fmt.Sprintf("%s/indexes/%s/search", b.host, b.index)
	respBody, err := b.doRequest(url, http.MethodPost, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	var resp msSearchResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON from meilisearch: %w", err)
	}

	phids := make([]string, 0, len(resp.Hits))
	for _, hit := range resp.Hits {
		if id, ok := hit["id"].(string); ok {
			phids = append(phids, id)
		}
	}
	return phids, nil
}

func (b *Backend) IndexExists() (bool, error) {
	url := fmt.Sprintf("%s/indexes/%s", b.host, b.index)
	_, err := b.doRequest(url, http.MethodGet, nil)
	if err != nil {
		if strings.Contains(err.Error(), "status 404") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// InitIndex drops the index and recreates it. Everything in it is gone
// afterwards and a full reindex has to follow.
func (b *Backend) InitIndex(_ []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(b.timeout)*time.Second)
	defer cancel()
	response, err := b.doRequestContext(ctx, fmt.Sprintf("%s/indexes/%s", b.host, b.index), http.MethodDelete, nil)
	if err != nil {
		var remote *responseError
		if !errors.As(err, &remote) || remote.Status != http.StatusNotFound {
			return err
		}
	} else if err := b.waitReceipt(ctx, response); err != nil {
		var failed *TaskFailure
		if !errors.As(err, &failed) || failed.Status != "failed" || failed.Code != "index_not_found" {
			return err
		}
	}
	createBody, err := json.Marshal(map[string]string{"uid": b.index, "primaryKey": "id"})
	if err != nil {
		return err
	}
	response, err = b.doRequestContext(ctx, b.host+"/indexes", http.MethodPost, bytes.NewReader(createBody))
	if err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	if err := b.waitReceipt(ctx, response); err != nil {
		return err
	}
	return b.configureIndex(ctx)
}

func (b *Backend) waitReceipt(ctx context.Context, response []byte) error {
	uid, err := decodeTaskReceipt(response)
	if err != nil {
		return err
	}
	return b.WaitTask(ctx, uid)
}

func (b *Backend) IndexStats() (contracts.IndexStats, error) {
	url := fmt.Sprintf("%s/indexes/%s/stats", b.host, b.index)
	body, err := b.doRequest(url, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}

	var stats msIndexStats
	if err := json.Unmarshal(body, &stats); err != nil {
		return nil, fmt.Errorf("invalid stats JSON: %w", err)
	}

	// Two keys where Elasticsearch reports four. The panel reading this has to
	// tolerate the difference; see contracts.IndexStats.
	return contracts.IndexStats{
		"documents": stats.NumberOfDocuments,
		"indexing":  stats.IsIndexing,
	}, nil
}

// IndexIsSane compares the live settings against the ones configureIndex
// writes. Document types play no part: Meilisearch has one flat schema per
// index and carries the type as an ordinary filterable attribute.
func (b *Backend) IndexIsSane(_ []string) (bool, error) {
	exists, err := b.IndexExists()
	if err != nil || !exists {
		return false, err
	}

	url := fmt.Sprintf("%s/indexes/%s/settings", b.host, b.index)
	body, err := b.doRequest(url, http.MethodGet, nil)
	if err != nil {
		return false, err
	}

	var settings msSettings
	if err := json.Unmarshal(body, &settings); err != nil {
		return false, fmt.Errorf("invalid settings JSON: %w", err)
	}

	expectedSearchable := b.searchableAttributes()
	if len(settings.SearchableAttributes) != len(expectedSearchable) {
		return false, nil
	}

	expectedFilterable := b.filterableAttributes()
	filterSet := make(map[string]bool, len(settings.FilterableAttributes))
	for _, f := range settings.FilterableAttributes {
		filterSet[f] = true
	}
	for _, f := range expectedFilterable {
		if !filterSet[f] {
			return false, nil
		}
	}

	return true, nil
}

// Internal helpers

func (b *Backend) configureIndex(ctx context.Context) error {
	settings := msSettings{
		SearchableAttributes: b.searchableAttributes(),
		FilterableAttributes: b.filterableAttributes(),
		SortableAttributes:   b.sortableAttributes(),
		// Only ids leave this service; Phorge loads the objects itself so that
		// its policy checks stay in the loop.
		DisplayedAttributes: []string{"id", "docType"},
		RankingRules: []string{
			"words", "typo", "proximity", "attribute", "sort", "exactness",
		},
	}
	body, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal index settings: %w", err)
	}
	url := fmt.Sprintf("%s/indexes/%s/settings", b.host, b.index)
	response, err := b.doRequestContext(ctx, url, http.MethodPatch, bytes.NewReader(body))
	if err != nil {
		return err
	}
	return b.waitReceipt(ctx, response)
}

func (b *Backend) searchableAttributes() []string {
	fields := esquery.AllFields()
	attrs := make([]string, 0, len(fields)+1)
	attrs = append(attrs, "title")
	attrs = append(attrs, fields...)
	return attrs
}

func (b *Backend) filterableAttributes() []string {
	rels := esquery.AllRelationships()
	attrs := make([]string, 0, len(rels)+2)
	// "id" is here because buildFilters renders SearchQuery.Exclude as
	// `id != {phid}`, and Meilisearch rejects a filter on any attribute that is
	// not declared filterable — so without it every excluding query fails with a
	// 400 instead of returning one fewer result. It is the primary key, which is
	// exactly why it is easy to assume it needs no declaration; Meilisearch
	// makes no exception for it.
	attrs = append(attrs, "id", "docType")
	attrs = append(attrs, rels...)
	return attrs
}

func (b *Backend) sortableAttributes() []string {
	return []string{"dateCreated", "lastModified"}
}

func (b *Backend) buildDocument(doc *contracts.Document) map[string]any {
	m := map[string]any{
		"id":           doc.PHID,
		"docType":      doc.Type,
		"title":        doc.Title,
		"dateCreated":  doc.DateCreated,
		"lastModified": doc.DateModified,
	}

	for _, f := range doc.Fields {
		key := f.Name
		existing, ok := m[key]
		if ok {
			if arr, isArr := existing.([]any); isArr {
				arr = append(arr, f.Corpus)
				if f.Aux != "" {
					arr = append(arr, f.Aux)
				}
				m[key] = arr
			} else {
				vals := []any{existing, f.Corpus}
				if f.Aux != "" {
					vals = append(vals, f.Aux)
				}
				m[key] = vals
			}
		} else {
			if f.Aux != "" {
				m[key] = f.Corpus + " " + f.Aux
			} else {
				m[key] = f.Corpus
			}
		}
	}

	for _, r := range doc.Relationships {
		key := r.Name
		existing, ok := m[key]
		if ok {
			if arr, isArr := existing.([]any); isArr {
				m[key] = append(arr, r.RelatedPHID)
			} else {
				m[key] = []any{existing, r.RelatedPHID}
			}
		} else {
			m[key] = []any{r.RelatedPHID}
		}
	}

	return m
}

func (b *Backend) buildSearchRequest(q *contracts.SearchQuery) map[string]any {
	req := map[string]any{
		"q":                    q.Query,
		"attributesToRetrieve": []string{"id"},
	}

	limit := q.Limit
	if limit == 0 {
		limit = defaultResultLimit
	}
	req["limit"] = limit
	req["offset"] = q.Offset

	filters := b.buildFilters(q)
	if len(filters) > 0 {
		req["filter"] = filters
	}

	if q.Query == "" {
		req["sort"] = []string{"dateCreated:desc"}
	}

	return req
}

// defaultResultLimit is Phorge's page size plus one: it asks for one more row
// than it shows in order to know whether a "next page" link is due.
const defaultResultLimit = 101

// buildFilters renders the query's relationship constraints in Meilisearch's
// filter syntax. A nested list is an OR and a top-level entry is an AND, which
// is why the document types go in as one nested list while each PHID goes in
// as its own entry.
func (b *Backend) buildFilters(q *contracts.SearchQuery) []any {
	var filters []any

	if len(q.Types) > 0 {
		typeFilters := make([]string, 0, len(q.Types))
		for _, t := range q.Types {
			typeFilters = append(typeFilters, fmt.Sprintf("docType = %s", t))
		}
		if len(typeFilters) == 1 {
			filters = append(filters, typeFilters[0])
		} else {
			filters = append(filters, typeFilters)
		}
	}

	if q.Exclude != "" {
		filters = append(filters, fmt.Sprintf("id != %s", q.Exclude))
	}

	relMap := map[string][]string{
		esquery.RelAuthor:     q.AuthorPHIDs,
		esquery.RelSubscriber: q.SubscriberPHIDs,
		esquery.RelProject:    q.ProjectPHIDs,
		esquery.RelRepository: q.RepositoryPHIDs,
	}
	// Sorted, so one query always renders to the same filter list.
	for _, field := range sortedFilterKeys(relMap) {
		for _, phid := range relMap[field] {
			filters = append(filters, fmt.Sprintf("%s = %s", field, phid))
		}
	}

	statusSet := make(map[string]bool, len(q.Statuses))
	for _, s := range q.Statuses {
		statusSet[s] = true
	}
	if statusSet[esquery.RelOpen] && !statusSet[esquery.RelClosed] {
		filters = append(filters, fmt.Sprintf("%s EXISTS", esquery.RelOpen))
	} else if !statusSet[esquery.RelOpen] && statusSet[esquery.RelClosed] {
		filters = append(filters, fmt.Sprintf("%s EXISTS", esquery.RelClosed))
	}

	if q.WithUnowned {
		filters = append(filters, fmt.Sprintf("%s EXISTS", esquery.RelUnowned))
	}

	if q.WithAnyOwner {
		filters = append(filters, fmt.Sprintf("%s EXISTS", esquery.RelOwner))
	} else if len(q.OwnerPHIDs) > 0 {
		for _, phid := range q.OwnerPHIDs {
			filters = append(filters, fmt.Sprintf("%s = %s", esquery.RelOwner, phid))
		}
	}

	return filters
}

// HTTP layer

func (b *Backend) doRequest(url, method string, body io.Reader) ([]byte, error) {
	return b.doRequestContext(context.Background(), url, method, body)
}

func (b *Backend) doRequestContext(ctx context.Context, url, method string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("meilisearch request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, &responseError{Status: resp.StatusCode}
	}

	return respBody, nil
}

// TaskFailure exposes only stable classification, never document-bearing messages.
type TaskFailure struct {
	UID    int64
	Status string
	Code   string
}

func (e *TaskFailure) Error() string {
	return fmt.Sprintf("meilisearch task %d %s (%s)", e.UID, e.Status, e.Code)
}

// Keep backend payloads (which may contain document text) out of errors/logs.
type responseError struct{ Status int }

func (e *responseError) Error() string {
	return fmt.Sprintf("meilisearch returned status %d", e.Status)
}

// Response types

type msSearchResponse struct {
	Hits []map[string]any `json:"hits"`
}

type msIndexStats struct {
	NumberOfDocuments int  `json:"numberOfDocuments"`
	IsIndexing        bool `json:"isIndexing"`
}

type msSettings struct {
	SearchableAttributes []string `json:"searchableAttributes,omitempty"`
	FilterableAttributes []string `json:"filterableAttributes,omitempty"`
	SortableAttributes   []string `json:"sortableAttributes,omitempty"`
	DisplayedAttributes  []string `json:"displayedAttributes,omitempty"`
	RankingRules         []string `json:"rankingRules,omitempty"`
}

func sortedRoles(roles map[string]bool) []string {
	out := make([]string, 0, len(roles))
	for r := range roles {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func sortedFilterKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
