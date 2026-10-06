# Search projection extraction

This change implements the first migration boundary and the durable acceptance
primitive. It does **not** switch live search writes or implement the rebuild
controller yet. Existing `/api/search/index`, CLI selection, local indexes and
PHP SearchWorker delegation remain in use.

## Implemented boundary

`PhabricatorFulltextEngine` now exposes:

- `buildFulltextDocument()`: builds and enriches an abstract document without
  running the local indexing extensions or publishing it.
- `indexLocalFulltextDocument(document)`: runs the local extensions after checking
  that the document belongs to the selected object.
- `buildFulltextIndexes()`: the compatible composition, preserving selection,
  build, enrich, local-index and external publication ordering.

`PhabricatorSearchDocumentSerializer` is shared by the existing storage adapter
and the new `search.export` Conduit method. The exporter requires the configured
Conduit service token independently of the gateway. It accepts 1–50 unique PHIDs,
limits the returned document payloads to 2 MiB in total, and preserves input order.
Each item reports `exported`, `missing`, `unsupported`, `too-large` or `retry`.
The returned `materialized:false` explicitly means these are previews, without a
reserved durable revision or a deletion receipt. A consumer must not publish a
`missing` result as a tombstone, or allocate a revision from arrival time.
This endpoint does not provide a transactionally consistent multi-database
snapshot or update local index versions.

## PHP persistent capture (shadow only)

Apply `20261006.search.01.gorgeprojection.sql` using `bin/storage upgrade`.
`PhabricatorSearchProjectionPublisher` allocates a signed int64 revision per
namespace/PHID under a database row lock and commits state plus outbox together.
The generated event ID hashes namespace/PHID, so it fits the 128-byte contract.
Unchanged content/source/serializer reuses the original immutable event. Explicit
force allocates a new revision; authoritative deletion and subsequent restoration
do not reset the counter. Overflow is rejected. Keep source outbox receipts:
removing the last receipt makes an unchanged publication fail rather than invent
another event.

`gorge.search.projection-shadow` defaults to false. When explicitly enabled, the
Gorge PHP storage adapter captures the snapshot before its existing synchronous
write. It does not switch production delivery ownership. Capture failure causes
SearchWorker retry; index extension versions are not marked current. The worker
also re-evaluates versions after its locked object reload and releases locks for
all Throwables.

The snapshot transaction is atomic within the search database, **not** with the
original business edit in another database. Callers must build live snapshots
under the existing object index lock. The publisher serializes publication, not
all source reads. Its default source token is the content hash; this is not a
complete business dependency version. `search.export` remains an unmaterialized
preview. Deletion capture has an explicit helper, but is not yet wired into the
object destruction transaction; export `missing` remains non-authoritative.

## Versioned protocol

`contracts.SearchProjection` and `search/projection` define version 1 envelopes.
They validate identity, namespace, canonical positive int64 revision strings,
upsert/delete shape, serializer/source versions and the payload hash. The decoder
rejects unknown fields, trailing JSON and invalid UTF-8. Full document replacement
preserves repeated field tuples and relationship timestamps.

PHP and Go share a restricted canonical hash: sorted object keys, preserved array
order, UTF-8 strings and integer document timestamps. It is not a general JSON
canonicalization implementation. Both test suites use the same golden hash,
including Chinese, HTML-sensitive characters and slashes.

## Durable acceptance primitive

`projection.MySQLStore.Accept` transactionally records:

- an immutable event receipt and full envelope;
- the identity of every observed object revision;
- the latest object envelope;
- one pending delivery per immutable backend/generation target.

Same-event replay returns its original receipt and original targets. A payload
change under the same event ID or object revision is rejected. Older revisions
are acknowledged as superseded without scheduling writes. New generations can
receive an existing revision through a distinct event ID; replaying the original
receipt does not implicitly change its targets.

The schema is exported as `projection.Schema`. Production startup does not
create it. Ingress and source relay are opt-in; ES shadow delivery is separately
opt-in through `projection.deliveries`. Receipts mean accepted, never applied
or visible. Backend completion is recorded in delivery rows.

