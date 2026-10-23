# Go Report Card

**Grade: A+** (91.1%)

| Metric | Value |
| ------ | ----- |
| Files | 350 |
| Issues | 90 |

## Checks

| Check | Score |
| ----- | ----- |
| go_vet | 100% |
| gofmt | 100% |
| ineffassign | 100% |
| gocyclo | 74% |
| license | 0% |
| misspell | 100% |

## Issues

### gocyclo

- `go/internal/maintenance/cleanup/integration_test.go`
  - Line 84: cyclomatic complexity 22 for function TestMySQLCleanupBoundariesAndCacheRefresh
- `go/internal/taskqueue/mysql_store.go`
  - Line 137: cyclomatic complexity 18 for function (*MySQLStore).Lease
- `go/internal/search/projection_runtime.go`
  - Line 29: cyclomatic complexity 32 for function PrepareProjectionDelivery
- `go/internal/imagetransform/http_test.go`
  - Line 77: cyclomatic complexity 30 for function TestRealImageService
- `go/internal/contracttest/contracttest.go`
  - Line 136: cyclomatic complexity 25 for function check
- `go/internal/search/projection/mysql.go`
  - Line 72: cyclomatic complexity 23 for function (*MySQLStore).Accept
- `go/internal/diff/unified/unified.go`
  - Line 215: cyclomatic complexity 18 for function lcs
- `go/internal/worker/outbox/relay.go`
  - Line 39: cyclomatic complexity 17 for function (*Relay).Once
- `go/internal/taskqueue/execution_integration_test.go`
  - Line 44: cyclomatic complexity 46 for function exerciseExecutionStore
  - Line 236: cyclomatic complexity 23 for function TestRedisBackupRestoreReplay
- `go/internal/imagetransform/service.go`
  - Line 151: cyclomatic complexity 27 for function (*Service).probe
  - Line 200: cyclomatic complexity 21 for function (*Service).compute
  - Line 64: cyclomatic complexity 17 for function New
- `go/internal/search/engine/elasticsearch/backend.go`
  - Line 628: cyclomatic complexity 21 for function (*Backend).buildSearchSpec
- `go/internal/imagetransform/http.go`
  - Line 14: cyclomatic complexity 20 for function RegisterRoutes
- `go/internal/search/source_scan_http.go`
  - Line 27: cyclomatic complexity 16 for function registerSourceScanRoutes
- `go/internal/integrations/mysql_integration_test.go`
  - Line 21: cyclomatic complexity 90 for function TestMySQLIntegration
- `go/internal/search/projection/source_scan.go`
  - Line 126: cyclomatic complexity 39 for function ValidatePage
  - Line 223: cyclomatic complexity 25 for function (*MySQLStore).SourceJob
  - Line 98: cyclomatic complexity 21 for function ValidateCatalog
- `go/internal/integrations/inbound.go`
  - Line 36: cyclomatic complexity 29 for function Normalize
- `go/internal/mailer/smtp.go`
  - Line 48: cyclomatic complexity 21 for function (*smtpAdapter).Send
- `go/internal/diff/prose/prose_test.go`
  - Line 448: cyclomatic complexity 21 for function TestCombineRunsLiftsOnlyLayoutCharacters
  - Line 289: cyclomatic complexity 19 for function TestEditDistanceDiff
- `go/internal/integrations/fact.go`
  - Line 30: cyclomatic complexity 18 for function (FactPage).Validate
- `go/internal/search/projection/rebuild_test.go`
  - Line 32: cyclomatic complexity 72 for function TestMySQLRebuildRecovery
- `go/internal/mailer/delivery.go`
  - Line 43: cyclomatic complexity 34 for function (*DeliveryService).Deliver
- `go/internal/filestorage/operations.go`
  - Line 35: cyclomatic complexity 26 for function (*Uploads).UsagePage
- `go/internal/filestorage/uploads.go`
  - Line 354: cyclomatic complexity 18 for function (*Uploads).Read
  - Line 420: cyclomatic complexity 16 for function (*Uploads).SweepExpired
- `go/internal/search/engine/meilisearch/backend.go`
  - Line 473: cyclomatic complexity 16 for function (*Backend).buildFilters
- `go/internal/imagetransform/compose.go`
  - Line 31: cyclomatic complexity 51 for function (*Service).Compose
- `go/internal/worker/consumer.go`
  - Line 230: cyclomatic complexity 22 for function (*Consumer).processTask
  - Line 116: cyclomatic complexity 20 for function (*Consumer).Run
- `go/internal/integrations/integrations_test.go`
  - Line 206: cyclomatic complexity 16 for function TestTwilioAndSNSTransports
