# db-api 模块

把 Phorge 对自己 MySQL 集群的自省搬到 HTTP 后面：`gorge-db-api` 回答「配了哪些库服务器、每台多健康、每台的 schema 与运行环境是否符合 Phorge 的预期、`bin/storage upgrade` 跑到哪了」。它不持有任何自己的状态——每个答案都是对着它所指向的集群现查一次 `SHOW` / `SELECT` / `INFORMATION_SCHEMA` 得来的。独占 `gorge-db-api` 这个二进制与 `:8080` 这个端口。

| | |
|---|---|
| 二进制 | `gorge-db-api` |
| 端口 | `:8080` |
| 包 | `go/internal/dbapi/` |
| 契约 | [`api/openapi/dbapi.yaml`](../../api/openapi/dbapi.yaml) |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) 第十一节 ← **改动前必读** |

独立服务迁入单仓库时，两处与既有域的根本不一致被消除了，而这正是本域最该先讲清楚的一件事。原独立服务用 `labstack/echo/v4` 加自定义的 `APIResponse{data,error,cursor}` 信封、**snake_case** 字段（`ref_key`/`is_fatal`）与自己那套 HTTP 状态码映射；迁入后 HTTP 层重写成 Fiber v3 + `internal/platform/httpx`，字段统一 **camelCase** 并沉淀进 `internal/contracts`，错误码收敛到平台六码加三个域级码。域逻辑本身——应用分区路由、只读降级、savepoint 命名、MySQL 错误码映射、版本比较、三级 `INFORMATION_SCHEMA` 查询——原样保留。

它在「有外部依赖」这一类里与 webhook / taskqueue 同源：数据库不是它写穿的一个后端，而是它的工作本身——所有七条路由都是对着 MySQL 现查。但它也有两处与那两个后台域不同：**它没有后台循环**（七条路由全部由入站请求驱动，像 file-storage 那样「有人来问、答一句」），而且**它只读**——它从不改 Phorge 的库、不建表、不需要 DDL 权限。

## 1. 职责边界

**负责**：如实报告集群的当前状态。四件事，对应四个内部 service：

- **健康**（`HealthService`）：逐台探测连接与复制状态，镜像 Phorge 的 `PhabricatorDatabaseRef::queryAll`——一次短连接、一次 ping、对 MySQL 再跑一次 `SHOW REPLICA STATUS`。一台连不上的节点照样出现在结果里，`connectionStatus` 为 `fail`、原因在 `connectionMessage` 里，因为「这台服务器挂了」正是健康报告存在的理由。
- **schema 诊断**（`DiffService`）：三级 `INFORMATION_SCHEMA` 遍历（Server → Database → Table → Column）产出每台服务器的实际 `SchemaNode` 树（`/schema-diff`，交给 Phorge 与应用 SchemaSpec 比较），并把 replica 相对其分区 master 的属性/缺失/多余差异拍平成 `SchemaIssue` 列表（`/schema-issues`），外加 `/charset-info` 报告每台能不能用 utf8mb4。
- **环境检查**（`SetupService`）：跑 Phorge 的 `PhabricatorDatabaseSetupCheck` 与 `PhabricatorMySQLSetupCheck` 检的那些项——每台节点的版本、InnoDB 和服务器变量，以及真正承载 `meta_data` 的分区中 `{namespace}_meta_data` 库在不在——每一条是一个 `SetupIssue`，`isFatal` 与 Phorge 的判定对齐。
- **迁移状态**（`MigrationService`）：读承载 `meta_data` 的 master 上 `{namespace}_meta_data.patch_status`，报告 `bin/storage upgrade` 跑到哪了（`/migrations/status`）。replica 的 patch_status 通过复制到达，不是它自己迁出来的。

**不负责**：**改动集群**。它不建库、不建表、不改 schema、不跑迁移——所有这些都是 Phorge 的 `bin/storage upgrade` 的事，本服务只观察。它也不做连接池以外的**缓存**：每次请求都现查，因为一份「五分钟前的健康报告」在这个域里几乎没有价值。它更不是 Phorge 数据的读写代理——它不碰业务表，只碰 `INFORMATION_SCHEMA`、`SHOW *`、`patch_status` 与 `hoststate` 这类元信息。

