# taskqueue / worker 模块

把 Phorge 的守护进程工作队列搬到 HTTP 后面：`gorge-taskqueue` 拥有 `{namespace}_worker` 的 worker_activetask、worker_taskdata、worker_archivetask，负责入队、租约、归档；`gorge-worker` 是它的 HTTP 客户端，租走任务、跑完、把结果回报。这是仓库里第一对拆成两个二进制的域。

| | |
|---|---|
| 二进制 | `gorge-taskqueue`（队列）、`gorge-worker`（消费者） |
| 端口 | `:8090`（taskqueue）、`:8170`（worker） |
| 包 | `go/internal/taskqueue/`、`go/internal/worker/`（handlers 在 `go/internal/worker/handlers/`） |
| 契约 | [`api/openapi/taskqueue.yaml`](../../api/openapi/taskqueue.yaml)（含 worker 的 `/api/worker/stats`） |
| 固件 | `tests/contract/taskqueue/`（含 `unavailable/`） |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) 第十节 ← **改动前必读** |

**这是仓库里第一对拆成两个二进制的域，而「为什么是两个」正是本域最该先讲清楚的一件事**——它和 render+diff 合并那段恰好相反。render 与 diff 合并成一个二进制，是因为两者都是无外部依赖的纯计算、拆进程换不来隔离收益。taskqueue 与 worker 不合并，是因为 **worker 是 taskqueue 的 HTTP 客户端，不是它的同进程协程**：worker 通过 `GORGE_WORKER_TASK_QUEUE_URL` 拨 taskqueue 的 `/api/queue/**` 租约，两者可以各自独立伸缩（一个 taskqueue 前面挂若干 worker，或给某个重类开一个专用 worker）。把它们塞进一个进程，要么把这层 HTTP 契约降级成进程内调用、要么逼 worker 直接调 `Store` 而绕过契约——两条路都把「可独立部署」这个既有事实弄没了。所以 `cmd/` 下是两个入口、compose 里是两个 service，本文档同时覆盖两个域。

taskqueue 本身几乎是 webhook 的翻版（同为「有外部依赖 + 后台性质」），读它之前值得知道的三件事和 webhook 同源：基础契约固件覆盖入队、租约及只读端点、e2e 脚本能跑通完整的 enqueue→lease→complete 但碰不到 worker 侧的真实任务执行、字段名是硬约束。worker 通过 HTTP 消费队列，并可直接连接 Feed/邮件 outbox 数据库；`/readyz` 校验队列协议、PHP capabilities、政策与原生执行依赖，`/healthz` 仍只报告进程存活。见 [worker](worker.md)。

## 1. 职责边界

### taskqueue

**负责**：把任务入队进 `worker_activetask`（带 `worker_taskdata` 里的负载）、按「优先级升序、id 升序」把行租给 worker、并在任务完成/永久失败/取消时把行搬进 `worker_archivetask`。租约到期、临时失败的重试退避、yield 窗口三件事由它管，见第 3 节。它有两个后端——MySQL（默认，读写 Phorge 自己的库）与 Redis（把队列挪出主库）——两者满足同一个 `Store` 接口。

**不负责**：**定义任务**。哪个事务该排哪个 worker、任务负载长什么样、优先级取哪个带，全部由 Phorge 侧的 `PhabricatorWorker::scheduleTask` 决定并写好；本服务只入队、租出、归档。它也不建表、不改表结构、不需要 DDL 权限——三张 worker 表都是 Phorge 的 `bin/storage upgrade` 的产出。

**它必须替换 Phorge 自己的 taskmaster 守护进程，而不是与之并存。** 队列在库里，`phd` 与 gorge-worker 谁都能取，旧 PHP 消费者不参与新 fencing/finalize 协议，混用可能重复执行或丢失收尾结果。让路的方式是停掉 PHP 侧的 taskmaster，保留仍承担 trigger、fact 等职责的守护进程；本 fork 的仓库拉取 daemon 已退役；与 webhook 第 38 条同源。

**有外部依赖，而且和 webhook 一样是这一类里最硬的那种。** 基础 `/readyz` 检查队列后端是否答话；启用 scheduler 后还会检查调度来源与持久调度 schema，见 3.5 和 [scheduler](scheduler.md)。基础 ping 不保证所有队列表结构正确。

### worker