## Meilisearch completion

The existing synchronous adapter now submits a full replacement, validates the
returned `taskUid` (including UID zero), and polls that exact task until success.
Failed/canceled/unknown/mismatched tasks cannot report indexed. A deadline leaves
completion unknown; the call does not cancel a task already accepted by Meili.
Initialization checks delete, creation and settings receipts separately. Only a
delete HTTP 404 or a failed deletion task with `index_not_found` is ignored. Queue-wide idle polling and silent timeout success
have been removed. Index initialization remains destructive; it is not an online
rebuild implementation.

`SubmitDocument`, `TaskState` and `WaitTask` expose the adapter boundary needed
for a future durable consumer to persist task UIDs rather than blocking one
HTTP request. The durable generation lane/unknown-submit recovery is not yet
implemented; these methods must not be treated as cross-worker ordering fences.

## Verification

Run the PHP suite with the supported Arcanist checkout:

```
GORGE_TEST_ARCANIST_DIR=/path/to/arcanist php tests/contract/search/projection.php
```

Run Go tests, optionally with isolated real services:

```
GORGE_TEST_SEARCH_MYSQL_DSN='root:test@tcp(127.0.0.1:3306)/' \
GORGE_TEST_MEILI_URL=http://127.0.0.1:7700 \
GORGE_TEST_MEILI_KEY=test \
go test -race ./internal/search/...
```

The MySQL integration creates/drops its own randomly named database. Meili uses a
randomly named index. Use dedicated test services and credentials.

## Remaining migration steps

1. Integrate durable materialization into batched export and source changes;
   separate exported versions from local and externally applied versions.
   Shadow snapshot capture and per-object revision allocation are implemented.
2. Wire the control store, durable delivery state machine and native wakeups;
   ES versions/tombstones and a polling delivery loop are implemented; native
   wakeups and Meili durable lane recovery remain.
3. Ingress, source relay and ES shadow delivery are opt-in. Validate complete
   shadow generations before switching production external writes.
4. Implement bounded source scans, persisted rebuild jobs/shards, real source
   barriers, generation validation, activation and rollback.
5. Capture business dirty intents and dependency invalidation in their source
   transactions; retain periodic repair for uncovered/cross-database changes.
6. Drain old SQL/Redis tasks and remove only obsolete external execution paths.
   Ferret, Ngram, Edge and the PHP domain builder remain in scope for PHP.

## Follow-up audit

The audit corrected three omissions: inbox rows now retain the complete immutable
historical envelope (`LoadEvent`), PHP relationship timestamps normalize numeric
SQL strings to integers, and the projection decoder rejects duplicate keys and
case-alias collisions before hashing. Tests cover these boundaries. Prototype
control databases created from the earlier schema need an envelope-column
migration and source-outbox replay before consumers can use historical events;
`CREATE TABLE IF NOT EXISTS` does not upgrade an existing table. A latest head
cannot reconstruct lost historical payloads.

The module supports opt-in ES shadow delivery. Production cutover blockers are:

- Meili task receipts and unknown-submit/lane recovery remain unimplemented;
- generation validation, read activation and rollback remain unimplemented;
- source transactions, authoritative destruction and dependency invalidation are
  not covered by shadow capture; default sourceVersion is only a document hash;
- rebuild scan/job/barrier/validation/activation/rollback remain unimplemented;
- the legacy CLI force flag does not request a forced projection revision;
- periodic backend health, operational alerting and cleanup policies still
  need integration before production operational use;
- tests still need actual PHP concurrent publishers, Worker capture-failure
  classification, real object exports and full source-to-backend crash recovery.

Do not remove legacy SearchWorker/RebuildIndexesWorker or enable outbox-only
production delivery on the strength of the current primitive-level tests.


## Opt-in durable ingress and source relay

`gorge-search` now accepts a `projection` object in its JSON config (or the same
JSON in `GORGE_SEARCH_PROJECTION`):