**写入路径抽象已迁出到 `internal/dbproxy`，七条路由本就不用它。** `Router.GetWriter` / `GetReader` 与只读降级、`TxManager` 的 savepoint 嵌套事务、写/连接重试，是从独立服务原样搬来的域逻辑（Phorge 的 `PhabricatorLiskDAO` 集群连接选择、`AphrontDatabaseConnection` 的 savepoint），但当前七个 handler 全是只读探测，走各 service 自己开的短连接，不经过它们。审计确认这些符号无任何 handler / 后台任务调用，故迁到独立内部包 `go/internal/dbproxy/`（迁移而非删除，保留为未来写端点的地基），owner 与未来入口见 [`../adr/0001-isolate-db-proxy.md`](../adr/0001-isolate-db-proxy.md)。**留在 `dbapi` 的是仍在只读路径上的部分**：共享 `Conn` 的只读双层拦截（`QueryContext`/`ExecContext` 守卫）、`isReadQuery`、`classifyMySQLError` 错误分类，以及 `ERR_READONLY`（见第 6 节）——后者绑定的是共享只读连接的写拒绝语义，是「定义了但七条路由到不了」的码，随只读守卫留在本包。

## 2. 路由与依赖

```go
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/db")
	g.Use(auth.Token(deps.Token, auth.WithQueryToken(false)))

	g.Get("/servers", listServers(deps))
	g.Get("/servers/:ref/health", serverHealth(deps))
	g.Get("/schema-diff", schemaDiff(deps))
	g.Get("/schema-issues", schemaIssues(deps))
	g.Get("/setup-issues", setupIssues(deps))
	g.Get("/charset-info", charsetInfo(deps))
	g.Get("/migrations/status", migrationStatus(deps))
	g.Get("/meta", meta(deps))
}
```

| 方法 | 路径 | 鉴权 | 成功响应 |
|---|---|---|---|
| GET | `/api/db/servers` | 需要 | 信封，`ServerRef[]`（每台一条） |
| GET | `/api/db/servers/:ref/health` | 需要 | 信封，单个 `ServerRef`；`:ref` 不匹配任何配置节点为 404 |
| GET | `/api/db/schema-diff` | 需要 | 信封，`SchemaNode[]`（每台一棵树） |
| GET | `/api/db/schema-issues` | 需要 | 信封，`SchemaIssue[]`（拍平的问题） |
| GET | `/api/db/setup-issues` | 需要 | 信封，`SetupIssue[]` |
| GET | `/api/db/charset-info` | 需要 | 信封，`CharsetInfo[]`（每台一条） |
| GET | `/api/db/migrations/status` | 需要 | 信封，`MigrationStatus[]`（`meta_data` master 一条） |
| GET | `/api/db/meta` | 需要 | 信封，`Capabilities`（`contractVersion`/`namespace`/`topologySource`/能力列表；不查库、不会因集群失败） |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） | 裸 `{"status":"ok"}` |

**路径按域命名而非按二进制命名**（理由同 render / file-storage）：`PhabricatorGorgeDBClient` 按字面调这七条，改路径要同步改 PHP。`:ref` 是 `host:port` 形式的 refKey，与 Phorge 自己的 `PhabricatorDatabaseRef` key 相同，所以两侧指的是同一台服务器。

`Deps` 里是四个 service（`Health` / `Schema` / `Setup` / `Migration`）、集群配置 `Cluster`、探测用的 `Password`、鉴权 `Token` 与 `TopologySource`。**不再持有 `Router`**：写入路径抽象已迁到 `internal/dbproxy`（见第 1 节与 [ADR 0001](../adr/0001-isolate-db-proxy.md)），而每条答复都是请求作用域内开合的短连接，没有长期连接池。**`NewDeps` 不开任何连接**：和 file-storage / taskqueue 一样，连接池是惰性的，所以进程能在 MySQL 起来之前就启动。`TopologySource` 是 `file` 或 `single-node`，由 `main.go` 按是否设置 `GORGE_DB_CONFIG_FILE` 判定，供 `/api/db/meta` 上报。

**本域的鉴权比其他域更紧：token 只认 `X-Service-Token` header，不认 `?token=` query。** 共享中间件 `auth.Token` 默认仍接受 query fallback（其他域的 runbook curl 依赖它），本域显式传 `auth.WithQueryToken(false)` 关掉它——URL 里的 token 会进访问日志、浏览器历史与 Referer，而 `/api/db/**` 不会被浏览器链接到，query fallback 在这里是纯风险。带对 token 但只放在 query 上的请求会和「没带 token」一样被 401 拒绝（contract fixture `token-via-query-param.json` 锁这条）。

