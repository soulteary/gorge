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

Both Go runners are thin wrappers; the replay logic lives in
`go/internal/contracttest/`. It is shared rather than duplicated so that the
assertion vocabulary stays identical between domains — with a runner each, the
two would soon describe their contracts in different terms.

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

Prefer the `html*` assertions over the `body*` ones when inspecting rendered
markup: JSON encoders escape `<` differently, so a raw-body substring check on
HTML is not portable between the Go and PHP runners. The `html*` group is only
used by the render domain, but it stays in the shared vocabulary so both
domains describe their contracts with one set of names.

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