```json
{"controlDSN":"USER:PASSWORD@tcp(CONTROL_HOST:3306)/CONTROL_DATABASE","namespace":"default","targets":[{"backendID":"es-shadow","generationID":"g1"}],"sourceOutboxDSN":"USER:PASSWORD@tcp(PHORGE_DB_HOST:3306)/PHORGE_SEARCH_DATABASE"}
```

For a fresh control database, apply `resources/sql/search/schema.sql` explicitly
in that dedicated database. It matches `projection.Schema`; the consistency is
tested. For existing control databases, apply only the missing upgrades. Startup probes all required tables/columns and requires a
nonempty service token. It never creates or upgrades tables. `sourceOutboxDSN`
is optional: when set, the service polls `search_gorgeoutbox` and atomically
accepts each valid event before acknowledging the source row. A failed source
acknowledgment is replayed safely; invalid rows retain retry state without
blocking later valid rows. Source rows are retained. Target IDs are selected
by configuration, never by the request body. Enabled ES deliveries bind their
configuration and physical index UUID in the control database.

Header-token-only endpoints:

- `GET /api/search/projections/capabilities`: protocol, namespace, targets,
  size limit, `inspection`, and `backendDelivery` (true only when configured ES
  workers start).
- `POST /api/search/projections`: one complete projection; returns a durable
  `accepted` or `superseded` receipt. Conflicting event/revision identities return
  409, malformed/cross-namespace events 400, database failures 503.

When ingress is enabled, the process HTTP body limit is 3 MiB so a valid 2 MiB document plus envelope is
not rejected prematurely; the strict decoder still enforces its smaller
projection limit. Existing synchronous `/index` writes remain active. The relay
only acknowledges acceptance. Without `deliveries`, pending delivery rows
accumulate; with configured ES workers, they are drained asynchronously.
Do not enable it as the sole production indexing path. Readiness
checks the control database as well as the existing search engine; relay errors
are retried with bounded backoff and sanitized diagnostics.


## Durable delivery leases

Delivery rows now support `pending → running → applied|superseded`, with a
fenced `running → pending` retry transition. `Claim` uses MySQL 8 `SKIP LOCKED`
and the database clock; it increments the lease epoch and attempt count.
`Renew` and `Finish` require the same event identity, owner, epoch, running state
and an unexpired lease. Stale owners cannot renew, acknowledge, mark superseded
or reschedule a row after takeover. Retry delays are persisted and bounded to
one hour. `appliedEpoch` records application, never search visibility.

Lease duration must be whole seconds between one second and one hour. Owners
are valid namespace-style IDs. Renewals explicitly increment a counter so two
heartbeats in the same database second cannot be mistaken for a lost lease
because MySQL reports zero changed rows.

Existing acceptance-only control databases need the explicit, apply-once
migration `resources/sql/search/20261006.delivery-leases.sql`. Fresh databases
use the updated `projection.Schema`. Startup now checks these columns and
refuses an outdated schema. No startup ALTER or table creation occurs.

Real MySQL tests cover exclusive claims, renewal, expiry/takeover, fencing of
all old-owner result paths, persisted retry backoff, and completed rows staying
terminal. The ES delivery worker renews every 10 seconds under a 30-second lease,
bounds each attempt to two minutes and cancels requests if renewal fails. SQL
leases alone cannot stop an already issued backend request: ES external versions
provide the independent backend fence. Meili delivery remains disabled.


## Opt-in ES shadow delivery

Add `deliveries` inside the projection configuration:

```json
{"deliveries":[{"backendID":"es-shadow","generationID":"g1","backend":{"type":"elasticsearch","version":8,"hosts":["http://ES_HOST:9200"],"index":"phorge_shadow_g1","timeout":15,"options":{"projection":"true"}}}]}
```

Each accepted target needs exactly one configured delivery when delivery is
active. Only ES 7/8, one endpoint per target, and explicit physical index names
are accepted. URL credentials/API-key authentication are not supported by this
adapter. The physical index must be distinct from every legacy backend index.
Meili and older ES versions are rejected before workers start.

