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
| `notification/admin/` | `gorge-notification` 的 admin 口 | `go/internal/notification/contract_admin_test.go` |
| `notification/client/` | 同上，client 口 | `go/internal/notification/contract_client_test.go` |
| `mailer/` | `gorge-mailer` | `go/internal/mailer/contract_test.go` |

**notification 一个域两个固件目录**，因为它是一个域两个端口，而同一条请求在两个端口上的正确答案不一样（`GET /` 在 admin 口是 200 探针、在 client 口必须是 501）。合成一个目录就没法表达这件事。

这些 runner 都只是三行 wrapper，真正的重放逻辑在 `go/internal/contracttest/`。它是 diff 迁入时从 render 的固件测试里抽出来的，抽出的理由不是省代码，而是**断言词汇必须在两个域之间保持一致**——各写一份 runner，两个域很快会开始用不同的方式描述自己的契约。它是普通包而非 `_test.go`，因为要被两个域的测试 import。

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

**notification 是这一条的例外**：那个域按设计不挂鉴权中间件（Phorge 的通知客户端不发凭据，配了 token 会让它发的每条消息都被拒），所以它的两个 runner 忽略 `contracttest.Token`，固件目录里也没有 `unauthorized.json`。`contracttest.go` 里那个常量的注释写明了这一点——**别看到少一份未授权固件就去补一份**。

Go runner 用 `httptest` 起一个内存中的 `httpx.New(...)` + `RegisterRoutes(...)`，不监听真实端口。

### 2.3 现有固件

**render 域 12 份**：正常渲染（python / go）、别名解析、大小写敏感的两条（`.R` / `.r`）、未知语言、空 source、CRLF、格式错误的请求体、未授权、查询参数认证、语言列表。

**diff 域 14 份**：hunk 头的三种计数形态、无尾换行的三种组合、identical 分支、normalize、prose 的三条、未授权、查询参数认证、格式错误的请求体。逐条对应见 [`../tests/contract/diff/README.md`](../tests/contract/diff/README.md)。

**notification 域 11 份**：admin 7 份（发消息、form-urlencoded 的 Content-Type、空 body、格式错误的 body、`/status/` 的扁平点号键、带 instance 的 `/status/`、根探针），client 4 份（`GET /` 与实例路径各一条 501、带 Upgrade 头但不是 WebSocket 的一条 501、`/healthz` 不被通配符吃掉）。逐条对应见 [`../tests/contract/notification/README.md`](../tests/contract/notification/README.md)。

这批固件让共享 runner 长了两处：`lookupJSONPath` 现在会**先把整段路径当字面量键查一次**再按 `.` 切分，否则 `clients.active` 这类键寻址不到（那些点是键名的一部分，不是嵌套）；`check` 现在对「只断言状态码与原始字节」的固件跳过 JSON 解码，否则 client 口那句纯文本 501 会在解码那一步就失败。两处都是共享词汇的扩展而非 notification 专用分支。

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

四份脚本，都对着**已经在跑**的实例执行，自己不启动也不清理任何东西：

```bash
BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/render.sh
BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/diff.sh
ADMIN_URL=http://127.0.0.1:22281 CLIENT_URL=http://127.0.0.1:22280 \
  bash tests/e2e/notification.sh
BASE_URL=http://127.0.0.1:8110 TOKEN=dev bash tests/e2e/mailer.sh
# 或
TOKEN=dev-token make e2e     # 四份都跑
```

render 与 diff 共用一个端口，所以那两份是「两个脚本打同一个 `BASE_URL`」，不是两套部署。notification 是另一个进程，而且**要两个变量**：`ADMIN_URL` 与 `CLIENT_URL` 不可互换，同一个请求在两个端口上的正确答案不一样，这正是它第 4、5 条场景要验证的东西。它也没有 `TOKEN`——那个域按设计不鉴权。

`render.sh` 六条场景：存活探针（并断言响应里**不出现** `"data"`，即探针没被套上信封）、就绪探针、无 token 得 401、渲染成功且 HTML 带 `k`/`nf`/`nb`/`mi` 类名、语言列表含 python。

`diff.sh` 九条场景：无 token 得 401、四种 hunk 头/标记形态做整值比对、identical 分支、normalize、prose 分段、格式错误的请求体得 400。

