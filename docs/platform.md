# 平台层

`go/internal/platform/` 下的四个包，被所有模块共用，且不允许反向依赖任何业务域（约束与强制手段见 [`architecture.md`](architecture.md) 第 3 节）。

| 包 | 行数 | 职责 |
|---|---|---|
| `httpx` | 265 | HTTP 引导、中间件栈、`{data,error}` 信封与全局错误处理器 |
| `auth` | 41 | 共享密钥中间件 |
| `health` | 50 | 容器探针端点 |
| `config` | 80 | 环境变量与 JSON 文件配置读取 |

## 1. httpx：统一的 HTTP 引导

`httpx.New(Config)` 返回一个预装好中间件栈与健康探针的 Echo 实例，域包只负责在 `srv.Echo()` 上注册自己的路由。中间件顺序：

```
RequestID → RequestLogger(slog) → Recover(slog) → BodyLimit(2M) → [域路由]
```

四点设计细节：

- **RequestID 复用入站值**。请求头带了 `X-Request-Id` 就透传，没带才生成。一次请求跨越 Phorge 与 Go 服务时保持同一个 id，日志才能串起来。
- **日志走 `log/slog`**。Echo 的 Recover 中间件默认用自己的 logger 打堆栈，这里通过 `LogErrorFunc` 改道到 slog，与进程其余日志汇合。响应体里永远只有一句通用文案，所以这条日志是 panic 现场的**唯一**记录。
- **`LogErrorFunc` 返回 err 而非吞掉**。返回错误才会让 `HTTPErrorHandler` 继续接手，把 panic 转成 500 信封；吞掉的话客户端会拿到一个空响应。
- **优雅关闭**。`Run()` 用 `signal.NotifyContext` 监听 SIGINT/SIGTERM，收到后调 `echo.Shutdown` 等待在途请求，`http.ErrServerClosed` 被归一化成 nil。

`Config.BodyLimit` 为空时取默认的 `2M`，`ShutdownTimeout` 非正时取默认的 10 秒。`Ready` 为 nil 表示服务没有外部依赖。

### 1.1 响应信封

```go
type Response struct {
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}
```

`/api/**` 的所有响应都是这个形状，`data` 与 `error` 恰有一个非空。`OK(c, data)` 与 `Fail(c, status, code, message)` 是两个写入口。

平台错误码六个，域包可以定义自己的码，但不得重定义这六个：

| 码 | 状态 | 出现场景 |
|---|---|---|
| `ERR_BAD_REQUEST` | 400 | 请求体不是合法 JSON，或不符合 schema |
| `ERR_UNAUTHORIZED` | 401 | token 缺失或不匹配 |
| `ERR_NOT_FOUND` | 404 | 没有路由匹配 |
| `ERR_METHOD_NOT_ALLOWED` | 405 | 路径存在但不接受该方法 |
| `ERR_TOO_LARGE` | 413 | 请求体超限 |
| `ERR_INTERNAL` | 500 | panic 或其他非预期失败 |

### 1.2 全局错误处理器：信封的兜底

`httpx` 用 `e.HTTPErrorHandler = errorHandler` 顶掉了 Echo 的默认处理器。这是整个平台层最关键的一处代码：没有它，「没进到 handler 就失败」的请求——路径不存在、请求体超过传输上限、handler panic——会漏出 Echo 默认的 `{"message": "..."}`，而 PHP 客户端既读不到 `error` 也读不到 `data`，只能退化成一句语焉不详的解析失败。

**别把这个处理器摘掉，也别在 `httpx.New()` 之外另建 Echo 实例。**

`classify()` 的分支设计有三处值得说明：

1. **非 `*echo.HTTPError` 一律 500 + 通用文案**。recover 兜住的 panic 和 handler 直接返回的裸 error 都落在这里，两者都没有自带状态码，也都不适合描述给调用方。
2. **未映射的 4xx 收敛成 `ERR_BAD_REQUEST`**，而不是每个状态码铸一个码。错误码是客户端 switch 的枚举，为本服务从不返回的状态码增加枚举项，只会让 PHP 侧写死用不上的分支。
3. **5xx 的 message 恒为 `internal server error`**。panic 值、堆栈、内部错误串只进 slog（`PANIC_RECOVERED` / `REQUEST_FAILED`），不进响应体。**排查 500 要看服务日志，不要指望响应体。**

两个特例：

