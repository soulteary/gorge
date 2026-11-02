# Isolated identity and mail regression

Run on a Linux Docker runner with Compose, Python 3 and OpenSSL:

```sh
python3 -m unittest discover -s tests/identity -p '*_test.py'
python3 tests/identity/run.py --output /tmp/identity-regression.json
```

The runner creates a unique internal-only six-service Compose project using the
immutable image/source/version lock in `images.json`: Stargate, Warden, Herald,
Owlmail, Webhook and Redis. It never reads an existing deployment, account list,
credential, Docker context override or supplied endpoint. It generates one
synthetic allowlisted account, independent random service credentials, and a
short-lived SMTP certificate trusted only by the test Herald container. SMTP
AUTH requires TLS; no certificate verification is disabled.

Twenty checks cover health/isolation, private audit spool, authenticated mail
capture, cross-origin rejection, allowlist denial, challenge delivery, captured
MIME, wrong-code denial, correct-code login, consumed-code replay denial and
valid/invalid/tampered HMAC requests. Webhook uses the version-only command;
its native signature failure status is 500 and is checked without command output.
Five more conditions verify that restarting only Owlmail preserves health,
message identity, raw MIME hash and the successful signed delivery receipt.

Every configured service must use the exact locked digest and source/version
labels. CI runs this job as a dependency of `Gorge required CI` for runtime or
unknown-path changes. Documentation-only changes intentionally skip runtime jobs;
this is not an identity test pass. Updating the fixture lock requires reviewing
published provenance and rerunning the actual stack; changing a tag alone is
not sufficient evidence of compatibility.

The receipt is sanitized and bound to the tested Git source/tree and runner
hash. Only that JSON is uploaded. Compose data, generated credentials/certificates,
mail bodies/codes and logs remain in a private temporary directory and are
removed. Cleanup targets only the generated project and its volumes; no image
pruning, production access, browser claims, external relay enablement, optional
Phorge features or permission migration occurs. This suite does not certify
account-maintenance RPC idempotency or unconfigured providers.