**负责**：一个租约循环——从 taskqueue 租任务、按 task class 找到 handler 跑它、把结果（完成/永久失败/临时失败/yield）回报给 taskqueue。它是 `PhabricatorTaskmasterDaemon` 的等价物。

**不负责队列存储**：队列租约与归档通过 taskqueue HTTP 契约完成。Worker 可直接访问 Feed/邮件 outbox 数据库，运行原生 handler，并通过 Conduit 执行仍委派给 PHP 的任务。完整执行模式与就绪依赖见 [worker](worker.md)。

**它的 `/api/worker/stats` 是只读的，而且是进程内计数**，不是查库——所以它描述的是「这个 worker 这辈子」的数字（重启即清零），与 taskqueue 的 `/api/queue/stats`（每次真查库）性质相反。

## 2. 路由与依赖

### taskqueue：`/api/queue`

```go
// 基础路由摘录；执行协议、inbox 与 operations 路由见下表。
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/queue")
	g.Use(auth.Token(deps.Token))

	g.Post("/enqueue", enqueue(deps))
	g.Post("/lease", lease(deps))
	g.Post("/complete", complete(deps))
	g.Post("/fail", fail(deps))
	g.Post("/yield", yield(deps))
	g.Post("/cancel", cancel(deps))
	g.Post("/awaken", awaken(deps))
	g.Get("/stats", stats(deps))
	g.Get("/tasks", listTasks(deps))
	g.Get("/tasks/:id", getTask(deps))
}
```

| 方法 | 路径 | 鉴权 | 成功响应 |
|---|---|---|---|
| POST | `/api/queue/enqueue` | 需要 | 信封，创建出的 `Task` |
| POST | `/api/queue/lease` | 需要 | 信封，`Task[]`（空队列是 `[]` 而非 `null`） |
| POST | `/api/queue/complete` | 需要 | 信封，归档行 |
| POST | `/api/queue/fail` | 需要 | 信封，`{"status":"ok"}` |
| POST | `/api/queue/yield` | 需要 | 信封，`{"status":"ok"}` |
| POST | `/api/queue/cancel` | 需要 | 信封，归档行 |
| POST | `/api/queue/awaken` | 需要 | 信封，`{"awakened": N}` |
| GET | `/api/queue/meta` | 需要 | 执行协议与 scheduler 能力 |
| POST | `/api/queue/finalize`、`/api/queue/resolve`、`/api/queue/renew` | 需要 | 租约保护的结果提交、恢复与续租，见后文 |
| POST | `/api/queue/enqueue-event` | 需要 | 持久 inbox 事件接收 |
| GET | `/api/queue/operations` | 需要 | 后端运行盘点；不支持或查询失败为 503 |
| GET | `/api/queue/stats` | 需要 | 信封，`{activeCount, leasedCount, archivedCount, failedCount}` |
| GET | `/api/queue/tasks` | 需要 | 信封，活跃任务 `Task[]`（不含 `data`） |
| GET | `/api/queue/tasks/:id` | 需要 | 信封，单个 `Task`（含 `data`）；不存在为 404、非数字 id 为 400 |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） | 裸 `{"status":"ok"}` |

**lease owner 走 `X-Lease-Owner` 头而不是 body**：它标识调用方（哪个 worker），不是这次请求。缺头时回落到 `IP:gorge-taskqueue`，够区分不同 worker 的租约。这个值直接写进 `worker_activetask.leaseOwner`，是 Phorge 的列、会被它的守护进程控制台读回，所以是契约的一部分。body 还可带 `taskClasses` 白名单；筛选发生在取得租约之前，避免专用 worker 先占用、再失败并延迟另一个池的任务。

`Deps` 包含 `Store`、`Token`、`SchedulerEnabled` 与 `SchedulerReady`。**`Store` 是 interface，这是契约固件能存在的前提**：两个后端（MySQL/Redis）之外，固件与 handler 单测注入的是第三份手写内存实现（`memstore_test.go`），照 webhook 的做法。

### worker：`/api/worker`

以下为 stats 路由摘录，完整接口见下表。

```go
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/worker")
	g.Use(auth.Token(deps.Token))

	g.Get("/stats", stats(deps))
}
```

| 方法 | 路径 | 鉴权 | 成功响应 |
|---|---|---|---|
| GET | `/api/worker/meta` | 需要 | 静态执行协议能力 |
| GET | `/api/worker/notification-stats` | 需要 | 可选通知统计，配置回调后才注册 |
| GET | `/api/worker/stats` | 需要 | 信封，`{processed, failed, active, supported}` |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） | 裸 `{"status":"ok"}` |

