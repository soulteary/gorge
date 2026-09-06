# 平台层

`go/internal/platform/` 下的四个包，被所有模块共用，且不允许反向依赖任何业务域（约束与强制手段见 [`architecture.md`](architecture.md) 第 3 节）。

| 包 | 行数 | 职责 |
|---|---|---|
| `httpx` | 311 | HTTP 引导、中间件栈、多监听器、`{data,error}` 信封与全局错误处理器 |
| `auth` | 41 | 共享密钥中间件 |
| `health` | 59 | 容器探针端点 |
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
- **优雅关闭**。`Run()` 用 `signal.NotifyContext` 监听 SIGINT/SIGTERM，收到后调 `echo.Shutdown` 等待在途请求，`http.ErrServerClosed` 被归一化成 nil。它现在只是 `return RunAll(s)` 的一层壳，见第 1.4 节。

`Config.BodyLimit` 为空时取默认的 `2M`，`ShutdownTimeout` 非正时取默认的 10 秒。`Ready` 为 nil 表示服务没有外部依赖。`SkipRootProbe` 把 `GET /` 让给域包，全仓库只有一个端口设它，见第 3 节。

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

### 1.4 `RunAll`：一个进程多个监听器

```go
func RunAll(servers ...*Server) error
```

notification 域引入的设施（[`modules/notification.md`](modules/notification.md)）：它一个进程要起两个端口，因为 Phorge 要求 admin 与 client 分开。`Server.Run()` 现在就是 `return RunAll(s)`，render 侧行为完全没变。

三处设计选择，都是「多个监听器」这件事逼出来的：

- **共用一个 `signal.NotifyContext`。** 每个端口各自注册一次信号处理，得到的是几个互相不知情的关闭流程；共用一个之后一次 SIGTERM 关掉全部。
- **任一 listener 出错即整体退出。** 一个服务摊在几个端口上，缺任何一个都没有意义——只有 client 口活着的 notification 服务会接住浏览器连接，然后永远没有东西可以告诉它们。所以单个 listener 失败拖着整组一起停，而不是留下一个「半可达」的服务让编排继续放在轮转里。这一条是它与「起 N 个 goroutine 各跑一个 `Run()`」的关键差别：后者绑不上端口的那个静静退出，进程还在，健康探针还绿。
- **`Shutdown` 并发做。** 等待时间是最长的那个 `ShutdownTimeout`，不是它们的和。错误用 `errors.Join` 汇总；但走「listener 报错」这一支时只返回那个根因，其余的关闭错误丢掉——它们是根因的后果，报出来只会盖住真正的第一现场。

`serveErr` 那个 channel 是按 server 数量**带缓冲**的。不带缓冲的话，没被 `select` 选中的那些 goroutine 会永远卡在发送上。

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

### 3.1 `skipRoot`：唯一一条豁免，只为一个端口存在

`Register(e, ready, skipRoot)` 的第三个参数为 true 时**只**注册 `/healthz` 与 `/readyz`，把 `GET /` 让给域包（由 `httpx.Config.SkipRootProbe` 透传）。

它存在的理由只有一条，`health.go` 的注释里也写着：Phorge 用一个纯 HTTP 的 `GET /` 去探 notification 的 client 端口，并且**把 501 当作健康信号**，拿到本包本来会返回的 200 反而判定服务器坏了（`PhabricatorNotificationServerRef::testClient()`）。

两点别误读：

- **这不是「可选的探针开关」。** 默认 false，除了 notification 的 client 端口没有第二个地方该设它。设错的后果不对称：该设没设，Phorge 报「Got HTTP 200, but expected HTTP 501」——会报错；不该设却设了，`GET /` 落到 404 或域路由上，而没有任何测试或探针会指向这个改动。
- **两个容器探针照样注册。** 所以 Docker `HEALTHCHECK` 与 Kubernetes 探针不受影响，这也是豁免只挑根路径而不是整包跳过的原因。`health_test.go` 的 `TestSkipRootLeavesRootToTheCaller` 与 `TestSkipRootKeepsContainerProbes` 分别压这两半。

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
| `platform/httpx` | 97.1% |

`httpx` 从 74.1% 升到 97.1%，是 `RunAll` 那批测试带来的：原先「要起真进程才测得到」的信号循环与 `Shutdown` 路径，现在用 `:0` 端口起真 listener 加真 `SIGTERM` 覆盖了，`RunAll` 与 `shutdownAll` 都是 100%。

剩下的三处缺口都不值得补，写在这里免得下一个人去追：`OK()` 只被域包调用（本包自己的测试用不到它，域测试的计数不进本包）；错误响应写失败那条 `slog.Error` 要一个已经断掉的连接；`classify()` 里 `httpErr.Message` 不是字符串的兜底分支，Echo 自己从不构造这种错误。
