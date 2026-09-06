# render contract fixtures

Fixtures for `gorge-render`'s highlight endpoints. See
[../README.md](../README.md) for the file format and the runner requirements.

| Fixture | Pins |
|---|---|
| `render-python.json` | The six Pygments classes Phorge's stylesheet targets (`k`, `nf`, `nb`, `s2`, `mi`, `c1`), and the absence of any `<pre>` / `<div class="highlight">` wrapper. |
| `render-go.json` | A second lexer, so a regression confined to one language table is still caught. |
| `render-language-alias.json` | Extension-style names such as `py` resolve to the right lexer, while `data.language` echoes back exactly what the caller sent. |
| `render-empty-source.json` | An empty source is a 200 with empty HTML, not an error. |
| `render-unknown-language.json` | An unrecognised language falls back to plain text rather than failing. |
| `render-crlf.json` | CRLF and lone-CR line endings are accepted; Pygments rejected the latter. |
| `languages-list.json` | `GET /api/highlight/languages` returns the lowercased lexer list. |
| `unauthorized.json` | A missing service token is 401 `ERR_UNAUTHORIZED` with no `data` field. |
| `token-via-query-param.json` | `?token=` is accepted alongside the `X-Service-Token` header. |
| `render-malformed-body.json` | An invalid JSON body is 400 `ERR_BAD_REQUEST`, not 500. |

The two paths themselves, `POST /api/highlight/render` and
`GET /api/highlight/languages`, are part of the contract: Phorge's
`PhabricatorGoHighlightClient` calls them as written.
