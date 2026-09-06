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
| `diff/` | `gorge-render`（同进程） | `go/internal/diff/contract_test.go` |

两个 runner 都只是三行 wrapper，真正的重放逻辑在 `go/internal/contracttest/`。它是 diff 迁入时从 render 的固件测试里抽出来的，抽出的理由不是省代码，而是**断言词汇必须在两个域之间保持一致**——各写一份 runner，两个域很快会开始用不同的方式描述自己的契约。它是普通包而非 `_test.go`，因为要被两个域的测试 import。

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

`json*` 的路径支持数组下标，数字段落即下标：`data.parts.0.type` 能钉住 prose diff 返回的片段序列。`jsonAbsent` 对越界下标返回「不存在」，所以 `data.parts.4` 可以用来断言片段数量。

`html*` 那组只有 render 域在用，但保留在共享词汇里——两个域要用同一套名字描述自己的契约。

### 2.2 runner 的前提

runner 必须用 token `contract-token` 启动服务：固件靠这个值认证，且其中一份固件断言不带 token 的请求被拒。其余一律用服务默认值。

Go runner 用 `httptest` 起一个内存中的 `httpx.New(...)` + `RegisterRoutes(...)`，不监听真实端口。

### 2.3 现有固件

**render 域 12 份**：正常渲染（python / go）、别名解析、大小写敏感的两条（`.R` / `.r`）、未知语言、空 source、CRLF、格式错误的请求体、未授权、查询参数认证、语言列表。

**diff 域 14 份**：hunk 头的三种计数形态、无尾换行的三种组合、identical 分支、normalize、prose 的三条、未授权、查询参数认证、格式错误的请求体。逐条对应见 [`../tests/contract/diff/README.md`](../tests/contract/diff/README.md)。

diff 域**刻意没有**超限固件：两道尺寸护栏都随部署可配，一份断言 413 的固件会随被测服务的启动参数时过时不过，而这正是契约固件不能有的性质。那些路径在 `go/internal/diff/http_test.go` 里覆盖，那里可以设限。

## 3. 断言的精度要按输出的稳定性来定

这一层最容易走极端：要么全做字节精确、要么全做包含。**两个域给出了相反的答案，而两个都是对的。**

### 3.1 render：contains 而非 golden

渲染出的 HTML 会随 Chroma 每次升级而变，而且变化方式**不构成回归**——一个 token 裂成两个、空白在 span 之间挪位。字节精确的 golden 文件会在每次 Chroma 升级时失败，然后被**不加阅读地重新录制**，那比没有测试更糟。

真正必须固定不动的是 Pygments CSS 类名集合，因为 Phorge 的样式表是按它写的。`k`、`nf`、`nb`、`s2`、`mi`、`c1` 消失意味着全站代码块失去样式，这才是值得捕捉的回归，`htmlContainsClasses` 捕捉的就是它。

### 3.2 diff：整值比对

unified diff 的输出不是被渲染的，是被 `ArcanistDiffParser` **解析**的——它读 hunk 头来决定之后每一行的归属。写成 `-1,1` 而 GNU 写 `-1`，仍然是一份语法合法的 unified diff，所以没有任何东西会拒绝它；只是它之后的行全部被归到错误的位置，错误在评审页面上表现为渲染错位，离出错点已经很远。

**这里不存在「可以安全断言的子集」**，所以 `data.diff` 做整值比对。期望值不是手写的，是从真实 `diff -U65535` 抓的（命令写在固件 README 里）。

不止抓一次：`go/internal/diff/unified/systemdiff_test.go` 每次 `go test` 都真的调系统 `diff` 交叉验证，所以格式漂移不依赖谁记得重跑命令。**这一层测出了手写用例没覆盖的一处偏差**——对齐存在多个同样最小的方案时（仅发生于含重复行的输入），引擎选的那个与 GNU 不同。保证因此是分层的：格式规则（hunk 头计数、`\ No newline` 位置）与 GNU 逐字节等同、编辑脚本始终最小，但歧义时的对齐选择不保证相同。实测 hunk 头 0 次不同，所以行号从不错位。完整数据见 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 4.6 节。

这件事本身是这一层价值的例证：**手写用例只能覆盖想到的形状。**

prose diff 落在两者之间：它的输出没有外部基准，所以固件钉住几个小输入的分段，而真正要紧的**无损不变量**（`=`+`-` 还原旧文本、`=`+`+` 还原新文本）交给单元测试用 17 组输入压。

### 3.3 判据

**问输出会不会在无回归的情况下变化。** 会（第三方渲染器的 markup）就断言不变量；不会（被下游解析的格式）就整值比对。搞反任何一边都有代价：前者会训练出「失败了就重新录制」的习惯，后者会让静默的格式漂移一路走到生产。

## 4. e2e 冒烟

两份脚本，都对着一个**已经在跑**的实例执行，自己不启动也不清理任何东西：

```bash
BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/render.sh
BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/diff.sh
# 或
TOKEN=dev-token make e2e     # 两份都跑
```

两个域共用一个端口，所以 `make e2e` 是「两个脚本打同一个 BASE_URL」，不是两套部署。

`render.sh` 六条场景：存活探针（并断言响应里**不出现** `"data"`，即探针没被套上信封）、就绪探针、无 token 得 401、渲染成功且 HTML 带 `k`/`nf`/`nb`/`mi` 类名、语言列表含 python。

`diff.sh` 九条场景：无 token 得 401、四种 hunk 头/标记形态做整值比对、identical 分支、normalize、prose 分段、格式错误的请求体得 400。

两份都在 `TOKEN` 为空时跳过 401 那条并明确打印 SKIP，而不是静默略过。

它填补的是单元测试与契约固件都够不着的地方：真实的 `main()`、真实的监听端口、真实的容器编排。`cmd/gorge-render` 与 `httpx.Run()` 的覆盖率缺口就靠它兜。

`diff.sh` 还有一个单元测试拿不到的作用：`\ No newline at end of file` 这个标记里含反斜杠，是整个 payload 里唯一会被 JSON 转义错误悄悄改坏的部分，而它只有过一趟真实的 HTTP 编解码才验证得到。

## 5. 覆盖率现状

| 包 | 覆盖率 |
|---|---|
| `platform/auth` | 100.0% |
| `platform/config` | 100.0% |
| `platform/health` | 100.0% |
| `platform/httpx` | 74.1% |
| `render` | 91.7% |
| `render/highlight` | 92.9% |
| `diff` | 92.1% |
| `diff/unified` | 100.0% |
| `diff/prose` | 100.0% |
| `contracttest` | 0.0%（见下） |
| `cmd/gorge-render` | 0.0% |
| **总计** | **79.0%** |

两处真实缺口都是刻意的：`httpx` 缺的是 `Run()` 的信号循环与 `Shutdown` 路径，`cmd` 缺的是 `main()`。两者都需要起真进程，测试成本高于收益，由 e2e 在集成层面覆盖。

`contracttest` 的 0.0% 是**度量假象，不是未测代码**：它没有自己的 `_test.go`，而 `go test` 默认只把一个包自己的测试计入该包的覆盖率。这份代码实际上被两个域的固件测试每次都完整跑过。**不要为了让这个数字变好看而给它加测试**——真要度量就用 `-coverpkg`。

生成报告：

```bash
make cover     # 写出 go/coverage.html 并打印 func 级明细
```

CI 每次跑测试都会上传 `coverage.html` 制品并推送到 Codecov。
