<!--
  This template applies to every change, but the "Paired cross-repo change"
  section is load-bearing for db-api contract changes. The Gorge db-api service
  and the phorge-fork PHP consumer share a wire contract; a change to one side
  is only proven safe against a specific branch/commit of the other. The
  cross-repo integration test (.github/workflows/db-api-cross-repo.yml) is
  dispatched with both refs, and reviewers need to see the pair recorded here.
-->

## Summary

<!-- What changes and why. -->

## Paired cross-repo change

<!--
  Fill this in for any change that touches the db-api wire contract:
    - gorge:      go/internal/contracts/dbapi.go, go/internal/dbapi/**
    - phorge-fork: the Gorge DB consumers (PhabricatorDatabaseRef,
      PhabricatorConfigSchemaQuery, the setup checks, PhabricatorGorgeDBClient)
  Otherwise write "N/A".

  Record the exact refs the cross-repo integration test was run against, and
  the intended merge order (usually: PHP consumer tolerant first, then Go).
-->

- gorge branch / SHA: `...`
- phorge-fork branch / SHA: `...`
- Cross-repo test run: <!-- link to the db-api-cross-repo workflow run --> `...`
- Merge order: <!-- e.g. phorge-fork #NNN then gorge #MMM -->

## Checklist

- [ ] `cd go && gofmt -s -l . && go build ./... && go vet ./... && go test ./...`
- [ ] If the wire contract changed, canonical fixtures regenerated
      (`GORGE_UPDATE_FIXTURES=1 go test ./internal/dbapi -run Canonical`) and
      copied into phorge-fork's `__tests__/data/gorge-contract/`.
- [ ] Cross-repo integration workflow dispatched with both refs above (or N/A).
