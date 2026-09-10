# Gorge

Phorge 的 Go 服务层单仓库。

Phorge 里若干原本靠子进程、PHP 内联实现或外部依赖完成的能力，在这里以 Go 服务重写，通过 HTTP 与 PHP 侧对接。仓库同时容纳 Go 代码、共享契约（OpenAPI + 契约固件）、容器编排，以及将来 PHP 侧的适配层。

仓库产出若干个二进制，每个二进制承载一个或多个域。**当前有哪些域、各由哪个二进制在哪个端口上服务，见 [`docs/README.md`](docs/README.md) 的模块表**——那张表是唯一一处需要维护这份清单的地方，这里刻意不重复它。

下面这份 README 讲的是**整个仓库共通的东西**（目录结构、两条调试路径、信封与错误码），并以默认二进制 `gorge-render` 为例。各域自己的配置、路由与兼容约束在 [`docs/modules/`](docs/modules/) 下一域一份。

## 目录结构

```
.
├── go/                       单一 Go module（github.com/soulteary/gorge/go）
│   ├── cmd/<二进制名>/        二进制入口，一个 cmd 一个服务
│   ├── internal/contracts/   线上数据结构，PHP / Go / OpenAPI / 固件的唯一真源
│   ├── internal/platform/    httpx / auth / health / config，不依赖任何业务域
│   ├── internal/<域名>/       一个域一个包：引擎 + HTTP 路由 + 配置
│   └── Dockerfile            一份 Dockerfile 服务所有二进制（ARG SERVICE 选择）
├── api/openapi/<域名>.yaml    各域的 HTTP 契约
├── compat/phorge/README.md   与 Phorge 的兼容约束，改动前必读
├── deploy/compose/           本地与单机部署编排
├── deploy/kubernetes/        （占位）
├── php/{extensions,adapters}/（占位）PHP 侧接入代码
├── docs/                     技术文档，跨模块四份 + 一域一份
└── tests/
    ├── contract/<域名>/       语言中立的契约固件，Go 与 PHP runner 共读
    └── e2e/<域名>.sh          对着运行中实例做的冒烟测试
```

具体展开成了哪些目录见 [`docs/architecture.md`](docs/architecture.md) 第 2 节。

`internal/platform/` 不允许反向依赖任何业务域。这是保持将来能把某个域单独拆出去的关键约束。

## 两条调试路径

### 路径一：容器编排（贴近生产）

```bash
cd deploy/compose
cp .env.example .env
docker compose up -d --build
```

或从仓库根：`make compose-up`。

```bash
curl -s http://127.0.0.1:8140/healthz
```

### 路径二：直接跑二进制（最快）

```bash
cd go
go test ./...
go build -o ../bin/gorge-render ./cmd/gorge-render
GORGE_SERVICE_TOKEN=dev-token ../bin/gorge-render
```

或从仓库根：`make test && make build && make run`。

服务默认监听 `:8140`。带 token 调试：

```bash
curl -s -H 'X-Service-Token: dev-token' \
  -d '{"source":"def f(): pass","language":"python"}' \
  http://127.0.0.1:8140/api/highlight/render | jq -r .data.html
```

输出里应当带 `class="k"` 这类 Pygments CSS 类名。跑完整冒烟：

```bash
TOKEN=dev-token make e2e
```

## 配置

Gorge 服务自身的环境变量只接受 `GORGE_*` 规范名称。阶段四已经移除独立服务时期的裸变量别名；升级部署时必须同步更新编排文件。外部后端的原生变量仍受支持，例如 mailer 的 `SMTP_*` 以及 `MAILER_ACCESS_KEY`、`MAILER_SECRET_KEY`、`MAILER_REGION`、`MAILER_ENDPOINT`、`MAILER_API_KEY`、`MAILER_DOMAIN`、`MAILER_API_HOSTNAME`、`MAILER_ACCESS_TOKEN`，还有 search 的 `ES_*` / `MEILI_*`；不要把这些名称改写成不存在的 `GORGE_*` 形式。`MAILER_CONFIG`、`MAILER_TYPE` 与 `MAILER_KEY` 不在例外范围内。