`notification.sh` 五条场景：admin 存活探针、用 curl 的**默认** `Content-Type`（即 form-urlencoded 贴在一段真 JSON 上，Phorge 的实际形状）发消息并拿到 fingerprint、`/status/` 的扁平点号键、client 口纯 HTTP 得 501、client 口真握手得 101。第 2 条是这份脚本存在的主要理由，而它的**payload 才是断言的关键**：里面那个 `100% done` 对表单解析器是非法的百分号转义，所以只有「不看头、直接按 JSON 解」的 handler 才会答 200。把那个百分号「清理」掉，这条场景就退化成一个永远通过的检查——理由见 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 5.4 节。

`mailer.sh` 八条场景：存活探针、就绪探针、无 token 得 401、后端列表、发一封并断言 `data.mailerKey`、带 base64 附件的一封、缺收件人得 400、`mailerKeys` 指向不存在的后端得 502。**它对被测实例有一个额外前提**：必须配了至少一个后端，否则第 2 条按设计就该失败——`/readyz` 报的正是「一个后端都没配」。用 `test` 后端起服务就能满足，`make e2e` 与 compose 的默认值都是它。

render、diff 与 mailer 三份都在 `TOKEN` 为空时跳过 401 那条并明确打印 SKIP，而不是静默略过。

它填补的是单元测试与契约固件都够不着的地方：真实的 `main()`、真实的监听端口、真实的容器编排。两个 `cmd` 包的覆盖率缺口就靠它兜。（`httpx.Run()` 一度也在这个名单上，现在不在了——见第 5 节。）

`diff.sh` 还有一个单元测试拿不到的作用：`\ No newline at end of file` 这个标记里含反斜杠，是整个 payload 里唯一会被 JSON 转义错误悄悄改坏的部分，而它只有过一趟真实的 HTTP 编解码才验证得到。

## 5. 覆盖率现状

| 包 | 覆盖率 |
|---|---|
| `platform/auth` | 100.0% |
| `platform/config` | 100.0% |
| `platform/health` | 100.0% |
| `platform/httpx` | 97.1% |
| `render` | 91.7% |
| `render/highlight` | 92.9% |
| `diff` | 92.1% |
| `diff/unified` | 100.0% |
| `diff/prose` | 100.0% |
| `notification` | 97.1% |
| `notification/hub` | 83.7% |
| `notification/peer` | 96.2% |
| `mailer` | 79.1%（见下） |
| `contracttest` | 20.2%（见下） |
| `cmd/gorge-render` | 0.0% |
| `cmd/gorge-notification` | 0.0% |
| `cmd/gorge-mailer` | 0.0% |
| **总计** | **81.4%** |

`httpx` 从 74.1% 升到 97.1%，是 notification 迁入时给 `RunAll` 补的那批测试带来的：原先被认为「要起真进程才测得到」的信号循环与 `Shutdown` 路径，用 `:0` 端口起真 listener 加真 `SIGTERM` 就覆盖到了。剩下的缺口与两个 `cmd` 的 0.0% 都是刻意的：`main()` 起真进程的成本高于收益，由 e2e 在集成层面兜；`httpx` 剩的三处写在 [`platform.md`](platform.md) 第 5 节。

`contracttest` 的 20.2% 仍然主要是**度量假象，不是未测代码**：它自己的 `_test.go` 只直接测两个纯函数（`lookupJSONPath` 与 `assertsStructure`，都是 notification 固件逼出来的），而重放逻辑本身被各域的固件测试每次完整跑过——`go test` 默认只把一个包自己的测试计入该包覆盖率。**不要为了让这个数字变好看而给它补测试**；那两个纯函数值得直接测，是因为它们的寻址与分派规则本身有分支，不是因为数字。真要度量就用 `-coverpkg`。

`mailer` 的 79.1% 是本表最低的一个真实数字，而它的缺口是**可指名的**：SMTP 的两条发送路径与 SendGrid / Mailgun / Postmark 的 HTTP 往返。永久失败分类本身测到了（`classifyProviderStatus` / `classifySMTPError` 有表驱动用例，sendmail 用 stub 脚本走了真实退出码路径，SES 因为端点可配而用 `httptest` 打了完整一圈），缺的是另外三家 provider 那一圈——它们的端点是编译期常量，测不了。修法与理由写在 [`findings.md`](findings.md) 第 14 条。

`notification/hub` 的 83.7% 有一部分是同一个假象：`Listener` 那几个要真 WebSocket 才调得到的方法，连接建在 `internal/notification` 的测试里，不计入 `hub`。`-coverpkg` 合并度量后它们都是 100%，覆盖率的真实缺口只剩三处，都登记在 [`findings.md`](findings.md) 第 9 条。

生成报告：

```bash
make cover     # 写出 go/coverage.html 并打印 func 级明细
```

CI 每次跑测试都会上传 `coverage.html` 制品并推送到 Codecov。
