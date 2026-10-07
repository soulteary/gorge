# Search projection extraction

The current implementation includes PHP snapshot extraction and shadow capture,
durable ingress/source relay, fenced delivery, generation backfill and bounded
business-source scans. These are opt-in; enabling PHP shadow capture alone does
not transfer production write ownership or activate a generation. Existing
synchronous indexing and PHP domain builders remain in use. Later sections
specify the separate rollout and recovery boundaries.

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
preview. Deletion capture now has a pre-destruction intent and primary-source recovery
for Lisk fulltext objects destroyed through PhabricatorDestructionEngine (see
below). It is not a distributed business transaction; export `missing` remains
non-authoritative.

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
for a future asynchronous durable consumer to persist task UIDs rather than
blocking one HTTP request. The current durable worker requires a version-fenced
backend and supports ES; Meili durable lane and uncertain-submit recovery are
not implemented. The synchronous adapter methods are not cross-worker fences.

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

## Migration and rollout boundaries

The implemented ingress, delivery, rebuild and source-scan controllers are
described below. Production activation still requires their explicit configuration,
complete shadow validation and a separately authorized cutover; the production
Compose override does not activate projection generations.

Business dirty intents and dependency invalidation are not captured in every
source transaction. Destruction capture covers engine-mediated Lisk objects,
not direct SQL or arbitrary non-Lisk deletion. Keep PHP domain builders and
Ferret/Ngram/Edge indexing, periodic repair and historical receipts.

Prototype databases must be migrated to the current control schema before
consumers use historical events. `CREATE TABLE IF NOT EXISTS` does not upgrade
an old table, and a latest head cannot reconstruct a missing historical envelope.
The decoder rejects duplicate keys and case aliases; relationship timestamps
are canonical integers.

Do not retire SearchWorker/RebuildIndexesWorker or claim production cutover
from primitive tests alone. Acceptance and recovery requirements are described
in the generation/source-scan sections and [operations](../operations.md).

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

## Forced CLI reindex

The existing `bin/search index --force` parameter now travels through the
fulltext extension and built document to shadow capture. It reserves a fresh
projection revision even when serialized content is unchanged. Background tasks
carry the same parameter. The flag is execution metadata only: it changes
neither the wire document nor its content hash. Normal indexing still reuses an
unchanged event. This does not implement rebuild generations or their barriers.

PHP projection tests cover extension propagation and hash stability; the real
MySQL outbox contract exercises the storage adapter's capture path before
backend host selection and verifies the forced revision.

## Recoverable authoritative destruction (shadow)

Apply `20261006.search.02.gorgedeletion.sql` before enabling capture on any PHP
node. With `gorge.search.projection-shadow=true`, the destruction engine takes
the same namespace/PHID index lock as SearchWorker, commits a search-database
intent, and then runs the existing object destruction. Intent insertion failure
stops destruction. A committed source absence is confirmed directly against the
source DAO's writer connection, without viewer policies, replicas or export
`missing`. Only then does a transaction publish the deletion projection/outbox
and complete the intent. The source deletion and search intent are deliberately
separate commits: the earlier durable intent repairs the crash window.

If destruction fails while the source still exists, the intent remains pending
and no tombstone is emitted. Database/table errors and retired source classes
also remain pending. Existing source/search transactions are rejected before
intent capture; recovery never publishes from an uncommitted source deletion.
This first boundary supports only Lisk fulltext objects entering the destruction
engine. Direct SQL deletion, non-Lisk objects, and domain flows with an outer
transaction need separate integration; they are not silently considered covered.

TriggerDaemon recovers at most 32 due intents per pass while capture is enabled,
with a 60-second retry interval and per-object locks. Poison rows are deferred
without blocking the rest of a batch. A manual bounded pass is available:

```sh
php scripts/setup/recover_gorge_search_deletions.php
```

Disabling capture pauses automatic recovery without discarding intents. Keep
completed intents and projection receipts for now; no retention/cleanup policy
is introduced. Recovery can emit new tombstones, so this command is not a
read-only audit. It never deletes business objects. Use a database backup and
complete the shadow comparison before production activation.