下表是 `gorge-render` 的。**每个二进制有自己的一张表**，在 [`docs/modules/`](docs/modules/) 下各自的第 4 节；`GORGE_LISTEN_ADDR` 与 `GORGE_SERVICE_TOKEN` 是所有服务共有的两个，只有默认端口不同。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GORGE_LISTEN_ADDR` | `:8140` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权 |
| `GORGE_CONFIG_FILE` | 无 | JSON 配置文件路径 |
| `GORGE_RENDER_MAX_BYTES` | `1048576` | 单次请求源码上限（字节） |
| `GORGE_RENDER_TIMEOUT_SEC` | `15` | 请求超时（秒） |
| `GORGE_RENDER_ENABLE_DIFF` | `true` | 是否注册 `/api/diff/*` 路由 |

`GORGE_RENDER_MAX_BYTES` 之外还有一道 `platform/httpx` 的传输层上限，固定 2M，不走环境变量。默认配置下前者（1MiB）更小，所以超限请求先被域级检查挡下；把它调到 2M 以上，挡下请求的就换成传输层中间件了。两条路径都返回 `413` + `ERR_TOO_LARGE`，只有 `message` 文案不同，客户端不必区分。

镜像里还有一个 `GORGE_HEALTHCHECK_PORT`（由 `go/Dockerfile` 的构建参数 `PORT` 决定，默认 `8140`）。它只被镜像的 `HEALTHCHECK` 指令使用，服务本身不读，所以不在上表里。改 `GORGE_LISTEN_ADDR` 的端口时要同步改它，否则探针一直打旧端口，容器会被判成 unhealthy。

## API 约定

完整契约见 [`api/openapi/render.yaml`](api/openapi/render.yaml)。

**鉴权**：请求头 `X-Service-Token` 优先，查询参数 `?token=` 兜底。服务端 token 为空时全部放行。

**响应信封**：`/api/**` 返回 `{data, error}`，两者恰有一个非空。

```json
{ "data": { "html": "...", "language": "python" } }
{ "error": { "code": "ERR_TOO_LARGE", "message": "source exceeds maximum allowed size" } }
```

这条对「没进到 handler 就失败」的请求同样成立：路径不存在、请求体超过传输上限、handler panic 被兜住，都由 `platform/httpx` 的全局错误处理器（`go/internal/platform/httpx/errors.go`）转成信封，而不是漏出 Fiber 默认的纯文本错误响应。唯一的例外是 `HEAD` 请求——协议不允许带响应体，只能靠状态码表达失败。

平台错误码：

| 码 | 状态 | 出现场景 |
|---|---|---|
| `ERR_BAD_REQUEST` | 400 | 请求体不是合法 JSON，或不符合 schema |
| `ERR_UNAUTHORIZED` | 401 | token 缺失或不匹配 |
| `ERR_NOT_FOUND` | 404 | 没有路由匹配（多写一个斜杠、base URL 拼错） |
| `ERR_METHOD_NOT_ALLOWED` | 405 | 路径存在但不接受该方法 |
| `ERR_TOO_LARGE` | 413 | 请求体超限，域级检查与传输层检查同码 |
| `ERR_INTERNAL` | 500 | panic 或其他非预期失败 |

域级错误码定义在各自的域包里，render 域目前有 `ERR_HIGHLIGHT_FAILED`(500)；其余域的见各自的模块文档第 6 节与 [`compat/phorge/README.md`](compat/phorge/README.md) 的附录。handler 已经用 `httpx.Fail` 应答过的响应不会被全局处理器改写，所以域级码不会退化成 `ERR_INTERNAL`。

两个路由细节容易被误判成错误的状态码：`/api/highlight/**` 下鉴权早于路由解析，所以不带 token 打不存在的路径返回 401 而非 404；同样在这个分组下，方法用错返回 404 而非 405（分组为了鉴权匹配了所有方法），因此 `ERR_METHOD_NOT_ALLOWED` 实际只在健康探针路径上见得到。

`ERR_INTERNAL` 的 `message` 恒为一句通用文案。panic 值、堆栈和内部错误串只写进 `slog` 日志（`PANIC_RECOVERED` / `REQUEST_FAILED`），不进响应体。

**健康探针不套信封**：`GET /`、`GET /healthz`、`GET /readyz` 返回裸 `{"status":"ok"}`，因为它们是给容器探针和负载均衡直接消费的。

| 端点 | 说明 |
|---|---|
| `POST /api/highlight/render` | 高亮源码，请求体 `{"source": "...", "language": "..."}` |
| `GET /api/highlight/languages` | 支持的语言列表 |
| `GET /healthz` | 存活探针 |
| `GET /readyz` | 就绪探针（render 域无外部依赖，起来即就绪） |

## 调试增强

- 请求关联：响应回传 `X-Request-Id`，请求头带了就透传，没带就生成。
- 优雅关闭：SIGINT / SIGTERM 触发，等待在途请求完成。

## 加一个新服务

1. 写 `go/cmd/<name>/main.go`，复用 `internal/platform` 引导；
2. `deploy/compose/docker-compose.yml` 加一个 service，`build.args.SERVICE` 填 `<name>`；
3. `.github/workflows/release.yml` 的 `matrix.service` 加一项。

`go/Dockerfile` 和 CI 不需要改。

## 开发

```bash
make help          # 列出所有目标
make check         # gofmt + go vet + go test，CI 的主要内容
make lint          # golangci-lint
make compose-config  # 校验 compose 文件
```

CI 带 `paths` 过滤（`go/**`、`tests/**`、`.github/workflows/**`），纯 PHP 改动不会触发 Go 流水线。

## 与 Phorge 的兼容约束

改动高亮输出、语言别名表、端口、路由、diff 输出格式、通知的线兼容、邮件错误码或搜索的字段名之前，**先读 [`compat/phorge/README.md`](compat/phorge/README.md)**。那里逐项记录了破坏之后**不会报错、只会静默失效**的约定，顶部一张总表按「破坏后的表现」索引到具体条目。

## 许可证

Apache License 2.0，见 [LICENSE](LICENSE)。
