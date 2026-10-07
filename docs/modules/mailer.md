# mailer 模块

替 Phorge 发出站邮件：收一封信，交给配置好的后端中第一个接受它的那个。独占 `gorge-mailer` 这个二进制与 `:8110` 这个端口。

| | |
|---|---|
| 二进制 | `gorge-mailer` |
| 端口 | `:8110` |
| 包 | `go/internal/mailer/` |
| 契约 | [`api/openapi/mailer.yaml`](../../api/openapi/mailer.yaml) |
| 固件 | `tests/contract/mailer/` |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) 第六节 ← **改动前必读** |

## 1. 职责边界

**负责**：把一封信交出去，并如实报告「交出去了没有」以及「这次失败还值不值得再试」。七个后端（SMTP / sendmail / SES / SendGrid / Mailgun / Postmark / test）按优先级串成 failover 链。

同步 `/send` 不持久化邮件，Dispatcher 与 PHP worker 只重试已确认未接受的提交；不确定结果由 PHP 保存为 `STATUS_UNKNOWN`，停止自动重发，等待核对。启用原生持久投递后，Gorge 还负责不可变快照、提交账本、重试与结果恢复；PHP 保留收件人决策与附件授权。

**有外部依赖**，这是它与 render / diff 的结构性差异，也是它单独占一个进程的原因：它持有适配器状态、要连出去打 SMTP 与各家 provider、并且有一个真实的就绪条件可报。按 [`../architecture.md`](../architecture.md) 的分界，它归到 notification 那一侧。

## 2. 路由与依赖

```go
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/mailer")
	g.Use(auth.Token(deps.Token))

	g.Post("/send", sendMail(deps))
	g.Get("/mailers", listMailers(deps))
}
```

| 方法 | 路径 | 鉴权 |
|---|---|---|
| POST | `/api/mailer/send` | 需要 |
| GET | `/api/mailer/mailers` | 需要 |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） |

上述代码是同步兼容路由摘录。配置 `GORGE_MAILER_DELIVERY_DSN` 后还注册 POST `/api/mailer/deliver`、`/api/mailer/prepare`、`/api/mailer/execute`、`/api/mailer/cancel`，以及 GET `/api/mailer/delivery`、`/api/mailer/operations`、`/api/mailer/delivery-capabilities`；均使用同组 token 鉴权。`Deps` 包含 `Dispatcher`、`Token`、`BodyLimit`、同步 `SendTimeout` 和可选的 `Delivery`。`TestRoutePathsAreStable` 断言这两条路径仍注册着——Phorge 侧 `PhabricatorGorgeMailerClient` 已经在调它们。

同步模式的服务器配置要点如下；启用原生投递时还会创建数据库连接与恢复循环，并扩展就绪检查：

```go
srv := httpx.New(httpx.Config{
	ListenAddr: cfg.ListenAddr,
	BodyLimit:  mailer.TransportBodyLimit, // "10M"，平台默认 2M 装不下带附件的信
	Ready:      dispatcher.Ready,
})
```

`/readyz` 先检查至少一个适配器配置成功；启用原生投递时还检查持久账本 schema。它不保证下一封信会送达。

刻意**不**在 `Ready` 里拨测 SMTP 或 provider：那会让就绪状态随第三方抖动而翻转，而多后端 failover 本来就是为此存在的。所以 200 的含义是「本服务能发起一次投递」，不是「下一封信会到」。

## 3. 核心实现

### 3.1 handler 的四道处理

`c.Bind().Body` 的错误分支与 render 域同形：非 400 的 `*fiber.Error`（例如传输层 413）原样交回平台错误处理器，只有真正的 JSON 语法错误留在本地当 400。理由见 [`render.md`](render.md) 第 3.1 节。

剩下三道：**必填校验**（from / to / subject 缺一即 400）、**正文截断**（`textBody` / `htmlBody` 超 `BodyLimit` 时按 UTF-8 边界切断，静默截断而非拒绝）、**交给 Dispatcher**。

必填校验返回 400 而不是 422，这个区分是给 Phorge 看的：422 的含义是「某个后端判定这封信投不出去」，而这三种情况根本没走到后端。答 422 会让 Phorge 为它自己的序列化 bug 记一笔永久投递失败。

