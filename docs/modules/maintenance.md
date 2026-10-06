# 日志与缓存清理

`gorge-maintenance` 在 `:8200` 提供健康检查和只读清理状态，并在后台执行固定的 MySQL 分批删除。登记九个清理器：`cache.general.ttl`、`cache.general`、`cache.markup`、`conduit.logs`、`daemon.processes`、`daemon.lock-log`、`differential.parse`、`differential.viewstate`、`multimeter.events`。文件、认证、业务销毁、任务归档、outbox/inbox 回执不在范围内。

## 配置与执行权

`GORGE_MAINTENANCE_CACHE_DSN`、`GORGE_MAINTENANCE_CONDUIT_DSN`、`GORGE_MAINTENANCE_DAEMON_DSN` 以及 `GORGE_MAINTENANCE_DIFFERENTIAL_DSN`、`GORGE_MAINTENANCE_MULTIMETER_DSN` 分别指向 Phorge 的对应数据库主库。新增角色必须先执行 storage upgrade，以建立控制表和 Multimeter 前导 epoch 索引。Webhook 投递历史、Herald 字段裁剪仍不在本次范围。可以只配置一个角色，但要导入该角色全部登记项。DSN 必须包含数据库名，禁止 multiStatements。`GORGE_SERVICE_TOKEN` 非空；状态接口仅接受 `X-Service-Token`，不接受 URL token。`GORGE_LISTEN_ADDR` 默认为 `:8200`。

每个目标库包含 `gorge_gc_control`。控制行和目标表都必须是 InnoDB，使用同一个本地事务；控制状态不放到另一台服务器。服务账号需要目标表的 SELECT/DELETE 和控制表的 SELECT/INSERT/UPDATE，不需要 CREATE、DROP、ALTER。只连接写主库，不路由到读副本。检查 MySQL read_only；Phorge 应用进入只读维护前应先 pause 已移交的清理器。

PHP 确认开关 `phd.gorge-cleanup` 默认 false。先 storage upgrade 再部署 PHP；在所有 PHP 节点确认部署完成并启用该值，才可导出可导入的策略。新 PHP 始终检查控制行，关闭确认开关不能绕过已有执行权保护。自动及手动 GC 共用基类 guard：在目标连接同一事务中初始化/锁住控制行，owner 不为 php 就跳过；控制 schema 缺失会失败，禁止回退删除。未登记的领域 GC 保持 PHP。

执行权默认 php。Go 的 import 只导入策略，不接管。owner 从 php 或 gorge 变更前必须 pause；pause 在控制行事务锁上等待当前批次，随后清空租约并提升 epoch/fence。旧租约无法继续执行。PHP 未升级或开关未启用的旧节点无法被控制行阻止，因此必须先完成全节点升级和确认在途工作退出。

## CLI 和策略

```text
gorge-maintenance import cleanup.json
gorge-maintenance list
gorge-maintenance dry-run cache.general.ttl
gorge-maintenance pause cache.general.ttl
gorge-maintenance resume cache.general.ttl gorge
gorge-maintenance run-once cache.general.ttl
gorge-maintenance serve
```

PHP `scripts/setup/export_gorge_cleanup.php` 读取有效保留策略，包括已加载的数据库覆盖，输出 version、guardEnabled、readOnly 和 policies。旧 null/0 归一为 indefinite；自动 TTL 使用 expires_at。只读或未启用 guard 的导出会被拒绝。策略只能引用登记 ID，不接受 SQL/表名/WHERE。import 必须在 php 或 paused 状态执行，不能热替换运行中的删除策略。跨库 import 不是全局事务；输出每个成功项，失败时保留已导入项并允许幂等重试。

预算范围：每批 1–100 行，批次超时 100–2000ms，每轮最多 30 秒/10000 行，周期 60–604800 秒。默认 100 行、2 秒、30 秒/10000 行、4 小时。后台单执行通道；每个数据库通过保留控制行 `__database_budget__` 串行执行 Go 删除批次，批次完成后至少间隔 200ms，多实例和手动执行共用此预算，持续删除速率不超过 500 行/秒。这条控制行不属于可移交的清理器，也不用于限制旧 PHP 清理器；轮转各清理器，预算耗尽 30 秒后续跑。错误指数退避，租约 15 秒，每个成功批次续租。手动 run-once 也受相同预算约束，只绕过 nextRun，不绕过执行权。

## SQL 与运行状态