`Deps` 包含 `Consumer`、`Token` 与可选的 `NotificationStats`。stats 读的是 `Consumer` 里的原子计数器，永不失败，也就没有错误路径。`supported` 在配了 Conduit fallback 时含 `*`，让 setup check 能区分「把一切委派给 PHP」与「只handle固定几类」。

Worker 的 `Consumer.Run` 运行租约消费循环；taskqueue 启用 scheduler 时也运行后台调度循环。停机需先停止取新任务并收尾在途工作，再释放数据库资源。见 [worker](worker.md) 和 [scheduler](scheduler.md)。

## 3. 核心实现

### 3.1 租约是两阶段的，字段名是硬约束

`Lease` 在一个事务里分两阶段取任务，两阶段的先后顺序就是 Phorge 的调度语义：

```sql
-- 阶段一：从未被租过的任务，按优先级、id 排序（新任务优先）
SELECT id FROM worker_activetask
 WHERE leaseOwner IS NULL AND leaseExpires IS NULL
   [AND taskClass IN (...)]
 ORDER BY priority ASC, id ASC LIMIT ?
-- 阶段二：租约已过期的任务（崩溃的 worker / 到期重试），补足名额
SELECT id FROM worker_activetask
 WHERE leaseExpires < ?
   [AND taskClass IN (...)]
 ORDER BY priority ASC, id ASC LIMIT ?
```

抢到之后把 `leaseOwner` 与 `leaseExpires`（= now + `LeaseDuration`）写上，再 `JOIN worker_taskdata` 把负载一起捞出来返回。**排序是 `priority ASC, id ASC`**：优先级数字越小越急（`PriorityAlerts=1000` < `PriorityDefault=2000` < … < `PriorityImport=4000`），同优先级下先进先出。

**每一个 JSON 字段名都是 Phorge 的列名，一个都不能改**：`taskClass`、`leaseOwner`、`leaseExpires`、`failureCount`、`dataID`、`failureTime`、`objectPHID`、`containerPHID`、`priority`、`dateCreated`、`dateModified`。它们由 Go 服务和 PHP 的 `PhabricatorWorkerActiveTask` / `PhabricatorWorkerArchiveTask` 双向读写，改名的后果见第 5 节，权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第十节。

`leaseExpires` 与 `failureTime` 是可空列，所以在 contract 里是 `*int64` 加 `omitempty`：一个从未被租的任务没有 `leaseExpires`，把它编码成「present-but-zero」会让读者分不清「在 epoch 时刻被租」和「从没被租」。

**`Enqueue` 的 `id` 由 `lisk_counter` 计数器分配，不走 `LastInsertId()`。**`PhabricatorWorkerActiveTask` 声明 `CONFIG_IDS => IDS_COUNTER`，它的 `id` 列是 `int unsigned NOT NULL` 且**无** AUTO_INCREMENT。所以 `Enqueue` 在同一事务里先跑 Phorge 那条 `INSERT INTO lisk_counter ... ON DUPLICATE KEY UPDATE counterValue = LAST_INSERT_ID(counterValue + 1)`（`nextCounterValue`）拿到 id，再把它显式写进 `worker_activetask`——省略 `id` 的 INSERT 会报 `Error 1364 Field 'id' doesn't have a default value`。`worker_taskdata` 则是默认的 `IDS_AUTOINCREMENT`，所以它的 `dataID` 照常靠 `LastInsertId()` 拿。用同一个计数器行是硬约束：`phd` 与 gorge-taskqueue 共享它才不会分配出撞号的 id，见第 5 节与 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第十节 10.1。


### 3.2 归档是一次事务里的「插入 + 删除」

`Complete`（result=0）、`Fail` 的永久分支（result=1）、`Cancel`（result=2）都走同一个 `archiveTask`：在一个事务里把行整个复制进 `worker_archivetask`（多带 `result`/`duration`/`archivedEpoch` 三列），再从 `worker_activetask` 删掉。**一个事务**是为了让任务永不同时在两张表里、也永不两张表都不在。result 的三个整数值（0/1/2）对齐 `PhabricatorWorkerArchiveTask::RESULT_*`，被 Phorge 的守护进程控制台读回，所以整数值本身也是契约。

