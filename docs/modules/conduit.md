# conduit 模块

Conduit API 的反向代理网关：接收 `ANY /api/:method`，完成 Service Token 鉴权与基于客户端 IP 的令牌桶限流后，反代到上游 Phorge PHP 的 `/api/*`。它是本仓库里唯一一个**消费方是其它 Go 服务而非 Phorge PHP** 的域——其它域都在 Phorge 前面被 Phorge 调用，而它挡在 Phorge 前面、被别的 Go 服务调用。

| | |
|---|---|
| 二进制 | `gorge-conduit` |
| 端口 | `:8150` |
| 包 | `go/internal/conduit/` |
| 契约 | [`api/openapi/conduit.yaml`](../../api/openapi/conduit.yaml) |
| 固件 | `tests/contract/conduit/` |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) ← **改动前必读** |

## 1. 职责边界

**负责**：把散落在各 Go 服务里的「直连 Phorge `/api/*`」收敛到一个网关，在这一层统一做三件事——Service Token 鉴权、按客户端 IP 的令牌桶限流、Conduit 调用的集中审计（方法名 / 状态码 / 耗时 / 客户端 IP）。成功时把上游的状态码、响应头与响应体**原样透传**，不重新编码。

**不负责**：Conduit 方法的语义。网关不理解 `conduit.search` 与 `conduit.ping` 有什么不同，只按方法名拼上游 URL 并转发；方法级豁免（见第 4 节）是它对方法名唯一的用途，且只用于限流判定。

**为什么要有这一层**。Phorge 的 Conduit API 原生暴露在 PHP 上，各 Go 服务各自直连，带来四个问题：认证凭据分散、无处施加限流、请求日志散落、以及每个调用方各自实现 HTTP 客户端配置。把这些收敛到一个网关之后，认证是一个配置点、限流保护上游 PHP 不被单个异常客户端拖垮、日志集中、上游地址变更只改网关一处。代价是它成了这一层里结构上最特别的一个：它替换的既不是一个子进程（render 的 Pygments）、也不是一条运行时依赖（notification 的 Aphlict）、也不是一个外部存储/投递通道（mailer/search/file-storage），而是一种**调用拓扑**——把「多对一直连」改成「多对一经网关」。

**无持久状态**。限流器的 visitor 表在内存里、每实例独立，没有要落盘的东西，所以 healthcheck 打 `/healthz` 而不是 `/readyz`（与 render 一致，与 mailer/search/file-storage/webhook 那一类相反）。它**刻意不**把「上游是否可达」纳入就绪判据：网关从不为了判断自己是否健康而去拨测上游，否则 `/readyz` 报告的就成了 Phorge 的健康而非网关自己的健康。

## 2. 路由与依赖

```go
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api")
	g.Use(auth 中间件(deps.Token))     // 产出 Conduit 风格错误体
	g.Use(deps.RateLimiter.Middleware()) // RPS<=0 时零开销
	g.All("/:method", proxyHandler(deps))
}
```

| 方法 | 路径 | 鉴权 |
|---|---|---|
| ANY | `/api/:method` | 需要 |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） |

`ANY /api/:method` 用 fiber 的 `All` 接收所有 HTTP 方法并透明转发，不限制调用方使用的 HTTP 方法。健康探针路径不经鉴权与限流中间件，确保 Docker `HEALTHCHECK` 与负载均衡器探测不受影响；限流中间件仅挂在 `/api` 组上。

`Deps` 由 `main.go` 组装（反代器 `Proxy`、限流器 `RateLimiter`、`Token`）。限流中间件在 `RATE_LIMIT_RPS <= 0` 时是零开销的快速放行。

## 3. 核心实现

### 3.1 反代处理链路

一次 `/api/:method` 请求依次经过：鉴权 → 限流 → 反代。反代步骤：

1. 从路由参数取方法名，为空返回 400 `ERR-CONDUIT-CORE`。
2. 拼 `{upstream}/api/{method}`，保留原始 query string。
3. 用原始请求的 Context 构建上游请求（上下文取消能传播到上游）。
4. 复制请求头，过滤 8 个 hop-by-hop 头（`Connection`、`Keep-Alive`、`Proxy-Authenticate`、`Proxy-Authorization`、`Te`、`Trailer`、`Transfer-Encoding`、`Upgrade`）。
5. 注入 `X-Forwarded-For`、`X-Forwarded-Proto`、`X-Conduit-Gateway: go-conduit`。
6. 通过复用的 HTTP 客户端发到上游；`CheckRedirect` 设为 `http.ErrUseLastResponse`，**不自动跟随重定向**——重定向响应原样回给调用方。
7. 成功则把上游状态码、响应头、响应体**原样透传**（流式复制 body）。

### 3.2 令牌桶限流

按客户端 IP 独立限流，经典令牌桶：

- **快速路径**：`rps <= 0` 或方法在豁免列表中直接放行。
- **惰性补充**：只在请求到来时按距上次访问的时间差一次性补充令牌（`elapsed * rps`，上限为桶容量 `burst`），避免为每个 visitor 跑后台定时器，单次判定 O(1)。
- **内存保护**：visitor map 上限 `maxVisitors = 100_000`，满了直接拒绝新 IP，防止用大量不同 IP 撑爆内存（HashDoS 变体）。
- **过期清理**：后台 goroutine 每 5 分钟清一次，删除 10 分钟内无活动的 visitor；`Stop()` 经 `done` channel 优雅停止，避免退出时 goroutine 泄漏。

