# Contract fixtures

Language-neutral fixtures describing what a gorge service must answer for a
given request. They live at the repository root, not under `go/`, because both
the Go services and the PHP adapters are meant to run them against the same
files: one description of the wire contract, two runners.

Each subdirectory belongs to one domain:

| Directory | Service | Go runner |
|---|---|---|
| `render/` | `gorge-render` | `go/internal/render/contract_test.go` |
| `diff/` | `gorge-render` (same process) | `go/internal/diff/contract_test.go` |
| `notification/admin/` | `gorge-notification`, admin port | `go/internal/notification/contract_admin_test.go` |
| `notification/client/` | `gorge-notification`, client port | `go/internal/notification/contract_client_test.go` |
| `mailer/` | `gorge-mailer` | `go/internal/mailer/contract_test.go` |
| `search/` | `gorge-search` | `go/internal/search/contract_test.go` |
| `search/unavailable/` | `gorge-search`, backend that fails | `go/internal/search/contract_test.go` |
| `file-storage/` | `gorge-file-storage` | `go/internal/filestorage/contract_test.go` |
| `webhook/` | `gorge-webhook` | `go/internal/webhook/contract_test.go` |
| `webhook/unavailable/` | `gorge-webhook`, database that fails | `go/internal/webhook/contract_test.go` |

The notification domain gets two directories rather than one because its two
ports are separate listeners with separate contracts; see
[`notification/README.md`](notification/README.md). The search domain gets two
for a different reason: its engine picks a backend by role and never by
anything in the request, so a store that answers and a store that refuses are
two service configurations rather than two request bodies, and the five 502
domain codes are only reachable from the second. See
[`search/README.md`](search/README.md).

Both Go runners are thin wrappers; the replay logic lives in
`go/internal/contracttest/`. It is shared rather than duplicated so that the
assertion vocabulary stays identical between domains — with a runner each, the
two would soon describe their contracts in different terms.

The webhook domain gets two directories on the search domain's grounds — a
database that does not answer is a service configuration, not a request — and
it is also the domain whose fixtures cover the least of it. Its real work is a
background loop, and the byte-exact document it delivers to a third-party
endpoint is a contract no fixture here can describe, because a fixture is a
request the service *answers*. That one is pinned in
`go/internal/webhook/dispatcher_test.go`. See
[`webhook/README.md`](webhook/README.md).

## Fixture format

One JSON object per file:

```json
{
  "name": "short human-readable name",
  "description": "why this case matters",
  "request": {
    "method": "POST",
    "path": "/api/highlight/render",
    "headers": { "X-Service-Token": "contract-token" },
    "body": "{\"source\":\"x = 1\",\"language\":\"python\"}"
  },
  "expect": {
    "status": 200,
    "headerEquals": { "Content-Type": "application/json; charset=UTF-8" },
    "jsonHas": ["data.html"],
    "jsonAbsent": ["error"],
    "jsonEquals": { "data.language": "python" },
    "jsonStringContains": { "data.html": ["<span"] },
    "htmlContainsClasses": ["k", "mi"],
    "htmlContains": ["<span"],
    "htmlNotContains": ["<pre>"],
    "bodyContains": ["\"python\""],
    "bodyNotContains": []
  }
}
```

`request.body` is a **string** holding the raw request payload, not a nested
object. That is deliberate: it lets a fixture describe a malformed payload,
which a nested object could not express.

`expect` fields, all optional except `status`:

| Field | Applies to | Meaning |
|---|---|---|
| `status` | HTTP status | Must match exactly. |
| `headerEquals` | response headers | Each named response header must equal the given value. Header names are matched canonically, so a fixture may spell one however it likes. |
| `jsonHas` | decoded response | Each dot-path must exist and be non-null; a string value must be non-empty. |
| `jsonAbsent` | decoded response | Each dot-path must not exist. |
| `jsonEquals` | decoded response | Each dot-path must equal the given value, compared as text. |
| `jsonStringContains` | decoded response | Each dot-path must be a string containing all the listed substrings. |
| `htmlContainsClasses` | decoded `data.html` | Each entry must appear as `class="<entry>"`. |
| `htmlContains` | decoded `data.html` | Literal substrings that must appear. |
| `htmlNotContains` | decoded `data.html` | Literal substrings that must not appear. |
| `bodyContains` | raw response body | Literal substrings that must appear. |
| `bodyNotContains` | raw response body | Literal substrings that must not appear. |

