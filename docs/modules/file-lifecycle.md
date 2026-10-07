# Resumable uploads and physical deletion

Gorge owns byte sessions and physical deletion attempts. Phorge continues to own
file permissions, metadata, references, storage formats, and object destruction.
Existing `chunks` handles and individually encrypted historical chunks keep their
PHP read/delete implementation. There is no automatic data migration.

## Enable and deploy

Upgrade both repositories and run Phorge storage upgrades before enabling the
new options. New images must contain these APIs; old image tags cannot provide
them. The deployment builder and setup checks reject missing capabilities.

* Set matching nonempty `GORGE_FILE_TOKEN` (PHP) / `GORGE_SERVICE_TOKEN` (Go).
* Set `GORGE_FILE_UPLOAD_ROOT` to a **persistent POSIX local volume**. For the
  bundled Phorge compose, `/var/gorge/files/.uploads` is inside the existing
  file volume; standalone Gorge uses `/var/lib/gorge/files/.uploads`.
* Set `GORGE_FILE_DELETION_DSN` to a writer DSN for the same `{namespace}_file`
  database used by Phorge. Its account needs SELECT on `file` and SELECT/UPDATE
  on `file_gorgedeletion`, plus the existing backend's byte deletion privileges.
  `GORGE_FILE_NAMESPACE` must match Phorge's namespace. The consumer rejects a
  different source database name: MySQL blob IDs are namespace-local. The DSN
  must still point to the correct database server.
* Set `GORGE_FILE_DELETION_OUTBOX=true` and then `GORGE_FILE_UPLOADS=true` in the
  Phorge deployment. The corresponding options are `gorge.file.deletion-outbox`
  and `gorge.file.uploads`. Both default false; raw uploads require the outbox.
* Back up the upload volume alongside file metadata. Multiple processes on one
  host may share it using flock. Replicas with different volumes, NFS/SMB,
  distributed mounts and S3-backed sessions are not supported by this version.

Raw upload sessions are explicitly refused when the installation has a default
AES storage key: this version must not silently bypass encrypted storage policy.
Leave session takeover disabled for such installations; legacy chunks continue
using their configured per-chunk storage formats.

## Upload contract

All routes require header authentication; query tokens are refused. The caller
is a trusted PHP service, never a directly authorized browser. PHP validates the
file viewer and edit capability before uploading a chunk.

* `GET /api/file/uploads/meta`: version 1, integrity version 1, enabled, fixed 4 MiB chunk size,
  maximum 64 GiB file size, raw format and POSIX volume durability.
* `POST /api/file/uploads/session`: `{id,size}`. ID is 32 lowercase hex digits.
  Same ID/size replays; a different size conflicts.
* `GET /api/file/uploads/:id`: state, exact chunk ranges, completed chunks and
  SHA-256 digests. Incomplete sessions expire seven days after allocation.
* `PUT /api/file/uploads/:id/chunk?start=N`: raw bytes with Content-Length.
  Offset and exact final-block length must match the manifest. Identical retries
  succeed; different bytes at an acknowledged offset return conflict.
* `POST /api/file/uploads/:id/complete`: requires all chunks, rereads and hashes
  every block, and records the whole-file SHA-256. It does not trust a client's
  claimed content hash. Verification is streaming but completion time is
  proportional to file size; validate gateway/client time budgets for large files.
* `GET /api/file/uploads/:id/data?start=N&end=M`: exclusive end, at most 4 MiB
  per response; only completed sessions can be read. Phorge's iterator requests
  bounded ranges rather than merging a large file in the storage engine.
  Each touched block is checked against its saved SHA-256 before streaming.
* `POST /api/file/uploads/:id/verify`: reread completed bytes and verify block
  and whole-file digests without changing the manifest. Phorge's
  `bin/files integrity` compares this result with the digest stored in its own
  database (`gorge-sha256:` integrity scheme). Legacy PHP chunks retain their
  existing behavior. Verification has the same large-file time-budget caveat
  as completion.
* `DELETE /api/file/uploads/:id`: persist cancellation before unlinking data.
  Cancellation is replayable and IDs cannot resurrect. A bounded background
  sweep resumes interrupted cancellations and removes expired partial data.

Manifest and block writes use atomic rename, file fsync and directory fsync.
Acknowledged block bytes precede the manifest acknowledgement. Crash-uncommitted
blocks can be safely overwritten on retry. Completed sessions never expire by
age: their retention follows Phorge's references. Cancellation manifests and
lock files remain as tombstones; monitor their count and disk capacity. This
version has per-file/input limits, not a global volume quota.

New PHP session handles are `chunks` / `gorge-upload/<id>`. Session byte hashes
are stored separately from Phorge's per-user resumable content hash. Existing
file.allocate/uploadchunk/querychunks return shapes remain unchanged.
PHP validates returned IDs, sizes, exact block ranges, completion flags and
digests. On allocation retry, a confirmed HTTP 410 expiry/cancellation retires
the obsolete partial file and allows a fresh allocation. Network errors and
missing volumes do not retire it. A completed Go session whose PHP completion
write was interrupted is reconciled under the file-row lock.

## Physical deletion contract

The Phorge migration creates `file_gorgedeletion`. Last-reference file-row
removal and its deletion intent commit on the same file-database transaction.
Copy-by-content-hash locks the source row, preventing a deleted handle from being
reintroduced through the normal deduplication path. Storage migrations also
persist intents for unused old Gorge handles in the replacement transaction.
Copy mode intentionally retains old bytes. Deletion reloads the current handle
under a row lock so a pre-migration snapshot cannot orphan the replacement.
Extensions introducing other ways
to clone storage references must follow that same locking discipline.

Go serializes consumers with an outbox row lock, rechecks references on every
attempt, and calls an idempotent backend delete. Referenced targets are cancelled;
transient backend errors retain `pending`, attempts, backoff and lastError.
A lost result commit replays physical deletion safely. Database deadlocks receive
bounded retries. Each backend attempt has a ten-second context budget. Only
Gorge composite handles and the new upload-session handles are accepted; old
native handles keep their existing PHP behavior.

`GET /api/file/lifecycle/meta` advertises configured upload/deletion capabilities.
It is not evidence that all historical files migrated. Readiness pings the
source DB but does not require the outbox schema before Phorge's first upgrade,
which avoids a startup dependency cycle. Monitor `file_gorgedeletion` pending
age, attempts/lastError and volume usage; completed intents are not automatically
purged in this version.

## Validation

容量、备份及墓碑保留策略见 [operations](../operations.md)。认证
`GET /api/file/uploads/usage` 返回有界容量快照；不删除任何会话或墓碑。

Go tests cover persistence across reopen, replay conflicts, missing chunks,
corruption, ranges, cancellation, expiry and cancelled lock acquisition. The
optional `GORGE_TEST_FILE_LIFECYCLE_DSN` suite uses the actual PHP migration and
requires the isolated database name `gorge_lifecycle_test`; it tests concurrent
consumers, reference protection and retry state. PHP's real client test uses
`GORGE_TEST_FILE_URL` and `GORGE_TEST_FILE_TOKEN`; fixture tests verify that the
last business reference and deletion intent are committed together.