### 3.3 恒定时间鉴权

Token 比较用 `crypto/subtle.ConstantTimeCompare` 而非 `==`，比较耗时与是否匹配无关，消除按响应时间逐字节猜 token 的时序攻击向量。Token 支持两种传递方式：请求头 `X-Service-Token`（优先）与查询参数 `?token=`（兜底）。`SERVICE_TOKEN` 为空时中间件直接放行，方便开发/测试环境零配置运行。

## 4. 配置

域级环境变量用 `GORGE_CONDUIT_` 前缀，进程级的监听地址与 token 沿用平台层的 `GORGE_LISTEN_ADDR` / `GORGE_SERVICE_TOKEN`。命名规则与「新名优先、旧名兜底」的查找机制见 [`../platform.md`](../platform.md) 第 4 节。

| 变量 | 兜底旧名 | 默认值 | 说明 |
|---|---|---|---|
| `GORGE_LISTEN_ADDR` | `LISTEN_ADDR` | `:8150` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | `SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权 |
| `GORGE_CONDUIT_UPSTREAM_URL` | `UPSTREAM_URL` | `http://phorge:80` | 上游 Phorge PHP 地址 |
| `GORGE_CONDUIT_RATE_LIMIT_RPS` | `RATE_LIMIT_RPS` | `0` | 每 IP 每秒请求上限，0 关闭限流 |
| `GORGE_CONDUIT_RATE_LIMIT_BURST` | `RATE_LIMIT_BURST` | `20` | 令牌桶容量（突发） |
| `GORGE_CONDUIT_RATE_LIMIT_EXEMPT` | `RATE_LIMIT_EXEMPT` | `conduit.ping,conduit.getcapabilities` | 豁免限流的方法，逗号分隔 |
| `GORGE_CONDUIT_PROXY_TIMEOUT_SEC` | `PROXY_TIMEOUT_SEC` | `30` | 上游请求超时秒数 |
| `GORGE_CONDUIT_MAX_BODY_SIZE` | `MAX_BODY_SIZE` | `10M` | 请求体上限 |

## 5. 与 Phorge 的兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md)，这里是概述。本域的兼容形状与其它域都不同：它不涉及输出格式的逐字节一致（diff）、也不涉及别名表这类映射（render），而涉及**错误信封的形状**与**透传的透明度**。共同特征仍是那句——**破坏后不报错，只静默失效。**

### 5.1 错误信封是 Conduit 专用的，不是平台的

网关自身产生的失败用 **Conduit 兼容信封** `{result, error_code, error_info}`，而**不是** Gorge 其它域的 `{data, error}`：

```json
{
  "result": null,
  "error_code": "ERR-CONDUIT-PROXY",
  "error_info": "Upstream request failed."
}
```

这是有意的例外，理由记在 `go/internal/contracts/conduit.go` 的注释里：调用 Conduit 端点的客户端本来就在解析 Phorge 自己的错误形状，网关沿用同一套，调用方就不必为「网关的错误」与「Conduit 的错误」写两套解析。错误码用连字符（`ERR-CONDUIT-AUTH`），照 Phorge Conduit 的写法，**不是**平台层那套下划线码（`ERR_UNAUTHORIZED`）。把它「顺手统一」成平台信封，会让所有已经按 Conduit 形状解析的调用方在遇到网关错误时静默拿到一个它们不认识的结构。

### 5.2 成功路径必须是纯透传

成功时上游的状态码、响应头、响应体**原样透传**，网关不重新编码。这条一旦被破坏——比如给成功响应套一层信封、或改写 content-type——调用方会拿到一个「结构对不上却又是 200」的响应，同样不报错。所以本域**只在失败路径上**产出 Conduit 信封，成功路径一个字节都不碰。

### 5.3 端口与拓扑

沿用原生端口 `:8150`。它替换的是「各 Go 服务直连 Phorge `/api/*`」的直连模式，所以接入方式是让消费方（如 `gorge-worker`）把 Conduit 调用指向网关地址（`http://gorge-conduit:8150`）而非直连 Phorge。PHP 侧默认无需改动——网关只服务 Go→Phorge 的流量，Phorge PHP 不主动经网关发 Conduit 调用。

## 6. 域级错误码

本域用四个 Conduit 协议错误码，都定义在自己的域包里，**不收敛进平台码**（平台码是 `{data,error}` 形状，与此不兼容）：

| 错误码 | HTTP | 产生位置 | 含义 |
|---|---|---|---|
| `ERR-CONDUIT-CORE` | 400 | 反代处理器 | URI 中未指定 Conduit 方法 |
| `ERR-CONDUIT-AUTH` | 401 | 鉴权中间件 | Token 缺失或不匹配 |
| `ERR-RATE-LIMIT` | 429 | 限流中间件 | 超出限流（仅 RPS>0 时可达） |
| `ERR-CONDUIT-PROXY` | 502 | 反代处理器 | 上游请求构建失败或上游无响应 |

`ERR-CONDUIT-PROXY` 在纯服务层栈里是常态而非异常：本栈没有 phorge 容器，`GORGE_CONDUIT_UPSTREAM_URL` 默认指向解析不到的 `phorge` 主机，于是每个反代调用都落到 502——这是「上游不存在」而非「网关坏了」。网关本身照常 `/healthz` 答 200。
