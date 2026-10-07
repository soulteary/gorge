# Integrations：按配置迁移外部执行与 Fact 投影

`gorge-integrations`（8210）承接 本地 MTA 原始邮件与 Mailgun/Postmark/SendGrid 已验证入站事件、Twilio/SNS 发送、JIRA/Asana/GitHub 网络协议及 Fact 投影编排。它不决定收件资格、邮件命令、外部对象映射或统计指标。默认未启用；Phorge 配置中明确选择的域才切换，不在失败时回落到旧副作用实现。

## 启用与使用量

先运行 Phorge `php scripts/setup/audit_gorge_integrations.php`。报告仅含配置键、历史计数、外部对象数量及 Fact 游标，不导出 token、邮件正文或手机号。外部对象数量不是 API 调用量；历史入站记录也不能推导出 provider 使用比例。启用后 `GET /api/integrations/usage` 展示发送结果及持久 inbox 的分组计数，读请求不计入发送次数。

每个 Phorge 实例使用独立 integration 数据库及服务配置，避免不同实例的数字邮件 ID 和 Message-ID 相撞。初始化专用数据库时执行 `go/internal/integrations/schema.sql`，Phorge 执行正常 `bin/storage upgrade` 创建入站回执。已有第一版两张 Go 表的测试／部署库仅执行 `go/internal/integrations/migrations/002-resolution.sql` 补齐审计表；不要重新执行完整建表 SQL。启动不自动修改 schema。

复制 `deploy/compose/integrations.example.json`，填写真实服务 token、Conduit token、MySQL DSN、目标和凭据，限制文件权限，挂载给服务。JIRA 的 consumerKey/RSA 私钥应对应现有 OAuth1 provider；native 启用后 PHP adapter 不再加载 RSA 私钥，私钥可仅保留在 Go 的受限配置中，关闭 native 回滚前必须恢复旧 provider 的私钥。API 请求继续使用当前关联用户的 OAuth token；Asana 同样保留关联用户身份。访问 token 只在请求内存中使用，不写入投递账本，也不复制到使用量输出。

Phorge 配置示例（目标键应与 Go 文件一致；SMS 映射左侧是现有 `cluster.mailers` adapter key；原 type/key 不变，启用后的 SMS provider 凭据仅需在 Go 目标配置中设置，PHP options 可为空，JIRA 映射左侧使用 provider domain）：

```json
{
  "uri": "http://integrations:8210",
  "token": "REPLACE_WITH_SERVICE_TOKEN",
  "inbound": ["mailgun", "postmark", "sendgrid", "raw"],
  "sms": {"twilio-main": "twilio-main", "sns-main": "sns-main"},
  "connectors": {"asana": "asana-main", "jira:jira.example.com": "jira-main", "github": "github-main"},
  "fact": true
}
```

通过 `bin/config set gorge.integrations '<JSON>'` 设置。先部署并校验 `/readyz` 和 `/api/integrations/capabilities`（协议版本 2），通过 `php scripts/setup/manage_gorge_integrations.php --action check` 核对两侧 provider/target/type 和 Fact 开关，再逐域切换；不能直接把示例凭据作为有效配置。Phorge 基础 Compose 可以叠加 `docker-compose.integrations.yml`，它没有公开宿主机业务端口。使用 Phorge overlay 时，配置文件中的 conduitURI 应为 `http://gorge-conduit:8150`，数据库主机应对应当前 Compose 的实际写库。

## 入站邮件

`scripts/mail/mail_handler.php` 在启用 raw 后直接转交最多 6 MiB 原始邮件，Go 负责嵌套 multipart、Base64/quoted-printable、字符集、编码头和附件解析；不加载 PHP MIME parser，也不要求 mailparse。保留 `--process-duplicates` 调试选项，为这次显式处理生成新身份。服务或持久写入失败时脚本以非零状态退出，让 MTA 保留／重试邮件。切换前应审查既有邮件大小，6 MiB 以上邮件会明确拒绝，不能静默丢弃。

原 provider URL 保持不变。PHP Mailgun 入口保留签名检查，Postmark 保留来源 CIDR 检查；SendGrid 仍依赖现有入口的外围认证，其 Parse 入口没有被本实现赋予不存在的签名机制。PHP 校验通过后传递白名单字段和附件给 Gorge；Go 负责标准化、验证附件编码及 6 MiB 限制、持久去重，然后才返回 accepted。有 Message-ID 的重复请求按 provider/收件地址去重；没有 Message-ID 时使用本次 PHP 接收生成的 ingressID，避免把两封相同内容的合法邮件合并。此时跨 provider 重传无法可靠去重，保留旧接入的限制。