**`/api/db/meta` 是切流前的握手。** PHP 消费端在把数据库控制台切给本服务之前先读它：`contractVersion`（`major.minor`，消费端只在自己看得懂的 major 上切流）、`namespace`（必须等于 Phorge 的 `storage.default-namespace`）、`topologySource`、以及本 build 提供的能力列表。契约版本或 namespace 不兼容时，PHP 显式报 setup issue 并继续走原生 SQL，而不是去读可能已经改名/改义的字段。这个端点不查库、不带主机/凭据信息，是唯一不会因集群故障而失败的 `/api/db` 路由。`ContractVersion` 常量与 `Capabilities` 结构在 [`../../go/internal/contracts/dbapi.go`](../../go/internal/contracts/dbapi.go)。

`main.go` 与 file-storage 同形：`httpx.New(httpx.Config{Ready: deps.Ready})`，退出时**显式** `deps.Close()` 而不用 `defer`——失败分支要 `os.Exit`，那会跳过 `defer`。`Close()` 当前是有文档的 no-op（返回 nil）：没有长期池要释放，保留它是为了让关停契约稳定、将来有真正的池时有显而易见的释放点。

## 3. 核心实现

### 3.1 单节点与集群两种配置，每次启动只用一种

有两种描述集群的方式，每次启动恰好用一种：`GORGE_DB_MYSQL_*` 那组标量描述单个节点，而 `GORGE_DB_CONFIG_FILE` 指向的 Phorge 风格 local.json 通过它的 `cluster.databases` 描述完整拓扑。**文件存在时文件赢**——有 local.json 的部署跑的是真集群，标量只够描述其中一个节点。节点角色和分区以 Phorge 当前的 `role` 字符串与 `partition` 字符串列表为标准；为兼容旧版或分支配置，解析器也接受 `roles` 布尔对象和标量 `partition`，进入拓扑前统一归一化。每个节点自己的 `pass` 优先于全局 `mysql.pass`，没有节点密码时才回落到全局值；所有探针和 Router 都遵守同一优先级。

**两条路径的失败方式刻意不同。** 标量单节点路径是默认部署，永远成功：一个主机名就够，主机连不上是「就绪」问题而非「启动」问题。但当 `GORGE_DB_CONFIG_FILE` 显式指向一个文件时，操作者是在描述一套真集群，于是这份描述的每一种失败——文件缺失、JSON 非法、字段类型错、能解析但没有任何可用 master——都是操作者**必须看见**的配置错误，而不是可以悄悄兜底的东西。此时把它降级成用标量兜底拼出来的单节点，会把所有应用路由到一台主机、在每份报告里藏掉 replica，还一路显示健康。所以文件路径**fail closed**：`BuildCluster` 返回配置错误，`main.go` 停止启动（错误消息只带操作者提供的文件路径，不含主机、库名或凭据）。标量单节点兜底只在该变量**未设置**时才可达。

### 3.2 应用分区路由与只读降级，已迁出到 `internal/dbproxy`

`dbproxy.Router` 镜像 Phorge 的 `PhabricatorLiskDAO` 集群连接选择：按 Phorge 应用（`meta_data` / `worker` / …）选 master 或 replica，优先选显式绑定到该应用的节点、否则回落到默认分区的节点，并按 `(节点, 应用, 只读)` 三元组缓存连接。`dbproxy.TxManager` 实现 Phorge `AphrontDatabaseConnection` 的 savepoint 嵌套事务（命名 `Aphront_Savepoint_%d` 逐字一致）。

**只读降级是 Phorge 自己就有的行为**：`GetReader` 连不上 master 时把 router 翻成只读、改从 replica 读，于是后续的写会被 `GetWriter` 拒成 `ERR_READONLY`，而不是被发去一个可能陈旧的 replica。这段逻辑连同 `TxManager`、写/连接重试一起迁到了 `internal/dbproxy`——当前七条只读路由都不驱动它，各 service 走自己的短连接（见第 1 节与 [ADR 0001](../adr/0001-isolate-db-proxy.md)）。共享 `Conn` 的只读守卫与 `ERR_READONLY` 码本身留在 `dbapi`。

