# render 模块

把源码产物渲染成 HTML 交给 Phorge，目前即语法高亮。diff 域与本域共用 `gorge-render` 这个二进制与 `:8140` 这个端口，但是独立的包，见 [`diff.md`](diff.md)。

| | |
|---|---|
| 二进制 | `gorge-render` |
| 端口 | `:8140` |
| 包 | `go/internal/render/`、`go/internal/render/highlight/` |
| 契约 | [`api/openapi/render.yaml`](../../api/openapi/render.yaml) |
| 固件 | `tests/contract/render/` |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) ← **改动前必读** |

## 1. 职责边界

**负责**：接收源码与语言提示，返回带 Pygments CSS 类名的 HTML 片段；列出支持的语言。

**不负责**：外层 DOM 结构。输出里没有 `<pre>`、没有 `<div class="highlight">`，只有 `<span class="...">` 序列与文本节点——Phorge 自己渲染外层容器，因为它要在上面挂行号与 diff 高亮。

**无外部依赖**。高亮是纯计算，没有数据库、缓存或下游服务，所以 `main.go` 显式传 `Ready: nil`，就绪等同于存活。diff 域同样如此，这也正是两者能共用一个进程的原因；需要数据库或下游服务的域则有各自的就绪判据。

## 2. 路由与依赖

```go
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/highlight")
	g.Use(auth.Token(deps.Token))

	g.Post("/render", renderHighlight(deps))
	g.Get("/languages", listLanguages(deps))
}
```

| 方法 | 路径 | 鉴权 |
|---|---|---|
| POST | `/api/highlight/render` | 需要 |
| GET | `/api/highlight/languages` | 需要 |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） |

路径按**域**命名而非按二进制命名（`/api/highlight/*` 而不是 `/api/render/*`），理由见 [`../architecture.md`](../architecture.md) 第 4.3 节。`TestRoutePathsAreStable` 遍历 `app.GetRoutes(true)` 断言这两条路径仍然注册着——Phorge 侧 `PhabricatorGorgeRenderClient` 已经在调它们，重命名是 PHP 侧的破坏性变更。

`Deps` 是一个三字段结构体（`Highlighter` / `Token` / `MaxBytes`），由 `main.go` 组装。整个仓库没有引入 DI 框架，也没有包级单例——`main.go` 串联「加载配置 → 建服务器 → 注册两个域的路由 → Run」四步。`cfg.ServiceToken` 同时传给两个域：token 认证的是调用方对这个进程的身份，不是对某个路由分组的身份。

## 3. 渲染处理器

### 3.1 四道处理

```go
if err := c.Bind().Body(&req); err != nil {
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) && fiberErr.Code != http.StatusBadRequest {
		return err
	}
	return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
}
```

**`Bind` 的错误分支是最容易写错的一处。** Fiber 可能在绑定请求体时返回带状态码的 `*fiber.Error`。如果无条件把绑定错误当成 400，传输层的 413 就会被压成 `ERR_BAD_REQUEST`，而 PHP 客户端是按码分支的。这里的做法是：非 400 的 `*fiber.Error` 原样交回平台错误处理器，只有真正的 JSON 语法错误留在本地当 400。

剩下三道：

- **空 source 短路**：`{"source": ""}` 返回 200 与空 `html`，**不是错误**。Phorge 渲染空文件时依赖这个行为。
- **大小检查**：`len(req.Source) > deps.MaxBytes` 返回 413 `ERR_TOO_LARGE`。
- **调用引擎**：失败返回域级码 `ERR_HIGHLIGHT_FAILED`(500)。

### 3.2 两道 413 是刻意同码的

| 来源 | 阈值 | 检查点 | 默认可达 |
|---|---|---|---|
| `GORGE_RENDER_MAX_BYTES` | 1 MiB | handler 内 | 是 |
| `httpx` 传输层 BodyLimit | 2 M（硬编码） | 中间件 | 仅当前者调到 2M 以上 |

两条路径都返回 413 + `ERR_TOO_LARGE`，只有 `message` 文案不同，且文案不属于契约。客户端不必区分是哪一道挡下的。

`TestOversizedBodyReportsTooLargeFromEitherLimit` 把 `MaxBytes` 调到 8MiB 后发一个 3MiB 的 body，验证平台传输层那条路径同样落在信封里，而不是 Fiber 默认的纯文本形状。`TestOversizedChunkedBodyIsNeverABadRequest` 覆盖流式路径，它只断言「码不是 `ERR_BAD_REQUEST`」而不断言一定被拒——因为 JSON 解码器在读到错误后还会继续消费多少字节，随实现版本而变，那部分不是契约。

### 3.3 语言列表

`GET /api/highlight/languages` 返回 Chroma 注册的全部 lexer 名，小写。两个使用注意事项写进了 OpenAPI：**顺序是 Chroma 的**（按原始混合大小写名排序后再逐个小写，所以 `atl` 排在 `actionscript` 前面），不要依赖它；`lexermap.go` 里的别名在 `render` 端点可解析，但**不出现在这个列表里**。

