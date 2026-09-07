# file-storage contract fixtures

Fixtures for `gorge-file-storage`'s blob endpoints. See
[../README.md](../README.md) for the file format and the shared runner
requirements; this domain adds one of its own, described at the bottom, and a
runner that skips it will fail `read-blob.json` and nothing else.

| Fixture | Pins |
|---|---|
| `write-blob.json` | The canonical write. The file is the **raw request body**, not base64 inside JSON, which is what makes this domain's contract different from every other one. The response is still the envelope, because what it reports is metadata: `data.engine` is as load-bearing as `data.handle`, since the service picks the engine when the caller does not and Phorge stores the pair. |
| `write-blob-empty.json` | A zero-byte body with `Content-Length: 0` is a legitimate write reporting `data.size: 0`, not a missing one. Phorge stores empty files, and a service that treats an empty body as "no body" rejects a file Phorge would then never read back. |
| `write-blob-named-engine.json` | `?engine=` overrides the priority order and **must not fall through**. Phorge names an engine when the file's engine is already recorded against it, so landing the bytes elsewhere would leave that record pointing at nothing. |
| `write-blob-unknown-engine.json` | An unknown `engine` is 400 `ERR_BAD_REQUEST`, not a substitution and not a 404. A handle is meaningless without the engine that minted it, so there is nothing to fall back to; 404 would read to a client like a base URL joined wrongly. |
| `read-blob.json` | **The one success response in `/api/**` that is not the `{data, error}` envelope.** `headerEquals` asserts `Content-Type: application/octet-stream` and `Content-Length: 11`; `bodyNotContains` asserts the bytes are not wrapped in JSON. Also pins the handle travelling as a query parameter — it contains slashes. **Requires the seeded object.** |
| `read-blob-missing.json` | The other half of the mixed contract: a read *failure* is still the envelope, 404 `ERR_NOT_FOUND`. Both halves have to hold or the PHP client cannot branch on the status code. Pairs with `read-blob.json`; one without the other only guards half the shape. |
| `read-blob-missing-params.json` | The engine is required, not inferred. Handle formats overlap between backends — a blob handle is a bare integer — so guessing could read the *wrong file* rather than fail. |
| `read-blob-bad-handle.json` | A handle no engine could have minted is *also* 404 on read, not 400. Read cannot act on the difference between "malformed" and "gone", so it collapses both. Pairs with `delete-blob-bad-handle.json`, which answers 400 for the same handle — the pair is what keeps that asymmetry a decision rather than a regression. |
| `delete-blob.json` | Delete is **idempotent**: bytes that are already gone answer 200 `data.status: "deleted"`. Phorge removes the bytes and the row pointing at them in one sequence, so a 404 here would leave a row it can never retire. Deliberately targets an **unseeded** handle; see below. |
| `delete-blob-missing-params.json` | Both parameters are required on delete too, with a sharper edge than on read: an endpoint that accepted a delete with no handle would have to decide what it meant, and every plausible answer deletes something the caller did not name. |
| `delete-blob-bad-handle.json` | A handle no engine could have minted is 400 `ERR_BAD_REQUEST`, not 500. It follows from the row above: with "already gone" reported as success, a malformed handle is the only failure left that is *not* a backend fault, so a 500 would blame the service for the caller's input. It also fixes what a 500 from delete means — the backend failed and the bytes may still be there. |
| `list-engines.json` | `GET /api/file/engines` reports the backends **in the order a write tries them**, lowest priority number first. `sizeLimit` is 0 for a backend with none, which is why `canWrite` is a separate field. Credentials never appear — an entry carries only those four keys. Asserts `data.1` is absent, so the runner must configure **exactly one** backend. |
| `unauthorized.json` | A missing service token is 401 `ERR_UNAUTHORIZED` with no `data` field, **on the binary endpoint** — the one where a missing guard leaks file contents rather than metadata. The answer is the envelope even though a successful read there is raw bytes. |
| `token-via-query-param.json` | `?token=` is accepted alongside the `X-Service-Token` header. It is checked only when the header is absent; the PHP client uses the header, and this path exists for the curl one-liners in a runbook. |

The four paths themselves — `POST`, `GET` and `DELETE /api/file/blob`, plus
`GET /api/file/engines` — are part of the contract:
`PhabricatorGorgeFileStorageClient` calls them as written. So are the engine
identifiers and handle formats the fixtures spell out; see
[`../../../compat/phorge/README.md`](../../../compat/phorge/README.md)
section 8 for what breaking one of those costs.

## The read and write fixtures assert different shapes on purpose

Every other domain in this directory has one response shape to describe. This
one has two, and the split is not per-endpoint tidiness — it is the contract:

- A **successful read** answers the file's bytes as
  `application/octet-stream`. There is no JSON to address with `jsonEquals`,
  which is why this is the only directory whose fixtures use `headerEquals`.
  The `Content-Type` is what the PHP client branches on to tell the two shapes
  apart, so it is the assertion that could not be expressed any other way.
- **Everything else**, including every failure of that same read, answers the
  `{data, error}` envelope.

A client therefore branches on the **status code**, never on whether the body
looks like JSON and never on whether it is empty: a zero-byte file is a
legitimate 200 with an empty body. `write-blob-empty.json` pins the write side
of that; the read side is covered in `go/internal/filestorage/http_test.go`,
where an empty object can be seeded directly.

## Runner requirements

Beyond the shared requirement that the service token be `contract-token`, a
runner for this directory has to do two things:

**Configure exactly one backend: `local-disk`, on an empty directory.** Local
disk is the only backend a second runner can stand up with **no external
service** — no database, no bucket — which is the whole point of keeping these
fixtures language-neutral. It also makes the fixtures describe the wire
contract rather than any backend's behaviour. Exactly one, because
`list-engines.json` asserts that `data.1` is absent.

**Pre-seed one object** under the storage root before running anything:

| | |
|---|---|
| Path | `ab/cd/0123456789abcdef0123456789ab`, relative to the storage root |
| Content | `hello gorge` — exactly 11 bytes, no trailing newline |

Why a runner has to do this rather than a fixture: **a fixture is a single
request**, so a fixture that reads a file cannot also have written it, and a
handle minted by a write is random and therefore not something a later fixture
could name. Seeding is one `mkdir -p` and one file write, which is why the
local disk backend is what makes this reproducible in any language — a handle
there *is* a path under the storage root, so no API call is involved.

The 11 bytes are load-bearing: `read-blob.json` asserts
`Content-Length: 11` exactly. A trailing newline breaks it.

**`delete-blob.json` deliberately targets a handle nothing seeds**
(`ef/01/…`). That serves two purposes at once: it makes the fixture pin
*idempotency* rather than deletion, and it keeps the directory
**order-independent** — a fixture that deleted the seeded object would break
`read-blob.json` depending on which of the two ran first, and fixture order is
not something this format can express.

`read-blob-missing.json` targets the same unseeded handle, for the same reason.
