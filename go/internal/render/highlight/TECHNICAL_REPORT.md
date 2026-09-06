# gorge-highlight 技术报告

## 1. 概述

gorge-highlight 是 Gorge 平台中的代码语法高亮微服务，为 Phorge（Phabricator 社区维护分支）提供高性能的代码高亮能力。

该服务的核心目标是替代 Phorge 原有的 Pygments 子进程调用模式——即每次需要高亮代码时，PHP 端都要 fork 一个 Python 进程执行 Pygments。gorge-highlight 将这一过程改为通过 HTTP API 调用独立的 Go 常驻服务，在保持输出完全兼容的前提下，显著提升性能并简化部署。

## 2. 设计动机

### 2.1 原有方案的问题

Phorge 默认通过 `PhutilPygmentsSyntaxHighlighter` 类调用 Pygments：

1. **进程开销**：每次高亮请求都需要 fork Python 子进程，启动 Pygments，处理完毕后销毁。进程创建和 Python 解释器初始化的开销远大于实际高亮计算。
2. **环境依赖**：PHP 服务器上必须安装 Python 和 Pygments 包，增加了运维复杂度和安全攻击面。
3. **扩展困难**：高亮计算与 PHP 应用耦合在同一台机器上，无法独立扩缩容。

### 2.2 gorge-highlight 的解决思路

将高亮逻辑抽取为独立的 HTTP 微服务：

- **常驻进程**：Go 编译的二进制常驻运行，无需反复启动解释器。
- **解耦部署**：作为独立容器运行，可根据负载独立扩缩容。
- **零 Python 依赖**：Phorge PHP 端只需发起 HTTP 请求，不再需要 Python 环境。
- **输出兼容**：生成与 Pygments 完全一致的 CSS 类名 HTML，Phorge 前端无需任何修改。

## 3. 系统架构

### 3.1 整体架构

```
┌─────────────────────────────────┐
│         Phorge (PHP)            │
│                                 │
│  PhutilPygmentsSyntaxHighlighter│
│         │                       │
│         │ HTTP POST             │
│         ▼                       │
│  ┌──────────────────────┐       │
│  │  gorge-highlight     │       │
│  │  :8140               │       │
│  │                      │       │
│  │  Echo HTTP Server    │       │
│  │    ├─ Token Auth     │       │
│  │    └─ Handlers       │       │
│  │         │            │       │
│  │         ▼            │       │
│  │    Highlighter       │       │
│  │    ├─ LexerMap       │       │
│  │    └─ Chroma Engine  │       │
│  └──────────────────────┘       │
└─────────────────────────────────┘
```

### 3.2 模块划分

项目采用 Go 标准布局，分为三个内部模块：

| 模块 | 路径 | 职责 |
|---|---|---|
| config | `internal/config/` | 配置加载与校验 |
| highlight | `internal/highlight/` | 语法高亮核心引擎 |
| httpapi | `internal/httpapi/` | HTTP 路由、认证与请求处理 |

入口程序 `cmd/server/main.go` 负责串联三个模块：加载配置 → 创建高亮器 → 启动 HTTP 服务。

## 4. 核心实现分析

### 4.1 语法高亮引擎

高亮引擎位于 `internal/highlight/highlight.go`，是整个服务最核心的模块。

#### 4.1.1 Chroma 格式化器配置

```go
formatter = html.New(
    html.WithClasses(true),
    html.PreventSurroundingPre(true),
)
defaultStyle = styles.Get("pygments")
```

两个关键配置项的设计意图：

- **`WithClasses(true)`**：让 Chroma 输出使用 CSS 类名（如 `<span class="k">`）而非内联样式（如 `<span style="color:#008000">`）。这样生成的类名体系（`k` = keyword, `n` = name, `nf` = name.function, `nb` = name.builtin 等）与 Pygments 完全一致，可以直接复用 Phorge 已有的 CSS 样式表。

- **`PreventSurroundingPre(true)`**：阻止 Chroma 在输出外层包裹 `<pre>` 或 `<div class="highlight">`。Phorge 前端自行负责外层 DOM 结构，高亮服务只需返回纯粹的 `<span>` 序列。

- **`styles.Get("pygments")`**：指定使用 pygments 风格，确保 token 到 CSS 类名的映射关系与 Pygments 原生输出一致。

#### 4.1.2 高亮处理流程

`Highlight(source, language)` 方法的执行流程：

