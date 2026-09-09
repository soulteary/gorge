# db-api canonical contract fixtures

These five files are the **shared-by-value** half of the cross-repository
contract test. Unlike the request/expect fixtures in the parent directory —
which a runner *replays* against a live handler — each file here is the literal
JSON body of one endpoint's `data` section, and it is read verbatim by **both**
sides:

- the Go producer, in
  `go/internal/dbapi/canonical_contract_test.go`
  (`TestDBAPICanonicalFixturesMatchContract` /
  `TestDBAPICanonicalFixturesRoundTrip`), and
- the PHP consumer, in
  `phorge-fork/src/infrastructure/cluster/__tests__/PhabricatorGorgeDBContractTestCase.php`,
  which reads its own byte-identical copy under `__tests__/data/gorge-contract/`.

| File | Endpoint | `data` shape |
|---|---|---|
| `servers.json` | `GET /api/db/servers` | `[]ServerRef` — a healthy master and a lagging replica |
| `schema-diff.json` | `GET /api/db/schema-diff` | `[]*SchemaNode` — db → table → column tree with charset/collation/engine/type/nullable/autoIncrement and two indexes (one composite, prefixed, unique) |
| `setup-issues.json` | `GET /api/db/setup-issues` | `[]SetupIssue` — one fatal, one non-fatal |
| `charset-info.json` | `GET /api/db/charset-info` | `[]CharsetInfo` — a utf8mb4 set |
| `migrations-status.json` | `GET /api/db/migrations/status` | `[]MigrationStatus` — an initialized master with applied patches and a state digest |

## They can not be hand-authored into agreement

The Go test **generates** these files from the contract types
(`GORGE_UPDATE_FIXTURES=1 go test ./internal/dbapi -run Canonical`) and, on a
normal run, **fails** if a committed file differs from what the current types
marshal to. So a renamed or retyped JSON tag on the Go side breaks the guard
here; a consumer that reads the wrong key or drops a property breaks the PHP
test there. Neither file can drift from its code silently.

## Keeping the two copies identical

The phorge-fork copy under `__tests__/data/gorge-contract/` must stay
byte-for-byte identical to these. The integration workflow
(`.github/workflows/db-api-cross-repo.yml`) runs
`scripts/check-contract-fixtures.sh`, which `diff`s the two directories and
fails the job if they diverge. When you regenerate here, copy the files across
and commit both.