专用 inbox relay 调用 `integration.inbound`。默认 4 个有界并发消费者，可通过 relayWorkers 调整为 1–16；数据库行锁与 processing lease 确保同一事件只有一个消费者领取。根据 /health 的积压与逾期时间调整并发，而不是启动无界邮件任务。PHP 保留邮件命令、权限和业务事务；回执防止同一事件重复执行。完成前断连可以重取 done 回执；业务处理标记之后的中断返回 unknown，避免盲目重复命令。此状态需要核对邮件 ID 与业务记录，不能承诺跨业务数据库 exactly-once。附件元数据仍由 PHP 文件对象创建；投递前失败可能留下未引用字节，沿用文件回收机制。

## SMS / 连接器

`/effect` 使用稳定身份和内容摘要，重复成功请求返回先前结果，内容冲突返回 409。真实网络调用在领取事务提交之后执行，随后持久结果。429 明确拒绝允许重试；其他明确 4xx 为 rejected，网络失败/5xx/异常响应为 unknown，不自动切换提供方。PHP 对 unknown 和 identity conflict 停止相应发送/worker，并指明需要检查。

一次 effect 的身份不代表整个 Asana 多步同步事务。PHP 仍生成每一步调用；每个 story/账户/路径/操作使用稳定身份，正文或字段变化触发内容冲突并停止该任务；主任务、按账户创建的子任务、项目和不同通知策略的 followers 使用不同操作键。修改操作身份算法仍需要单独升级策略。切换时先 drain 在途旧 connector/SMS 任务，保持 stable story 与 mail identity；不要在失败重试过程中混用 PHP 和 Go。SMS 在首次请求前持久化 adapter、目标号码和正文快照；超时／429 重试只能使用同一 adapter 与内容，不能切换 mailer，也不能在发送中途关闭该 adapter 的 native 标记。接收方接受不代表用户实际收到 SMS。

`/read` 为 JIRA/Asana/GitHub 的新鲜读取；404 仍表示对象不可见，权限由现有关联账户决定。JIRA OAuth1 RSA-SHA1 协议签名在 Go；Asana Bearer transport 在 Go；Asana、GitHub OAuth2 code/token 交换及 Asana refresh、JIRA OAuth1 request/access-token 交换与用户身份读取均通过 Go。PHP 仍管理浏览器状态、授权回调、用户绑定和 token 存储。`/auth` 只在请求内存中处理认证参数，返回 `Cache-Control: no-store`，不写入 effect/inbox；交换结果不确定时重新开始登录，不自动重放一次性 code/verifier。GitHub token endpoint 固定为 github.com，不能由请求指定。GitHub Issue/User bridge 与 Nuance repository/issues events 导入的上下文 token 同样仅在 Go 请求内存中使用。Nuance 保留分页、If-None-Match/ETag、304、X-Poll-Interval 和限流 reset；Go 只返回这些白名单响应头，导入业务和游标仍由 PHP 管理。只接受配置固定 HTTPS base 和白名单 API 路径，禁止跟随重定向。JIRA/Asana、SMS 当前 Go 路径关闭后才回到旧实现，不存在单次调用错误后的自动 fallback。

## Fact

停止现有 PHP Fact daemon 和手动 analyze 工作，等待完成后设置 fact=true，再启动 Go projector。PHP daemon、CLI 数据点写入和 `fact cursors --reset` 都在启用时拒绝执行，游标列表仍可读取；配置是进程启动快照，已经运行的旧进程必须先停止。

Go 调用 `integration.fact` 的 catalog/scan，PHP 仅生成每页最多 20 个对象、4096 个数据点的业务快照。Go 在现有 fact 数据库上锁住 `fact_cursor` 行，原子替换对象数据点与推进游标；维度和 int64 值保持现有格式，空列表会清除旧数据。沿用原先 15 秒延后扫描及 update-time 游标语义，不新增历史删除扫描；对象删除与 Fact 重建仍按原业务维护规则处理。

factDSN 必须指向当前实例的 fact 写库，四张表必须支持事务。不使用影子库，也不自动做读切换；不要给它整个 Phorge 数据库的任意写权限。回滚前先停止 Go 并等待当前页完成，再关闭 fact 标记并启动 PHP，沿用同一游标。

