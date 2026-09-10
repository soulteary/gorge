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

**这是仓库里第一对拆成两个二进制的域，而「为什么是两个」正是本域最该先讲清楚的一件事**——它和 render+diff 合并那段恰好相反。render 与 diff 合并成一个二进制，是因为两者都是无外部依赖的纯计算、拆进程换不来隔离收益。taskqueue 与 worker 不合并，是因为 **worker 是 taskqueue 的 HTTP 客户端，不是它的同进程协程**：worker 通过 `TASK_QUEUE_URL` 拨 taskqueue 的 `/api/queue/**` 租约，两者可以各自独立伸缩（一个 taskqueue 前面挂若干 worker，或给某个重类开一个专用 worker）。把它们塞进一个进程，要么把这层 HTTP 契约降级成进程内调用、要么逼 worker 直接调 `Store` 而绕过契约——两条路都把「可独立部署」这个既有事实弄没了。所以 `cmd/` 下是两个入口、compose 里是两个 service，本文档同时覆盖两个域。

taskqueue 本身几乎是 webhook 的翻版（同为「有外部依赖 + 后台性质」），读它之前值得知道的三件事和 webhook 同源：契约固件只覆盖那几个只读/幂等端点、e2e 脚本能跑通完整的 enqueue→lease→complete 但碰不到 worker 侧的真实任务执行、字段名是硬约束。worker 则在一个维度上是本类第一个：**它没有 `db.go`**，不碰任何数据库，唯一的外部依赖是 taskqueue 服务本身，所以它的 `/readyz` 退化为 `/healthz`。

## 1. 职责边界

### taskqueue

**负责**：把任务入队进 `worker_activetask`（带 `worker_taskdata` 里的负载）、按「优先级升序、id 升序」把行租给 worker、并在任务完成/永久失败/取消时把行搬进 `worker_archivetask`。租约到期、临时失败的重试退避、yield 窗口三件事由它管，见第 3 节。它有两个后端——MySQL（默认，读写 Phorge 自己的库）与 Redis（把队列挪出主库）——两者满足同一个 `Store` 接口。

**不负责**：**定义任务**。哪个事务该排哪个 worker、任务负载长什么样、优先级取哪个带，全部由 Phorge 侧的 `PhabricatorWorker::scheduleTask` 决定并写好；本服务只入队、租出、归档。它也不建表、不改表结构、不需要 DDL 权限——三张 worker 表都是 Phorge 的 `bin/storage upgrade` 的产出。

**它必须替换 Phorge 自己的 taskmaster 守护进程，而不是与之并存。** 队列在库里，`phd` 与 gorge-worker 谁都能取，所以「多一个消费者」在这个域里不是扩容而是**把每个任务跑两遍**。让路的方式是停掉 PHP 侧的 `phd`；与 webhook 第 38 条同源。

**有外部依赖，而且和 webhook 一样是这一类里最硬的那种。** 数据库不是它写穿的一个后端，而是它的工作本身——三张表就是队列。所以「一个后端都没配」这个状态在这里不存在，`/readyz` 只有一条判据（后端答话），见 3.5。选 Redis 后端时判据换成 Redis 答话，形状不变。

### worker

**负责**：一个租约循环——从 taskqueue 租任务、按 task class 找到 handler 跑它、把结果（完成/永久失败/临时失败/yield）回报给 taskqueue。它是 `PhabricatorTaskmasterDaemon` 的等价物。

**不负责**：**存储**。它不碰数据库，不知道队列在 MySQL 还是 Redis——它只认 taskqueue 的 HTTP 契约。它也不定义任务如何被执行的「真理」：本地实现的 handler 只覆盖几个类，其余全靠 Conduit 委派回 PHP（`worker.execute`），见 3.6。

**它的 `/api/worker/stats` 是只读的，而且是进程内计数**，不是查库——所以它描述的是「这个 worker 这辈子」的数字（重启即清零），与 taskqueue 的 `/api/queue/stats`（每次真查库）性质相反。

