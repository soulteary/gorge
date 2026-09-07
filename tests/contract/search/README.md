# search contract fixtures

Fixtures for `gorge-search`. See [../README.md](../README.md) for the file
format and the runner requirements.

| Fixture | Pins |
|---|---|
| `index-document.json` | A complete document is 200 and echoes `data.phid`. Its body carries the four-character field and relationship names, which are the literal index keys. |
| `index-cjk-document.json` | A Han corpus is an ordinary document, not a 400. |
| `index-missing-phid.json`, `index-missing-type.json` | An incomplete document is 400, not 502: it never reached a store, so nothing downstream judged anything. |
| `index-malformed-body.json` | Invalid JSON is 400 `ERR_BAD_REQUEST`, not 500. |
| `query-documents.json` | The answer is `data.phids` and `data.count` — identifiers only, never document bodies. |
| `query-unfiltered-listing.json` | An empty query is a listing, not an error. |
| `query-owner-states.json` | `withAnyOwner` and `withUnowned` travel as their own fields, separate from `ownerPHIDs`. |
| `init-index.json` | `docTypes` is what an index is built from; the reply is `data.status: initialized`. |
| `init-missing-doctypes.json`, `sane-missing-doctypes.json` | An empty `docTypes` is 400 on both endpoints. |
| `index-exists.json`, `sane-index.json` | `data.exists` and `data.sane` are booleans, and `false` is a normal answer on both. |
| `stats-index.json` | `GET /api/search/stats` answers an open map carrying at least `documents`. |
| `list-backends.json` | Every backend entry carries `type`, `index` and `roles`, and no credential. |
| `unauthorized.json` | A missing service token is 401 `ERR_UNAUTHORIZED` with no `data` field. |
| `token-via-query-param.json` | `?token=` is accepted alongside the `X-Service-Token` header. |
| `unavailable/*.json` | The five domain error codes, each 502. See below. |

The seven paths themselves are part of the contract:
`PhabricatorGorgeFulltextStorageEngine` calls them as written.

## Two directories, because a broken store is a different service

The mailer's fixtures reach a failing adapter through `mailerKeys` in the
request body, so one runner can pin every outcome. This domain has no such
lever: the engine selects a backend by *role*, never by anything in the
request. A store that answers and a store that refuses are therefore two
service configurations, and the fixtures for each need their own runner.

`unavailable/` is that second configuration — one backend that fails every
operation on demand. It is the only way to produce all five domain codes in a
suite with no cluster to break, and producing all five matters because they are
not interchangeable:

- `ERR_SEARCH_FAILED` exists so that a query against a dead cluster is *not* an
  empty result list with 200. That would be indistinguishable from "nothing
  matched", and Phorge's search page would render its empty state for an outage
  — the one outcome nobody investigates.
- `ERR_CHECK_FAILED` exists so that "I could not ask" is not reported as
  `exists: false` or `sane: false`. Both of those readings tell an
  administrator to rebuild the index, which is the most expensive and most
  destructive thing this domain can be told to do, and neither should ever be
  triggered by a connection error.
- `ERR_STATS_FAILED` exists so that a status panel does not render a missing
  number as zero documents, which reads as an empty index.

All five are 502 rather than 500 throughout: they mean a store this service
talks to misbehaved, not that this service broke, and a 500 sends whoever is
debugging to the wrong logs.

## No fixture may depend on another having run

Every fixture in a directory replays against one handler holding one index, and
the runner's file order is not part of the contract. So `index-document.json`
writes a document and `query-documents.json` asserts the *shape* of an answer
rather than which PHIDs came back — `init-index.json` sorts between them and
drops the index. For the same reason `index-exists.json` and `sane-index.json`
pin only that their key is present and boolean.

The round trip that these fixtures deliberately do not assert — a document goes
in, a query for its text brings its PHID back — lives in
`go/internal/search/http_test.go`, where one test owns its own index, and in
`tests/e2e/search.sh`, where it runs against a real Elasticsearch.

## What is pinned elsewhere, and why not here

**`storage_bytes`.** It is the single snake_case name on this service's wire,
kept because Phorge's cluster panel reads it by that spelling; renaming it to
`storageBytes` blanks a column with no error anywhere. It is not asserted here
because it is an Elasticsearch key — a Meilisearch deployment reports
`documents` and `indexing` instead, and a fixture demanding it would be
unrunnable against half the supported backends. `stats-index.json` therefore
pins only `documents`, and `storage_bytes` is pinned in
`go/internal/search/engine/elasticsearch/backend_test.go`.

**The CJK analyser chain.** `index-cjk-document.json` proves a Han document
survives the wire, and that is all it can prove. Whether the text is *findable*
depends on the `cjk` subfield and the `cjk_bigram` filter inside Elasticsearch,
which no fixture running against an in-memory backend can observe. The mapping
is pinned in `elasticsearch/backend_test.go`; the behaviour is checked by
`tests/e2e/search.sh`.

**Fan-out across backends.** A write goes to every backend with the `write`
role and a read goes to the first with `read` that answers. That is engine
behaviour with no wire representation at all — the response looks identical
whether one backend or three accepted the document — so it lives in
`go/internal/search/engine/engine_test.go`.
