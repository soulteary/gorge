// Package search is the fulltext domain: it takes Phorge's documents and
// queries and hands them to whichever store a deployment configured —
// Elasticsearch, Meilisearch, or the in-memory backend used for wiring
// checks. It owns no index of its own. See docs/modules/search.md.
package search

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/engine"
)

// Domain-specific codes, all predating the monorepo and all 502.
//
// They are five rather than one because the caller acts on them differently:
// Phorge's setup checks and bin/search report an initialisation failure
// somewhere quite different from a query failure, and collapsing them would
// leave an operator with "the search service returned 502" and nothing else.
//
// 502 rather than 500 throughout: every one of them means a store this service
// talks to misbehaved, not that this service broke. A 500 here would send
// whoever is debugging to the wrong logs.
const (
	CodeIndexFailed  = "ERR_INDEX_FAILED"
	CodeSearchFailed = "ERR_SEARCH_FAILED"
	CodeInitFailed   = "ERR_INIT_FAILED"
	CodeCheckFailed  = "ERR_CHECK_FAILED"
	CodeStatsFailed  = "ERR_STATS_FAILED"
)

// Deps is everything the search routes need to serve a request.
type Deps struct {
	Engine *engine.SearchEngine
	Token  string
}

// RegisterRoutes mounts the search endpoints.
//
// The paths are named after the domain and must not change: Phorge's
// PhabricatorGorgeFulltextStorageEngine calls them as written. The health
// probes are not registered here — the platform's httpx.New already did that,
// and registering them twice makes Echo panic at startup.
func RegisterRoutes(e *echo.Echo, deps *Deps) {
	g := e.Group("/api/search")
	g.Use(auth.Token(deps.Token))

	g.POST("/index", indexDocument(deps))
	g.POST("/query", searchQuery(deps))
	g.POST("/init", initIndex(deps))
	g.GET("/exists", indexExists(deps))
	g.GET("/stats", indexStats(deps))
	g.POST("/sane", indexIsSane(deps))
	g.GET("/backends", listBackends(deps))
}

// bindJSON decodes the request body, distinguishing a malformed payload from a
// transport-level rejection.
//
// A body over the platform limit surfaces here as Echo's 413 whenever the
// client streams without a Content-Length; handing those back to the platform
// error handler is what keeps them reported as ERR_TOO_LARGE rather than
// flattened into ERR_BAD_REQUEST. Malformed JSON is a genuine 400 and is
// answered here.
//
// It reports whether the caller may continue, and that boolean is the whole
// point of the signature: httpx.Fail returns nil once it has written the
// response, so a helper returning only an error cannot tell "decoded fine"
// from "already answered". Getting that wrong appends a second JSON document
// to a response that has already been sent — which is not a status code
// anyone sees, just two objects in one body.
func bindJSON(c echo.Context, dst any) (bool, error) {
	if err := c.Bind(dst); err != nil {
		var httpErr *echo.HTTPError
		if errors.As(err, &httpErr) && httpErr.Code != http.StatusBadRequest {
			return false, err
		}
		return false, httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
	}
	return true, nil
}

func indexDocument(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var doc contracts.Document
		if ok, err := bindJSON(c, &doc); !ok {
			return err
		}

		// Both are 400 rather than 502: a document this incomplete never
		// reached a store, so nothing downstream has judged anything. The PHID
		// is the document's identity in the index and the type is its mapping,
		// so neither has a usable default.
		if doc.PHID == "" {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "phid is required")
		}
		if doc.Type == "" {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "type is required")
		}

		if err := deps.Engine.IndexDocument(&doc); err != nil {
			return httpx.Fail(c, http.StatusBadGateway, CodeIndexFailed, err.Error())
		}

		return httpx.OK(c, contracts.IndexAck{PHID: doc.PHID, Status: "indexed"})
	}
}

func searchQuery(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var q contracts.SearchQuery
		if ok, err := bindJSON(c, &q); !ok {
			return err
		}

		// An empty query is not an error: Phorge's search UI opens on an
		// unfiltered listing, which arrives here as a query with no text and
		// is answered with the newest documents.
		phids, err := deps.Engine.Search(&q)
		if err != nil {
			return httpx.Fail(c, http.StatusBadGateway, CodeSearchFailed, err.Error())
		}

		return httpx.OK(c, contracts.SearchResults{PHIDs: phids, Count: len(phids)})
	}
}

func initIndex(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req contracts.DocTypesRequest
		if ok, err := bindJSON(c, &req); !ok {
			return err
		}
		if len(req.DocTypes) == 0 {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "docTypes is required")
		}

		if err := deps.Engine.InitIndex(req.DocTypes); err != nil {
			return httpx.Fail(c, http.StatusBadGateway, CodeInitFailed, err.Error())
		}

		return httpx.OK(c, contracts.InitAck{Status: "initialized"})
	}
}

func indexExists(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		exists, err := deps.Engine.IndexExists()
		if err != nil {
			return httpx.Fail(c, http.StatusBadGateway, CodeCheckFailed, err.Error())
		}
		return httpx.OK(c, contracts.IndexPresence{Exists: exists})
	}
}

func indexStats(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		stats, err := deps.Engine.IndexStats()
		if err != nil {
			return httpx.Fail(c, http.StatusBadGateway, CodeStatsFailed, err.Error())
		}
		return httpx.OK(c, stats)
	}
}

func indexIsSane(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req contracts.DocTypesRequest
		if ok, err := bindJSON(c, &req); !ok {
			return err
		}
		// Required here for the same reason as on /init, but the consequence
		// of leaving it out is worse. The sanity check compares the live index
		// against the configuration this service would build for the types it
		// was given, and an empty type list builds an empty expectation, which
		// any index at all satisfies. Answering "sane: true" to a question
		// nobody asked properly is exactly the silent success this domain has
		// to avoid.
		if len(req.DocTypes) == 0 {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "docTypes is required")
		}

		sane, err := deps.Engine.IndexIsSane(req.DocTypes)
		if err != nil {
			return httpx.Fail(c, http.StatusBadGateway, CodeCheckFailed, err.Error())
		}
		return httpx.OK(c, contracts.IndexSanity{Sane: sane})
	}
}

func listBackends(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		return httpx.OK(c, deps.Engine.BackendInfo())
	}
}
