# webhook contract fixtures

Fixtures for `gorge-webhook`'s two status endpoints. See
[../README.md](../README.md) for the file format and the shared runner
requirements; this domain adds one of its own, described at the bottom.

**These fixtures describe the smaller half of the domain.** What
`gorge-webhook` actually does is drain a queue and POST to third-party
endpoints, and none of that is reachable through its API — nothing calls in to
start a delivery. The delivered payload is a byte-exact contract too, and it is
pinned in `go/internal/webhook/dispatcher_test.go` instead, because a fixture
here can only describe a request this service *answers*, not one it *makes*.

| Fixture | Pins |
|---|---|
| `stats.json` | The four counts of `GET /api/webhook/stats`, which are the only window into a service whose work is a background loop. Each is a live `COUNT(*)` over Phorge's tables rather than a counter this service keeps, so the numbers survive a restart and include every other instance's work. `activeWebhooks` excludes disabled hooks. |
| `hooks.json` | `GET /api/webhook/hooks` counts **every** hook, disabled ones included. That is the whole reason it is a second endpoint: "no hooks yet" and "hooks that are all switched off" are different states an operator has to act on differently, and `stats.activeWebhooks` reports both as `0`. |
| `unauthorized.json` | 401 `ERR_UNAUTHORIZED` with no `data` field. The guard is not protecting a mutation — the API is read-only — but the queue's shape, which is what an attacker would want to know before attacking a hook's endpoint. |
| `token-via-query-param.json` | `?token=` is accepted alongside the `X-Service-Token` header, and checked only when the header is absent. The PHP client uses the header; this path is for a browser or a runbook's curl. |
| `unavailable/stats-database-unreachable.json` | A 500 `ERR_INTERNAL` carrying the generic message and **nothing about the database** — not the host, the port, the query or the driver's error. See below for why it needs a directory of its own. |

The two paths themselves are part of the contract:
`PhabricatorGorgeWebhookClient` calls them as written.

## Two directories, because a broken database is a configuration

`unavailable/` holds the failure fixture, the way the search domain's does, and
for the same structural reason: **what an endpoint answers when its store does
not is not something a request can ask for.** Both endpoints here do exactly
one thing — count rows — so their only failure mode is the database, and a
runner produces it by starting the service against one that does not answer
rather than by sending a different request.

That fixture is also where this domain's decision *not* to define an error code
of its own is written down. There is one failure and `ERR_INTERNAL` already
names it; a `ERR_QUEUE_UNAVAILABLE` would be a second name for a state
`/readyz` reports better, with a reason string attached. What the fixture
guards is the other side of that choice: with no domain code to carry detail,
the message must stay generic and the body must not leak the query or the host.

## Runner requirements

Beyond the shared requirement that the service token be `contract-token`, a
runner for this directory has to **inject a store rather than connect to
MySQL**, and seed it with an exact state.

That injection is not a testing convenience, it is why
`go/internal/webhook/store.go` defines a `Store` interface at all. The file
storage domain can point its fixtures at a local directory and the mailer at a
`test` adapter; this domain has no equivalent, because *both* of its endpoints
read the database and there is no configuration of the service that answers
either one without it. A fixture set that required a live MySQL would not be
language-neutral and would not run in CI.

The seeded state:

| | |
|---|---|
| Hooks | 2, exactly one of them with `status = disabled` |
| Requests with `status = queued` | 3 |
| Requests with `status = sent` | 2 |
| Requests with `status = failed` | 1 |

**Every one of those numbers is different on purpose.** With two of them
equal, `stats.json` would pass against an implementation that answered the
wrong field — and the two counts most easily confused, `activeWebhooks` and
`hooks.total`, are precisely the pair the one disabled hook separates.

For `unavailable/`, the same service with a store that fails every call.
