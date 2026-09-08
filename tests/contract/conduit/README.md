# conduit contract fixtures

Fixtures for `gorge-conduit`, the Conduit API gateway. See
[../README.md](../README.md) for the file format and the runner requirements.

`gorge-conduit` is the one domain that does **not** speak the platform
`{data, error}` envelope. It fronts Phorge's Conduit API (`ANY /api/:method`)
and, on success, relays the upstream response byte-for-byte — and that upstream
response is already the Conduit protocol envelope `{result, error_code,
error_info}`. So the gateway answers Conduit's shape on both outcomes: a
success is the relayed `{result: ...}`, and a failure it raises itself is
`{result: null, error_code: "...", error_info: "..."}`. The fixtures here assert
against that shape, not `{data, error}`; every one checks `data` and `error`
are absent.

| Fixture | Pins |
|---|---|
| `unauthorized.json` | A missing/invalid service token is 401 `ERR-CONDUIT-AUTH` with `result: null` and no `data`/`error`. |
| `missing-method.json` | A request to `/api` with no method segment is 400 `ERR-CONDUIT-CORE`, in the Conduit shape, not a platform 404. |
| `rate-limited.json` | A client over its per-IP bucket is 429 `ERR-RATE-LIMIT`, raised before the upstream is touched. |
| `proxy-pass.json` | An authenticated call is relayed to `{upstream}/api/{method}` and the upstream's Conduit body comes back unwrapped. |

The path itself, `ANY /api/:method`, is part of the contract: it mirrors
Phorge's own `/api/{method}` surface so a Conduit client can point at the
gateway unchanged.

## Runner requirements

Two of these fixtures need the runner to arrange state rather than only send a
request, because the gateway's behaviour depends on configuration the request
cannot carry. The Go runner
([`go/internal/conduit/contract_test.go`](../../../go/internal/conduit/contract_test.go))
does this per fixture rather than against one shared app, which is why it reads
the fixtures itself instead of calling `contracttest.Run`; the reason is spelled
out in that file.

- **`proxy-pass.json`** needs a **stub upstream**. There is no live Phorge in a
  contract run, so the runner starts a local HTTP server that answers a fixed
  Conduit success body (`{"result": {...}, "error_code": null, "error_info":
  null}`) and points the gateway's proxy at it. The fixture then asserts the
  relayed shape — `result` present, `data`/`error`/`error_code` absent.

- **`rate-limited.json`** needs a **limiter tuned to refuse**. The runner builds
  the gateway with a rate limiter whose bucket is small enough that the
  fixture's single request is rejected (RPS and burst low, and the method it
  targets, `differential.query`, is not on the exempt list). Every other
  fixture runs with the limiter disabled so it is not accidentally tripped.

- `unauthorized.json` and `missing-method.json` need only the service token set
  to `contract-token` (the value in [../README.md](../README.md)); they never
  reach the upstream.