### 3.2 Dispatcher：优先级 failover + 单适配器重试

```
按 priority 降序遍历适配器
  └─ 单个适配器内重试 MaxRetries 次，间隔 RetryWait
       ├─ 成功        → 返回 SendResult{mailerKey, messageId}
       ├─ PermanentError → 立即返回，不重试、也不换后端
       └─ SafeRetryError → 确认未接受，可重试；耗尽后换下一个适配器
       └─ 未分类失败  → 立即返回，不在 Dispatcher 重试或切换
```

`mailerKeys` 只**收窄**候选集，不改变顺序——请求里写 `["b","a"]` 仍按服务端的优先级试。

**遇 `PermanentError` 不换后端**，因为永久失败描述的是这封信而不是这个后端，换一个只会更慢地拿到同样的拒绝。

### 3.3 永久失败分类

错误分类必须区分永久消息拒绝、已确认未接受的安全重试，以及提交结果不确定：

| 后端 | 永久消息拒绝 | 可安全重试/切换 | 不确定结果 |
|---|---|---|---|
| SMTP | 消息阶段明确 5xx | 提交前连接/TLS/认证失败；明确非永久拒绝 | DATA 发送或接受确认丢失；已确认接受后的 QUIT 失败仍算成功 |
| SES / SendGrid / Mailgun / Postmark | 除 401/403/429 外的 HTTP 4xx | 拨号失败；429；401/403 标为后端配置问题 | 提交后的网络失败、超时、5xx、无法确认接受的成功响应 |
| Postmark | `ErrorCode` 300/406/409/422 | 按 HTTP 状态分类 | 其余非零 ErrorCode 或无法解析的响应 |
| Mailgun | 无效 base64 附件 | 按 HTTP 状态分类 | 不能确认接受状态的失败 |
| sendmail | 退出码 64/65/66/67/68 | 进程无法启动；77/78 配置或权限错误 | 其他退出/执行错误 |

`Dispatcher` 只对 `SafeRetryError` 重试或切换。未分类失败既不判永久，也不代表可以安全再发。同步 `/send` 将永久错误映射为 422 `ERR_PERMANENT_FAILURE`，已确认未接受的失败映射为 502 `ERR_SEND_FAILED`，不确定提交映射为 502 `ERR_OUTCOME_UNKNOWN`；PHP 将最后一种保存为 unknown 并停止自动重投。取消发生在安全重试的等待阶段时仍是 `ERR_SEND_FAILED`，因为尚未开始下一次提交。原生账本路径继续将不确定提交记为 unknown。

PHP legacy worker 在同步网络调用前先原子地将 queued 改为并提交 `STATUS_UNKNOWN`，不允许在外层数据库事务中发送。明确安全拒绝后才恢复 queued；坏回执、发送后的保存失败和进程崩溃都保留 unknown。因此即使崩溃发生在实际提交前，也会保守停止重投；操作员核对结果后才可以显式 resend。这个 fence 缩小了「邮件已发出但数据库仍排队」导致重复发送的窗口。

同步 dispatch 使用明确的 25 秒总期限，包括适配器内重试与等待。Fiber 默认 `c.Context()` 没有期限，也不随 HTTP 调用方断开取消，不能依靠它限制业务执行。HTTP provider 使用专用 client，单请求上限 20 秒，连接/TLS 上限 5 秒，响应头上限 10 秒/64 KiB，响应体读取上限 64 KiB，并禁止自动跳转。截断或读取错误不会把未知结果变成安全重试；SES 与 SendGrid 已由成功 HTTP 状态确认接受时，丢失可选诊断正文或 message ID 不推翻接受结果，Mailgun/Postmark 缺少完整接受回执则保持 unknown。

sendmail 的 stdout/stderr 诊断保留上限也为 64 KiB；取消进程或父进程退出后，继承输出管道的子进程最多再等待 1 秒。父进程已正常退出 0 时视为接受，丢失诊断不会推翻回执；没有确认接受的取消或执行错误保持 unknown。这个清理余量独立于 25 秒提交期限。

### 3.4 MIME 构建