A dot-path segment that is a number indexes an array, so `data.parts.0.type`
reaches into the segment list the diff domain returns. `jsonAbsent` treats an
out-of-range index as absent, which makes `data.parts.4` a way to assert how
many segments came back.

A key may itself contain a dot, and at each step the longest matching literal
key wins before the path is split. That is how `clients.active` in the
notification domain's `/status/` response is addressable: those dots are part
of the key name, not a nesting convention. A document without dotted keys
resolves exactly as it always did.

Prefer the `html*` assertions over the `body*` ones when inspecting rendered
markup: JSON encoders escape `<` differently, so a raw-body substring check on
HTML is not portable between the Go and PHP runners. The `html*` group is only
used by the render domain, but it stays in the shared vocabulary so both
domains describe their contracts with one set of names.

`headerEquals` is the newest entry, and the file storage domain is why it
exists. A successful `GET /api/file/blob` answers the file's raw bytes rather
than the `{data, error}` envelope, so there is no JSON document for the
`json*` assertions to address — and what the PHP client branches on to tell
the two shapes apart is the `Content-Type`. That made it the one thing about
that response which could not be expressed against a body: `bodyNotContains`
can catch bytes that got wrapped in an envelope, but nothing about a body
catches a response whose bytes are right and whose declared type is wrong —
and a wrong `Content-Type` alone is enough to send a client down the
error-parsing branch for a perfectly good file. Like the `html*` group it stays in the
shared vocabulary even though one domain uses it, so a second domain that ever
needs a header assertion does not invent a second name for it.

## How precise should an assertion be?

The two domains answer this in opposite ways, and both are right. The question
to ask is whether the output can change without that being a regression.

**It can, for render — so assert invariants.** The rendered HTML changes
whenever Chroma changes its lexers, and it changes in ways that are not
regressions: a token splits in two, whitespace moves between spans. A
byte-exact golden file would fail on every Chroma bump and would be re-recorded
without being read, which is worse than no test.

What actually has to hold still is the set of Pygments CSS class names, because
Phorge's stylesheet is written against them. `k`, `nf`, `nb`, `s2`, `mi` and
`c1` disappearing means the site renders unstyled code; that is the regression
worth catching, and `htmlContainsClasses` catches it.

**It cannot, for the unified diff — so compare whole values.** That output is
parsed by `ArcanistDiffParser` rather than rendered. A hunk header that says
`-1,1` where GNU writes `-1` is still a valid unified diff, so nothing rejects
it; the lines after it are simply attributed to the wrong positions. There is
no safe subset to assert on. See [`diff/README.md`](diff/README.md) for how
those expectations were captured.

Getting either side backwards has a cost: assert snapshots on unstable output
and you train people to re-record on failure; assert substrings on a parsed
format and a silent drift reaches production.

## Runner requirements

A runner must start the service with the service token set to
`contract-token`, since the fixtures authenticate with that value and one
fixture asserts that a request without it is rejected. Everything else uses the
service defaults.

The notification domain is the exception: it has no token at all, because
Phorge's notification client sends no credentials and a token there would
silently reject every message it posts. Its runners ignore
`contracttest.Token`, its fixtures send no auth header, and neither of its
directories has an `unauthorized.json`. Its client port also answers one
response in plain text rather than JSON, which is why the runner decodes the
body only for fixtures that assert something about its structure — a fixture
with just a `status` and `bodyContains` never requires JSON.

The file storage domain needs **state prepared before the run**, which is the
only place in this directory where that is true. Its runner must configure
exactly one backend — `local-disk`, on an empty directory — and **pre-seed one
object** there: the file `ab/cd/0123456789abcdef0123456789ab` under the storage
root, holding exactly the 11 bytes `hello gorge`. The reason is structural
rather than incidental: a fixture is a single request, so the fixture that
reads a file cannot be the one that wrote it, and a handle minted by a write is
random and so cannot be named by a later fixture. Local disk is what keeps this
reproducible in any language, since a handle there is just a path and seeding is
one `mkdir` plus one file write. See
[`file-storage/README.md`](file-storage/README.md) for the exact requirement and
for why its delete fixture deliberately targets a handle nothing seeds.

The webhook domain needs state prepared too, and goes further than that: its
runner must **inject a store instead of connecting to MySQL**. Both of its
endpoints read the database and it has no local-disk equivalent to point them
at, so there is no configuration of that service which answers a fixture
without one. That is why `go/internal/webhook/store.go` defines an interface —
the fixtures pin the wire shape of two status endpoints, not MySQL's ability to
count. The seeded numbers are exact and deliberately all different; see
[`webhook/README.md`](webhook/README.md).