```
输入 source + language
        │
        ▼
resolveLexer(language)    ← 查询别名映射表
        │
        ▼
lexers.Get(resolved)      ← 尝试精确匹配 Lexer
        │
        ├─ 找到 → 使用该 Lexer
        │
        ▼ (未找到)
lexers.Analyse(source)    ← 根据源码内容自动检测（如 shebang）
        │
        ├─ 检测到 → 使用检测结果
        │
        ▼ (未检测到)
lexers.Fallback           ← 使用纯文本 Lexer 兜底
        │
        ▼
chroma.Coalesce(lexer)    ← 合并相邻同类 token
        │
        ▼
lexer.Tokenise(nil, src)  ← 词法分析，生成 token 流
        │
        ▼
formatter.Format(buf, style, iterator) ← 将 token 流渲染为 HTML
        │
        ▼
返回 Result{HTML, Language}
```

**三级 Lexer 查找策略**确保了在任何输入情况下都能产生有意义的输出：

1. **显式匹配**：客户端指定语言（如 `python`），先经别名映射表转换（如 `py` → `python`），再通过 Chroma 的 `lexers.Get()` 查找对应的 Lexer。
2. **内容分析**：若客户端未指定语言或指定的语言找不到 Lexer，则调用 `lexers.Analyse(source)` 根据源码内容自动检测。此方法会检查 shebang 行（如 `#!/bin/bash`）和内容特征来判断语言。
3. **兜底降级**：若自动检测也失败，使用 `lexers.Fallback`（纯文本 Lexer），保证永远不会返回错误。

`chroma.Coalesce(lexer)` 的作用是将相邻的同类型 token 合并，减少输出 HTML 中的 `<span>` 元素数量，优化输出体积。

#### 4.1.3 语言别名映射表

`buildLexerMap()` 构建了一个包含 100+ 条映射的表，对应 Phorge PHP 端 `PhutilPygmentsSyntaxHighlighter::getPygmentsLexerNameFromLanguageName` 方法中的映射关系。

代表性映射示例：

| 别名 | 目标 Lexer | 说明 |
|---|---|---|
| `cc`, `cxx`, `c++`, `h++`, `hh`, `hpp`, `hxx` | `cpp` | C++ 系列文件扩展名 |
| `py`, `pyw`, `sc`, `tac` | `python` | Python 相关扩展名 |
| `rs` | `rust` | Rust |
| `ts` | `typescript` | TypeScript |
| `sh`, `ksh`, `ebuild`, `eclass` | `bash` | Shell 脚本 |
| `yml` | `yaml` | YAML |
| `h` | `c` | C 头文件 |
| `dockerfile`, `containerfile` | `docker` | Docker 配置 |

这张映射表确保了 Phorge PHP 端发送任何它已知的语言标识符时，Go 服务都能正确识别并使用对应的 Lexer。映射表同时包含了 Pygments 原有的别名和一些现代语言的补充（如 `rs`, `ts`, `tsx`, `kt`, `tf` 等）。

### 4.2 HTTP API 层

#### 4.2.1 路由结构

HTTP 层基于 Echo 框架实现，路由设计如下：

| 方法 | 路径 | 功能 | 认证 |
|---|---|---|---|
| GET | `/` | 健康检查 | 不需要 |
| GET | `/healthz` | 健康检查 | 不需要 |
| POST | `/api/highlight/render` | 渲染高亮 | 需要 |
| GET | `/api/highlight/languages` | 列出语言 | 需要 |

`/api/highlight/*` 路由组通过 `tokenAuth` 中间件统一保护。

#### 4.2.2 认证中间件

```go
func tokenAuth(deps *Deps) echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            if deps.Token == "" {
                return next(c)    // Token 为空时不启用认证
            }
            token := c.Request().Header.Get("X-Service-Token")
            if token == "" {
                token = c.QueryParam("token")
            }
            if token == "" || token != deps.Token {
                return 401 ERR_UNAUTHORIZED
            }
            return next(c)
        }
    }
}
```

设计要点：

- **可选认证**：`ServiceToken` 为空字符串时，中间件直接放行，方便开发和测试。
- **双通道获取**：支持 `X-Service-Token` 请求头和 `?token=` 查询参数两种方式，适配不同场景。

#### 4.2.3 渲染处理器

渲染处理器 `renderHighlight` 包含多层防护：

1. **请求绑定校验**：自动解析 JSON 请求体，格式错误返回 400。
2. **空源码短路**：`source` 为空时直接返回空 HTML，避免无意义计算。
3. **大小限制**：`source` 超过 `MaxBytes` 限制时返回 413，防止超大文件消耗资源。
4. **调用引擎**：将 `source` 和 `language` 传递给 Highlighter 执行高亮。

Echo 框架层面还设置了全局 `BodyLimit("2M")`，作为第一道防线。

### 4.3 配置模块

配置模块提供两种加载方式：

- **`LoadFromEnv()`**：从环境变量读取，适合容器化部署。
- **`LoadFromFile(path)`**：从 JSON 文件读取，未设定的字段使用默认值。

