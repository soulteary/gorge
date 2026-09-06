# Phorge 兼容契约

本文件记录 Gorge 的 Go 服务与 Phorge PHP 端之间**不能随意改动**的三项约定。这些约束此前只以注释形式散落在代码里，一旦被「顺手优化」掉，故障表现是全站高亮静默失效（页面正常渲染，只是没有颜色），不会有任何报错，因此单独成文。

改动其中任何一项，都必须同步改动 PHP 侧并在这里更新说明。

---

## 一、Pygments 语言别名表必须双向同步

**Go 侧**：`go/internal/render/highlight/lexermap.go` 的 `buildLexerMap()`
**PHP 侧**：`PhutilPygmentsSyntaxHighlighter::getPygmentsLexerNameFromLanguageName()`
（参考实现见 `phorge-fork/src/infrastructure/markup/syntax/highlighter/PhutilPygmentsSyntaxHighlighter.php`）

Go 侧的别名表是从 PHP 侧那张 `static $map` 逐条抄过来的（PHP 约 166 条，Go 侧约 174 条，Go 多出的几条是 Chroma 与 Pygments 之间的 lexer 命名差异补丁）。它的作用是把 Phorge 数据库里存量的语言标识（`adb`、`ads`、`ahkl`、`bat`、`cxx` 这类历史别名）翻译成 Chroma 认得的 lexer 名。

之所以不能只维护一侧：Phorge 并没有全量切到 Go 服务，`PhutilPygmentsSyntaxHighlighter` 仍是可选的高亮后端。两张表漂移后，同一个 `.adb` 文件在两个后端下会走不同 lexer，产生不同的 HTML，且没有任何断言会捕捉到。

**新增别名时**：Go 与 PHP 两侧同时加，并在 `tests/contract/render/` 补一条固件。

## 二、Chroma formatter 的三项配置是固定的

`go/internal/render/highlight/highlight.go` 顶部：

```go
formatter = html.New(
    html.WithClasses(true),
    html.PreventSurroundingPre(true),
)
defaultStyle = styles.Get("pygments")
```

三项都不能改，各有各的理由：

| 配置 | 原因 |
|---|---|
| `styles.Get("pygments")` | Chroma 的 style 决定输出的 CSS **类名**。只有 `pygments` 这一套的类名（`k`、`nf`、`nb`、`s2`、`mi`、`c1` …）与 Phorge 既有样式表对得上。换成 `monokai`、`github` 之类会输出另一批类名，页面上所有 token 都失去样式。 |
| `WithClasses(true)` | 输出 `class="k"` 而不是内联 `style="color:#008000"`。内联样式会绕过 Phorge 的样式表与暗色主题，且体积暴涨。 |
| `PreventSurroundingPre(true)` | 只输出 token 片段，不带外层 `<pre>`。外层容器由 Phorge 自己渲染（它要挂行号、diff 高亮等附加结构），Chroma 再包一层会造成嵌套 `<pre>`。 |

守住这条的是 `go/internal/render/highlight/compat_test.go` 的 `TestPygmentsCSSClassCompatibility`：它断言输出里出现 `k`/`n`/`nf`/`nb`/`s2`/`mi`/`c1` 这批类名。契约固件同样只做 contains 断言——Chroma 升级会改变 HTML 的具体结构，精确 golden 匹配必然频繁误报，真正要锁死的只是类名集合。

## 三、端口与路由变更记录

### 路由：按域命名，不按二进制命名

`gorge-render` 这一个进程承载整个 render 域。路由保持 `/api/highlight/*`，**不是** `/api/render/*`：

- `POST /api/highlight/render`
- `GET /api/highlight/languages`

这两个路径是 `PhabricatorGoHighlightClient`（`src/infrastructure/cluster/PhabricatorGoHighlightClient.php`）已经在调的，改了要同步改 PHP。按域而非按二进制命名的好处是，将来 diff 并进同一进程时直接加 `/api/diff/*` 即可，两边都不用动。

### 端口：`:8130` 并入 `:8140`

原先 highlight 与 diff 是两个独立服务：

| 服务 | 旧端口 | 现状 |
|---|---|---|
| `gorge-highlight` | `:8140` | 由 `gorge-render` 继承，保持 `:8140` |
| `gorge-diff` | `:8130` | **废弃**，diff 并入 `gorge-render` 后走 `:8140` |

对 PHP 侧的影响：`go-highlight.url` 配置项（或 `GO_HIGHLIGHT_URL` 环境变量）不需要改。将来接入 diff 时，原本指向 `:8130` 的 diff 配置要改指 `:8140`。旧的 `phorge/docker/services/docker-compose.yml` 里 `diff` 服务那一段届时应当整体删除，而不是留着空跑。

### 环境变量：新名优先，旧名兜底