## 4. 高亮引擎

`internal/render/highlight/` 包含高亮引擎与单独维护的别名表。

### 4.1 Chroma 配置的三项不可变设定

```go
formatter = html.New(
	html.WithClasses(true),
	html.PreventSurroundingPre(true),
)
defaultStyle = styles.Get("pygments")
```

| 配置 | 作用 | 改掉的后果 |
|---|---|---|
| `styles.Get("pygments")` | 决定输出的 CSS **类名**体系（`k`/`nf`/`nb`/`s2`/`mi`/`c1`…） | 换成 `monokai`、`github` 会输出另一批类名，页面上所有 token 失去样式 |
| `WithClasses(true)` | 输出 `class="k"` 而非内联 `style="color:#008000"` | 内联样式绕过 Phorge 样式表与暗色主题，且体积暴涨 |
| `PreventSurroundingPre(true)` | 只输出 token 片段，不带外层 `<pre>` | Phorge 自己渲染外层容器，会造成嵌套 `<pre>` |

三项失效都不会报错，只会静默丢样式，所以 `compat_test.go` 的 `TestPygmentsCSSClassCompatibility` 显式断言这批类名的存在。

Chroma 本身就是 Pygments 的 Go 移植，用 `pygments` style 时类名体系天然一致：

| 类名 | 含义 | 例 |
|---|---|---|
| `k` | Keyword | `def`、`class` |
| `nf` / `nb` | Name.Function / Name.Builtin | 函数名 / `print` |
| `s1` / `s2` | String.Single / String.Double | 单双引号字符串 |
| `mi` | Number.Integer | 整数字面量 |
| `c1` | Comment.Single | 单行注释 |

### 4.2 三级 Lexer 查找

```
language ──► resolveLexer()（别名表）──► lexers.Get()
                                            │ nil
                                            ▼
                                     lexers.Analyse(source)   ← shebang / 内容特征
                                            │ nil
                                            ▼
                                     lexers.Fallback          ← 纯文本
                                            │
                                     chroma.Coalesce()        ← 合并相邻同类 token
                                            │
                                     Tokenise → Format → HTML
```

三级兜底保证 `Highlight()` 不会因为「不认识这个语言」而失败——未知语言退化成纯文本，是有意义的输出。`Coalesce` 合并相邻同类型 token，减少输出里的 `<span>` 数量。

返回的 `language` 字段有一处容易误解的语义（OpenAPI 里有完整描述）：请求带了非空 `language` 就**原样回显**，别名、未知名、原始大小写都不变，所以 `py` 回 `py` 而不是 `python`；只有省略 `language` 的请求才回报内容嗅探选中的 lexer 名（小写）。

### 4.3 别名表与大小写敏感

`buildLexerMap()` 有 184 条映射，对应 PHP 侧 `PhutilPygmentsSyntaxHighlighter::getPygmentsLexerNameFromLanguageName()` 的 166 条，Go 侧多出 20 条现代语言键（`ts`/`tsx`/`jsx`/`rs`/`kt`/`swift`/`toml`/`tf`/`hcl`/`graphql` 等）与小写补充。

**这张表区分大小写**，`resolveLexer` 的查找顺序是「先原样查，未命中才降级到小写再查」：

```go
if mapped, ok := h.lexerMap[language]; ok {
	return mapped
}
lang := strings.ToLower(language)
if mapped, ok := h.lexerMap[lang]; ok {
	return mapped
}
```

原因是 PHP 侧用 `idx()` 查一个普通的大小写敏感数组，而传进来的语言名是 `getLanguageFromFilename()` 从文件名里切出的扩展名，**没有归一化**，所以 `foo.R` 真的以 `R` 的形式到达。表里有两组同字母异映射：

| 键 | 目标 lexer | 含义 |
|---|---|---|
| `R` / `S` | `splus` | R 语言（`splus` 是 Chroma 里 R lexer 的别名） |
| `r` | `rebol` | REBOL，Chroma 无对应 lexer，退化为纯文本 |
| `s` | `gas` | GAS 汇编 |

早期实现无条件先 `ToLower`，把 `R` 折成 `r`、`S` 折成 `s`，结果所有 `.R` 文件按 REBOL 处理（即无高亮）、所有 `.S` 文件按汇编处理。这是这套兼容约束目前唯一一次真实漂移，修复见 `7c0d996`，现在由 `TestCaseSensitiveAliasesReachDistinctLexers` 与两份大小写固件锁住。`lexermap.go` 末尾单列了一组混合大小写键，与 PHP 表逐条对应，方便 diff。

## 5. 与 Phorge 的兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md)，这里是判据的概述。三件事的共同特征：**破坏后不报错，只静默失效。**

### 5.1 别名表：PHP 表是下界，Go 表可以是超集

约束是**不对称**的，根源在 PHP 侧的 `idx($map, $language, $language)`——未命中就把语言名原样透传给 pygmentize。