### 3.3 临时失败 vs 永久失败，是 Phorge 分的两种

`Fail` 按 `permanent` 分岔，对齐 Phorge worker 层的两种失败：

- **永久失败**（`PhabricatorWorkerPermanentFailureException`）：归档成 `ResultFailure`，永不重试。
- **临时失败**：任务留在队列里，`failureCount + 1`、记 `failureTime`、清 `leaseOwner`、把 `leaseExpires` 设为 `now + retryWait`（`retryWait` 可由请求覆盖，否则用配置的 `RetryWait`，默认 300 秒 = Phorge 的 `getWaitBeforeRetry`）。清 owner 又设未来的 expires，意味着这行在退避期内既不算「未租」也不算「租约过期」，租不到——退避就是这么实现的。

`failureCount > 0` 是 `stats.failedCount` 的判据：它数的是「至少失败过一次、仍在队列里」的任务，不是归档里的失败。

### 3.4 yield 借用 leaseOwner 当哨兵，没有新增列

`Yield`（`PhabricatorWorkerYieldException`：任务请求稍后重试）把 `leaseOwner` 写成哨兵值 `(yield)`（`contracts.YieldOwner`）、`leaseExpires` 设为 `now + duration`（下限 5 秒，对齐 Phorge 最小值）。**用一个 leaseOwner 值而不是新增 status 列**，理由和 webhook 用 `dateModified` 抢占同源：这张表的形状是 Phorge 的，不能长列。一个 yield 的任务「被没有人真正持有」，`Awaken` 靠这个字符串精确识别它——改名会让每一个 yield 的任务失联。

`Awaken`（`PhabricatorWorker::awakenTaskIDs`：Phorge 想把 yield 的任务提前拉回）只拉「owner 是 `(yield)`、`failureCount = 0`、且 yield 窗口（一小时）还没过」的任务，把它们的 `leaseExpires` 提前。窗口外的老 yield 任务不动。

### 3.5 队列就绪与调度就绪

未启用 scheduler 时，判据是后端答话（MySQL 后端 `PingContext`，Redis 后端 `Ping`）。基础队列探针不检查 worker 表存不存在；启用 scheduler 后还需校验调度 schema 和 Conduit source，见 [scheduler](scheduler.md)。**基础探针不查表**，理由与 webhook / file-storage 同源：三张表由 Phorge 的 `bin/storage upgrade` 建，跑那条命令的容器可能后启动，所以要求表存在等于把一个健康部署在它首次迁移期间报成坏的。`OpenDB` 同样**不 ping**、`NewMySQLStore` 也不在打不开时 `os.Exit`——那让排在数据库之前的容器进重启循环。

和 webhook 一样，**「只 ping」没有把探针从首启闭环里摘出来**：`WorkerDSN()` 带的是库名 `{namespace}_worker`，库不存在时 ping 在连接阶段就失败（`Error 1049 Unknown database`），而那个库也是 `bin/storage upgrade` 建的。所以首次启动必有一段 `/readyz` 不通的窗口，由 Phorge 自己结束；编排侧对 taskqueue 用 `service_started` 而非 `service_healthy` 来躲开死锁，和 webhook 那条依赖同形。

**这个缺口是 `/readyz` 必须存在的全部理由**：一个连不上后端的 taskqueue 在监听、`/healthz` 答 200、却租不出一个任务，和「慢了一拍」长得一模一样。容器 healthcheck 因此打 `/readyz`。

### 3.6 Worker：租约生命周期与领域委托

`Consumer.Run` 按任务类领取，执行前确认 v1 和 leaseOutcomes 能力并校验
当前租约，执行中周期续租；失去所有权立即取消 handler。成功结果统一
通过 finalize 原子提交，失败和 yield 通过 resolve 提交。结果上报使用
独立的十秒停机安全上下文，409 不再重试，确认写入后才更新进程计数。

`handlers.RegisterWithFeedPolicy` 配置原生 Feed 政策文件时，版本 1 Feed
直接由 Go 检查静默模式和 URI 并投递，不调用 PHP prepare。旧 key 任务仍
委托 PHP，但只转换为快照后续任务。其余未迁移任务通过 Conduit 委托。
无 Conduit 时只领取已注册原生类；TaskClassFilter 在领取前筛选。