因为 highlight 与 diff 共用一个进程，`MAX_BYTES`、`TIMEOUT_SEC` 这类裸名会真的撞车。新配置引入 `GORGE_` 前缀，服务级知识再加域名段；`platform/config` 按顺序查找，取第一个非空值，所以旧编排文件里的裸名仍然能跑。

| 新名 | 旧名（兜底） | 默认值 |
|---|---|---|
| `GORGE_LISTEN_ADDR` | `LISTEN_ADDR` | `:8140` |
| `GORGE_SERVICE_TOKEN` | `SERVICE_TOKEN` | 空（空则不鉴权） |
| `GORGE_CONFIG_FILE` | `HIGHLIGHT_CONFIG_FILE` | 无 |
| `GORGE_RENDER_MAX_BYTES` | `MAX_BYTES` | `1048576` |
| `GORGE_RENDER_TIMEOUT_SEC` | `TIMEOUT_SEC` | `15` |

旧名保留是为了让 `phorge/docker/services/docker-compose.yml` 不改也能起来，属于过渡措施，不要在新编排里使用。

---

## 附：鉴权与响应信封

`PhabricatorGoHighlightClient` 依赖以下两点，改动会直接打断 PHP 侧：

**鉴权**：请求头 `X-Service-Token` 优先，查询参数 `?token=` 兜底；服务端 token 配置为空时全部放行。PHP 客户端走的是请求头。

**响应信封**：`/api/**` 返回 `{data, error}`，`data` 与 `error` 恰有一个非空。PHP 客户端先检查 `$envelope['error']`，非空则抛异常（异常消息里带 `error.code`），否则返回 `$envelope['data']`。

这条对没进到 handler 就失败的请求同样成立。`go/internal/platform/httpx/errors.go` 用 `e.HTTPErrorHandler` 顶掉了 Echo 的默认错误处理器，所以路由不匹配、请求体超过传输上限、handler panic 被 `Recover` 兜住这几种情况，PHP 客户端拿到的仍是信封，而不是 Echo 默认的 `{"message": "..."}`——后者会让客户端既读不到 `error` 也读不到 `data`，退化成一句语焉不详的解析失败。**别把这个处理器摘掉，也别在 `httpx.New()` 之外另建 Echo 实例。**

唯一不带信封的错误响应是 `HEAD` 请求：协议不允许带响应体，只有状态码。PHP 客户端只发 POST/GET，不受影响。

平台级错误码六个：

| 码 | 状态 | 出现场景 |
|---|---|---|
| `ERR_BAD_REQUEST` | 400 | 请求体不是合法 JSON |
| `ERR_UNAUTHORIZED` | 401 | token 缺失或不匹配 |
| `ERR_NOT_FOUND` | 404 | 没有路由匹配 |
| `ERR_METHOD_NOT_ALLOWED` | 405 | 路径存在但不接受该方法 |
| `ERR_TOO_LARGE` | 413 | 请求体超限 |
| `ERR_INTERNAL` | 500 | panic 或其他非预期失败 |

`ERR_NOT_FOUND` 对 PHP 侧最有诊断价值：`go-highlight.url` 尾部多一个斜杠、或 base URL 拼接出双斜杠时，拿到的就是它。两个路由细节别误判：`/api/highlight/**` 分组的鉴权早于路由解析，不带 token 打不存在的路径返回 401 而不是 404；同样在这个分组下方法用错返回 404 而不是 405（分组为了鉴权匹配了所有方法），所以 `ERR_METHOD_NOT_ALLOWED` 实际只在健康探针路径上见得到。

`ERR_TOO_LARGE` 有两个来源，同码是刻意的：`GORGE_RENDER_MAX_BYTES`（默认 1MiB）由 handler 检查，`httpx` 的传输层上限（固定 2M）由中间件检查。默认配置下只会命中前者；把 `GORGE_RENDER_MAX_BYTES` 调到 2M 以上就会改走后者。PHP 客户端按码分支即可，不需要知道是哪一道。

`ERR_INTERNAL` 的 `message` 恒为一句通用文案，panic 值与堆栈只进 `slog` 日志。**排查 500 要看服务日志，不要指望响应体。**

render 域额外保留一个域级错误码 `ERR_HIGHLIGHT_FAILED`(500)：高亮 handler 内部失败时返回它而不是 `ERR_INTERNAL`。这是迁移前就有的码，Phorge 侧已经在用，故未收敛进平台码。全局错误处理器不会覆盖它——`httpx.Fail` 一写响应就 committed，处理器见到 `Committed` 就不再落笔。新增域级错误码时同样加在 `internal/render/http.go`，不要塞进 `platform/httpx`。

**空 source 不是错误**：`{"source": ""}` 返回 200 与空 `html`，不返回 400。Phorge 渲染空文件时依赖这个行为。

**健康探针不套信封**：`GET /`、`GET /healthz`、`GET /readyz` 返回裸 `{"status":"ok"}`。这是给容器探针和负载均衡用的，不要「顺手统一」成信封格式。
