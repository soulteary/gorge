package engine

import (
	"errors"
	"fmt"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// SearchEngine dispatches one operation over the configured backends,
// selecting them by role.
//
// Reads and writes fan out differently, and the asymmetry is the point:
//
//   - A write goes to every backend holding the write role, and any failure is
//     reported. Two write backends mean two indexes kept in step, which is the
//     only way to build a replacement index beside a live one.
//   - A read goes to the first backend holding the read role that answers; the
//     rest are a failover chain.
//
// This is also why a deployment should configure exactly one gorge entry in
// Phorge's cluster.search rather than several. Phorge has read and write roles
// and failover across refs of its own, and splitting the fan-out over both
// layers leaves half the configuration in each. See docs/modules/search.md.
type SearchEngine struct {
	backends []SearchBackend
}

// New wraps the backends in the order they will be tried.
func New(backends []SearchBackend) *SearchEngine {
	return &SearchEngine{backends: backends}
}

// HasBackends reports whether anything at all is configured.
func (se *SearchEngine) HasBackends() bool { return len(se.backends) > 0 }

// Ready reports whether the service can answer a search, and it backs /readyz.
// The single criterion is that at least one backend holds the read role.
//
// It deliberately does not dial Elasticsearch or Meilisearch. Readiness would
// then flip with every third-party hiccup, and the per-host health table plus
// the failover chain already exist to absorb exactly that. So a 200 here means
// "this service can attempt a search", not "the next search will succeed".
//
// What it does catch is the state this domain actually boots into when its
// configuration is wrong or missing: a listening service whose every query
// fails, which /healthz reports as perfectly fine.
func (se *SearchEngine) Ready() error {
	if len(se.backends) == 0 {
		return errors.New("no search backends configured")
	}
	for _, b := range se.backends {
		if b.HasRole("read") {
			return nil
		}
	}
	return errors.New("no search backend has the read role")
}

// BackendInfo describes the configured backends for GET /api/search/backends.
func (se *SearchEngine) BackendInfo() []contracts.BackendInfo {
	result := make([]contracts.BackendInfo, 0, len(se.backends))
	for _, b := range se.backends {
		result = append(result, b.Info())
	}
	return result
}

// IndexDocument writes the document to every backend holding the write role.
//
// Having no write backend at all is an error rather than a no-op. Answering
// "indexed" to a request that stored nothing is the failure mode this domain
// is most prone to: a reindex of a large install would run to completion,
// report success for every document, and leave the index empty.
func (se *SearchEngine) IndexDocument(doc *contracts.Document) error {
	var lastErr error
	written := 0
	for _, b := range se.backends {
		if !b.HasRole("write") {
			continue
		}
		written++
		if err := b.IndexDocument(doc); err != nil {
			lastErr = err
		}
	}
	if written == 0 {
		return errors.New("no writable search backends configured")
	}
	return lastErr
}

// Search returns the PHIDs of the matching documents, from the first read
// backend that answers.
func (se *SearchEngine) Search(q *contracts.SearchQuery) ([]string, error) {
	var lastErr error
	for _, b := range se.backends {
		if !b.HasRole("read") {
			continue
		}
		phids, err := b.Search(q)
		if err != nil {
			lastErr = err
			continue
		}
		return phids, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("all fulltext search backends failed: %w", lastErr)
	}
	return nil, errors.New("no readable search backends configured")
}

// IndexExists asks the first read backend whether its index is there.
func (se *SearchEngine) IndexExists() (bool, error) {
	for _, b := range se.backends {
		if !b.HasRole("read") {
			continue
		}
		return b.IndexExists()
	}
	return false, errors.New("no readable search backends configured")
}

// InitIndex creates the index on every write backend.
//
// This drops the index and everything in it before recreating it, so a full
// reindex has to follow. That is the backends' behaviour rather than a choice
// made here, and it is why the endpoint is reachable only from bin/search and
// never from anything Phorge does while serving.
func (se *SearchEngine) InitIndex(docTypes []string) error {
	var lastErr error
	initialised := 0
	for _, b := range se.backends {
		if !b.HasRole("write") {
			continue
		}
		initialised++
		if err := b.InitIndex(docTypes); err != nil {
			lastErr = err
		}
	}
	if initialised == 0 {
		return errors.New("no writable search backends configured")
	}
	return lastErr
}

// IndexStats reports the first read backend that can produce statistics. A
// backend that fails is skipped rather than reported, because this endpoint
// feeds a status panel, where a second opinion is more useful than an error.
func (se *SearchEngine) IndexStats() (contracts.IndexStats, error) {
	for _, b := range se.backends {
		if !b.HasRole("read") {
			continue
		}
		stats, err := b.IndexStats()
		if err != nil {
			continue
		}
		return stats, nil
	}
	return nil, errors.New("no search backend could report index statistics")
}

// IndexIsSane asks the first read backend whether its live index still matches
// the configuration this service would create today.
//
// Only the first is asked, deliberately: a false from any backend means the
// same thing — reindex — and asking the rest would turn one clear answer into
// a partial one nobody knows what to do with.
func (se *SearchEngine) IndexIsSane(docTypes []string) (bool, error) {
	for _, b := range se.backends {
		if !b.HasRole("read") {
			continue
		}
		return b.IndexIsSane(docTypes)
	}
	return false, errors.New("no readable search backends configured")
}