schema 的两条路由分工不同：`/schema-diff` 返回完整实际属性——库字符集/排序规则、表引擎/排序规则、列字符集/排序规则/完整类型/nullability/auto_increment，以及每张表的索引（`keys`：名称、按 `SEQ_IN_INDEX` 排序并保留 `Sub_part` 前缀的列名、唯一性、索引类型）——Phorge PHP 侧用自己的 `PhabricatorConfigSchemaSpec`（应用代码的唯一规范来源）构造预期 schema 并逐字段比较。每库的 `COLUMNS` 与 `STATISTICS` 各只查一次再在内存里按表分组，避免每表一查的 N+1。`/schema-diff` 还接受可选的 `databases` 查询参数（逗号分隔、限量限长的期望库名）：存在但当前用户无权查看的库会回一个 `accessDenied: true` 的库节点，因为仅凭可见集无法把「受限」与「缺失」区分开——缺失留给 Phorge 自己的期望-实际比较。`/schema-issues` 另外做集群内一致性检查，把每个应用分区的 replica 与该分区路由到的 master 比较（字符集/排序规则/引擎/列类型/nullability/auto_increment 差异，及缺失或多余的表列），都会生成带 `issueKey`、`expected`、`actual` 的记录。这样 Go 不复制一份会随 Phorge 应用代码漂移的 SchemaSpec，同时 replica 的 schema 漂移也不会被空列表掩盖。

### 3.3 健康探测：连接一半 + 复制一半，且区分「没权限」与「坏了」

`probeRef` 用一个 2 秒超时、**不重试**的短连接探一台节点——健康检查要报「此刻」的状态，而不是等一个重试循环跑完。连接或 ping 失败即 `connectionStatus = fail`、原因进 `connectionMessage`。连得上就优先跑 `SHOW REPLICA STATUS` 探复制；MySQL 8.0.0–8.0.21 对这个新拼法返回 1064 时，回落到兼容的 `SHOW SLAVE STATUS`。

这里有一处判据值得单记，它也是 `connectionStatus` 有 `replication-client` 这个取值的理由：**「探测用户没有权限跑 `SHOW REPLICA STATUS`」不是一次失败，是一个独立状态**。节点答了话、只是这个用户看不到复制信息——这是一个去授权（GRANT）能解决的问题，不是一台要修的服务器。所以它被分类成 `replication-client` 而不是 `fail`。同理 `1045` 一族被分成 `auth`。复制延迟从结果里**按列名**取：MySQL 8.0.26+ 的 `Seconds_Behind_Source` 或旧版的 `Seconds_Behind_Master`（结果列会随版本变，按位置取会错位），`>30` 秒标为 `replica-slow`；同时检查新版 `Replica_IO_Running` / `Replica_SQL_Running` 与旧版 `Slave_IO_Running` / `Slave_SQL_Running`，任一线程停止都标为 `not-replicating`，即使 lag 恰好还是 0；结果集在 `Columns` / `Next` / `Scan` 阶段中断则整次探测标为 `fail`，不会把不完整结果写成 `okay`。

### 3.4 迁移状态读 `patch_status`，并用 `hoststate` 的摘要暴露集群状态

`MigrationService.Status` 遍历所有承载 `meta_data` 分区的 enabled master（而非只取路由会选中的第一个），对每个 `checkRef` 连接它的 `{namespace}_meta_data` 库读 `SELECT patch FROM patch_status`；其它应用的专属 master 不承载这个库，不会被列入也不会被误报成未初始化。报告全部 master 让 Phorge 能比较它们各自提交的集群状态，抓出某个以过期配置启动的 master。**建连或 Ping 失败时 `initialized` 留 `false`，这是如实报告而不是错误**：`{namespace}_meta_data` 库还不存在，正是 `bin/storage upgrade` 跑之前的状态，调用方读到「未初始化」就对了。但 Ping 已成功后，读取 `patch_status` 失败会按域错误显式返回（权限不足为 403、连接中断为 503），不能伪装成 `initialized:true` 且 patch 列表为空。服务只上报观察到的 `appliedPatches`，不持有期望 patch 列表——期望列表（`PhabricatorSQLPatchList`）由 Phorge 持有并在 PHP 侧求差集，所以不返回 `totalExpected`/`missingPatches`。