- `go/internal/mailer/delivery_integration_test.go`
  - Line 74: cyclomatic complexity 59 for function TestMailDeliveryMySQLIntegration
  - Line 253: cyclomatic complexity 16 for function TestDeliveryInspectionMySQLIntegration
- `go/internal/imagetransform/cache.go`
  - Line 35: cyclomatic complexity 22 for function (*Service).Transform
- `go/internal/diff/unified/budget_test.go`
  - Line 53: cyclomatic complexity 18 for function FuzzBoundedEditsPreserveBothInputs
- `go/internal/integrations/http.go`
  - Line 27: cyclomatic complexity 43 for function (*Service).Register
- `go/internal/taskqueue/execution.go`
  - Line 23: cyclomatic complexity 33 for function registerExecutionRoutes
- `go/internal/worker/handlers/delegate.go`
  - Line 44: cyclomatic complexity 31 for function newConduitExecutionHandler
- `go/cmd/gorge-worker/main_test.go`
  - Line 21: cyclomatic complexity 28 for function TestWorkerSignalStopsIntakeBeforeHTTPDrain
- `go/internal/maintenance/cleanup/policy.go`
  - Line 82: cyclomatic complexity 27 for function (Policy).Validate
- `go/cmd/gorge-search/main.go`
  - Line 23: cyclomatic complexity 23 for function main
- `go/internal/maintenance/cleanup/scheduler.go`
  - Line 20: cyclomatic complexity 22 for function (*Store).Run
- `go/internal/integrations/config.go`
  - Line 55: cyclomatic complexity 59 for function (Config).Validate
- `go/cmd/gorge-mailer/main.go`
  - Line 24: cyclomatic complexity 21 for function main
- `go/internal/mailer/inspection_test.go`
  - Line 15: cyclomatic complexity 18 for function TestDeliveryInspection
- `go/internal/search/projection/relay.go`
  - Line 35: cyclomatic complexity 16 for function (*Relay).Once
- `go/cmd/gorge-worker/main.go`
  - Line 32: cyclomatic complexity 43 for function main
- `go/internal/diff/prose/prose.go`
  - Line 285: cyclomatic complexity 28 for function editDistanceDiff
  - Line 53: cyclomatic complexity 18 for function buildProseDiff
- `go/internal/integrations/completion_test.go`
  - Line 64: cyclomatic complexity 19 for function TestAuthenticationExchanges
- `go/internal/filestorage/deletion_integration_test.go`
  - Line 13: cyclomatic complexity 41 for function TestRealDeletionOutbox
- `go/internal/taskqueue/scheduler_integration_test.go`
  - Line 13: cyclomatic complexity 37 for function exerciseSchedulerMySQL
- `go/internal/doccheck/doccheck_test.go`
  - Line 392: cyclomatic complexity 35 for function TestMountedAPIRoutesHaveDocumentation
  - Line 335: cyclomatic complexity 17 for function TestModuleIndexUsesConfiguredDefaultPorts
- `go/internal/search/projection/protocol.go`
  - Line 83: cyclomatic complexity 28 for function Validate
- `go/internal/taskqueue/outbox_integration_test.go`
  - Line 19: cyclomatic complexity 24 for function exerciseOutboxRelay
- `go/internal/search/engine/elasticsearch/recovery_test.go`
  - Line 190: cyclomatic complexity 24 for function TestProjectionReleasesRecoveryProbeAfterReadFailure
- `go/internal/contracttest/restore.go`
  - Line 13: cyclomatic complexity 23 for function RestoreFixture
- `go/internal/taskqueue/scheduler_test.go`
  - Line 56: cyclomatic complexity 16 for function TestScheduleConduitTransport
- `go/internal/search/projection/source_scan_test.go`
  - Line 100: cyclomatic complexity 98 for function TestMySQLSourceScanRecovery
- `go/internal/search/projection/source_scan_worker.go`
  - Line 17: cyclomatic complexity 41 for function (*MySQLStore).StepSourceShard
- `go/cmd/gorge-integrations/main.go`
  - Line 24: cyclomatic complexity 34 for function run
- `go/internal/search/engine/elasticsearch/projection.go`
  - Line 20: cyclomatic complexity 32 for function (*Backend).ApplyProjection
  - Line 130: cyclomatic complexity 17 for function (*Backend).ProjectionIndexUUID
- `go/internal/search/projection/rebuild.go`
  - Line 118: cyclomatic complexity 31 for function (*MySQLStore).StepRebuild
- `go/internal/worker/shutdown_test.go`
  - Line 127: cyclomatic complexity 20 for function TestDrainReconcilesFinalizationWithLostReply
- `go/internal/filestorage/uploads_test.go`
  - Line 14: cyclomatic complexity 20 for function TestUploadsResumeCompleteAndRanges