Provision the fresh physical generation explicitly using the ES backend's
`InitIndex` with `options.projection=true`; initialization is destructive and
must target a fresh shadow index. Startup never initializes or deletes an index.
It verifies the actual ES major version, physical index UUID and `_gorge`
metadata mapping. Apply `resources/sql/search/20261006.delivery-targets.sql`
when upgrading a control database created before target bindings existed.
Target identities pin the configuration hash and index UUID; changing either
is refused. One physical UUID cannot be reused for another target/generation.
Do not delete/recreate a generation or retarget its endpoint while workers run;
stop it and create a new generation/index identity instead.

Writes use `version_type=external` with the allocated positive int64 revision.
HTTP 409 triggers a realtime GET: matching revision/hash/operation is applied;
a newer revision with compatible namespace/generation is superseded; every
unverified conflict remains retryable. Deletes are full replacement soft
tombstones with preserved external versions; no HTTP DELETE is used for object
removal. Projection-mode queries exclude `_gorge.deleted=true`, and ordinary
unversioned IndexDocument calls are rejected in projection mode. Applied means
backend acceptance, not query visibility. Tombstones have no automatic cleanup.

Tests against real MySQL and ES 8 cover API acceptance through persisted delivery
into a searchable backend, restart binding, retry state, same-revision replay,
newer tombstones blocking old writes, tombstone filtering and later restoration.
Set `GORGE_TEST_ES_URL` alongside `GORGE_TEST_SEARCH_MYSQL_DSN`; CI provisions
both services. ES 7 compatibility is not yet covered by a live CI service.
The complete rebuild/source-intent migration is still required before removing
PHP synchronous execution or activating these shadow generations for live reads.


## Read-only delivery inspection

The header-token-only inspection routes are enabled with the durable ingress:

- `GET /api/search/projections/status?eventID=ENCODED_EVENT_ID` returns the
  immutable original receipt, the current `latestRevision`, and each original
  target's delivery state. `eventID` is a query parameter because IDs commonly
  contain slashes; URL-encode it. An unknown receipt returns 404. Malformed IDs
  return 400. Database/corrupt-record failures return sanitized 503 responses.
- `GET /api/search/projections/stats` returns per-configured-target pending,
  running, applied, superseded, retrying, expired-running and age-unknown counts,
  plus the oldest known pending/running creation epoch. When a source outbox is
  configured, `sourceOutbox` separately reports pending, due and retrying counts.
  If that source database is unavailable, the endpoint returns 503 rather than
  presenting control-store statistics as the complete queue state.

Event status uses the original receipt's target/revision identity, including
when two event IDs shared one delivery row. It never substitutes the latest
head for an older event. A superseded acceptance has no scheduled deliveries.
The original `accepted` receipt does not change after a backend application;
inspect `deliveries[].status` to observe application. No route reports query
visibility, returns document bodies or exposes lease owners/credentials.
Inspection does not renew leases, requeue deliveries or modify receipts.

Control-store inspection uses a read-only repeatable-read transaction and the
database clock. Source counts come from a separate source-database statement
and carry their own `observedEpoch`; this is not a cross-database atomic snapshot.
Relay eligibility, acknowledgement and retry scheduling now also use the source
database clock, avoiding disagreement caused by host clock skew.

Apply `resources/sql/search/20261006.delivery-inspection.sql` once to existing
control databases. New delivery creation times use the database clock. Earlier
rows retain `createdEpoch=0` and contribute to `unknownPendingAge`; the upgrade
does not invent historical creation times. `oldestPendingEpoch=null` means no
known age, not necessarily no backlog. The PHP source outbox has no creation
time: source `ageAvailable` remains false. These epochs measure control delivery
age, not source-to-search latency. Aggregate queries cover retained history;
use bounded operational polling rather than treating them as cheap per-request
metrics. Requests have a five-second inspection timeout.

Live tests cover historical receipt inspection, shared-revision receipts,
namespace isolation, expired leases, unknown ages, source poison rows, sanitized
source failures, and actual heartbeat takeover cancelling an in-flight writer
without changing the new owner's lease. ES conflict verification now requires
an explicit delete marker and a valid hash before classifying a replay/newer
version; missing metadata is never treated as a successful write.
