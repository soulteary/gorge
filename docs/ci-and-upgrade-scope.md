# Native-core CI and upgrade support scope

Decision date: 2026-11-18. This policy does not enable optional features, change
permission semantics, migrate live permissions, or publish a new image pair.

## Development, publication and feature enablement

Gorge services already used by the native-core deployment are core product
functionality. Their authentication, transaction fencing, task ownership,
file bytes, durable mail/notification delivery and Phorge wire contracts remain
required development checks. A disabled Phorge governance feature does not make
its already-used Gorge transport optional.

Phorge's closed governance/recovery/media candidate qualification has a different
schedule: core development checks closed defaults/no-op/native boundaries;
publication runs the complete optional suites against the exact release source.
The Phorge release workflow owns that fan-out. Passing it is not authority to
enable a candidate feature; feature enablement still requires its own runtime,
browser and security acceptance. Do not relabel historical fixtures as current
formal-image acceptance.

The required `Gorge required CI` context and its fail-closed applicable-job union
remain unchanged. This document does not weaken or edit branch protection.

## Pair and runtime identity

A supported deployment uses one published paired manifest containing immutable
Phorge and Gorge source revisions and image digests. Verify version/source,
architecture, signatures and attestations at publication. A tool image used to
run source tests is a test carrier, not a substitute for formal paired-image
qualification. Never update a release source-pair lock merely to point it at an
unpublished development commit.

## Historical compatibility retirement

Do not promise arbitrary upgrades from every historical release. Before retiring
an old path, record its concrete producers/consumers, data format and supported
starting release(s), then satisfy all of the following:

- the replacement path is qualified in a formal paired release;
- a read-only inventory proves that supported deployments no longer depend on
  the old producer, reader, chunk format or configuration input;
- interrupted upgrade and durable-state compatibility tests cover the supported
  start and target, with an explicit operator procedure;
- the deprecation notice states the last supporting version and replacement;
- removal has a reviewed change and retains negative/rejection regression tests.

No path has been declared retired merely by this policy. Existing native file
readers, provider ownership/fencing, data migrations and transaction recovery
checks remain until these conditions are evidenced. A MySQL version in a fixture
or a legacy image reference is not, by itself, a new production support promise
or sufficient proof that the fixture can be deleted.

## Acceptance boundaries

Version rollback certification is deferred to a future chosen baseline and is
not a prerequisite for the current non-sensitive native-core trial. Backup and
isolated restore remain separate recoverability evidence. Transaction rollback,
last-reference protection and queue fencing are correctness tests and remain.
No procedure may restore an old database over new writes as an implied rollback.

Use a single isolated Linux environment for future media identity qualification;
historical macOS hosts/trust/host-443 handoffs are not supported product entrypoints.
Retain their receipts without extending those temporary backends.

Owlmail admission belongs to its identity gateway (Stargate and the configured
identity services), not Phorge/Gorge account-disable state. Native mail tests
must still verify recipient selection and actual delivery. No cross-service
account-disable coupling is introduced by this policy.

Historical publication/acceptance receipts retain their as-of timestamp and scope.
A separate current status projection records publication, deployment, native
acceptance and disabled candidate qualification independently. Keep addresses,
credentials, raw messages, cookies and bearer links out of public records.
