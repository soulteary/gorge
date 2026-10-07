# 接管、容量、备份与退役验收

## 单一报告入口

在已升级的 Phorge 实例执行 `php scripts/setup/audit_gorge_status.php`。
`--local-only` 仅读取配置和数据库，不发 HTTP；每个失败观测标为 unavailable，
不能按零记录理解。报告版本为 1。配置 owner 与数据库 owner 分别列出；配置开启
不等于工作已开始，也不等于旧进程已停止。schedulerIdentityMatch=null 表示未证明。

worker/maintenance 没有 PHP 服务 URI 配置，可用 `--runtime-file /secure/runtime.json`
补充 `{ "worker": { "uri": "http://worker:8170", "token": "..." },
"maintenance": { "uri": "http://maintenance:8200", "token": "..." } }`。
此文件只允许运维读取，不提交仓库。报告只打印端点摘要、白名单能力、计数与状态；
不会打印 token、DSN、邮件正文、任务内容或原始异常。mailer/search 的 operations
自动使用已有 cluster 配置，也允许 runtime-file 显式提供 mailer/search uri/token。
worker stats 补充实际注册的任务类；注册不等于具体 payload 能 native 执行。
端点摘要只证明配置目标，
不能证明物理数据库身份。SQL operations 额外读取 server_uuid + DATABASE() 的摘要，
queuePhysicalIdentityMatch 对照 PHP 与 Go 的实际连接；调度器另有持久数据库 UUID 校验。
Redis 报告配置目标摘要，物理实例身份保持 not_proven，不用地址相同推断恢复正确。
文件卷的 `.gorge-volume-id` 在初始化时持久创建，恢复必须保留。它标识逻辑卷；复制卷
也会复制身份，不能当作两个克隆同时可写的证明。独立新部署应使用新卷身份。

`audit_gorge_retirement.php` 复用同一报告的本地部分和 Worker 边界。
旧文件读取器删除要求历史非 Gorge 文件和历史 chunks 均为零。默认审计仅提供
`historyDrained`，不会放行。停止所有文件写入者与迁移任务后，执行
`php scripts/setup/audit_gorge_retirement.php --verify-integrity --writers-paused`，
只有本次完整扫描无失败、无 partial，且具有停写声明，才输出 canRemove=true。
`writersPausedAttestation` 是运维声明，不是自动探测旧进程已停止；报告不执行删除。
SQL 队列为空不能证明 Redis 已排空。搜索 shadow generation 完成不能证明生产读取切换。
Fact native 开启时必须先停止 PHP daemon/CLI，已有进程的启动配置不会自动改变。

## 持久状态所有者与保留期

* 上传卷：未完成会话 7 天过期；完成会话跟随 PHP 文件引用，不按年龄删除。
  cancelled manifest 和 lock 长期保留，避免旧 session ID 复活；不要手工删墓碑。
* `file_gorgedeletion`：pending 必须保留并恢复；done/cancelled 当前长期保留。
  不提供仅按年龄删除的自动任务。改变保留期前必须明确最大重放窗口和升级兼容。
* integration effect：accepted/rejected 和 unknown/submitting 均保留稳定 ID/digest。
  unknown 必须核对提供方证据，不能作为过期失败自动重发。
* integration inbox：默认 30 天后仅擦除 done 的 payload，每批最多 500 行；
  pending/retry/processing/unknown 不清理。身份、digest、终态墓碑长期保留。
* integration resolution：核对证据长期保留，不能早于对应身份记录删除。
* PHP inbound receipt：与 Go inbox 一起保留；丢失回执可能使恢复无法判断业务已执行。
* queue archive/finalize、事件 inbox、邮件账本与搜索墓碑遵循各域重放语义。
  不要独立清空表来“修复积压”，也不要用 cache/log collector 管这些记录。

这些长期保留不是无限容量承诺。未来归档要保留可查询的 identity/digest/终态，
先证明归档后的迟到重试仍拒绝重复副作用，才能收缩在线记录。

## 容量与告警

`GET /api/file/uploads/usage`（header token）返回最多 10000 个目录项的只读扫描，
包括 logicalBytes、状态计数、invalidManifests、truncated 和所在卷 availableBytes。
truncated=true 时字节/计数为下界，不能作为完整容量报告。用 `?cursor=<nextCursor>&limit=10000`
继续读取，直到 truncated=false，逐页累加 entries/logicalBytes/sessions/invalidManifests。
每页最多 10000 项、超时 5 秒，cursor 为路径摘要，不输出会话 ID。扫描期间目录改变可能
使 cursor 失效，应重新开始；一致总量需要停止写入。logicalBytes 不等于磁盘
分配量。完整物理容量应由宿主机磁盘/卷监控提供。接口不返回路径或 session ID。