固定谓词是时间列非 NULL 且严格小于本轮 cutoff；年龄策略 cutoff 为数据库时间减 TTL，自动过期 cutoff 为数据库时间。DELETE 按时间列、id 排序并有 LIMIT。缓存刷新与删除在数据库锁下重新检查，不能先取 ID 再无条件删除。无永久 ID 高水位，低 ID 回填仍可被后续批次发现。

每批在短事务中锁控制行，验证 owner/epoch/fence/expiry/hash，删除并累计已删行数；失败全部回滚。提交响应丢失时不盲目修改累计数，后续从真实数据和控制行恢复。schema 校验检查 InnoDB、id 主键、时间列类型及可见的前导时间索引；启动和每批删除前检查实际 EXPLAIN，必须使用索引范围访问，运行中删除索引也会停止执行；缺少索引不执行 run-once，后台失败退避。迁移或修改索引期间先 pause。

`GET /api/maintenance/collectors` 返回执行权、lease、累计数量、下次运行时间和状态，不返回原始内容。`/healthz` 表示存活；`/readyz` 校验配置角色的 schema 和已导入策略，indefinite 不算故障。结构化 batch 日志包含 collector、deleted、fence；故障状态不保存数据库凭据。`GET /api/maintenance/metrics` 同样鉴权，输出累计删除数、最近成功时间与连续失败数的 Prometheus 文本；未新增长期运行历史表。

## 切换与回滚

先备份并跑 storage upgrade；升级所有 PHP/CLI/daemon 并启用 guard；导出策略、import；dry-run；逐项 pause，再 resume 到 gorge。先 TTL 缓存，再年龄缓存和日志；lock-log 保留 indefinite。观察完整周期后再扩围。

回滚时 pause，等待事务完成，resume 到 php，再恢复 PHP 自动调度。只切换执行权，不能恢复已删除数据。暂不删除 PHP 清理 SQL，保留灰度回滚路径；数据验收后才能删除具体 collectGarbage 实现，仍有领域 GC 时不能删除 Trigger/基类。

## 验证

`GORGE_TEST_MYSQL_DSN` 指向一次性 MySQL 后执行 `go test -race ./internal/maintenance/cleanup`。测试创建随机数据库并删除，覆盖 TTL NULL/边界、刷新竞争、回填、并发领取、fence/expiry、事务回滚、退避、无索引和暂停串行。PHP `tests/contract/cleanup/runtime.php` 验证有效策略、真实基类 guard、目标连接同事务及缺 schema 失败关闭。

## 修改保留策略与异常恢复

登记项的 `garbage set-policy` 在 owner=gorge 时拒绝变更，必须先 pause。
写入本地配置前，会提交策略失效标记并提高 epoch/fence；写入期间状态为
`policy_changing`，Go 拒绝 import。完成后为 `policy_pending`，旧策略无法
resume 到 gorge。重新从 PHP 导出有效配置、import 后，才能恢复 Go 清理。
这也保护原来由 PHP 执行的登记项，防止未来移交时误用之前的导出。

若 PHP 在写配置时退出，`policy_changing` 会保留，清理不会恢复。
先确认配置文件与有效配置一致、确认没有仍在执行的策略修改进程，再由维护人员
将该控制行的 lastState 改为 `policy_pending`，重新导出并导入。
直接修改配置文件、数据库配置覆盖或使用其他配置命令时，也必须遵循
pause → 修改 → 重新导出/import → resume；这些入口不能自动同步 Go 策略。

跨仓库检查已接入 Gorge 的 Contract drift 工作流和 Phorge 的 runtime contracts。
前者支持两个仓库 ref，后者支持 gorge_ref。PHP 先运行真实 MySQL 测试并生成导出，
Go 再验证该导出和 PHP 迁移 DDL；所需文件缺失会失败，不能以跳过代替通过。

## 新增技术清理器

`differential.parse` 默认保留 14 天，`differential.viewstate` 默认 180 天，
`multimeter.events` 默认 90 天，仍以 PHP 有效策略导出为准。解析缓存的目标是
`differential_changeset_parse_cache`，不是业务 changeset 表。
生产覆盖配置要求五个角色 DSN，启动检查要求九项均 owner=gorge；
仅开启部分角色时不要使用全量生产切换配置，导入文件也应仅包含对应角色策略。
新增控制表位于 20261007 differential/multimeter 迁移；Multimeter 同时添加 epoch 索引。