它还额外跑一次 `SELECT stateValue FROM hoststate WHERE stateKey = 'cluster.databases'`，并用**两个**字段如实表达这次读取：`clusterStatePresent`（该 `cluster.databases` 行是否存在，与它的值无关）与 `clusterStateDigest`（对读到的**原始串**算 SHA-256）。三种情形分别是：行存在 → `present=true`、`digest` 为原始字节的 SHA-256（`stateValue` 为 SQL NULL 时视为「存在但状态无效」，对空字节算摘要，绝不当成一致）；行确实缺失（`sql.ErrNoRows`）→ `present=false`、`digest` 省略（空），这是「该 master 没有已提交集群状态」的如实观察，成功返回；查询失败（权限不足 / 连接中断 / 其它）→ 按域错误显式返回，**绝不伪装成 `present=false`**。`hoststate` 是 Phorge 在多 master 之间提交 `cluster.databases` 状态用的表，原始值含主机名故绝不外泄；只回摘要 + presence 既能让 Phorge 用本地 `getPartitionStateForCommit()` 的同字节 SHA-256 比对、发现两个 master 对已提交拓扑不一致（`db.state.desync`，含某个 master 根本没有已提交状态这一情形），又不让本服务持有拓扑。两张表名（`patch_status`、`hoststate`）与库名约定（`{namespace}_meta_data`）都是兼容契约，见第 5 节。

### 3.5 `/readyz` 只 ping，且 DSN 不带库名——这是刻意躲开首启死锁

`/readyz` 的判据只有一条：至少一台配置的 master 能被 ping 通（`anyReachable`）。所有 enabled master 在同一个 readiness deadline 内并发探测，前面的黑洞节点不会耗尽期限、阻止后面的健康节点得到机会。**它只 ping、不查表，而且探测用的 DSN 不指定任何库名。** 两件事都是刻意的，理由与 file-storage 第 3.5 节、webhook 完全同源：

`{namespace}_meta_data` 这个库由 Phorge 的 `bin/storage upgrade` 建，而那条命令跑在**排在本服务之后启动**的 Phorge 容器里。如果 readiness 去查这个库或这张表，就构成一个闭环——本服务等一个只有 Phorge 能建的库、Phorge 等本服务健康，两个容器一起停在启动阶段。而且，如 compat 第 8.7 节实测所示，**光「不查表」还不够**：go-sql-driver 在握手阶段就把 DSN 里的库名发过去，库不存在时 ping 会失败在连接上。所以本服务的探测 DSN **根本不带库名**，一个 ping 就能打通一台 Phorge 库还没建出来的服务器。

对应的编排结果：本服务在 compose 里的 healthcheck 打 `/healthz` 而非 `/readyz`，且 phorge-fork 侧对它的依赖必须是 `service_started` 而非 `service_healthy`（与 file-storage / webhook 一致）。**别把 healthcheck 改成 `/readyz`，也别把依赖改成 `service_healthy`**——两者都会让首启死锁。见 [`../../deploy/compose/docker-compose.yml`](../../deploy/compose/docker-compose.yml) 里 `gorge-db-api` 那段注释与 [`../../compat/phorge/README.md`](../../compat/phorge/README.md) 第 8.7 节。

## 4. 配置

命名规则与「新名优先、旧名兜底」的查找机制见 [`../platform.md`](../platform.md) 第 4 节。

