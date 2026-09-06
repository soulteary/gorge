# diff contract fixtures

Fixtures for `gorge-render`'s diff endpoints. See [../README.md](../README.md)
for the file format and the runner requirements.

## Why these assert on exact bytes

The render fixtures deliberately avoid comparing whole values: they assert that
particular Pygments class names appear, because Chroma's exact markup shifts
between releases and pinning it would produce failures that mean nothing.

The unified diff endpoint is the opposite case. Its output is not rendered, it
is *parsed* — `ArcanistDiffParser` reads the hunk header to decide where every
following line belongs. A header that says `-1,1` where GNU writes `-1` is
still a syntactically valid unified diff, so nothing rejects it; the lines
after it are simply attributed to the wrong positions, and the corruption
surfaces much later as a review that renders wrongly. There is no safe subset
to assert on, so `data.diff` is compared in full.

The expectations were captured from the real binary rather than written by
hand:

```
printf '<old>' > a; printf '<new>' > b
diff -U65535 -L 'a 9999-99-99' -L 'b 9999-99-99' a b
```

If one starts failing, re-run that command before editing the fixture.

Comparing in full works here because every fixture below pins an *unambiguous*
shape. It does not generalise: when a line repeats, several alignments can be
equally minimal and the engine may pick a different one than GNU. What holds in
general is narrower — identical hunk headers and an equally small edit script,
not identical bytes. That boundary is measured and explained in
`compat/phorge/README.md` section 4.6, and enforced by
`go/internal/diff/unified/systemdiff_test.go`. Do not add a fixture whose
inputs repeat a line without checking it against the real binary first.

The prose fixtures sit in between: their output has no external reference, so
they pin the segmentation for a handful of small inputs, and the reconstruction
invariant that actually matters is enforced in the Go unit tests instead.

| Fixture | Pins |
|---|---|
| `generate-two-lines.json` | The canonical shape: explicit counts on both sides, one context line, then a delete/insert pair in that order. |
| `generate-single-line.json` | A one-line side is written `-1`, not `-1,1`. |
| `generate-empty-old.json`, `generate-empty-new.json` | An empty side is `0,0`, not `1,0`. Kept as a pair because one helper formats both counts. |
| `generate-no-trailing-newline.json` | Both last lines unterminated and differing: the marker appears once per side. |
| `generate-one-side-no-newline.json` | An unterminated line is not equal to the same text terminated, so adding a trailing newline is a real change. |
| `generate-identical.json` | The one response that follows PHP rather than `diff`, quirks included: `"a\nb\n"` counts as 3 lines and the body ends with a context line holding a single space. |
| `generate-normalize.json` | `normalize` strips every space and tab but not newlines. |
| `prose-word-change.json` | A word-level change leaves the surrounding text in two unchanged segments. |
| `prose-layout-chars.json` | Shared punctuation is lifted out of the change; an arbitrary shared prefix is not. |
| `prose-identical.json` | Unchanged prose collapses to a single segment. |
| `unauthorized.json` | A missing service token is 401 `ERR_UNAUTHORIZED` with no `data` field. |
| `token-via-query-param.json` | `?token=` is accepted alongside the `X-Service-Token` header. |
| `malformed-body.json` | An invalid JSON body is 400 `ERR_BAD_REQUEST`, not 500. |

## What is deliberately not here

There is no oversized-input fixture. Both size guards are configurable per
deployment — `GORGE_DIFF_MAX_BYTES` for the combined byte count, and a fixed
cell budget for the comparison table — so a fixture asserting 413 would pass or
fail depending on how the server under test was started, which is exactly the
property a contract fixture must not have. Those paths are covered in
`go/internal/diff/http_test.go`, where the limit can be set for the test.

The two paths themselves, `POST /api/diff/generate` and `POST /api/diff/prose`,
are part of the contract.
