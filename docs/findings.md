# 偏差与改进建议

代码与文档比对时发现的实际问题。按模块分节，新模块迁入后在下面新开一节，不要混进别的模块。基线 `da522f9`。

修掉一条就把它从这里删掉，别标记成「已完成」留着——这个文件的价值在于短。

---

## 平台层

### 1. `GORGE_RENDER_TIMEOUT_SEC` 并非请求超时（文档不符）

**影响**：高。服务当前实际上没有请求级超时。

`deploy/compose/.env.example` 写的是 "Per-request timeout in seconds"，根 `README.md` 写的是「请求超时（秒）」。但 `cfg.TimeoutSec` 在代码里的唯一用途是：

```go
srv := httpx.New(httpx.Config{
	ListenAddr:      cfg.ListenAddr,
	ShutdownTimeout: time.Duration(cfg.TimeoutSec) * time.Second,
```

它控制的是**优雅关闭的排空时长**。全仓库没有 `middleware.Timeout`，也没有给 `http.Server` 设 `ReadTimeout` / `WriteTimeout`，即单个请求没有超时上限，一个慢客户端可以长期占住连接。

**建议二选一**：把变量按实际语义改名为 `GORGE_SHUTDOWN_TIMEOUT_SEC` 并修正两处文档（改动小）；或者补上请求超时中间件，让文档描述成真（才真正补上防护）。

这条对将来的模块影响更大：render 是纯计算且有 1MiB 输入上限，最坏情况可控；conduit、search 这类要打下游的模块没有超时会直接堆积连接。

### 2. 传输层 BodyLimit 无法通过环境变量配置

**影响**：中。是个运维陷阱。

`httpx.Config.BodyLimit` 在代码里可设，但 `main.go` 没传，于是恒为硬编码的 `2M`。把 `GORGE_RENDER_MAX_BYTES` 调到 2M 以上时，实际生效的上限仍是 2M——**配置看起来生效了但没有**。

根 README 已经描述了这个行为，属于「已知且已记录」，但记录不等于不会踩。建议给平台层加一个 `GORGE_BODY_LIMIT`，默认仍是 `2M`。

---

## render 模块

### 3. `Languages()` 每次请求重建列表

**影响**：低。该端点调用频率低。

```go
func (h *Highlighter) Languages() []string {
	names := lexers.Names(false)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, strings.ToLower(name))
	}
	return result
}
```

Chroma 的 lexer 注册表在进程生命周期内不变，这个列表（数百项）却在每次 `GET /api/highlight/languages` 时重新分配与转小写。用 `sync.Once` 缓存是一行改动。

---

## 文档

### 4. 存在两份过期的技术报告

**影响**：中。会把下一个人引到不存在的路径上。

`go/internal/render/highlight/TECHNICAL_REPORT.md` 与仓库外的 `gorge-highlight/TECHNICAL_REPORT.md` 描述的是**单仓库改造之前**的布局（`internal/config/`、`internal/httpapi/`、`cmd/server/main.go`），这些路径现在都不存在；文中的依赖版本（Chroma v2.14.0、Echo v4.12.0）与运行时基础镜像（alpine 3.20）也已过期。

更要紧的是 `highlight.go` 的包注释仍指向它：

```go
// Package highlight renders source code to Pygments-compatible HTML using
// Chroma. See TECHNICAL_REPORT.md in this directory for the design rationale
```

**建议**：删除 `go/internal/render/highlight/TECHNICAL_REPORT.md`，把包注释改指 `docs/modules/render.md` 与 `compat/phorge/README.md`。

### 5. 兼容测试没有反向指回 `compat/`

**影响**：低，但错过成本高。

`compat/phorge/README.md` 是三条兼容约束的权威描述，守住它们的测试却分散在 `highlight/compat_test.go`、`highlight/highlight_test.go`、`render/http_test.go` 与 12 份固件里。目前只有单向引用（文档提到测试）。

在这些测试文件顶部加一行指回 `compat/phorge/README.md` 的注释，能让「为什么这个断言存在」在改动现场就可见。这正是这批测试最容易被当成冗余删掉的地方——它们断言的都是些看起来无关紧要的 CSS 类名。

### 6. `layering_test.go` 的禁止列表需要人工维护

**影响**：低（当前只有一个域），但随模块数量线性增长。

```go
var forbiddenPrefixes = []string{
	"github.com/soulteary/gorge/go/internal/render",
	"github.com/soulteary/gorge/go/internal/contracts",
}
```

新增域包时忘了加一行，这个测试对新域就是静默失效的。可以改成扫描 `internal/` 下除 `platform` 外的所有目录自动生成列表，这样新域自动被纳入。
