# Required main CI

`Gorge required CI` is emitted by `.github/workflows/ci.yml` on every main/develop
pull request and push. Do not configure a path-filtered workflow as the only
required context: GitHub can otherwise wait forever for a check that never runs.

The changes job only omits the expensive Go/runtime jobs for known documentation
and generated-report paths. Unknown paths, scripts, locks and workflows run all
runtime jobs. The always-run policy job validates release/source-pair rejection
paths, this gate's fail-closed behavior and documentation contracts. The final
job requires every applicable dependency to succeed; failure, cancellation,
missing decisions and unexpected skipped jobs fail closed. A skipped runtime job
on a documentation-only change is an applicability decision, not a test pass.

## Apply and verify

After a PR's exact final head has a successful `Gorge required CI` check from
GitHub Actions, an authorized administrator runs:

```sh
python3 .github/scripts/governance/protect_main.py --apply --verified-ref <40-character-final-head>
python3 .github/scripts/governance/protect_main.py
```

The checked-in policy binds the context to GitHub Actions app ID 15368, enables
strict up-to-date checks, administrator enforcement, conversation resolution,
linear history and PR-based changes, and blocks force pushes/deletion. Applying
it preserves other existing required contexts and stronger approval counts.
No token or deployment configuration belongs in this repository.

The existing review bot records its exact-head review as a PR comment rather
than a GitHub approving review. Therefore the policy uses zero required approving
reviews; the release operator must independently verify a completed exact-head
review with no unresolved findings before merging. This is not an automated
review-enforcement claim. Never bypass a failed or pending required check.

Report generation retains artifacts and does not push to main. Changes to
tracked reports use an ordinary reviewed PR. No report bot receives a branch
protection bypass. Existing release signature/source-pair gates remain separate;
a green main check does not publish or deploy an image.
