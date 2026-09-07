// Package engine holds the backend abstraction for the search domain and the
// dispatcher that fans one operation out over the configured backends.
//
// The concrete backends live in subpackages (elasticsearch, meilisearch) and
// depend on this one, never the other way round. That is also why BackendDef
// lives here rather than beside the service configuration: a backend has to
// read its own settings, and pointing it at internal/search would close a
// cycle.
package engine

import "github.com/soulteary/gorge/go/internal/contracts"

// DefaultIndexName is the index every backend falls back to. Phorge's own
// Elasticsearch engine defaults to the same name, so an existing install can
// be pointed at this service without moving its data.
const DefaultIndexName = "phabricator"

// BackendDef is one configured backend, as it appears in GORGE_SEARCH_BACKENDS
// or in the JSON config file.
//
// Roles are what make a migration possible: a backend holding only "write" is
// filled but never read from, which is how a replacement index is built up
// beside a live one. An empty Roles means both.
type BackendDef struct {
	Type     string   `json:"type"`
	Hosts    []string `json:"hosts"`
	Index    string   `json:"index"`
	Version  int      `json:"version"`
	Roles    []string `json:"roles"`
	Timeout  int      `json:"timeout"`
	Protocol string   `json:"protocol"`
	APIKey   string   `json:"apiKey,omitempty"`

	// Options carries backend-specific switches that do not deserve a field of
	// their own. Only the test backend reads it today, and it is how the
	// contract fixtures reach the failure paths behind the five domain error
	// codes; see test.go.
	Options map[string]string `json:"options,omitempty"`
}

// SearchBackend is one fulltext store. Each implementation owns its protocol,
// its query translation and its index management, so adding a backend never
// touches the HTTP layer.
type SearchBackend interface {
	Type() string
	HasRole(role string) bool

	IndexDocument(doc *contracts.Document) error
	Search(q *contracts.SearchQuery) ([]string, error)
	IndexExists() (bool, error)
	InitIndex(docTypes []string) error
	IndexStats() (contracts.IndexStats, error)
	IndexIsSane(docTypes []string) (bool, error)

	Info() contracts.BackendInfo
}
