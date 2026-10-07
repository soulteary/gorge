<!--
  Record the tested source pair for shared wire, execution, deployment or
  recovery contracts. A change on one side is only verified against a specific
  revision of its consumer. Use docs/testing.md and docs/operations.md to select
  the applicable paired checks; the db-api workflow is one domain-specific check.
-->

## Summary

<!-- What changes and why. -->

## Paired cross-repo change

<!--
  Fill this in when changing shared contracts or their Go/PHP consumers,
  deployment configuration, fencing or recovery semantics. Otherwise use N/A.

  Record the exact refs and local dirty/source identity if applicable. Choose
  merge order based on actual compatibility; do not assume either side can run
  safely with an older pair.
-->

- gorge branch / SHA: `...`
- phorge-fork branch / SHA: `...`
- Cross-repo test run: <!-- workflow link or paired acceptance receipt, with scope and skips --> `...`
- Merge order: <!-- e.g. phorge-fork #NNN then gorge #MMM -->

## Checklist

- [ ] From the repository root: `make check && make lint && make docs-check`.
- [ ] If the db-api wire contract changed, canonical fixtures regenerated
      (`GORGE_UPDATE_FIXTURES=1 go test ./internal/dbapi -run Canonical`) and
      copied into phorge-fork's `__tests__/data/gorge-contract/`.
- [ ] Shared OpenAPI, fixtures, compatibility and module documentation updated where affected.
- [ ] Applicable paired checks ran with both refs above (or N/A); named skips and untested runtime paths recorded.