通过 `HIGHLIGHT_CONFIG_FILE` 环境变量控制使用哪种方式。文件加载时仍然会从环境变量获取 `SERVICE_TOKEN` 的默认值，允许将敏感信息单独通过环境变量注入（如 Kubernetes Secrets）。

## 5. Pygments 兼容性设计

兼容性是 gorge-highlight 最核心的设计约束。以下四个维度保证了与 Pygments 的无缝替换：

### 5.1 CSS 类名兼容

Chroma 本身就是 Pygments 的 Go 移植，使用 `pygments` style 时会生成与 Pygments 相同的 CSS 类名体系：

| CSS 类名 | 含义 | 示例 |
|---|---|---|
| `k` | Keyword | `def`, `class`, `import` |
| `n` | Name | 标识符 |
| `nf` | Name.Function | 函数名 |
| `nb` | Name.Builtin | 内置函数如 `print` |
| `s2` | String.Double | 双引号字符串 |
| `s1` | String.Single | 单引号字符串 |
| `mi` | Number.Integer | 整数字面量 |
| `c1` | Comment.Single | 单行注释 |

兼容性测试（`compat_test.go`）显式验证了这些关键类名的存在。

### 5.2 HTML 结构兼容

- 不生成 `<pre>` 包裹元素
- 不生成 `<div class="highlight">` 包裹元素
- 仅输出 `<span class="...">` 序列和文本节点
- Phorge 前端自行负责外层 DOM 结构

### 5.3 语言别名兼容

100+ 条别名映射与 Phorge PHP 端 `PhutilPygmentsSyntaxHighlighter::getPygmentsLexerNameFromLanguageName` 保持同步，确保 PHP 端发送的语言标识符在 Go 端都能正确解析。

### 5.4 换行符处理

测试覆盖了三种换行符格式：

- LF (`\n`)：Unix 标准
- CRLF (`\r\n`)：Windows 标准
- Lone CR (`\r`)：旧 Mac 格式

## 6. 部署方案

### 6.1 Docker 镜像

采用多阶段构建：

- **构建阶段**：基于 `golang:1.26-alpine3.22`，使用 `CGO_ENABLED=0` 静态编译，`-ldflags="-s -w"` 去除调试信息和符号表以缩小二进制体积。
- **运行阶段**：基于 `alpine:3.20`，仅包含编译后的二进制和 CA 证书。

内置 Docker `HEALTHCHECK`，每 10 秒检查一次 `/healthz` 端点。

### 6.2 资源控制

多层资源保护机制：

| 层级 | 机制 | 默认值 |
|---|---|---|
| Echo 框架 | `BodyLimit` 中间件 | 2 MB |
| 应用层 | `MaxBytes` 配置 | 1 MiB |
| 应用层 | `TimeoutSec` 配置 | 15 秒 |

## 7. 依赖分析

| 依赖 | 版本 | 用途 |
|---|---|---|
| `alecthomas/chroma/v2` | v2.14.0 | 语法高亮引擎 |
| `labstack/echo/v4` | v4.12.0 | HTTP 框架 |
| `dlclark/regexp2` | v1.11.0 | Chroma 使用的正则引擎（间接） |

直接依赖仅两个，保持了最小化原则。Chroma 是 Go 生态中最成熟的语法高亮库，本身就是 Pygments 的 Go 移植版本，天然具备与 Pygments 的兼容性基础。

## 8. 测试覆盖

项目包含四组测试文件：

| 测试文件 | 覆盖范围 |
|---|---|
| `config_test.go` | 环境变量默认值、值覆盖、文件加载、文件不存在 |
| `highlight_test.go` | Python/Go 高亮、别名解析、未知语言处理、自动检测、语言列表、空源码、无包裹元素 |
| `compat_test.go` | Pygments CSS 类名兼容性、HTML 结构、CRLF/CR 换行符处理 |
| `handlers_test.go` | 健康检查、401 认证、渲染成功、空源码、过大请求、语言列表、Query 参数认证、无 Token 模式 |

测试设计体现了对关键行为的系统性验证，特别是兼容性测试单独成文件，突出了这一设计约束的重要性。

## 9. 总结

gorge-highlight 是一个设计精巧的代码高亮微服务，核心价值在于：

1. **性能提升**：Go 常驻进程取代 Python 子进程 fork，消除了进程启动和解释器初始化的开销。
2. **部署简化**：静态编译的单一二进制，消除了 Python 环境依赖。
3. **无缝替换**：通过 CSS 类名兼容、语言别名映射、HTML 结构对齐三个维度，确保 Phorge 前端零修改即可切换。
4. **稳健设计**：三级 Lexer 查找、多层资源保护、可选认证等机制，保证了服务在各种边界情况下的稳定运行。