## 检查与未知结果

所有接口仅接受 `X-Service-Token`，不接受 query token。`GET /effect?id=...` 查看单次发送；`GET /inbound?id=...` 查看接收处理状态，不返回正文；`/usage` 为状态分组计数。submitting/unknown 需要人工核对提供方结果，没有自动重新发送按钮。配置文件与数据库包含敏感业务数据，应使用部署现有凭据/备份策略。effect 结果只持久化 PHP 业务映射所需的 provider ID，不保留 provider 回显的 SMS 正文、任务 notes 或评论 body。

### 状态检查、核对及保留期

`--action capacity` / `GET /api/integrations/capacity` 返回近似数据/索引分配空间、
载荷保留天数和长期身份/审计保留策略。统一接管报告与一致备份步骤见
[operations](../operations.md)。此接口只读，不会删除 identity 或 unknown。

`php scripts/setup/manage_gorge_integrations.php --action health` 输出 unknown 数量、待处理邮件数量、最早逾期时长，以及 Fact 最后尝试/成功时间和连续失败数；`--action usage` 输出累计分组计数。服务每分钟输出异常状态日志。将这些字段接入现有监控：unknown > 0、逾期超过 300 秒、Fact consecutiveFailures > 0 或 stale=true 应通知值班人员；实现不会擅自注册外部告警渠道。

`--action effect --id <identity>` 或 `--action inbound --id <identity>` 返回状态与 digest，不返回邮件正文。核对 provider 接收记录、PHP mailID 和业务事务后，可通过 `--action resolve --resolution-file /secure/path/resolution.json` 固化人工确认的终态。输入示例：

```json
{
  "domain": "effect",
  "id": "IDENTITY_FROM_INSPECTION",
  "digest": "SHA256_FROM_INSPECTION",
  "state": "accepted",
  "status": 201,
  "result": {"gid": "ACTUAL_PROVIDER_OBJECT_ID"},
  "operator": "ONCALL_IDENTITY",
  "evidence": "PROVIDER_RECEIPT_OR_BUSINESS_TRANSACTION_REFERENCE"
}
```

SMS 使用真实 sid/messageID；Asana 使用真实 gid，不能填写示例或猜测 ID。只能将 unknown 或至少 120 秒未完成的 submitting 设为 accepted/rejected；拒绝结果必须是已核实的非 429 4xx。digest 必须匹配现有记录，终态不能覆写。operator 是持有服务 token 的操作者声明，服务 token 是权限边界，应由运维凭据系统控制使用；证据中不要填写 token/正文。

入站核对使用 domain=inbound、state=done，不填 result/status；Go 必须先让 PHP 核对同一 payload/digest 的旧 processing 回执并标记完成，再提交本地状态。没有回执或仍活跃的请求拒绝核对。这个动作确认已检查业务结果，不重新执行邮件命令。缺失的业务动作需要按具体应用流程补办，不能盲目重发原邮件。每次成功核对在独立审计表记录身份、摘要、操作者及证据。SMS effect 核对为 accepted 后，可用现有 `bin/mail resend --id <原 mailID>` 恢复 PHP 队列：它保留原 ID、adapter 和正文快照，Go 返回已有成功结果，不再发送。不要新建一封 SMS 代替恢复原记录。连接器 worker 同样使用原 story/task 身份恢复。

`inboxRetentionDays` 默认 30。每分钟最多清理 500 条已 done 且超过保留期的邮件载荷，保留 id/digest/state，防止迟到重传再次执行。pending/retry/processing/unknown 永不自动清理，必须先核对；去重墓碑与核对审计长期保留。PHP 原始邮件记录仍按既有 GC 策略管理。完成的计数表示 provider 接收或业务回执完成，不能用来推断 SMS 最终送达率。

本地测试：`go test -race ./internal/integrations ./cmd/gorge-integrations`。真实 MySQL 用 `GORGE_TEST_INTEGRATIONS_DSN` 指向名为 `gorge_integrations_test` 的独立一次性库；测试会创建/删除该库内测试表，拒绝其他数据库名称。PHP HTTP 契约入口是 `tests/contract/integrations/runtime.php`，需要 `GORGE_TEST_ARCANIST_DIR`。PHP 真实回执验证入口 `tests/contract/integrations/inbound_mysql.php` 使用 `GORGE_TEST_INTEGRATIONS_MYSQL_PORT` 和可选测试密码，仅创建/删除固定测试 namespace。
