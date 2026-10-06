# 持久触发器调度

调度器运行在 `gorge-taskqueue` 中，第一版只支持 MySQL，与 Phorge
`{namespace}_worker` 的事件和任务表共用事务。Redis 队列不能启用此调度器。
默认不开启；数据库执行权默认 `php`，不是服务启动后自动接管。

## 职责与范围

PHP `trigger.plan` 只计算业务时钟并导出任务，不执行 action、不推进事件。
Go 扫描变更和到期记录，使用短事务锁认领，然后在同一事务内完成任务入队、
`lastEventEpoch` / `nextEventEpoch` 更新和版本记录。没有持锁的远程 RPC。

第一版接受 `PhabricatorScheduleTaskTriggerAction`，包括其 priority、objectPHID、
containerPHID、delayUntil 选项及 `trigger.last-epoch` / `trigger.this-epoch` 数据。
Log action 和扩展自定义 action 不支持接管；owner 命令会拒绝已有不支持的触发器。
接管预检还会计算当前排期和下一次排期，拒绝计算异常、不前进或超出 uint32
时间范围的时钟；预检不执行 action，也不改动事件。后续每次计划继续校验这些约束，
预检不能保证业务对象或时钟参数在未来变化后仍然有效。
接管之后新增的不支持触发器会报告错误并退避，不会静默执行 PHP action。

原有 OneTime、Never、Metronomic、DailyRoutine、Subscription 等时钟继续由 PHP
计算，保留其时区和夏令时规则。PHP TriggerDaemon 的 GC、Nuance、日历通知、搜索
删除恢复循环暂时保留；这里只转移通用触发器排期和到期任务入队。

## 配置与首次切换

1. 升级 **全部** PHP Web / daemon / 管理命令实例到带所有权守卫的版本，执行
   `bin/storage upgrade`，包括 `20261007.worker.scheduleridentity.sql` 身份迁移。
   旧版本 daemon 不认识所有权，不能混跑。
2. 先保持 `GORGE_TASKQUEUE_SCHEDULER_ENABLED=false` 启动 Phorge、队列和 Conduit。
   配置共享 Conduit token，确认现有服务启动完成。
3. 设置 `GORGE_TASKQUEUE_SCHEDULER_ENABLED=true`，以及
   `GORGE_TASKQUEUE_SCHEDULER_CONDUIT_URI` 和 `GORGE_TASKQUEUE_SCHEDULER_CONDUIT_TOKEN`，
   重建/重启 taskqueue。Compose 将最后一项映射为 `GORGE_CONDUIT_TOKEN`。
   第一版需要 MySQL writer；不可将调度器指向只读副本。
4. 确认 taskqueue `/readyz` 成功，认证 `/api/queue/meta` 返回
   `schedulerProtocol: 1`、`schedulerAtomicEnqueue: true`，以及与 Phorge
   `worker_gorgeschedulercontrol.databaseID` 一致的 `schedulerDatabaseID`。
5. 在 Phorge 中执行：

   ```sh
   bin/trigger owner
   bin/trigger owner --set paused
   bin/trigger owner --set gorge
   ```

   `owner` 命令验证队列能力和现有触发器支持范围。切换要经过 `paused`。
   它等待同一个数据库控制锁，排空已开始的 PHP/Go 调度事务，然后增加 owner epoch。保留已有事件与物化版本，按 PHP 游标补齐历史版本记录，
   防止待执行的首次周期事件被重置或一次性事件被重发。

先升级迁移、再启用调度器，避免新安装依赖链中队列 `/readyz` 等待尚未启动的 PHP。
默认容器健康检查仍是 `/healthz`；调度器启用后的 `/readyz` 额外验证 schema、InnoDB
和 PHP 协议，运行循环每次都重新验证能力。

身份迁移为 worker 数据库生成持久 UUID。就绪检查、接管、每次计划请求及提交事务
均核对该身份，避免 Go 队列或 Conduit 误指向另一套数据库。身份是数据库标识，
不是服务凭据；备份恢复保留身份。克隆到独立环境会复制身份，需在克隆库处于
`paused` 且没有调度事务时重新生成 UUID，并配置独立服务凭据。身份校验不能证明
连接目标是 writer，仍需按配置要求连接 writer。

## 恢复、更新与取消

计划包含 trigger ID/version、事件 ID/last/next、物化版本和 owner epoch。
提交事务重新锁定并检查全部快照：其他副本推进、编辑、删除、暂停或切换执行权
都会拒绝过期计划。队列写入失败会回滚整个事件/任务事务。

进程提交前退出：数据库回滚，事件仍到期；提交后退出：事件已推进，旧快照重放
不能再次入队。这里保证的是同一未变更事件的原子入队，不是邮件/Webhook 的外部
副作用恰好一次。业务 worker 仍需定义自己的幂等和过期行为。

编辑将触发新版本排期；删除会让尚未提交的计划失效。已入队任务不因删除 trigger
自动取消，因为它已经是独立任务；有此要求的业务应在执行时检查当前对象状态。

每批最多 100 个候选；按上次物化/执行/错误处理时间公平轮转。计划错误持久退避
30 秒，单个失败 trigger 不阻塞同批其他 trigger。下次运行扫描数据库到期事件，
不依赖进程内 timer，因此停机不会丢失尚未入队的事件。重复时钟的补偿行为由原
PHP clock 决定，不承诺无上限积压能在固定延迟内处理完。

## 暂停与回切

先 `bin/trigger owner --set paused`，再 `--set php` 或 `--set gorge`。
控制行可在调度器服务停止后由 PHP 命令修改；不得通过删表、清空 last epoch 或
只改环境开关完成回切。仅关闭 Go 开关而 owner 仍为 gorge，会停止调度。

更新且迁移后的 PHP TriggerDaemon 应继续运行附属循环；不要删除该进程。
回切到 PHP 仍需使用带共享所有权守卫的版本，不能回切到未迁移 schema 的旧镜像。

## 验证

Go：`go test -race ./internal/taskqueue -run 'Test(Schedule|Scheduler|ExecutionMySQLIntegration)'`。
设置 `GORGE_TEST_MYSQL_DSN`，仅使用可创建/删除随机测试数据库的隔离 MySQL。
覆盖多副本竞争、提交后重放、编辑/删除、owner epoch、持久退避、入队失败回滚。

PHP：`tests/contract/scheduler/runtime.php` 使用隔离 MySQL，覆盖真实时钟计划、
空 epoch、选项、认证、过期版本、动作支持范围以及共享所有权锁和事务回滚。该测试同时运行既有六项时钟回归（含夏令时和订阅）。
设置 `GORGE_TEST_ARCANIST_DIR`、`GORGE_TEST_MYSQL_PORT`、`GORGE_TEST_MYSQL_PASSWORD`。

只读模式会拒绝新的 capability/plan 请求，使 Go 停止生成计划。严格维护停机应先切
`paused` 并等待命令完成，再进入只读或停止数据库；只读配置变化本身不能撤销
已经计算且正在提交的计划。回切 PHP 时，其旧游标会跳过已由 Go 物化的相同版本，
保留待执行事件而不重新计算首次周期时间。接管预检按每页 100 个触发器检查。