Conduit 仍使用表单编码 params JSON，`worker.execute` 独立校验服务令牌，
并协商 capabilities/prepare/execute。PHP 返回业务结果、重试政策和后续
任务，Go 负责队列状态。长请求由心跳会话取消控制，取消 HTTP 客户端不
保证服务端副作用停止，因此任务业务必须考虑幂等。

可选 Feed outbox relay 使用 feed 数据库，职责是将已提交事件发送到队列
inbox，不执行领域逻辑。`/readyz` 检查队列协议、配置的 Feed 政策和 outbox
表；`/healthz` 仍表示存活，编排无需因暂时依赖故障重启进程。

## 4. 配置

### taskqueue（`gorge-taskqueue`，`:8090`）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GORGE_LISTEN_ADDR` | `:8090` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权 |
| `GORGE_TASKQUEUE_BACKEND` | `mysql` | 后端：`mysql` 或 `redis` |
| `GORGE_TASKQUEUE_MYSQL_HOST` | `127.0.0.1` | 见下 |
| `GORGE_TASKQUEUE_MYSQL_PORT` | `3306` | |
| `GORGE_TASKQUEUE_MYSQL_USER` | `phorge` | |
| `GORGE_TASKQUEUE_MYSQL_PASS` | 空 | |
| `GORGE_TASKQUEUE_NAMESPACE` | `phorge` | DSN 里的库名由它拼成 `{namespace}_worker`，必须与该装置的 `storage.default-namespace` 一致 |
| `GORGE_TASKQUEUE_REDIS_ADDR` | `127.0.0.1:6379` | 仅 `backend=redis` 时读 |
| `GORGE_TASKQUEUE_REDIS_PASSWORD` | 空 | |
| `GORGE_TASKQUEUE_REDIS_DB` | `0` | |
| `GORGE_TASKQUEUE_REDIS_KEY_PREFIX` | `gorge:tq:` | 键前缀，两个装置共用一个 Redis 时靠它隔离 |
| `GORGE_TASKQUEUE_LEASE_DURATION` | `7200` | 租约时长（秒），Phorge 的 `PhabricatorWorkerLeaseQuery` 默认 |
| `GORGE_TASKQUEUE_RETRY_WAIT` | `300` | 临时失败重试退避（秒），Phorge 的 `getWaitBeforeRetry` |

**`GORGE_TASKQUEUE_MYSQL_HOST` 不是开关**，与 webhook 同、与 file-storage 的对应变量相反：本域没东西可关，两个后端都要读库，「没配」与「连不上」不是两个值得区分的状态——留空落到 `127.0.0.1`，服务起来、不就绪。

### worker（`gorge-worker`，`:8170`）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GORGE_LISTEN_ADDR` | `:8170` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | 空 | 保护 `/api/worker/stats` |
| `GORGE_WORKER_TASK_QUEUE_URL` | `http://gorge-taskqueue:8090` | 要租的 taskqueue 地址 |
| `GORGE_WORKER_TASK_QUEUE_TOKEN` | 空 | 出示给 taskqueue 的 token，须等于对方的 `GORGE_SERVICE_TOKEN` |
| `GORGE_WORKER_LEASE_LIMIT` | `4` | 每次轮询租多少 |
| `GORGE_WORKER_POLL_INTERVAL_MS` | `1000` | 有活时的轮询间隔 |
| `GORGE_WORKER_MAX_WORKERS` | `4` | 并发跑多少 |
| `GORGE_WORKER_IDLE_TIMEOUT_SEC` | `180` | 空闲状态日志间隔（秒）；0 关闭日志，不暂停轮询 |
| `GORGE_WORKER_CONDUIT_URL` | 空 | 见 3.6，空 = 只租并运行本地实现的类 |
| `GORGE_WORKER_CONDUIT_TOKEN` | 空 | Conduit token |
| `GORGE_WORKER_TASK_CLASS_FILTER` | 空 | 逗号分隔的类白名单，空 = 全部支持的类 |

规范变量命名规则见 [`../platform.md`](../platform.md) 第 4 节。taskqueue 的连接池（`maxOpenConns=25` / `maxIdleConns=5` / `connMaxLifetime=5m`）定在 `db.go`，按「池与 Phorge 共用一台库、要留余量」定；`platform/operations` 提供只读数据库盘点辅助；连接池与写入策略仍由各域负责，判断同 [`../findings.md`](../findings.md) 第 39 条。

## 5. 兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第十节，这里是概述。本域的约束整节都是**静默型**（破坏后不报错、只错到没人发现），分两组：