`mime.go`，被 SMTP / sendmail / SES 三个后端共用——它们要一封完整的 RFC 5322 报文（SES 走 `SendRawEmail`，正是为了保住附件与自定义头）。SendGrid / Mailgun / Postmark 三家字段型 API 不用这个构建器，它们收字段、自己组装。

三处值得知道的细节：附件 `data` **原样透传不重新编码**（进来就是 base64，出去也是）；正文一律 base64 编码而非 8bit（Phorge 的信带 UTF-8 主题与正文，base64 是路径上没有中继能弄坏的那一种）；自定义头的 CR/LF 会被剥掉，`TestBuildMIMEStripsHeaderInjection` 守着这一条。

## 4. 配置

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GORGE_LISTEN_ADDR` | `:8110` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权 |
| `GORGE_MAILER_CONFIG` | 无 | 后端列表，JSON 数组 |
| `GORGE_MAILER_CONFIG_FILE` | 无 | JSON 配置文件路径 |
| `GORGE_MAILER_MAX_RETRIES` | `2` | 单个适配器内的重试次数 |
| `GORGE_MAILER_RETRY_WAIT` | `2` | 重试间隔（秒） |
| `GORGE_MAILER_BODY_LIMIT` | `524288` | 单封信正文截断上限（字节） |
| `GORGE_MAILER_TYPE` | 无 | 单后端速配：设了它才会从下面那批扁平变量拼出一个后端 |
| `GORGE_MAILER_KEY` | `default` | 同上，该后端的 key |

规范变量命名规则见 [`../platform.md`](../platform.md) 第 4 节。

各后端选项另有一批扁平变量（`SMTP_HOST` / `SMTP_PORT` / `SMTP_USER` / `SMTP_PASSWORD` / `SMTP_PROTOCOL`、`MAILER_ACCESS_KEY` / `MAILER_SECRET_KEY` / `MAILER_REGION` / `MAILER_ENDPOINT`、`MAILER_API_KEY` / `MAILER_DOMAIN` / `MAILER_API_HOSTNAME`、`MAILER_ACCESS_TOKEN`），原样保留。**迁入时删掉了 `MAILER_FROM_NUMBER` / `MAILER_ACCOUNT_SID` / `MAILER_AUTH_TOKEN` 三条**：它们没有任何对应适配器，是某个 Twilio 实现的残留，SMS 默认由 Phorge 的 `PhabricatorMailTwilioAdapter` 负责，也可显式迁入 [integrations](integrations.md)，不属于 gorge-mailer。`TestMailerConfigDropsTwilioOptions` 钉住这次删除。

`Load()` 的取值顺序与 render 域一致：指了配置文件就读文件，否则读环境变量；走文件时先从环境变量预填 `GORGE_SERVICE_TOKEN`，但 JSON 中显式的 `serviceToken` 会覆盖它（包括空值）。这一条在本域比在别处更要紧——mailer 的配置文件里装着这个部署的全部后端凭据。

每个后端必须配置非空、非纯空白且互不重复的 `key`。启动时拒绝缺失或重复身份，避免供应商已接受邮件后，PHP 因不明确的 `mailerKey` 回执将结果隔离为 unknown。

### 重试默认值是一次刻意的变更

老默认值是 `MaxRetries=250` / `RetryWait=15`，但老代码**从不读它们**。迁入时把它们真正接进发送循环，同一组值会让一次 HTTP 请求最坏阻塞一小时以上，而 PHP 客户端只等 30 秒。所以默认值改为 **2 次 / 2 秒**，发送循环接收 Fiber 的 `c.Context()`；不能据此承诺客户端断开会立刻取消发送。

这不是把重试变弱了：同步路径仍由 worker 队列安排外层重试；原生持久投递的重试由账本状态决定，Go 侧只吸收秒级抖动。记在 [`../findings.md`](../findings.md)。

## 5. 兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第六节，这里是概述。四条约定：`/api/mailer/*` 两条路径、wire 字段名一律 camelCase、永久失败/安全重试/不确定提交的状态语义、附件的 base64 编码位置。

`ERR_PERMANENT_FAILURE` 与 `ERR_OUTCOME_UNKNOWN` 都会停止自动重投，分别表示明确永久拒绝与需要人工核对的不确定提交。`ERR_SEND_FAILED` 只表示可以安全再试。错误分类必须与 PHP 客户端和 worker 的持久状态分支一起更新。

把它收敛进平台码，或者把某个临时失败误报成它，都不会有任何一处报错：前者让写错的收件人地址被永久重投，后者让本可以发出去的信被记成 `FAIL`。两个方向都只在几天后的邮件统计里看得出来。

配置形态上还有一条约定不在 Go 侧但影响这里：端点与 token 全部由 Phorge 的 `cluster.mailers` 条目的 `options` 承载，**不新增任何全局 config key**。这消掉了老实现里「设了 URL 却依然不发信」的两层配置坑。

## 6. 域级错误码

三个，定义在 `internal/mailer/http.go`；unknown 分类与 PHP 的停止重投状态一起引入：

| 码 | 状态 | 含义 |
|---|---|---|
| `ERR_PERMANENT_FAILURE` | 422 | 某个后端判定这封信投不出去，重试无用 |
| `ERR_SEND_FAILED` | 502 | 已确认未接受，可安全重试；没有匹配的 `mailerKeys` 也是它 |
| `ERR_OUTCOME_UNKNOWN` | 502 | 无法确认接受结果，保留 unknown 等待核对，禁止自动重发 |

它们不会被全局错误处理器改写成 `ERR_INTERNAL`——`httpx.Fail` 一写响应就 committed（见 [`../platform.md`](../platform.md) 第 1.2 节）。

**后端失败不是 500。** 500 在本域只意味着「服务自己出了问题」，投递失败一律落在 422 或 502。

**新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。**

## Durable native email delivery (v1)

Opt-in native orchestration supplements the existing synchronous `/send` API.
PHP keeps domain preparation (recipients, subscriptions, routing, threading and
attachment authorization). Gorge owns an immutable delivery identity, the
submission ledger and queue retry scheduling. Provider acceptance is not proof
of delivery to a recipient inbox.

The PHP producer commits a mail record and `metamta_gorgeoutbox` on the same
connection. A source relay uses queue inbox acceptance. `GorgeMailDeliveryWorker`
prepares through `mail.delivery`, persists the snapshot through `/api/mailer/prepare`,
and atomically exports one `GorgeMailSubmitWorker` followup. That task contains
only `mailID` and `deliveryID`, never base64 attachments. Submission uses
`/api/mailer/execute`; retries do not regenerate content or call PHP worker.execute.
A separate `authorize` call checks the prepared recipient decision before each
submission. Changes cancel still-pending snapshots rather than sending stale
recipient content. The shared execution-policy file gates global silent mode.

`gorge_mail_delivery` and `gorge_mail_attempt` live alongside mail metadata in the
metamta database. Payload hash collisions return 409. States are prepared,
submitting, retry_wait, accepted, failed, unknown, expired and cancelled. Expired
submitting ownership becomes unknown, never another send. A terminal outcome and
`projectionPending=1` commit together: this indexed ledger flag is the result
outbox, rather than a fourth table. A worker projector applies monotonic result
revisions through PHP, then conditionally clears the flag. Acknowledgment failure,
queue cancellation or task archival does not erase that receipt.

Native delivery disables retries within each individual adapter. One ledger
attempt may still traverse several adapters when each prior failure confirms
safe nonacceptance; the limit of 12 ledger attempts is not a limit of 12 total
provider calls. Ledger retries use exponential delay with jitter and a 24-hour
producer deadline. Only explicitly confirmed nonacceptance can retry or fail over. HTTP 401/403 isolate
backend configuration failures, 429 is retryable, and unclassified errors,
including transport ambiguity and 5xx responses without a nonacceptance guarantee,
become unknown. SMTP connections honor cancellation; lost DATA acknowledgment is
unknown and failed QUIT after a positive DATA acknowledgment remains accepted.
Unknown results require review; neither Message-ID nor a queue receipt promises
exactly-once SMTP delivery.

The native submit path starts with its own 90-second operation context
(including admission and store work), with at most 75 seconds for dispatch.
It does not inherit the synchronous `/send` 25-second budget. Outcome persistence
uses a separate five-second context after dispatch, including after cancellation,
so that cleanup may outlive the original operation budget. An unconfirmed store
write cannot prove safe nonacceptance. A submitting row at
least 120 seconds old becomes unknown through recovery, rather than another send.

Configuration:

- `GORGE_MAILER_DELIVERY_DSN`: metamta MySQL database; required to expose native
  endpoints. A nonempty mailer service token is required.
- `GORGE_MAILER_DELIVERY_CONCURRENCY`: per-process native request concurrency,
  default 4, allowed 1..64. This is not an account-wide rate budget.
- `GORGE_WORKER_MAILER_URL`, `GORGE_WORKER_MAILER_TOKEN`: native mailer connection.
- `GORGE_WORKER_MAIL_OUTBOX_DSN`: same metamta database; enables source relay and
  independent terminal-result projection.
- `GORGE_WORKER_FEED_POLICY_FILE`: existing shared silent-policy authority.
- `GORGE_MAIL_DELIVERY_MODE=native`: deployment builder enables new PHP email
  producers. Default legacy preserves the existing producer during rollout.

Deploy migrations before enabling producers. Read `phorge-fork/DOCKER.md` for
bounded historic-mail claiming and rollback. Do not remove the legacy worker
until existing unclaimed email and non-email task classes have been drained.
The first version retains inline attachments and does not automatically resolve
ambiguous provider submissions. Provider-specific idempotency/query APIs,
large-file references and account-wide rate budgets remain separate extensions.

### Recovery and projection follow-up

`20261006.metamta.03.gorgerecovery.sql` upgrades existing v1 ledgers: it backfills
snapshot deadlines and adds indexed expiry/recovery and projection retry fields.
The mailer independently settles stale submissions to unknown and pending expired
snapshots to expired every five seconds, even if their queue task was cancelled.
It also closes the matching abandoned attempt audit; recovery never sends mail.

Projection failures now defer the individual row with bounded exponential backoff,
so broken early records cannot monopolize the first 32 results. Observe
`projectionAttempts`, `projectionNextAttempt` and `projectionLastError`.
Worker readiness negotiates `/api/mailer/delivery-capabilities` and verifies the
projection schema, rather than accepting a healthy synchronous-only mailer.

New snapshots pin the PHP-selected `mailerURI` and `adapterKey`; the configured
worker rejects a different route. Accepted audit retains the PHP adapter key and
stores the Go provider key separately. Sent actor/routing audit is promoted only
on provider acceptance. Repeated cancellation returns its persisted result;
a concurrent submission is reread without permitting a new send.
Queue task cancellation only stops that wakeup: use the mailer's cancellation
endpoint to cancel a still-pending delivery itself.

### Read-only operational inspection

`GET /api/mailer/delivery?deliveryID=...` requires the mailer service token and
returns metadata without sending, authorizing, cancelling or changing the ledger.
It is available only with native delivery enabled. Use `curl --get --data-urlencode`
for identities containing slashes:

```sh
curl --get "$GORGE_WORKER_MAILER_URL/api/mailer/delivery" \
  -H "X-Service-Token: $GORGE_WORKER_MAILER_TOKEN" \
  --data-urlencode 'deliveryID=mail/PHID-MAIL-example/1'
```

The response carries authoritative result state/revision, provider receipt when
present, deadline and submission start time, projection pending/retry metadata,
and the latest twelve attempt audits in descending attempt order. A prepared
snapshot has an empty attempts array; an unfinished attempt has a null
`finishedEpoch`. Message bodies, addresses, attachment data and the snapshot
payload are excluded. Ledger and audits use one repeatable-read transaction,
bounded to five seconds. Missing identities return 404; store failures return
503 without changing the outcome.

For `unknown`, inspect attempt timestamps and consult provider logs before any
manual resend. This API does not turn an ambiguous result into a safe retry.
For accepted results with `projectionPending=true`, troubleshoot the PHP Conduit
connection and projection retry metadata; resubmitting the email does not repair
result projection. Timestamps are Unix seconds, and zero means no scheduled time.