- **PHP → Go 必须同步**：PHP 表里做了非恒等映射的键（现存 166 条全部满足），Go 侧必须有且映射到等价 lexer。漏一条，Go 会把 `adb` 原样交给 Chroma 落到内容嗅探，两个后端就此分叉。
- **Go → PHP 不必回补**：Go 独有的 20 条无害，因为 pygmentize 本身就认 `ts`、`rs`、`kt` 这类别名，PHP 透传后落到同一个 lexer。新增 Go 独有键时唯一要确认的就是这个前提。

### 5.2 命名偏离的完整清单

「能照抄就照抄」——即使 Chroma 接受同义写法，目标名也一律用 PHP 的那个（`rb` 而非 `ruby`、`coffee-script` 而非 `coffeescript`），这样两张表能逐字 diff。已实测（Chroma v2.27）的偏离只有四组：

| PHP 写法 | Chroma 情况 | Go 侧 |
|---|---|---|
| `antlr-ruby`（`g`/`G`） | 不存在 | 写 `antlr` |
| `ragel-em`（`rl`） | 不存在 | 写 `ragel` |
| `v`（`sv`） | 存在，但指向 **V/vlang** 而非 Verilog | 写 `verilog` |
| `html+evoque` / `xml+evoque` | 不存在 | 整条不进表 |

`v` 那条是判据的来处：**`lexers.Get()` 返回非 nil 不代表解析对了。** 对齐某个写法时要比对两个写法拿到的 lexer **身份**（`Config().Name`），只判空会把 `sv` → `v`（Verilog 变成 vlang）放过去。

另有两条刻意保留的空映射：`rebol`（`r`/`r3`）与 `rconsole`（`Rout`）在 Chroma 里不存在，照抄 PHP 的写法让它们退化为纯文本，忠实反映 PHP 的意图。这不算漂移，**不要「顺手修正」回 Pygments 的原名。**

**新增别名的检查清单**：PHP 表有的键 Go 必须有，键的大小写照抄；Go 独有键先确认 pygmentize 透传后落到同一 lexer；目标名先用 `lexers.Get()` 确认 Chroma 认得；最后在 `tests/contract/render/` 补一条固件。

### 5.3 端口与路由

`gorge-highlight` 的 `:8140` 由 `gorge-render` 继承；`gorge-diff` 原先的 `:8130` **已废弃**，diff 域现在在 `:8140` 上以 `/api/diff/*` 提供服务。对 PHP 侧的影响：`gorge.render.uri` 与 `gorge.render.token` 两个配置项不需要改。

## 6. 配置

| 变量 | 兜底旧名 | 默认值 | 说明 |
|---|---|---|---|
| `GORGE_LISTEN_ADDR` | `LISTEN_ADDR` | `:8140` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | `SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权 |
| `GORGE_CONFIG_FILE` | `HIGHLIGHT_CONFIG_FILE` | 无 | JSON 配置文件路径 |
| `GORGE_RENDER_MAX_BYTES` | `MAX_BYTES` | `1048576` | 单次请求源码上限（字节） |
| `GORGE_RENDER_TIMEOUT_SEC` | `TIMEOUT_SEC` | `15` | ⚠️ 名不副实，见 [`../findings.md`](../findings.md) 第 1 条 |

命名规则与「新名优先、旧名兜底」的查找机制见 [`../platform.md`](../platform.md) 第 4 节。

`Load()` 的取值顺序：指了 `GORGE_CONFIG_FILE` 就读 JSON 文件，否则读环境变量。走文件时**仍然从环境变量取 `SERVICE_TOKEN`**，其余字段先填默认值再由文件覆盖，这样密钥可以单独注入而不进配置文件。

## 7. 域级错误码

`ERR_HIGHLIGHT_FAILED`(500)，定义在 `internal/render/http.go`：

```go
// CodeHighlightFailed is domain-specific and predates the monorepo. Phorge's
// client already branches on it, so it stays instead of collapsing into
// httpx.CodeInternal.
```

它不会被全局错误处理器改写成 `ERR_INTERNAL`——`httpx.Fail` 写响应前会在 Fiber `Locals` 标记已应答，处理器见到标记就不再落笔（见 [`../platform.md`](../platform.md) 第 1.2 节）。

**新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。**

## 8. 排查时的两处反直觉

- `/api/highlight/**` 分组的**鉴权早于路由解析**，所以不带 token 打一个不存在的路径返回 **401 而非 404**。
- 同样在这个分组下，方法用错返回 **404 而非 405**，因为分组为了鉴权匹配了所有方法。`ERR_METHOD_NOT_ALLOWED` 实际只在健康探针路径上见得到。

对 PHP 侧诊断价值最高的是 `ERR_NOT_FOUND`：`gorge.render.uri` 尾部多一个斜杠、或 base URL 拼出双斜杠，拿到的就是它。

## 9. 覆盖率

覆盖率不在模块文档里维护快照；当前结果用 `make cover` 生成，分层解释见 [`../testing.md`](../testing.md) 第 5 节。
