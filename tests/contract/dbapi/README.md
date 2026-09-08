# db-api contract fixtures

Fixtures for `gorge-db-api`'s seven read endpoints. See
[../README.md](../README.md) for the file format and the shared runner
requirements; this domain adds requirements of its own, described at the
bottom.

**Every field here is derived live from the MySQL cluster.** This service owns
no state — each answer is a `SHOW` / `SELECT` / `INFORMATION_SCHEMA` query
against the configured servers or a projection of the cluster topology — and
`PhabricatorGorgeDBClient` reads the camelCase keys straight back. So the field
names these fixtures assert on *are* the wire contract.

| Fixture | Pins |
|---|---|
| `servers.json` | `GET /api/db/servers` returns one `ServerRef` per configured node, each carrying static topology (`refKey`, `host`, `port`, `isMaster`, `isDefaultPartition`) and a live probe result (`connectionStatus`, `replicationStatus`). It never fails on a dead node, so the array is the whole answer. |
| `server-health.json` | `GET /api/db/servers/{ref}/health` probes the one node whose key matches, addressed by the `refKey` (`host:port`) `servers` hands out, and returns a single object rather than an array. |
| `server-health-unknown.json` | A `refKey` that matches no configured node is a `404 ERR_NOT_FOUND`, not a database failure — the caller is addressing a server that does not exist. |
| `schema-diff.json` | `GET /api/db/schema-diff` returns a `Server → Database → Table → Column` tree per node, the input Phorge's schema comparison expects. |
| `schema-issues.json` | `GET /api/db/schema-issues` flattens the tree's problems into `SchemaIssue` records; a healthy node reports an **empty array**, not null or an error. |
| `setup-issues.json` | `GET /api/db/setup-issues` runs Phorge's own environment checks (version, InnoDB, the `meta_data` database, server variables); a healthy install reports an **empty array**. |
| `charset-info.json` | `GET /api/db/charset-info` reports the charset/collation set each node supports, choosing utf8mb4 when present — the exact inputs `PhabricatorStorageManagementAPI::getCharsetInfo` produces. |
| `migrations-status.json` | `GET /api/db/migrations/status` reads `{namespace}_meta_data.patch_status` on each master; `initialized:false` means the database does not exist yet, the pre-upgrade state rather than an error. |
| `unauthorized.json` | `401 ERR_UNAUTHORIZED` with no `data`, on a route that would otherwise touch the database. The token is checked before the path is resolved, so the guard protects the cluster's shape. |
| `token-via-query-param.json` | `?token=` is accepted alongside the header and checked only when the header is absent. The PHP client uses the header; this is the fallback for a browser or a runbook's curl. |
| `unavailable/schema-diff-database-unreachable.json` | `503 ERR_DB_UNREACHABLE` with the generic message and **nothing about the database** — not the host, the port, the name, the query or the password. See below. |
| `unavailable/charset-info-database-unreachable.json` | The same 503 guarantee on a second code path, so a leak that appeared on only one would still be caught. |
| `unavailable/servers-node-unreachable.json` | The other side of the failure: `/api/db/servers` reports an unreachable node **in-band** at 200 with `connectionStatus:"fail"`. The `refKey` is present on purpose — it is the node's public identifier — but no password or SQL may appear. |

The seven paths are part of the contract: `PhabricatorGorgeDBClient` calls them
as written, and renaming any is a breaking change on the PHP side.

## Two directories, because a broken database is a configuration

`unavailable/` holds the failure fixtures, the way the other DB-backed domains
do, and for the same structural reason: **what an endpoint answers when the
cluster does not is not something a request can ask for.** A runner produces it
by starting the service against a cluster that does not answer rather than by
sending a different request.

This is also where the domain's error-code decision is pinned. Unlike webhook —
which defines no code of its own and answers `ERR_INTERNAL` — db-api adds three
caller-actionable codes (`ERR_DB_UNREACHABLE` 503, `ERR_READONLY` 409,
`ERR_DB_ACCESS_DENIED` 403), because "the database is unreachable" is a
different thing for a caller to do about than "the service is broken". The
`unavailable/` fixtures pin the other side of that choice: the code carries the
*kind* of failure, and the message stays generic so the body still leaks no
host, database name, query or credential. The one endpoint that reports the
failure in-band, `/api/db/servers`, is allowed to name the `refKey` it probed
because that is the report's whole job — but it, too, leaks no password or SQL.

## Runner requirements

Beyond the shared requirement that the service token be `contract-token`, a
runner for this directory has to **inject database connections rather than
connect to a real MySQL**, and seed them to answer as one healthy single-node
cluster: a master at `db1:3306` that is its own default partition, on a modern
MySQL with InnoDB, the `phorge_meta_data` database present with one applied
patch, utf8mb4 available, and sane server variables.

That injection is why the four read services take a connection factory. A
fixture set that required a live MySQL with a specific schema and specific
server variables would not be language-neutral and would not run in CI; the Go
runner seeds a `sqlmock`-backed connection whose expectations are matched by
regexp and out of order, so one seeded state answers whichever of the seven
endpoints a fixture drove. A second runner in another language needs the same
freedom — what the fixtures describe is the wire shape of the seven endpoints,
not MySQL's ability to answer `INFORMATION_SCHEMA`.

For `unavailable/`, the same cluster with a connection factory that fails every
dial.