- **`Committed` 检查**。handler 已经用 `httpx.Fail` 应答过的响应不会被改写。这是域级错误码不会退化成 `ERR_INTERNAL` 的原因，也避免了往响应体里追加第二个 JSON 文档。`render/http_test.go` 的 `TestHighlightFailedSurvivesTheErrorHandler` 用「先 Fail 再抛错」的形状把它钉住，并断言解码后 `decoder.More()` 为假。
- **HEAD 请求走 `NoContent`**。协议不允许 HEAD 响应带 body，只能用状态码表达失败。这是信封承诺唯一的例外。

### 1.3 状态码到错误码的映射是双向对齐的

```go
var statusCodes = map[int]string{ ... http.StatusRequestEntityTooLarge: CodeTooLarge ... }
```

同一个状态码，不论由 Echo 还是由 handler 产生，都报同一个码。413 是实际会发生的那一例：域级限额与传输层限额是两道独立的检查，客户端不应该需要区分是哪一道挡下的。细节见 [`modules/render.md`](modules/render.md) 第 3.2 节。

## 2. auth：可选的共享密钥

```go
if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
```

三点：

- 用 `crypto/subtle` 做定长时间比较，避免按字节短路的比较泄漏 token 前缀。
- `X-Service-Token` 请求头优先，`?token=` 查询参数兜底，后者是给无法设置请求头的场景（运维手册里的 curl 单行命令、浏览器直接打开的链接）。PHP 客户端走请求头。
- 服务端 token 为空时中间件整个跳过。这是本地开发与 compose 默认值不需要额外配置的原因，代价是**空 token 等于完全不鉴权**，只在私网可接受。

中间件挂在路由组上，它不知道自己保护的是哪些路径。由此产生一个反直觉的行为：**鉴权早于路由解析**，所以不带 token 打一个不存在的路径返回 401 而不是 404。副作用是不会向未认证调用方泄漏哪些路径存在。

## 3. health：刻意不套信封

`GET /`、`/healthz`、`/readyz` 返回裸 `{"status":"ok"}`。`health.go` 把理由写在了代码注释里：这些端点被 Docker HEALTHCHECK、Kubernetes 探针和负载均衡直接消费，它们的配置是按这个扁平形状写的，「为了一致性」包上信封会静默打断所有部署里的所有探针。**不要顺手统一。**

`ReadyFunc` 为 nil 表示服务没有外部依赖，起来即就绪——render 域正是这种情况，`main.go` 里显式传 `Ready: nil` 并配了注释。有依赖的模块（将来的 conduit、search）在依赖不可用时应返回 503，让编排把实例摘出轮转而不是杀掉。

`httpx/errors_test.go` 的 `TestProbesStayBareUnderTheErrorHandler` 与 e2e 脚本第 1 条场景都在断言探针响应里**不出现** `"data"`。

## 4. config：新名优先、旧名兜底

`EnvStr(fallback, keys...)` 按顺序取第一个非空值，调用方把新名写在前面：

```go
ListenAddr: EnvStr(defaultListenAddr, "GORGE_LISTEN_ADDR", "LISTEN_ADDR"),
```

旧的裸名保留，是为了让既有的 `phorge/docker/services/docker-compose.yml` 不改也能起来，属于**过渡措施，新编排请只用新名**。

引入 `GORGE_` 前缀的直接原因是：多个域将共用一个进程，`MAX_BYTES`、`TIMEOUT_SEC` 这类裸名会真的撞车。因此约定分两级——服务级用 `GORGE_`，域级再加一段域名（`GORGE_RENDER_MAX_BYTES`）。新模块的域级配置照此命名。

`EnvInt` 有一处细节：值解析失败时**跳到下一个 key** 而不是返回零值。写错一个数字会降级成旧名或默认值，而不是把限额悄悄设成 0。`EnvBool` 同理，走 `strconv.ParseBool`。

`LoadJSONFile[T any](path, dst *T)` 用泛型解码到调用方预填了默认值的结构体，文件里没提到的字段保持原值——「文件只覆盖它提到的东西」这一语义因此不需要每个服务各写一遍。域侧的用法是：先填默认值，**再从环境变量取 token**，最后让文件覆盖其余字段，这样密钥可以单独通过 Kubernetes Secret 之类注入而不进配置文件。

## 5. 覆盖率

| 包 | 覆盖率 |
|---|---|
| `platform/auth` | 100.0% |
| `platform/config` | 100.0% |
| `platform/health` | 100.0% |
| `platform/httpx` | 74.1% |

`httpx` 的缺口集中在 `Run()` 的信号循环与 `Shutdown` 路径——要起真进程才测得到，e2e 脚本在集成层面覆盖了它。