| 变量 | 兜底旧名 | 默认值 | 说明 |
|---|---|---|---|
| `GORGE_LISTEN_ADDR` | `LISTEN_ADDR` | `:8080` | 监听地址，沿用迁入前的端口 |
| `GORGE_SERVICE_TOKEN` | `SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权 |
| `GORGE_DB_MYSQL_HOST` | `MYSQL_HOST` | `127.0.0.1` | 单节点模式下的库主机。不是开关（与 webhook / taskqueue 同、与 file-storage 相反）：本域没东西可关，留空落到 `127.0.0.1`、服务起来、不就绪 |
| `GORGE_DB_MYSQL_PORT` | `MYSQL_PORT` | `3306` | |
| `GORGE_DB_MYSQL_USER` | `MYSQL_USER` | `root` | |
| `GORGE_DB_MYSQL_PASS` | `MYSQL_PASS` | 空 | |
| `GORGE_DB_NAMESPACE` | `STORAGE_NAMESPACE` | `phorge` | 库名由它拼成 `{namespace}_meta_data` 等，必须与该装置的 `storage.default-namespace` 一致 |
| `GORGE_DB_CONFIG_FILE` | `PHORGE_CONFIG` | 空 | Phorge local.json 路径。非空则读它的 `cluster.databases` 拓扑，标量退化为每节点兜底；空 = 用标量描述的单节点。**非空时 fail closed**：文件缺失/JSON 非法/类型错/无可用 master 会让启动失败，而不是降级成单节点 |

## 5. 兼容契约

权威描述在 [`../../compat/phorge/README.md`](../../compat/phorge/README.md) 第十一节，这里是概述。本域的约束整节都是**静默型**——破坏后不报错，只让 PHP 侧读到空值或错值——分三组：

- **字段名一律 camelCase，改一个就是一次兼容性变更。** 它们声明在 [`../../go/internal/contracts/dbapi.go`](../../go/internal/contracts/dbapi.go)，是 PHP 侧 `PhabricatorDatabaseRef` / `DatabaseSetupCheck` / `MySQLSetupCheck` / `PhabricatorConfigSchemaQuery` 直接按键读的线上契约本身。迁入时从旧的 snake_case 改过来：`ref_key`→`refKey`、`is_fatal`→`isFatal`、`connection_status`→`connectionStatus`、`replication_status`→`replicationStatus`、`seconds_behind_master`→`secondsBehindMaster` 等。Schema 位置字段是 `databaseName` / `tableName` / `columnName`，问题标识是 `issueKey`。**`isFatal` 尤其载重**——它镜像 Phorge 的 `PhabricatorSetupIssue::isFatal`，Phorge 据它决定阻断启动还是仅告警，改错名字会让每个 setup issue 都退化成告警。
- **错误码语义**：三个域级码（`ERR_DB_UNREACHABLE` 503 / `ERR_READONLY` 409 / `ERR_DB_ACCESS_DENIED` 403）区分的是「调用方需要区别对待」的三类失败，见第 6 节。
- **库名与表名约定**：库名按 Phorge 的方式拼成 `{namespace}_meta_data`（以及其它 `{namespace}_<app>`）；迁移状态读 `patch_status` 表，多 master 同步状态读 `hoststate` 表——三个名字都是 Phorge 的，不能「顺手现代化」。

## 6. 域级错误码

三个，都从独立服务时期沿用，因为它们是**调用方需要区分**的失败——PHP 侧据不同的码走不同的动作，所以不能塌进平台的 `ERR_INTERNAL`（对照 file-storage 的 `ERR_NO_ENGINE`）：

| 码 | 状态 | 含义 |
|---|---|---|
| `ERR_DB_UNREACHABLE` | 503 | 一台配置的库服务器连不上。503 因为**本服务没坏**——是数据库down了或还没起来 |
| `ERR_READONLY` | 409 | 对一个已降级为只读的连接/router 发起了写。409 因为调用方可以把写改发向一台可达的 master 来解决 |
| `ERR_DB_ACCESS_DENIED` | 403 | 配置的库用户缺少该操作所需的权限。403 因为这是数据库上的授权问题，与本服务自己的 token 校验（401）不同——修法是 GRANT，不是重试 |

映射由 `codeForKind` 完成（`errors.go`）：域内把驱动错误分类成 `DBError`（`mysqlerr.go` 按 errno 表，access-denied 一族 → `kindAccessDenied`，2006/2013 连接中断 → `kindUnreachable`），handler 的 `fail` 再把可被调用方处置的 kind 翻成上面三个码，**并且答一句通用文案**——`genericMessage` 只说「哪一类东西出了问题」，绝不带主机名、库名或查询。一个通过了 token 校验的服务间调用方，仍然不该从响应体里拿到集群的拓扑细节；真正的错误留给 `slog` 日志（走 `ERR_INTERNAL` 那条 return err 的路径）。

**其余失败一律收敛进平台六码**：鉴权→`ERR_UNAUTHORIZED`(401)、`:ref` 无匹配→`ERR_NOT_FOUND`(404)、无路由→`ERR_NOT_FOUND`、内部/不可处置的驱动错误→`ERR_INTERNAL`(500)。如第 1 节所述，`ERR_READONLY` 当前是一条定义了但七条只读路由都到不了的码；它绑定的是共享 `Conn` 的只读守卫（`conn.go`），随只读连接层留在 `dbapi`——写入路径抽象（`Router` / `TxManager` / 写重试）本身已迁到 `internal/dbproxy`（见 [ADR 0001](../adr/0001-isolate-db-proxy.md)）。

**新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。**