**一组是任务字段名**：`taskClass` / `dataID` / `leaseOwner` / `leaseExpires` / `failureCount` / `failureTime` / `objectPHID` / `containerPHID` / `priority` / `dateCreated` / `dateModified`，以及归档表多出的 `result`（整数 0/1/2）/ `duration` / `archivedEpoch`。它们直接映射 Phorge 的 `worker_activetask` / `worker_archivetask` 列名，改错一个不会报错，只会让 PHP 侧读到空值、或让 `bin/worker` 的界面把任务显示成另一个样子。

**另一组是抢占语义的地基，和 webhook 的 `status` 同源**：这里没有 `status` 列，但 `leaseOwner` + `leaseExpires` 的组合承担了同样的角色——「谁持有这行、持到几时」。`leaseOwner = (yield)`（`YieldOwner` 哨兵）是 yield 任务的唯一标识，`leaseExpires` 是租约/退避/yield 三个窗口共用的时间列。多一个值、错一个语义，会让任务被两个 worker 同时取走、或让 `awaken` 认不出 yield 的任务——任何一层都不报错。

两个 result 值域细节容易踩：

- **归档的 `result` 是整数不是字符串**（0=success / 1=failure / 2=cancelled），对齐 `PhabricatorWorkerArchiveTask::RESULT_*`。写成字符串会让 Phorge 的控制台读不出结果。
- **优先级带的数值是契约**（`PriorityAlerts=1000` … `PriorityImport=4000`），因为它们决定 `ORDER BY priority ASC` 的排队顺序，而 Phorge 侧按同一套数值入队。

**Redis 后端与 MySQL 后端语义必须一致，但可见性不同。** 选 Redis 时队列不在 Phorge 的库里，所以 Phorge 的 `bin/worker` 与 Web UI 的任务视图会读到一个空队列——这不是 bug，是把队列挪出主库的代价，部署前要知道。两个后端满足同一个 `Store` 接口、共享 `memstore_test.go` 之外的同一批 store 单测语义。

## 6. 域级错误码

**基础队列操作使用平台错误码，扩展执行协议有专用冲突码。** `ERR_LEASE_CONFLICT`（409）表示租约已失效或不属于当前执行；`ERR_EVENT_CONFLICT`（409）表示事件身份与内容冲突；`ERR_OPERATIONS`（503）表示观测不可用或后端不支持。详见 execution、inbox 与 operations 的实现。

taskqueue 的失败模式要么是入参错（`taskClass` 为空、`taskID` 缺失、id 非数字 → `ERR_BAD_REQUEST` 400；任务不存在 → `ERR_NOT_FOUND` 404），要么是「后端没答话」（→ 平台 `ERR_INTERNAL` 500）。真正需要区分的「服务活着但连不上队列」由 `/readyz` 报告并附原因，一个 `ERR_QUEUE_UNAVAILABLE` 在这上面改进不了任何东西，只会得到第二处更差的说法。

这个选择由 `tests/contract/taskqueue/unavailable/`（`stats.json`、`tasks.json`）**从反面**钉住：后端不可达时 message 必须保持通用（`internal server error`），body 里不得出现 SQL、库名、主机、端口或「connection refused」。handler 把 store 的错误**原样往上抛**给平台错误处理器：

```go
s, err := deps.Store.Stats(c.Context())
if err != nil {
	return err
}
```

worker 的 `/api/worker/stats` 读进程内计数器，永不失败，连错误路径都没有。

**新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。**

## 委派执行协议 v1

配套更新 gorge-taskqueue、gorge-worker 和 Phorge 后，worker 在执行业务前检查 `/api/queue/meta` 的 `executionVersion=1`，再以空 taskClass 探测 PHP capabilities，确认协议版本后调用 `worker.execute` 的 prepare 阶段。PHP 返回任务类的最大重试检查结果和所需租约时间；Go 通过 `/api/queue/renew` 续期后，调用 execute 阶段。PHP 收到 failureCount、priority 和当前租约上下文，临时失败使用任务类自定义的等待时间。

成功响应携带序列化的 followups。Go 通过 `/api/queue/finalize` 在 MySQL 事务或 Redis Lua 脚本中提交父任务归档及全部子任务，子任务保留延迟、对象和容器关联，未指定优先级时继承父任务优先级。归档保留 owner/expiry 作为重试回执，重复提交不会重复入队；当前租约不匹配或已经过期时返回 409 `ERR_LEASE_CONFLICT`。这保证队列提交的一致性，不保证 PHP 业务副作用恰好执行一次：PHP 响应丢失或进程崩溃仍需业务幂等或持久化执行结果。

