# Worker 模块

`gorge-worker` 默认监听 `:8170`，通过 taskqueue HTTP 租约执行任务，按任务类选择 Go 原生 handler 或 PHP Conduit 委派。它不直接替代 PHP 的业务对象、权限与事务。

## 接口与共用配置

`GORGE_LISTEN_ADDR`、`GORGE_SERVICE_TOKEN` 控制本服务的监听与鉴权；状态接口使用 `X-Service-Token`；当前共享中间件也接受 query token，运维请求应使用 header。`GET /api/worker/meta` 返回静态协议，`GET /api/worker/stats` 返回计数与已注册类，可选 `GET /api/worker/notification-stats` 返回通知计数。`/healthz` 表示进程存活，`/readyz` 检查执行依赖。

队列 URL/token、Conduit URL/token、并发与领取批量见 [taskqueue](taskqueue.md) 的配置节；全部规范变量以 [worker/config.go](../../go/internal/worker/config.go) 为准。原生邮件、Feed、通知的 policy/outbox 配置见下文；执行、心跳和原子收尾见 [taskqueue](taskqueue.md)，恢复与退役验收见 [operations](../operations.md)。

## 空闲轮询

Worker 始终按 `GORGE_WORKER_POLL_INTERVAL_MS`（默认 1000ms）轮询队列，
空闲后不再固定休眠三分钟。`GORGE_WORKER_IDLE_TIMEOUT_SEC`（默认 180）
仅控制空闲状态日志的间隔，设为 0 时关闭该日志，不影响任务领取。
新任务无需唤醒空闲 Worker；实际处理延迟仍取决于队列、可用执行槽位与请求耗时。
轮询间隔、并发数和领取批量必须大于 0，空闲日志间隔不能为负数；
不合法的配置会在连接业务依赖前明确退出，避免 ticker panic 或任务处理卡住。

每次仅领取 `min(LeaseLimit, MaxWorkers - 当前占用槽位)` 个任务。每个任务独立执行、心跳和收尾，短任务完成后会立即触发补槽；一个长任务不会再阻塞整批下一次领取。队列返回超出请求上限或 null task 时拒绝该响应，避免在本地等待槽位的任务失去心跳。

取消运行上下文只停止新领取，已接受的任务继续持有心跳并在独立的排空预算内完成；租约冲突、过期和续租失败仍取消相应 handler 并阻止旧 owner 收尾。`GORGE_WORKER_DRAIN_TIMEOUT_SEC` 默认 30 秒，0 回落到默认，上限 3600 秒。预算到期取消 handler 与心跳，再给独立归档报告最多 10 秒及取消清理 1 秒；忽略 context 的 handler 不能无限阻止进程退出。部署停止宽限必须大于该预算与报告/清理时间，默认预算配合 45 秒停止宽限。

取消并不证明 PHP 或外部供应商的副作用已停止。排空到期使用现有 fenced `Resolve outcome=failure` 将仍归本 worker 的任务永久归档，阻止自动重领；这是需人工核对的行政隔离，不表示明确业务失败，也不表示副作用已回滚。取消时刷新同 owner 的当前有效 lease token，以应对续租已经提交但回执丢失；不能归档已归另一个 worker 的任务。归档网络失败或 ownership 已失效会明确记录“归档未确认”，仍可能走原有崩溃/租约恢复路径，不能声称持久化 unknown 或安全重投。

## 原生 Feed 与 Outbox 配置

`GORGE_WORKER_OUTBOX_DSN`：可选 MySQL 驱动 DSN，指向 Phorge feed 数据库。
配置后后台 relay 将 feed_gorgeoutbox 事件提交到 queue 的 enqueue-event。
在 bundled compose 中默认配置，必须先运行 Phorge storage upgrade。

`GORGE_WORKER_FEED_POLICY_FILE`：可选 JSON 文件，内容为
`{"silent": false, "uris": ["https://example.test/feed"]}`。
每次投递重新读取；文件缺失或字段缺失按临时失败处理，静默模式跳过投递，
已移除 URI 按永久失败处理。版本 1 Feed 快照直接在 Go 执行，无 PHP prepare。
旧 key 格式仍需委托 PHP，必须先排空历史任务再移除该兼容路径。
不配置政策文件时保留之前的 PHP prepare 路径；原生化部署必须配置它。

就绪检查验证 queue 的 v1 生命周期能力、PHP worker.execute capabilities（配置了
Conduit 时）、政策文件、outbox 表及配置的原生 mailer 能力。首次检查全部成功前不会
领取业务任务；失败时继续等待，取消进程会结束等待。/healthz 仍只表示存活。

鉴权的 GET `/api/worker/meta` 返回 `{data:{executionVersion:1,leaseOutcomes:true}}`，
只报告此 worker 实现的协议版本，不访问队列或 PHP，可供 PHP 启动前握手。
/readyz 才检查运行依赖；缺少表、政策或 PHP capability 会返回 503。
迁移角色不能依赖 worker ready，PHP Web 的 bootstrap 也只能等待静态 meta；
PHP 启动后再让 daemon 等待 worker ready，避免首次启动闭环。

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
