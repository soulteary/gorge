# notification contract fixtures

Fixtures for `gorge-notification`. See [../README.md](../README.md) for the file
format and the runner requirements.

Two subdirectories, because this domain is one service on **two ports** that
Phorge probes differently and refuses to let you merge:

| Directory | Port | Go runner |
|---|---|---|
| `admin/` | 22281, what PHP posts to | `go/internal/notification/contract_admin_test.go` |
| `client/` | 22280, what browsers connect to | `go/internal/notification/contract_client_test.go` |

Three things about this domain differ from `render/` and `diff/`, and all three
show up in the fixtures:

- **No service token.** Aphlict authenticated nothing and Phorge's notification
  client sends no credentials, so neither port has auth middleware. There is no
  `unauthorized.json` here and the runners ignore `contracttest.Token`.
- **Successful responses carry no `{data, error}` envelope.** Every fixture
  below asserts `data` and `error` are *absent* on the success path. Failures do
  use the envelope, which is safe because the PHP client calls `resolvex()` and
  throws on a non-2xx without ever parsing the body.
- **One response is not JSON at all.** The client port's 501 answers plain text,
  so `get-root-501.json` asserts only a status and raw-body substrings. The
  shared runner decodes the body only when a fixture asserts something about its
  structure, which is what makes that expressible.

## admin/

| Fixture | Pins |
|---|---|
| `post-message.json` | The receipt is a bare `{"fingerprint": ...}`. Phorge indexes that key off the decoded body, so an envelope would hide it without raising anything. |
| `post-form-content-type.json` | A JSON body labelled `application/x-www-form-urlencoded` is accepted. See below — this is the subtle one. |
| `post-empty-body.json` | An empty body is 400, not an accepted no-op that would hand back a fingerprint for a message nobody sent. |
| `post-malformed-body.json` | A truncated body is 400 `ERR_BAD_REQUEST`, not 500. |
| `status.json` | `GET /status/` is a **flat** map whose keys contain literal dots (`clients.active`, `messages.in`), and `clients`/`messages`/`history` are *not* nested objects. |
| `status-instance.json` | `?instance=` is echoed, and an instance nobody has connected to yet is a 200 with zeroed counters rather than a 404. |
| `root-probe.json` | `POST /` does not shadow the platform's `GET /` liveness probe. Also records the domain's one known deviation from Aphlict, which answered 405 there. |

`post-form-content-type.json` is the fixture worth knowing about. Phorge posts
its JSON through `HTTPSFuture`, which leaves curl's default
`application/x-www-form-urlencoded` on the request; the handler therefore
decodes the body itself rather than calling `c.Bind`, which dispatches on
Content-Type. It reads as if it were written the long way round for no reason,
so this fixture is what stops it being "cleaned up".

Its payload contains a `%`, an `&` and an `=` inside a string value, and that
is not decoration. A `c.Bind` here does **not** answer 415 for this
Content-Type — a content-type-driven binder takes the label at its word and form-parses the body, which
percent-decodes it and splits it on separators. So `build 100% done` is an
invalid escape sequence and the request is rejected, while a payload without
those characters binds "successfully" into a single garbage key and reports a
fingerprint for a message the hub then fans out as nonsense. A fixture that
only asserted 200 and a fingerprint would pass against exactly the change it is
supposed to catch; this one was checked by making that change and watching it
fail.

`status.json` is the response that made `contracttest.lookupJSONPath` try a
whole dotted path as a literal key before splitting it. Without that, no
assertion could address `clients.active` and these keys would have had to fall
back to raw-body substring matching.

Its exact-value assertions are limited to the counters that cannot move —
`instance`, `clients.active`, `clients.total`, `version` — while `messages.in`
and friends are only asserted present. The runner replays fixtures in filename
order against one shared hub, so the message counters depend on how many `post-*`
fixtures sort before `status.json`. Pinning those numbers would make adding a
fixture break an unrelated one.

## client/

| Fixture | Pins |
|---|---|
| `get-root-501.json` | A plain `GET /` answers 501 with Aphlict's wording byte for byte, trailing newline included. |
| `get-instance-path-501.json` | The same for `/~{instance}/`, the path a multi-instance deployment is probed at. |
| `upgrade-not-websocket-501.json` | An `Upgrade` header naming another protocol is still a plain request; the handler checks the value, not the header's presence. |
| `healthz.json` | `/healthz` keeps answering underneath the `GET /*` wildcard, so the container healthcheck does not read the 501 as a failure. |

The 501 is a **health signal, not an unimplemented endpoint**.
`PhabricatorNotificationServerRef::testClient()` reads it as the healthy answer
and raises `Got HTTP 200, but expected HTTP 501 (WebSocket Upgrade)` for
anything else, so a well-meaning 200 here takes the notification server out of
service on the Phorge side. That is the entire reason
`httpx.Config.SkipRootProbe` exists, and the client runner sets it.

The successful handshake is **not** covered here:
`httptest.ResponseRecorder` does not implement `http.Hijacker`, so an upgrade
cannot complete in memory. The 101 is checked in `tests/e2e/notification.sh`
against a real listener instead.