`worker.execute` 独立校验 `X-Service-Token`，`gorge.conduit.token` 必须配置非空值并与 Go worker 和 gateway 使用的 token 一致。部署顺序为先更新 taskqueue 和 PHP，再更新 worker；升级期间暂停 taskmaster/worker 消费，完成后只恢复 Go 消费者。旧 worker 与新版 PHP 的委派协议不兼容，回滚应同步回滚配套组件。

Feed HTTP 新任务包含 deliveryVersion=1、uri 和 PHP 生成的完整表单 body。配置 Feed policy 文件时，Go 每次执行读取当前 silent 和 hooks 政策后发送快照；未配置时保留 PHP prepare；旧 key/uri 任务仍委派 PHP 处理，避免丢弃队列存量。

旧 complete/fail/yield 接口仍保留兼容用途；新消费者使用 finalize/resolve 的租约保护。原生 PHP taskmaster 已退役，不能将失败结果回退到它。后文描述当前生命周期协议与持久回执；旧格式任务须单独排空。

验证：`go test ./...`；真实 Redis 使用 `GORGE_TEST_REDIS_ADDR=host:port go test ./internal/taskqueue -run TestExecutionRedisIntegration -v`，测试只清理自己创建的随机前缀。PHP 使用 `php tests/contract/worker/execution.php`。

## 执行生命周期与事务事件

执行协议仍为 v1，`GET /api/queue/meta` 另返回 `leaseOutcomes: true`。
新的 Worker 要求此能力；混合版本必须先升级 queue 再升级 worker。
`POST /api/queue/resolve` 使用 `taskID/leaseOwner/leaseExpires`，并指定
`outcome: retry|failure|yield`；`retryWait` 为秒，yield 使用 `duration`。
旧租约、已过期租约、重复 resolve 返回 409，避免重复增加失败次数。
成功结果统一走 finalize；409 不应重试或回退到旧接口。
旧 complete/fail/yield 路由暂供历史 PHP 客户端使用，不能混用来规避租约校验。

Worker 执行前确认租约，执行中每至多 30 秒续租一次；续租请求受当前租约
截止时间约束。失去所有权时取消 handler，不再报告结果。PHP 请求取消不能
保证服务端已停止，因此外部操作仍需要幂等键，不能宣称 exactly-once。

`POST /api/queue/enqueue-event` 接受 `{eventID, task: EnqueueRequest}`。
相同 ID 和请求返回原任务；相同 ID 不同请求返回 `ERR_EVENT_CONFLICT` (409)。
MySQL 需要 Phorge migration 创建 `worker_gorgeinbox`；Redis 回执存于队列
前缀下的 inbox hash。回执不自动过期，以免历史事件重放产生重复任务；清理
必须先确认 outbox 与备份中的事件均不再重放。Redis Lua 使用动态键，沿用
单实例 Redis 部署约束，不声明 Redis Cluster 支持。

Feed 的业务记录与 `feed_gorgeoutbox` 写入同一个 feed 数据库事务。
`GORGE_WORKER_OUTBOX_DSN` 指向该 feed 库；relay 通过队列 inbox 提交事件，
成功后标记 deliveredEpoch。HTTP 响应或源库确认丢失可安全重放；失败事件
保留 attempts/lastError 并按上限一小时退避。此阶段只覆盖 Feed 发布事件，
不是任意 PHP 业务事务的通用 outbox。不要在事件仍可能重放时清理 inbox。

## 持久触发器调度（可选，MySQL）

`GORGE_TASKQUEUE_SCHEDULER_ENABLED` 默认 `false`。
`GORGE_TASKQUEUE_SCHEDULER_CONDUIT_URI` / `GORGE_TASKQUEUE_SCHEDULER_CONDUIT_TOKEN`
指定认证的 PHP 时钟计算入口；启用时二者必须非空。Go 在同库事务中推进事件并入队，
数据库 `php / paused / gorge` 执行权与服务开关分开管理。
认证 `/api/queue/meta` 增加 `schedulerProtocol` / `schedulerAtomicEnqueue` 能力字段。
详细启用、支持范围和回切见 [持久触发器调度](scheduler.md)。
