# mailer contract fixtures

Fixtures for `gorge-mailer`'s send endpoints. See [../README.md](../README.md)
for the file format and the runner requirements.

| Fixture | Pins |
|---|---|
| `send-message.json` | A complete message is 200 and reports `data.mailerKey` — the backend that actually accepted it, which is what Phorge records against the sent mail. |
| `send-with-attachment.json` | `attachments[].data` is base64 at this layer. The PHP adapter encodes before serialising; moving that to either side corrupts every attachment without changing a status code. |
| `send-permanent-failure.json` | An undeliverable message is 422 `ERR_PERMANENT_FAILURE`. **The most load-bearing fixture here** — see below. |
| `send-temporary-failure.json` | A backend that failed transiently is 502 `ERR_SEND_FAILED`, a distinct code from the permanent one. |
| `send-missing-from.json`, `send-missing-recipient.json` | An incomplete message is 400, not 422: nothing judged it undeliverable, it never reached a backend. |
| `send-malformed-body.json` | Invalid JSON is 400 `ERR_BAD_REQUEST`, not 500. |
| `list-mailers.json` | `GET /api/mailer/mailers` reports the backends in failover order, highest priority first. |
| `unauthorized.json` | A missing service token is 401 `ERR_UNAUTHORIZED` with no `data` field. |
| `token-via-query-param.json` | `?token=` is accepted alongside the `X-Service-Token` header. |

The two paths themselves, `POST /api/mailer/send` and
`GET /api/mailer/mailers`, are part of the contract:
`PhabricatorGorgeMailerClient` calls them as written.

## The two failure fixtures are a pair

`ERR_PERMANENT_FAILURE` is the one code in this domain that changes what Phorge
*does* rather than what it reports. The PHP client raises it as
`PhabricatorMetaMTAPermanentFailureException`, which is what stops the worker
queue from re-submitting the message; every other failure is re-queued.

So both directions cost something, and they cost different things:

- A permanent rejection reported as `ERR_SEND_FAILED` leaves a mistyped
  recipient address retrying forever, with nothing raising an error anywhere.
- A transient failure reported as `ERR_PERMANENT_FAILURE` drops mail that would
  have gone out a minute later, and records it as if the address were bad.

The second is worse, which is why the service classifies conservatively: only
the specific signals that describe the *message* — an SMTP 5xx, a provider 4xx
that is not 429, a sendmail exit code from the `EX_NOUSER` family — are
permanent, and everything unrecognised is transient.

Both fixtures reach a backend through `mailerKeys`, because only the `test`
adapter can be made to fail on demand. The runner therefore configures three
backends: `test-mailer` (accepts), `rejects` (permanent) and `down`
(transient).

## Retries are deliberately not exercised here

The dispatcher retries a single adapter before failing over, but a fixture
cannot usefully assert on that: retries change how long a failure takes, not
what it answers, so a fixture would only make the suite slow. The runner
disables them. The counts and the context binding are covered in
`go/internal/mailer/dispatch_test.go`.

Nor is there a fixture for the transport size limit. `gorge-mailer` raises it to
10M for attachments, but that value is a deployment setting, and a fixture
asserting 413 would pass or fail depending on how the service under test was
started — which is exactly what a contract fixture must not do. That path is
covered in `http_test.go`, where the limit can be set.
