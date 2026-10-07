# mailer contract fixtures

Fixtures for `gorge-mailer`'s send endpoints. See [../README.md](../README.md)
for the file format and the runner requirements.

| Fixture | Pins |
|---|---|
| `send-message.json` | A complete message is 200 and reports `data.mailerKey` — the backend that actually accepted it, which is what Phorge records against the sent mail. |
| `send-with-attachment.json` | `attachments[].data` is base64 at this layer. The PHP adapter encodes before serialising; moving that to either side corrupts every attachment without changing a status code. |
| `send-permanent-failure.json` | An undeliverable message is 422 `ERR_PERMANENT_FAILURE`. **The most load-bearing fixture here** — see below. |
| `send-temporary-failure.json` | A backend that failed transiently is 502 `ERR_SEND_FAILED`, a distinct code from the permanent one. |
| `send-outcome-unknown.json` | Uncertain acceptance is 502 `ERR_OUTCOME_UNKNOWN`; PHP retains unknown and stops automatic re-submission. |
| `send-missing-from.json`, `send-missing-recipient.json` | An incomplete message is 400, not 422: nothing judged it undeliverable, it never reached a backend. |
| `send-malformed-body.json` | Invalid JSON is 400 `ERR_BAD_REQUEST`, not 500. |
| `list-mailers.json` | `GET /api/mailer/mailers` reports the backends in failover order, highest priority first. |
| `unauthorized.json` | A missing service token is 401 `ERR_UNAUTHORIZED` with no `data` field. |
| `token-via-query-param.json` | `?token=` is accepted alongside the `X-Service-Token` header. |

The synchronous compatibility paths, `POST /api/mailer/send` and
`GET /api/mailer/mailers`, are part of the contract:
`PhabricatorGorgeMailerClient` calls them as written.

## The three failure outcomes must stay distinct

`ERR_PERMANENT_FAILURE` changes what Phorge does. The PHP client raises it as
`PhabricatorMetaMTAPermanentFailureException`, which is what stops the worker
queue from re-submitting the message. `ERR_OUTCOME_UNKNOWN` also stops automatic
re-submission, but retains the mail as unknown for reconciliation.

So both directions cost something, and they cost different things:

- A permanent rejection reported as `ERR_SEND_FAILED` leaves a mistyped
  recipient address retrying forever, with nothing raising an error anywhere.
- A transient failure reported as `ERR_PERMANENT_FAILURE` drops mail that would
  have gone out a minute later, and records it as if the address were bad.

Adapters distinguish permanent message rejection, confirmed nonacceptance
(`SafeRetryError`) and ambiguous submission errors. HTTP 401/403 and sendmail
77/78 are backend configuration failures, not permanent message failures;
429 is retryable. Only confirmed nonacceptance permits Dispatcher retries or
failover. Network/5xx and unclassified execution failures must not be assumed
safe to resubmit. The synchronous API maps proven nonacceptance to
`ERR_SEND_FAILED` and uncertain acceptance to `ERR_OUTCOME_UNKNOWN`.
See [mailer](../../../docs/modules/mailer.md) for both paths.

All failure fixtures reach a backend through `mailerKeys`, because only the `test`
adapter can be made to fail on demand. The runner therefore configures four
backends: `test-mailer` (accepts), `rejects` (permanent) and `down`
(confirmed safe to retry) and `unknown` (uncertain acceptance).

## Retries are deliberately not exercised here

The dispatcher retries confirmed nonacceptance before failing over, but a fixture
cannot usefully assert on that: retries change how long a failure takes, not
what it answers, so a fixture would only make the suite slow. The runner
disables them. The counts and the context binding are covered in
`go/internal/mailer/dispatch_test.go`.

Nor is there a fixture for the transport size limit. `gorge-mailer` raises it to
10M for attachments, but that value is a deployment setting, and a fixture
asserting 413 would pass or fail depending on how the service under test was
started — which is exactly what a contract fixture must not do. That path is
covered in `http_test.go`, where the limit can be set.

The test adapter and local transport stubs do not prove acceptance by a real
external mail provider. Durable delivery, interruption and reconciliation have
separate MySQL/paired runtime tests; provider credentials, final receipt and
production delivery still require environment acceptance, as described in
[testing](../../../docs/testing.md).
