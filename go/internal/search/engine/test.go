package engine

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// TestBackend is an in-memory fulltext store. It backs tests/e2e/search.sh and
// the contract fixtures, and it is also what a deployment configures to prove
// the wiring works before an Elasticsearch or Meilisearch instance exists.
//
// Its failure injection is the reason it is production code rather than a
// _test.go helper. This domain has five error codes, all 502, and all five
// describe a downstream store misbehaving — which no real backend can be asked
// to do on demand. Without an injectable backend only ERR_SEARCH_FAILED is
// reachable from a fixture and the other four would be asserted nowhere.
//
// Its matching is deliberately crude: a case-insensitive substring scan over
// the title and every field corpus. It exists to make "a document went in and
// came back out" assertable, not to be a search engine. Relevance, stemming
// and CJK segmentation are properties of the real backends and are not
// imitated here — a fixture that passed against this and failed against
// Elasticsearch would be worse than no fixture at all.
type TestBackend struct {
	roles map[string]bool
	index string

	// fail names the operations to reject, so one fixture can reach one error
	// code. See the failOp constants.
	fail map[string]bool

	mu       sync.Mutex
	docs     map[string]*contracts.Document
	docTypes []string
	inited   bool
	queries  int
}

// The operations TestBackend can be told to fail, as they appear in the
// backend's "fail" option: a comma-separated list.
//
// They are named after the operation rather than after the error code, because
// one code has two routes into it: ERR_CHECK_FAILED is what GET /exists
// reports and also what POST /sane reports, since a sanity check starts by
// asking whether the index is there at all.
const (
	failOpIndex  = "index"
	failOpSearch = "search"
	failOpInit   = "init"
	failOpExists = "exists"
	failOpSane   = "sane"
	failOpStats  = "stats"
)

var knownFailOps = []string{
	failOpIndex, failOpSearch, failOpInit, failOpExists, failOpSane, failOpStats,
}

// NewTestBackend builds the in-memory backend. An unrecognised fail operation
// is an error rather than a shrug: a typo would otherwise turn a fixture that
// means to assert a 502 into one that quietly asserts a 200.
func NewTestBackend(def BackendDef) (*TestBackend, error) {
	b := &TestBackend{
		roles: make(map[string]bool),
		index: def.Index,
		fail:  make(map[string]bool),
		docs:  make(map[string]*contracts.Document),
	}
	if b.index == "" {
		b.index = DefaultIndexName
	}
	for _, r := range def.Roles {
		b.roles[r] = true
	}
	if len(b.roles) == 0 {
		b.roles["read"] = true
		b.roles["write"] = true
	}

	for _, op := range strings.Split(def.Options["fail"], ",") {
		op = strings.TrimSpace(op)
		if op == "" {
			continue
		}
		if !contains(knownFailOps, op) {
			return nil, fmt.Errorf("test backend: unknown fail operation %q: expected one of %s",
				op, strings.Join(knownFailOps, ", "))
		}
		b.fail[op] = true
	}

	return b, nil
}

func (b *TestBackend) Type() string { return "test" }

func (b *TestBackend) HasRole(role string) bool { return b.roles[role] }

func (b *TestBackend) Info() contracts.BackendInfo {
	return contracts.BackendInfo{
		"type":  b.Type(),
		"host":  "memory",
		"index": b.index,
		"roles": sortedKeys(b.roles),
	}
}

func (b *TestBackend) IndexDocument(doc *contracts.Document) error {
	if b.fail[failOpIndex] {
		return fmt.Errorf("test backend: injected index failure for %q", doc.PHID)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	stored := *doc
	b.docs[doc.PHID] = &stored
	return nil
}

func (b *TestBackend) Search(q *contracts.SearchQuery) ([]string, error) {
	if b.fail[failOpSearch] {
		return nil, fmt.Errorf("test backend: injected search failure")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.queries++

	needle := strings.ToLower(q.Query)

	phids := make([]string, 0, len(b.docs))
	for phid, doc := range b.docs {
		if len(q.Types) > 0 && !contains(q.Types, doc.Type) {
			continue
		}
		if q.Exclude != "" && phid == q.Exclude {
			continue
		}
		if needle != "" && !documentContains(doc, needle) {
			continue
		}
		phids = append(phids, phid)
	}

	// Map iteration order is random, and a fixture asserting on the first
	// result would then pass or fail at random. Sorting is not a relevance
	// ranking and does not pretend to be one; it only makes the answer stable.
	sort.Strings(phids)

	if q.Offset >= len(phids) {
		return []string{}, nil
	}
	phids = phids[q.Offset:]
	if q.Limit > 0 && len(phids) > q.Limit {
		phids = phids[:q.Limit]
	}
	return phids, nil
}

func (b *TestBackend) IndexExists() (bool, error) {
	if b.fail[failOpExists] {
		return false, fmt.Errorf("test backend: injected index existence check failure")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inited, nil
}

func (b *TestBackend) InitIndex(docTypes []string) error {
	if b.fail[failOpInit] {
		return fmt.Errorf("test backend: injected index initialisation failure")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	// Mirrors what the real backends do: init drops the index and everything
	// in it, so no fixture can accidentally assert that documents survive one.
	b.docs = make(map[string]*contracts.Document)
	b.docTypes = append([]string(nil), docTypes...)
	b.inited = true
	return nil
}

func (b *TestBackend) IndexStats() (contracts.IndexStats, error) {
	if b.fail[failOpStats] {
		return nil, fmt.Errorf("test backend: injected stats failure")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return contracts.IndexStats{
		"queries":   b.queries,
		"documents": len(b.docs),
		"deleted":   0,
		// snake_case on purpose; see contracts.IndexStats.
		"storage_bytes": 0,
	}, nil
}

func (b *TestBackend) IndexIsSane(docTypes []string) (bool, error) {
	if b.fail[failOpSane] || b.fail[failOpExists] {
		return false, fmt.Errorf("test backend: injected sanity check failure")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.inited {
		return false, nil
	}
	for _, t := range docTypes {
		if !contains(b.docTypes, t) {
			return false, nil
		}
	}
	return true, nil
}

// Documents returns the indexed documents, so a test can assert that what came
// back out is what went in.
func (b *TestBackend) Documents() map[string]*contracts.Document {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make(map[string]*contracts.Document, len(b.docs))
	for k, v := range b.docs {
		cp[k] = v
	}
	return cp
}

func documentContains(doc *contracts.Document, needle string) bool {
	if strings.Contains(strings.ToLower(doc.Title), needle) {
		return true
	}
	for _, f := range doc.Fields {
		if strings.Contains(strings.ToLower(f.Corpus), needle) ||
			(f.Aux != "" && strings.Contains(strings.ToLower(f.Aux), needle)) {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// sortedKeys renders a role set as a stable list. The backends all keep roles
// in a map, and an unsorted range over it would make GET /api/search/backends
// answer a different ordering on every call.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