`GET /api/integrations/capacity` 或 `manage_gorge_integrations.php --action capacity`
返回三张表的近似分配空间及当前载荷保留策略；InnoDB 数据/索引空间不是精确载荷大小。
统一报告补充 PHP 删除终态、入站回执、outbox、队列归档/数据、搜索源墓碑的记录数与
近似空间；`/api/queue/operations`、`/api/mailer/operations`、
`/api/search/projections/operations` 补充各 Go 域的历史计数与状态。每张缺表/失败单独
标为 unavailable，不能作为零。Redis 同时输出存留归档索引数和累计归档数；SCANNED
key memory/inbox 在 truncated=true 时为下界，memoryIncomplete 表示部分空间观测失败。
报告保留 unknown、最早逾期与 Fact 进展。

初始告警建议：unknown>0、逾期>300 秒、Fact stale/连续失败、invalidManifests>0、
卷可用空间<20%、预计 7 天内耗尽。阈值需要按实际峰值调整，报告不注册通知渠道。
容量规划使用日增 identity 数 × 实测行/索引字节 × 保留天数，加上传峰值和备份空间；
长期墓碑用累计 identity 数计算。每周记录计数与物理空间，不能只看成功率。保存一次报告后，可用
`audit_gorge_status.php --previous-report <上次报告>` 得到 SQL 表记录与近似空间的净变化、
每日净增长率；只比较相同物理数据库摘要，时间/版本不兼容或更换实例不会计算增长。
这不是插入速率，清理和空间分配粒度都会影响数值。

## 一致备份与恢复

1. 先将 scheduler/cleanup owner 切 paused 并等待命令返回；停 worker、inbound relay、
   Fact projector、文件写入/删除消费者，阻止新的业务写入。停止旧 PHP 执行者。
2. 同一恢复点备份 Phorge file/metamta/worker/fact 数据库、integration 库、邮件账本、
   搜索控制库、taskqueue 数据库/Redis 快照，以及原始文件卷和 `.uploads` 卷（含 manifest、lock、墓碑）。跨库
   事务不存在；不能把不同日期的数据库和字节卷拼成一个恢复点。Redis 队列、归档和 finalize 去重键
   必须同点备份；不要在恢复时 flush 队列或重建 task ID。
3. 记录两仓库版本、schema 版本、owner epoch/数据库 UUID、模式和配置引用。
   凭据通过独立受控备份保存，不写入状态报告。保留 POSIX 权限和 fsync 语义。
4. 在无出站投递的隔离环境恢复，校验 integrity、引用、pending、unknown 和身份。
   不通过删除旧账本或生成新 effect/session ID 消除 unknown。核对已有副作用证据。
5. 逐域恢复执行权；检查 backlog 和容量后开放写入。调度克隆库必须按 scheduler
   文档在 paused 状态生成独立数据库 UUID，使用独立凭据，避免误接生产。

## 验收入口

Phorge CI 的 Gorge runtime contracts 运行 PHP planning/receipt/retirement 与 paired Go
真实 MySQL/Redis 测试。`PHORGE_FORK_DIR` 指向参与验收的实际 PHP checkout；不得默认
拿另一版本迁移文件。测试使用随机库或固定 `gorge_lifecycle_test` /
`gorge_integrations_test` 专用库，不能使用生产 DSN。

必需故障证据：同计划多副本竞争只有一次入队；owner pause/php/gorge 使旧计划失效；
任务原子收尾丢响应重放；上传进程退出后块确认可恢复；物理删除成功但结果回写失败
时重复删除安全；effect/inbound 不确定结果停止并可核对；旧文件迁移前后字节与完整性
一致，copy 保留旧字节、move 产生删除意图，共享引用不能误删。


## 完整验收与版本配对

完整 Docker 入口位于实际 Phorge checkout 的 `deploy/acceptance/accept.py`。从 Phorge
仓库根运行：

```sh
python3 deploy/acceptance/accept.py --gorge-dir ../gorge --check
python3 deploy/acceptance/accept.py --gorge-dir ../gorge --output /private/tmp/gorge-acceptance
```

默认布局是 `gorge/` 与 `phorge-fork/` 互为兄弟。Phorge CI 的 Gorge checkout 位于
`.test-gorge/`，须改传 `--gorge-dir .test-gorge`；Go 配对检查的 `PHORGE_FORK_DIR`
始终指向实际 PHP checkout。输出目录必须在双方源码之外且不可覆盖已有验收结果。
`--check` 仅验证隔离配置和锁定摘要，完整运行才创建独立 Compose 项目与临时后端，
覆盖真实 MySQL、Redis、S3、image/render、Elasticsearch 和 Meilisearch。

`tests/contract/worker/acceptance.py` 必须显式提供当前 image 和独立真实 S3 URL、
测试凭据及 MySQL 配置；缺任一后端立即失败。支持 `--environment <受限 JSON>`，
只接收 GORGE_TEST_ 字符串配置。配置文件不得提交仓库。

文件矩阵验证 local-disk、MySQL blob、旧 chunks 和 S3 的 copy/move、内容与完整性，
以及共享 blob 最后引用保护。S3 是独立兼容服务，AWS 签名经过真实服务校验；本地 TLS
代理使用临时 CA，保留证书验证。macOS OpenSSL 与 SecureTransport 的区别由 Phorge 内置运行库的固定兼容补丁处理；
验收不再复制或临时修改运行库源码，私有 CA 和生产证书校验继续有效。

