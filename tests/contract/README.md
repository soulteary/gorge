# Contract fixtures

Language-neutral fixtures describing what a gorge service must answer for a
given request. They live at the repository root, not under `go/`, because both
the Go services and the PHP adapters are meant to run them against the same
files: one description of the wire contract, two runners.

Each subdirectory belongs to one domain:

| Directory | Service | Go runner |
|---|---|---|
| `render/` | `gorge-render` | `go/internal/render/contract_test.go` |

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
| `htmlContainsClasses` | decoded `data.html` | Each entry must appear as `class="<entry>"`. |
| `htmlContains` | decoded `data.html` | Literal substrings that must appear. |
| `htmlNotContains` | decoded `data.html` | Literal substrings that must not appear. |
| `bodyContains` | raw response body | Literal substrings that must appear. |
| `bodyNotContains` | raw response body | Literal substrings that must not appear. |

Prefer the `html*` assertions over the `body*` ones when inspecting rendered
markup: JSON encoders escape `<` differently, so a raw-body substring check on
HTML is not portable between the Go and PHP runners.

## Why contains, not golden files

The rendered HTML changes whenever Chroma changes its lexers, and it changes in
ways that are not regressions: a token splits in two, whitespace moves between
spans. A byte-exact golden file would fail on every Chroma bump and would be
re-recorded without being read, which is worse than no test.

What actually has to hold still is the set of Pygments CSS class names, because
Phorge's stylesheet is written against them. `k`, `nf`, `nb`, `s2`, `mi` and
`c1` disappearing means the site renders unstyled code; that is the regression
worth catching, and `htmlContainsClasses` catches it.

## Runner requirements

A runner must start the service with the service token set to
`contract-token`, since the fixtures authenticate with that value and one
fixture asserts that a request without it is rejected. Everything else uses the
service defaults.