## 2. 路由与依赖

### taskqueue：`/api/queue`

```go
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
| GET | `/api/queue/stats` | 需要 | 信封，`{activeCount, leasedCount, archivedCount, failedCount}` |
| GET | `/api/queue/tasks` | 需要 | 信封，活跃任务 `Task[]`（不含 `data`） |
| GET | `/api/queue/tasks/:id` | 需要 | 信封，单个 `Task`（含 `data`）；不存在为 404、非数字 id 为 400 |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） | 裸 `{"status":"ok"}` |

**lease owner 走 `X-Lease-Owner` 头而不是 body**：它标识调用方（哪个 worker），不是这次请求。缺头时回落到 `IP:gorge-taskqueue`，够区分不同 worker 的租约。这个值直接写进 `worker_activetask.leaseOwner`，是 Phorge 的列、会被它的守护进程控制台读回，所以是契约的一部分。body 还可带 `taskClasses` 白名单；筛选发生在取得租约之前，避免专用 worker 先占用、再失败并延迟另一个池的任务。

`Deps` 只有 `Store` 与 `Token`。**`Store` 是 interface，这是契约固件能存在的前提**：两个后端（MySQL/Redis）之外，固件与 handler 单测注入的是第三份手写内存实现（`memstore_test.go`），照 webhook 的做法。

### worker：`/api/worker`

```go
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/worker")
	g.Use(auth.Token(deps.Token))

	g.Get("/stats", stats(deps))
}
```

| 方法 | 路径 | 鉴权 | 成功响应 |
|---|---|---|---|
| GET | `/api/worker/stats` | 需要 | 信封，`{processed, failed, active, supported}` |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） | 裸 `{"status":"ok"}` |

`Deps` 是 `Consumer` 与 `Token`。stats 读的是 `Consumer` 里的原子计数器，永不失败，也就没有错误路径。`supported` 在配了 Conduit fallback 时含 `*`，让 setup check 能区分「把一切委派给 PHP」与「只handle固定几类」。

**两个 `main.go` 各多一个 goroutine，形状和 webhook 一样：** taskqueue 的 `main.go` 没有后台循环（它是被动服务），但 worker 的有——租约循环整个住在 `Consumer.Run` 里，`main.go` 起一个 goroutine 跑它，一个信号同时停两半（`srv.Run` 在 SIGINT/SIGTERM 返回、取消 context 让循环收尾），**先排空循环再退出**，否则在途任务的结果回报会被切断。taskqueue 的 `main.go` 则是显式 `store.Close()`（`os.Exit` 会跳过 `defer`），与 webhook / file-storage 同理。

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

### 3.5 `/readyz` 只 ping，不查表

判据只有一条——后端答话——因为这个服务在数据库之外无事可做（MySQL 后端 `PingContext`，Redis 后端 `Ping`）。**它不检查 worker 表存不存在**，理由与 webhook / file-storage 同源：三张表由 Phorge 的 `bin/storage upgrade` 建，跑那条命令的容器可能后启动，所以要求表存在等于把一个健康部署在它首次迁移期间报成坏的。`OpenDB` 同样**不 ping**、`NewMySQLStore` 也不在打不开时 `os.Exit`——那让排在数据库之前的容器进重启循环。

和 webhook 一样，**「只 ping」没有把探针从首启闭环里摘出来**：`WorkerDSN()` 带的是库名 `{namespace}_worker`，库不存在时 ping 在连接阶段就失败（`Error 1049 Unknown database`），而那个库也是 `bin/storage upgrade` 建的。所以首次启动必有一段 `/readyz` 不通的窗口，由 Phorge 自己结束；编排侧对 taskqueue 用 `service_started` 而非 `service_healthy` 来躲开死锁，和 webhook 那条依赖同形。

**这个缺口是 `/readyz` 必须存在的全部理由**：一个连不上后端的 taskqueue 在监听、`/healthz` 答 200、却租不出一个任务，和「慢了一拍」长得一模一样。容器 healthcheck 因此打 `/readyz`。

### 3.6 worker：租约循环与 Conduit 委派

`Consumer.Run` 是 worker 的全部工作：按 `PollIntervalMs` 轮询 `lease`（一次要 `LeaseLimit` 个），对每个任务按 `taskClass` 从 `Registry` 找 handler 跑，然后回报——成功 `complete`、`PermanentError` 走 `fail(permanent=true)`、`YieldError` 走 `yield`、其余错误走 `fail(permanent=false)`。结果回写失败会在任务租约上下文内重试，只有 taskqueue 确认写入后才更新进程计数，避免一次短暂队列故障被误记成已完成。队列空了就进入 `IdleTimeoutSec` 之后的退避。`TaskClassFilter` 非空时通过 lease 请求的 `taskClasses` 在取得租约前筛选；没有 Conduit fallback 时，即使未显式配置过滤器，也只请求 Registry 真正支持的类。

**handler 的注册在 `handlers.RegisterAll`**：本地实现目前覆盖 `FeedPublisherHTTPWorker`，并且无论是否配置 Conduit 都优先使用原生 handler；配了 `GORGE_WORKER_CONDUIT_URL` 才装一个兜底 handler，把任何未本地实现的 task class 通过 Conduit 的 `worker.execute` 委派回 Phorge 的 PHP。这让 worker 不必重写 Phorge 的每一个 worker 就能跑一个装置的全部任务类。没配 Conduit 时，未实现的类不会被这个 worker 租走。`supported` 里的 `*` 就是「配了 fallback」的信号。

**Conduit 委派必须用表单编码，不能发 JSON。**Phorge 的 `PhabricatorConduitAPIController` 明确拒绝 `Content-Type: application/json`（"Use form-encoded data to submit parameters to Conduit endpoints"），一个它无法当作 Conduit 请求解析的 body 会被更外层的 HTTP 栈用一张 HTML 页面回应——这正是委派环节 `invalid character '<'` 的来源。所以 `handlers/conduit.go` 的 `ConduitClient.Call` 走 Phorge 自家客户端（arcanist 的 `ConduitClient`、旧 `PhabricatorGoConduitGatewayClient`）的线格式：`POST /api/<method>`，`Content-Type: application/x-www-form-urlencoded`，body 里带一个 `params` 字段（值是参数 map 的 JSON），API token 塞在 `__conduit__.token` 里，再加 `output=json` 强制 JSON 信封；网关另外用 `X-Service-Token` 头认证。收到非 JSON body 时不再抛裸的解码错误，而是截一段可诊断的片段。Conduit 客户端本身不设与任务无关的固定 30 秒上限；Consumer 以 taskqueue 返回的 `leaseExpires` 作为执行上下文截止时间，允许合法的长任务使用完整租约窗口。

**`worker.execute` 是 phorge-fork 侧新增的 Conduit method**（`PhabricatorWorkerExecuteConduitAPIMethod`，`shouldRequireAuthentication()=false` 且 `shouldAllowUnguardedWrites()=true`，因为它是经网关认证的机器间内部调用、无用户会话、无 CSRF 面）。它按 `taskClass`+`data` 用 `newv()` 造出真正的 `PhabricatorWorker` 并跑 `executeTask()`，把 `PhabricatorWorkerActiveTask::executeTask` 的分类**回报**而非自己驱动队列（队列归 gorge-worker 管）：正常返回 `success`，`PhabricatorWorkerYieldException`→`yield`（带 `retry`），`PhabricatorWorkerPermanentFailureException`→`permanent-failure`，其余异常→`failure`（临时、可重试）。委派 handler 据此把结果翻译回 worker 的错误词汇（`nil`/`YieldError`/`PermanentError`/普通 error），从而正确 `complete`/`yield`/`fail`。

worker 的 `/readyz` 是 `nil`（`httpx.Config{Ready: nil}`）：它没有自持的外部存储要拨测，连不上 taskqueue 只是租不到、会一直重试，那是 taskqueue 的就绪问题，不该让编排重启 worker。所以它的 healthcheck 打 `/healthz`。

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

**`MYSQL_HOST` 不是开关**，与 webhook 同、与 file-storage 的同名变量相反：本域没东西可关，两个后端都要读库，「没配」与「连不上」不是两个值得区分的状态——留空落到 `127.0.0.1`，服务起来、不就绪。

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
| `GORGE_WORKER_IDLE_TIMEOUT_SEC` | `180` | 队列空后按此频率再等多久才退避 |
| `GORGE_WORKER_CONDUIT_URL` | 空 | 见 3.6，空 = 只租并运行本地实现的类 |
| `GORGE_WORKER_CONDUIT_TOKEN` | 空 | Conduit token |
| `GORGE_WORKER_TASK_CLASS_FILTER` | 空 | 逗号分隔的类白名单，空 = 全部支持的类 |

规范变量命名规则见 [`../platform.md`](../platform.md) 第 4 节。taskqueue 的连接池（`maxOpenConns=25` / `maxIdleConns=5` / `connMaxLifetime=5m`）定在 `db.go`，按「池与 Phorge 共用一台库、要留余量」定；**`platform/` 仍然没有数据库设施——三个域（file-storage / webhook / taskqueue）现在共用一个驱动，仍不构成共享关切**，判断同 [`../findings.md`](../findings.md) 第 39 条。

## 5. 兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第十节，这里是概述。本域的约束整节都是**静默型**（破坏后不报错、只错到没人发现），分两组：

**一组是任务字段名**：`taskClass` / `dataID` / `leaseOwner` / `leaseExpires` / `failureCount` / `failureTime` / `objectPHID` / `containerPHID` / `priority` / `dateCreated` / `dateModified`，以及归档表多出的 `result`（整数 0/1/2）/ `duration` / `archivedEpoch`。它们直接映射 Phorge 的 `worker_activetask` / `worker_archivetask` 列名，改错一个不会报错，只会让 PHP 侧读到空值、或让 `bin/worker` 的界面把任务显示成另一个样子。

**另一组是抢占语义的地基，和 webhook 的 `status` 同源**：这里没有 `status` 列，但 `leaseOwner` + `leaseExpires` 的组合承担了同样的角色——「谁持有这行、持到几时」。`leaseOwner = (yield)`（`YieldOwner` 哨兵）是 yield 任务的唯一标识，`leaseExpires` 是租约/退避/yield 三个窗口共用的时间列。多一个值、错一个语义，会让任务被两个 worker 同时取走、或让 `awaken` 认不出 yield 的任务——任何一层都不报错。

两个 result 值域细节容易踩：

- **归档的 `result` 是整数不是字符串**（0=success / 1=failure / 2=cancelled），对齐 `PhabricatorWorkerArchiveTask::RESULT_*`。写成字符串会让 Phorge 的控制台读不出结果。
- **优先级带的数值是契约**（`PriorityAlerts=1000` … `PriorityImport=4000`），因为它们决定 `ORDER BY priority ASC` 的排队顺序，而 Phorge 侧按同一套数值入队。

**Redis 后端与 MySQL 后端语义必须一致，但可见性不同。** 选 Redis 时队列不在 Phorge 的库里，所以 Phorge 的 `bin/worker` 与 Web UI 的任务视图会读到一个空队列——这不是 bug，是把队列挪出主库的代价，部署前要知道。两个后端满足同一个 `Store` 接口、共享 `memstore_test.go` 之外的同一批 store 单测语义。

## 6. 域级错误码

**taskqueue 与 worker 都没有域级错误码，这是决定不是遗漏，理由同 webhook。**

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