`tests/contract/search/deletion.php` uses disposable MySQL databases to cover
source-present suppression, post-source-commit recovery, atomic publication
rollback, replay, no-intent absence, source table errors, caller rollback, poison
row isolation, and the actual destruction engine's pre-intent/crash behavior.
Real process termination and cross-database concurrent restoration remain
additional production acceptance tests. The controllers below cover bounded
source ranges and control transport, not production activation.

## Durable generation backfill (opt-in)

This is a resumable **control-head backfill**, not a complete business-source
rebuild. Apply `resources/sql/search/rebuild.sql` to the control database and set
`projection.rebuild:true` alongside configured ES delivery generations. Startup
fails if either new table is absent or no delivery backend is configured. Default
false keeps existing deployments unchanged. Do not confuse this schema with the
PHP source database migrations. Existing CI MySQL tests exercise the new schema.

Authenticated endpoints (header token only):

- `POST /api/search/projections/rebuilds` with
  `{"jobID":"backfill-g2","backendID":"es-shadow","generationID":"g2"}`
  creates an immutable job against a configured, bound target. Repeating the same
  ID/target returns its persisted cursor; a different target conflicts.
- `GET /api/search/projections/rebuilds/backfill-g2` reads durable progress.
- `POST /api/search/projections/rebuilds/backfill-g2/check` persists a repeatable-read
  checkpoint of current control heads and their exact target revision receipts.

The background runner captures an upper PHID, scans at most 32 heads per page,
uses stable per-job/PHID/revision event IDs, and advances its cursor only after
all acceptances commit. A partial page or process crash replays safely. SQL
ownership/epoch/expiry fence cursor updates; a page has a 20-second deadline
inside a 30-second lease. Failed pages defer 30 seconds. Multi-instance ownership
serializes job progress; only currently configured targets are scheduled, so
removing a target or disabling rebuild pauses work without deleting its cursor; pending jobs rotate rather than monopolizing the runner.

After the captured range is scanned, `awaiting-delivery` remains a repair phase.
Every 60 seconds it fills missing current-revision deliveries, including late
lower-PHID objects and new revisions. Tombstones are copied with their original
revision/source identity; old versions cannot resurrect deleted backend objects.
The original inbox receipts are never rewritten. A new target receives separate
rebuild receipts; unchanged source replay does not implicitly retarget history.

A checkpoint reports `knownHeads`, `unappliedHeads`, `transportCaughtUp`, and
its persisted `checkID/observedEpoch`. Only exact current revisions with
`status:applied` count. Pending/running/superseded receipts cannot satisfy this
check. New events can invalidate a prior checkpoint, so re-check current state.
Backend acceptance is not a refresh/query-equivalence guarantee.

`sourceCoverageVerified:false` and `activationAllowed:false` are deliberate and
cannot be enabled by the API. Missing business objects that have never emitted a
projection are invisible to this scan. There is no live-read activation endpoint.
The bounded source scanner below adds registered Lisk class enumeration and
materialization. Complete domain coverage, dirty-intent barriers, backend query
comparison, activation and rollback remain
required before retiring synchronous PHP indexing. Job/check/receipt retention
and cancellation are not implemented; keep their rows during shadow rollout.

The real MySQL suite covers partial-page fault injection, restart replay,
concurrent claims, target conflicts, pending/applied checkpoints, late/new-version
repair, and expired-lease takeover. These validate the control transport boundary;
this change does not claim to have tested a real ES rebuild or production cutover.


## Durable business-source scan (opt-in)

Apply `resources/sql/search/source-scan.sql` to the Go control database. Enable
`projection.rebuild:true` and `projection.sourceScan:{"conduitURL":"http://your-conduit-gateway"}`
with a configured, bound delivery target. Set `GORGE_SEARCH_SOURCE_TOKEN` to the
PHP `gorge.conduit.token` value. PHP must enable both `gorge.search.source-scan`
and `gorge.search.projection-shadow`; both default false. Its existing search
projection/outbox migration is required. Startup probes both new control tables.

