
## 原生 Feed 与 Outbox

`GORGE_WORKER_OUTBOX_DSN`：可选 MySQL 驱动 DSN，指向 Phorge feed 数据库。
配置后后台 relay 将 feed_gorgeoutbox 事件提交到 queue 的 enqueue-event。
在 bundled compose 中默认配置，必须先运行 Phorge storage upgrade。

`GORGE_WORKER_FEED_POLICY_FILE`：可选 JSON 文件，内容为
`{"silent": false, "uris": ["https://example.test/feed"]}`。
每次投递重新读取；文件缺失或字段缺失按临时失败处理，静默模式跳过投递，
已移除 URI 按永久失败处理。版本 1 Feed 快照直接在 Go 执行，无 PHP prepare。
旧 key 格式仍需委托 PHP，必须先排空历史任务再移除该兼容路径。
不配置政策文件时保留之前的 PHP prepare 路径；原生化部署必须配置它。

就绪检查现在验证 queue 的 v1 生命周期能力、配置的政策文件和 outbox
表。健康检查仍只表示存活。首次升级时缺少表会使 /readyz 返回失败，但
/healthz 不受影响；迁移角色不能依赖 Worker ready，避免首启闭环。

## Native realtime notification delivery

`GORGE_WORKER_NOTIFICATION_POLICY_FILE` points to the atomic deployment-owned
`notification-policy.json`. `GORGE_WORKER_NOTIFICATION_MODE` supports `auto`
(native when a policy path exists), `delegated`, `shadow`, and `native`.
Shadow validates only, then delegates to PHP; it never sends a second request.
The native handler is registered independently of Conduit and supports both
legacy `{message: ...}` and version 1 delivery envelopes. Missing legacy IDs
are derived from instance/task ID and remain stable across retries.

Policy schema: `{"version":1,"mode":"required","instance":"default","endpoints":[]}`.
Only private `type:notification` messages with nonempty subscribers are accepted;
invalid messages cannot become broadcasts. Instance must match the deployment.
Unknown message fields and producer uniqueIDs survive unchanged. Each endpoint
uses a 2-second timeout, pooled HTTP connections, and no redirects. HTTP success
means service acceptance, not browser delivery. Authentication failures retry
and never degrade silently. Other failures retry after 60 seconds in required
mode, or complete with a degraded counter in fallback mode. Off suppresses
sending. Empty endpoints preserve PHP's no-op behavior with a warning/counter.
`phabricator.silent` does not independently suppress notifications.

Authenticated `/api/worker/notification-stats` reports process lifetime counters
for accepted, degraded, suppressed, retry, invalid, policyError, shadowError and
noEndpoints outcomes. Native lease heartbeats/fenced outcomes use the common
consumer. No durable notification receipt or exactly-once browser delivery is
introduced; peer fingerprints prevent loops, not request retries.

The native handler covers Feed `type:notification` tasks. Conpherence `message`
and Maniphest `workboards` still publish directly from PHP and are not silently
reclassified as Feed messages. Migrating those producers requires their own
transaction/outbox boundaries before the PHP notification client can retire.
Replay and history statistics are filtered by the WebSocket/request instance;
matching PHIDs and unaddressed broadcasts cannot cross instance boundaries.
