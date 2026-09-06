# 测试体系

四层，各守一类东西。新模块迁入时四层都要补，其中契约固件是唯一跨语言共享的一层。

| 层 | 位置 | 守什么 | 谁跑 |
|---|---|---|---|
| 单元测试 | `go/internal/**/*_test.go` | 各包行为 | CI |
| 分层测试 | `internal/platform/layering_test.go` | 平台层不反向依赖域包 | CI |
| 契约固件 | `tests/contract/<域>/*.json` + 域内 runner | 线上契约 | CI（Go runner）+ 将来的 PHP runner |
| e2e 冒烟 | `tests/e2e/<域>.sh` | 对着真实运行实例的端到端行为 | 手动 / `make e2e` |

## 1. 分层测试

用 `go/parser` 解析 `platform/` 下所有文件的 import，断言不出现域包与契约包。55 行把一条通常只写在 README 里的架构规则变成 CI 失败。详见 [`architecture.md`](architecture.md) 第 3.1 节——那里也写了**新增域包时要往 `forbiddenPrefixes` 加一行**，这是本层唯一需要人工维护的地方。

## 2. 契约固件：一份描述，两个 runner

固件放在仓库根的 `tests/` 而不是 `go/` 下，因为它们是给 Go 服务与将来的 PHP 适配层**共同**运行的：一份线上契约的描述，两个执行器。

每个子目录归属一个域：

| 目录 | 服务 | Go runner |
|---|---|---|
| `render/` | `gorge-render` | `go/internal/render/contract_test.go` |

### 2.1 格式

一个文件一个 JSON 对象：

```json
{
  "name": "short human-readable name",
  "description": "why this case matters",
  "request": {
    "method": "POST",
    "path": "/api/highlight/render",
    "headers": { "X-Service-Token": "contract-token" },
    "body": "{\"source\":\"x = 1\",\"language\":\"python\"}"
  },
  "expect": {
    "status": 200,
    "jsonHas": ["data.html"],
    "jsonAbsent": ["error"],
    "jsonEquals": { "data.language": "python" },
    "htmlContainsClasses": ["k", "mi"],
    "htmlContains": ["<span"],
    "htmlNotContains": ["<pre>"]
  }
}
```

`request.body` 是**字符串**而非嵌套对象。这是刻意的：嵌套对象无法表达一个格式错误的 payload，而 `render-malformed-body.json` 正需要它。

`expect` 的字段除 `status` 外全部可选，分三组作用域：`json*` 作用于解码后的响应、`html*` 作用于解码出的 `data.html`、`body*` 作用于原始响应体。**检查渲染出的标记时优先用 `html*`**——JSON 编码器对 `<` 的转义方式不同，对原始 body 做子串匹配在 Go 与 PHP runner 之间不可移植。

### 2.2 runner 的前提

runner 必须用 token `contract-token` 启动服务：固件靠这个值认证，且其中一份固件断言不带 token 的请求被拒。其余一律用服务默认值。

Go runner 用 `httptest` 起一个内存中的 `httpx.New(...)` + `RegisterRoutes(...)`，不监听真实端口。

### 2.3 render 域现有的 12 份固件

覆盖：正常渲染（python / go）、别名解析、大小写敏感的两条（`.R` / `.r`）、未知语言、空 source、CRLF、格式错误的请求体、未授权、查询参数认证、语言列表。

## 3. 断言 contains 而非 golden

固件与 `compat_test.go` 都只做包含断言，不做字节精确匹配。理由写在 `tests/contract/README.md` 里，值得重复：

渲染出的 HTML 会随 Chroma 每次升级而变，而且变化方式**不构成回归**——一个 token 裂成两个、空白在 span 之间挪位。字节精确的 golden 文件会在每次 Chroma 升级时失败，然后被**不加阅读地重新录制**，那比没有测试更糟。

真正必须固定不动的是 Pygments CSS 类名集合，因为 Phorge 的样式表是按它写的。`k`、`nf`、`nb`、`s2`、`mi`、`c1` 消失意味着全站代码块失去样式，这才是值得捕捉的回归，`htmlContainsClasses` 捕捉的就是它。

这条原则对将来的模块同样适用：**对不稳定的输出断言不变量，不断言快照。** diff 渲染会面临一模一样的问题。

## 4. e2e 冒烟

`tests/e2e/render.sh` 对着一个**已经在跑**的实例执行，自己不启动也不清理任何东西：

```bash
BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/render.sh
# 或
TOKEN=dev-token make e2e
```

五条场景：存活探针（并断言响应里**不出现** `"data"`，即探针没被套上信封）、就绪探针、无 token 得 401、渲染成功且 HTML 带 `k`/`nf`/`nb`/`mi` 类名、语言列表含 python。`TOKEN` 为空时跳过 401 那条并明确打印 SKIP，而不是静默略过。

它填补的是单元测试与契约固件都够不着的地方：真实的 `main()`、真实的监听端口、真实的容器编排。`cmd/gorge-render` 与 `httpx.Run()` 的覆盖率缺口就靠它兜。

## 5. 覆盖率现状

| 包 | 覆盖率 |
|---|---|
| `platform/auth` | 100.0% |
| `platform/config` | 100.0% |
| `platform/health` | 100.0% |
| `platform/httpx` | 74.1% |
| `render` | 91.7% |
| `render/highlight` | 92.9% |
| `cmd/gorge-render` | 0.0% |
| **总计** | **80.5%** |

两处缺口都是刻意的：`httpx` 缺的是 `Run()` 的信号循环与 `Shutdown` 路径，`cmd` 缺的是 `main()`。两者都需要起真进程，测试成本高于收益，由 e2e 在集成层面覆盖。

生成报告：

```bash
make cover     # 写出 go/coverage.html 并打印 func 级明细
```

CI 每次跑测试都会上传 `coverage.html` 制品并推送到 Codecov。