The header-authenticated `POST /api/search/projections/source-scans` takes
`{"jobID":"source-g2","backendID":"es-shadow","generationID":"g2"}`.
`GET /api/search/projections/source-scans/source-g2` reports durable progress.
A repeated job/target returns the original catalog and cursor without calling
PHP again. Changing that job's target conflicts. There is no activation endpoint.

PHP `search.source` enumerates registered fulltext classes backed by Lisk tables
with numeric `id` and auxiliary PHID, listing unsupported classes separately.
It captures each table's primary `MAX(id)` as a decimal string; Go freezes those
ranges into independent durable shards. PHP reads each page from the primary,
locks each PHID using the indexing/deletion lock, reloads the row and builds its
domain document. It publishes an immutable projection and source outbox receipt
before returning the materialized envelope. No PHP prepare/execute worker is used.

Pages contain at most 32 rows, a 3 MiB estimated payload budget and a 10-second
soft budget checked between objects. A single document over 2 MiB fails the page.
Go bounds the HTTP response to 4 MiB and the page operation to 20 seconds inside
a 30-second lease. Ownership/epoch/expiry fence cursor commits. All page events
must reach the inbox before progress advances; partial capture, partial acceptance
or a lost response safely replays. Stable job/class/ID/revision receipts schedule
the target even when original receipts already belong to another generation.
Failures defer the shard 30 seconds; removing its configured target pauses it.

Missing rows and objects without an engine produce explicit counts, never
synthetic tombstones. Source/table/engine errors keep the cursor pending.
`sourceScanComplete` means only the captured ID ranges were visited. It does not
mean unsupported classes, deletions, backend delivery, query equality or all
business dependencies are covered. Source upper bounds are not a cross-database
snapshot; engine/extension dependency queries retain their existing consistency.
New IDs above the upper bound and late inserts into scanned gaps require another
job or normal source capture. All eligible production classes still require
compatibility validation; the real PHP fixture validates the protocol and failure
boundary rather than every domain engine. `sourceCoverageVerified:false` and
`activationAllowed:false` remain unconditional.

Disposable MySQL tests cover partial capture/acceptance, idempotent retries,
frozen ranges, retargeting receipts, expired lease takeover and provider errors.
PHP tests cover primary range bounds, missing/no-engine handling, pagination,
payload prefixes and token/feature guards; Go validates a real PHP page sample.
No production data or live Elasticsearch cutover is exercised by these tests.


### Source completion and incremental control repair

The final source shard now commits its terminal cursor and creation of a stable
`controlRebuildJobID` in one control-database transaction. Parent-row locking
serializes simultaneous shard completions; an insertion failure leaves the
source cursor pending. The existing rebuild runner then continuously repairs
missing current-revision deliveries every 60 seconds, including events accepted
after source scanning and authoritative deletion projections. It uses the same
bound target and separate inbox receipts; no new schema is required beyond the
source-scan and rebuild schemas above.

`POST /api/search/projections/source-scans/{jobID}/check` reports the source job
and, after range completion, persists a control rebuild checkpoint. It also
repairs a missing linked rebuild for source jobs completed by an older scanner.
`transportCaughtUp` requires completed source ranges, a control job in
`awaiting-delivery`, and applied receipts for every exact current control-head
revision. Later accepted events invalidate that observation. Incomplete source
scans return `transportCaughtUp:false` without a control checkpoint.

This check does not inspect uncommitted or unexported business changes, deferred
PHP indexing tasks, pending deletion intents, backend refresh, or query results.
It is not a global dirty-intent barrier. The source outbox relay must remain
running, and domain mutation capture/commit integration is still required before
any live cutover. Coverage and activation flags remain false even when control
transport has caught up. Tests simulate receipt states; they do not assert real
Elasticsearch application or refresh.