paired_recovery.py 启动两个当前 Go integration 副本和真实 PHP Conduit 方法。
PHP 业务回执提交后，在发出响应前 SIGKILL 两端，停写备份并恢复两端 SQL，再用原
identity 重试。还通过真实 HTTP 核对 unknown，恢复审计账本后再重试，断言只产生
一条 receivedmail 和一条 resolution。测试消息使用重复来源保护，避免触发外部业务命令。
这是业务接收与回执链路验收，不替代各应用邮件命令自身的业务测试。

Go 真实后端套件另外覆盖 Redis DUMP/RESTORE 后的 inbox/finalize 去重，上传 SIGKILL
与卷复制恢复，删除终态、effect、邮件账本和搜索投影身份的 SQL 恢复与迟到重试。
这些恢复都是隔离 fixture，不执行生产备份或恢复。

Phorge CI 要求 `GORGE_CONTRACT_REF` 仓库变量或 workflow_dispatch 的 gorge_ref 为匹配的
已发布 40 位 Gorge commit SHA；不回退到 main。两仓库改动发布后更新此配对值。
每次成功验收保存双方 commit、dirty 状态和包含未提交源码的 SHA256，及 Phorge 内置兼容运行库
的版本、上游来源、内容摘要和补丁记录。常规 CI 必跑当前 image 与 S3；手动 published-image 检查是额外项。
本地工作树验收通过不代表远端 CI 已运行，也不代表生产实例满足退役条件。

Gorge Release 反向以 `PHORGE_CONTRACT_REF` 或 `phorge_ref` 固定 Phorge commit，要求
双方工作树干净，并把十四个候选 digest 与成功验收绑定到 `release-manifest.json`。
完整 Go 阶段中，带用例名的 skip 判为失败；无 `Test` 字段的包级无测试文件事件不计
为跳过。各候选镜像均验包装，实际 image/render 容器还单独绑定 digest；这不是十四个
候选容器的完整业务验收。发布和 `latest` 不倒退规则见 [delivery](delivery.md)。

切流前在 Phorge 根运行 `python3 scripts/operations/preflight.py` 检查 production
overlay；加 `--check-db-grants` 才核验运行中 db-api 的实际账号权限。后者从 db-api
网络命名空间使用实际凭据读取 `SHOW GRANTS`，拒绝写权限、未知授权或角色。
配置检查会核对真实 backend、唯一非空 mailer key、通知端口回环绑定和 Worker
`stop_grace_period >= drain + 15s`；默认 drain 30 秒、stop grace 45 秒。通过 preflight
不表示备份恢复或 provider 投递已验证。

## Docker 单机栈的可执行运维入口

Phorge 仓库新增 `bin/ops`：backup、verify、restore-test、restore、report 和 archive。
操作说明与适用拓扑见配对 Phorge 仓库 `scripts/operations/README.md`。
基础备份覆盖 bundled 单 MySQL 与应用命名卷。Redis、S3、integrations 状态、外部
MySQL、自定义数据库集群和未覆盖的持久写者继续拒绝自动打包；不能跳过后仍称完整。
显式单 Elasticsearch 8 拓扑可提供 `--search-es-endpoint` 和
`--search-es-repository` 使用原生快照 adapter。所有搜索 backend 与 projection 必须
指向同一 endpoint，索引全部在范围内；认证通过受控 `GORGE_BACKUP_ES_API_KEY`
环境变量提供，不写入 URL 或命令行。其他搜索引擎、多集群和数据目录复制不受支持。

adapter 在已捕获应用写者停写后协调搜索原生快照与 SQL/卷备份，只有完整 `SUCCESS`
快照才记录外部引用。`search-snapshot.json` 不包含快照字节，必须独立保存外部
repository、兼容版本和保留期。`--exclusive-access` 仍是操作员对外部 CLI/写者的
声明，不是自动分布式锁。失败或可捕获中断尝试恢复原先运行的写者；SIGKILL、掉电
或 Docker 不可用时需按 manifest 核对并恢复，不能假定 finally 已执行。

restore-test 是隔离 SQL/卷恢复演练，检查归档摘要、文件字节/链接/权限、SQL 导入及
表数，保留 `businessIntegrity=not_verified`。含搜索引用时还保留
`externalSearchRestore=not_verified`，report 发出 `SEARCH_RESTORE_UNVERIFIED`。
隔离 ES 回归成功不替代具体生产 repository 的恢复；切流前须在隔离搜索集群恢复该
快照，核对索引、对象、权限及 SQL/outbox 状态，再完成业务引用和 unknown 核对。
archive 复制有效备份包并保留源文件；业务账本冷归档仅输出前置条件，不执行删除。
report 提供 JSON/退出码告警及同物理库容量净增长，不自动注册通知或定时任务。
