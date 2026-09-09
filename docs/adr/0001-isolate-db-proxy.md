# ADR 0001：把 db-api 未进入运行路径的写入抽象隔离到 `internal/dbproxy`

- 状态：已采纳（2026-09-09）
- 范围：仅 Gorge Go 仓库 `go/internal/dbapi/` 与新建的 `go/internal/dbproxy/`
- Owner：db-api 域维护者（`gorge-db-api` 二进制）

## 背景

`gorge-db-api` 从独立服务迁入单仓库时，连同域逻辑一起搬来了一组**写入路径**抽象，用于忠实复现 Phorge `PhabricatorLiskDAO` / `AphrontDatabaseConnection` 的集群路由与嵌套事务语义：

- `Router`（`GetWriter` / `GetReader` / 只读降级 / 按 `(节点, 应用, 只读)` 缓存连接池）
- `TxManager`（嵌套事务 + MySQL savepoint，命名 `Aphront_Savepoint_%d`，与 Phorge 逐字一致）
- 写/连接重试 `QueryWithRetry` / `ConnectWithRetry` / `RetryPolicy` / `DefaultRetryPolicy`

审计（`go build` 全量引用 grep）确认：这些符号**没有任何 HTTP handler 或后台任务调用**，仅被单元测试覆盖。db-api 现有七条路由全是只读探测，各 service 用自己持有的 `ConnFactory` 开短连接直查 `INFORMATION_SCHEMA` / `SHOW *`，从不经过 `Router`。`Deps.Router` 在生产路径中唯一的用途是 `Deps.Close()` 关闭它的连接池——而那个连接池因为 `GetWriter`/`GetReader` 从不被调用，永远是空的。

## 决策

**迁移，不删除。** 把上述写入路径抽象连同其专属测试（`router_test.go`、`tx_test.go`、`retry_test.go`）迁到独立内部包 `go/internal/dbproxy/`：

- 保留这段与 Phorge 对齐的路由/事务/重试语义，作为「未来引入写端点时已被审阅、已被测试的地基」，而不是把它当死代码删掉、将来重写。
- 让 `dbapi` 生产包只留真正在运行路径上的读侧抽象，读代码时不再被一整套没人调用的写入机制干扰。

`dbproxy` 复用 `dbapi` 的共享数据类型（`Conn` / `DSN` / `ClusterConfig` / `DatabaseRef`）与错误分类（`DBError` 分类学），通过 `dbapi` 一个刻意小而有文档的桥接面（`dbproxy_bridge.go`）消费所需谓词与构造器，而不复制一份错误分类学。

`Deps` 不再持有仅为 `Close()` 存在的 `Router`。因为每条 db-api 答复都是请求/探测作用域内开合的短连接，**没有任何长期连接池需要释放**，`Deps.Close()` 变为一个有文档的 no-op（返回 nil），`main.go` 的关停契约保持不变，将来若引入真正的长期池，`Close()` 是它显而易见的释放点。

## 未迁移（经确认仍被生产使用，留在 `dbapi`）

- `Conn` / `DSN` / `NewConn` / `NewConnFromDB` / `ConnFactory`，以及 `conn.go` 的只读双层拦截（`QueryContext` / `ExecContext` / `QueryRowContext` 的 readonly 守卫）——四个读 service 全部用 `connFactory(dsn, true)` 开**只读**连接跑 SELECT/SHOW，这是共享的连接层。
- `isReadQuery` / `readQueryRe`——只读守卫的判据，读路径在用。
- `mysqlerr.go` 的 `classifyMySQLError` / 错误码表——`schema.go` / `migration.go` / `health.go` 直接调用，把驱动错误分类成域错误。
- `errors.go` 的 `ERR_READONLY` / `CodeReadonly` / `kindReadonly` / `codeForKind` / `genericMessage`——是**共享只读连接**的写拒绝语义与线上错误码契约（`PhabricatorGorgeDBClient` 按字符串分支），不是只服务于 `Router` 的写分支。`ERR_READONLY` 当前仍是一条「定义了但七条只读路由到不了」的码，但它绑定的是共享 `Conn` 的只读守卫，故留在 `dbapi`。

## 未来引入写端点时的入口

若 db-api 需要一个写端点（例如触发一次受控迁移），入口应是：

1. 在 `Deps` 上持有一个 `*dbproxy.Router`（或按需惰性构造），并在 `Deps.Close()` 里关闭它的连接池；
2. handler 通过 `router.GetWriter(ctx, application)` 取 master 连接，用 `dbproxy.QueryWithRetry` / `TxManager` 执行；
3. 失败经 `dbapi` 的 `fail` / `codeForKind` 映射成既有域错误码（含 `ERR_READONLY`）。

**约束不变**：不新增会把 db-api 变成通用 SQL 代理的端点，不碰业务表，写入仅限元信息 / 迁移这类受控操作。

## 影响

- `go build ./... && go vet ./... && go test ./...` 全绿；迁移的三个测试文件随代码进入 `dbproxy` 包并全部通过。
- `dbapi` 新增一个桥接文件 `dbproxy_bridge.go`，导出面仅为 `dbproxy` 服务，均为对既有内部符号的零分配薄封装。
- `DatabaseRef.passwordOr` 导出为 `PasswordOr`，供 `dbproxy` 以同样的每节点密码优先级构造 DSN。