- `go/internal/worker/handlers/mail_delivery.go`
  - Line 113: cyclomatic complexity 19 for function NewMailSubmitHandler
- `go/internal/integrations/auth.go`
  - Line 22: cyclomatic complexity 27 for function (AuthRequest).Validate
  - Line 79: cyclomatic complexity 26 for function Authenticate
- `go/internal/imagetransform/recipes_test.go`
  - Line 103: cyclomatic complexity 26 for function TestRealMemeAndCompose
- `go/internal/integrations/store.go`
  - Line 48: cyclomatic complexity 19 for function (Store).Effect
- `go/internal/taskqueue/execution_http_test.go`
  - Line 30: cyclomatic complexity 18 for function TestExecutionHTTPProtocol
- `go/internal/dbapi/config.go`
  - Line 142: cyclomatic complexity 16 for function (*ClusterConfig).ServesApplication
- `go/cmd/gorge-maintenance/main.go`
  - Line 27: cyclomatic complexity 40 for function run
- `go/internal/worker/handlers/notification_publish.go`
  - Line 122: cyclomatic complexity 32 for function newNotificationPublishHandler
- `go/internal/search/engine/elasticsearch/projection_test.go`
  - Line 117: cyclomatic complexity 19 for function TestElasticsearchProjectionRuntime
- `go/internal/platform/conduitclient/client.go`
  - Line 57: cyclomatic complexity 18 for function (*Client).Call
- `go/internal/conduit/contract_test.go`
  - Line 123: cyclomatic complexity 18 for function checkConduit
- `go/internal/search/source_scan_test.go`
  - Line 78: cyclomatic complexity 17 for function TestConduitSourceProviderWireAndStrictResponse
- `go/internal/maintenance/cleanup/schema.go`
  - Line 12: cyclomatic complexity 17 for function (*Store).ValidateSchema
- `go/internal/worker/consumer_test.go`
  - Line 425: cyclomatic complexity 16 for function TestConsumerRefillsFreeSlotAndDrainsOnStop
- `go/internal/search/projection/mysql_integration_test.go`
  - Line 22: cyclomatic complexity 163 for function TestMySQLDurableProjectionIntegration
- `go/internal/search/projection_runtime_test.go`
  - Line 43: cyclomatic complexity 39 for function TestProjectionIngressToElasticsearchIntegration
- `go/internal/integrations/maintenance.go`
  - Line 25: cyclomatic complexity 25 for function (Resolution).Validate
- `go/internal/filestorage/operations_test.go`
  - Line 14: cyclomatic complexity 24 for function TestUploadProcessInterruption
  - Line 94: cyclomatic complexity 20 for function TestUploadInventoryPagesAndRestore
- `go/internal/filestorage/fetch.go`
  - Line 77: cyclomatic complexity 24 for function (*fetcher).fetch
- `go/cmd/gorge-file-storage/main.go`
  - Line 21: cyclomatic complexity 21 for function main
- `go/internal/taskqueue/scheduler_mysql.go`
  - Line 62: cyclomatic complexity 17 for function (*MySQLStore).lockSchedule
- `go/internal/search/projection_http.go`
  - Line 46: cyclomatic complexity 17 for function OpenProjection
- `go/internal/integrations/transport.go`
  - Line 50: cyclomatic complexity 47 for function (Request).Validate
  - Line 231: cyclomatic complexity 40 for function send
  - Line 143: cyclomatic complexity 21 for function oauthHeader
- `go/internal/worker/handlers/mail_delivery_test.go`
  - Line 17: cyclomatic complexity 29 for function TestMailPreparationAndSubmissionBoundary
- `go/internal/imagetransform/meme.go`
  - Line 146: cyclomatic complexity 24 for function (*Service).Meme
  - Line 70: cyclomatic complexity 22 for function memeOverlay
- `go/internal/maintenance/cleanup/store.go`
  - Line 234: cyclomatic complexity 21 for function (*Store).Batch
  - Line 181: cyclomatic complexity 17 for function (*Store).Claim
- `go/internal/filestorage/s3_test.go`
  - Line 94: cyclomatic complexity 17 for function TestS3RoundTrip
- `go/internal/worker/handlers/delegate_shutdown_test.go`
  - Line 16: cyclomatic complexity 36 for function TestConsumerDrainArchivesBlackholeDelegateWithoutRedelivery
- `go/internal/integrations/mime.go`
  - Line 22: cyclomatic complexity 36 for function ParseRaw
- `go/internal/filestorage/uploads_http.go`
  - Line 14: cyclomatic complexity 24 for function registerUploadRoutes

### license

- See [http://choosealicense.com/](http://choosealicense.com/)

---

_Generated by [Go Report Card](https://github.com/soulteary/goreportcard-action)._
